// Package runner wires the analyzer modules into `cbrt analyze run`:
// collect sessions, compute metrics and candidate tasks, update the durable
// state, and write the reviewer's inputs to out/<run-id>/.
package runner

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/collect"
	"github.com/alex-danilin/cbrt/internal/analyze/metrics"
	"github.com/alex-danilin/cbrt/internal/analyze/projects"
	"github.com/alex-danilin/cbrt/internal/analyze/report"
	"github.com/alex-danilin/cbrt/internal/analyze/review"
	"github.com/alex-danilin/cbrt/internal/analyze/state"
	"github.com/alex-danilin/cbrt/internal/analyze/tasks"
)

// Options select what one run looks at.
type Options struct {
	Since   string   // "14d", an RFC 3339 time, or "" (since the last run; first run: config window)
	Tools   []string // adapter IDs; empty = all
	Project string   // substring of the session's project root; "" = all
	NoCache bool
	Now     func() time.Time
}

// Result says where the outputs went and what was read.
type Result struct {
	RunID    string
	OutDir   string
	Window   analyze.Window
	Sessions int
	Events   int
	Problems int
	Notes    []string // things the user should know, one line each
}

// Run performs one analyzer run under home.
func Run(home string, o Options) (Result, error) {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	t := now().UTC()
	cfg, err := analyze.LoadConfig(home)
	if err != nil {
		return Result{}, err
	}
	w, err := window(home, cfg, o.Since, t)
	if err != nil {
		return Result{}, err
	}
	adapters, err := pick(o.Tools)
	if err != nil {
		return Result{}, err
	}
	ds, err := collect.Collect(cfg, home, w, collect.Options{Adapters: adapters, NoCache: o.NoCache, Now: now})
	if err != nil {
		return Result{}, err
	}
	if o.Project != "" {
		keep := ds.Sessions[:0]
		for _, s := range ds.Sessions {
			if strings.Contains(strings.ToLower(s.Project), strings.ToLower(o.Project)) {
				keep = append(keep, s)
			}
		}
		ds.Sessions = keep
	}

	ms := sessionMetrics(ds)
	overrides, err := tasks.LoadOverrides(filepath.Join(home, "task_overrides.json"))
	if err != nil {
		return Result{}, err
	}
	ids, err := state.LoadIdentity(home)
	if err != nil {
		return Result{}, err
	}
	tks, ids := tasks.Group(ds.Sessions, ds.Window, cfg.RangeGapHours, overrides, ids)
	if err := state.SaveIdentity(home, ids); err != nil {
		return Result{}, err
	}

	// The previous window's archived metrics, for the trend.
	span := ds.Window.Until.Sub(ds.Window.Since)
	prev, err := previous(home, ds.Window.Since.Add(-span), ds.Window.Since)
	if err != nil {
		return Result{}, err
	}
	// The archive freezes sessions whose transcript is gone, so it is only
	// updated by full runs: a --tools or --project run would freeze the rest.
	if len(o.Tools) == 0 && o.Project == "" {
		present := map[analyze.SessionKey]bool{}
		for _, k := range ds.Present {
			present[k] = true
		}
		if err := state.UpsertArchive(home, ms, present, t); err != nil {
			return Result{}, err
		}
	}

	red, err := report.LoadRedactor(home)
	if err != nil {
		return Result{}, err
	}
	ex, err := report.LoadExclusions(home, *cfg.ExcludeHomeProject)
	if err != nil {
		return Result{}, err
	}
	// The project context inventory: every project with a session in the
	// window that is not excluded.
	perProject, excluded := map[string]int{}, map[string]bool{}
	for _, s := range ds.Sessions {
		if ex.Excluded(s) {
			excluded[s.Project] = true
			continue
		}
		perProject[s.Project]++
	}
	nExcluded := 0
	for p := range excluded {
		if _, ok := perProject[p]; !ok {
			nExcluded++
		}
	}
	inv := projects.Inventory(perProject, projects.Options{Adapters: adapters, Roots: cfg.Roots})

	runID := state.NewRunID(t)
	out := filepath.Join(home, "out", runID)
	if err := report.Write(out, report.Input{
		Dataset: ds, Metrics: ms, Tasks: tks, Previous: prev, RunID: runID, ExcerptTokens: cfg.ExcerptTokens,
		Projects: inv, ExcludedProjects: nExcluded,
	}, red, ex); err != nil {
		return Result{}, err
	}
	var notes []string
	if _, note, err := review.InstallRubric(home, t); err != nil {
		return Result{}, err
	} else if note != "" {
		notes = append(notes, note)
	}
	recs, err := state.LoadRecs(home)
	if err != nil {
		return Result{}, err
	}
	if err := state.WriteExport(home, recs); err != nil {
		return Result{}, err
	}

	res := Result{RunID: runID, OutDir: out, Window: ds.Window, Sessions: len(ds.Sessions), Problems: len(ds.Problems), Notes: notes}
	versions := map[string]string{}
	for _, a := range adapters {
		versions[a.ID()] = a.Version()
	}
	for _, s := range ds.Sessions {
		res.Events += len(s.Events)
	}
	err = state.AppendRun(home, state.Run{
		RunID: runID, RanAt: t, Window: ds.Window, Sources: len(ds.Present), Sessions: res.Sessions, Events: res.Events,
		AdapterVersions: versions, MetricsVersion: metrics.Version, RubricVersion: review.RubricVersion,
		RedactionVersion: report.RedactionVersion, Tools: o.Tools, Project: o.Project,
	})
	return res, err
}

// window resolves --since: "Nd", an RFC 3339 time, or since the end of the
// last full run (state.Run.Cursor; first run: the configured number of days).
// Filtered runs and review note lines never move the cursor, so a default run
// after a --tools or --project run still covers the other tools and projects.
func window(home string, cfg analyze.Config, since string, now time.Time) (analyze.Window, error) {
	w := analyze.Window{Until: now}
	switch {
	case since == "":
		runs, err := state.LoadRuns(home)
		if err != nil {
			return w, err
		}
		w.Since = now.AddDate(0, 0, -cfg.WindowDays)
		for i := len(runs) - 1; i >= 0; i-- {
			if !runs[i].Cursor() {
				continue
			}
			if u := runs[i].Window.Until; u.Before(now) && u.After(w.Since.AddDate(0, 0, -cfg.WindowDays)) {
				w.Since = u
			}
			break
		}
	case strings.HasSuffix(since, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(since, "d"))
		if err != nil || n <= 0 {
			return w, fmt.Errorf("--since %q: want a number of days like 14d, or an RFC 3339 time", since)
		}
		w.Since = now.AddDate(0, 0, -n)
	default:
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return w, fmt.Errorf("--since %q: want a number of days like 14d, or an RFC 3339 time", since)
		}
		w.Since = t.UTC()
	}
	if !w.Since.Before(w.Until) {
		return w, errors.New("the window is empty: --since must be in the past")
	}
	return w, nil
}

func pick(tools []string) ([]analyze.Adapter, error) {
	all := analyze.Adapters()
	if len(tools) == 0 {
		return all, nil
	}
	var out []analyze.Adapter
	for _, id := range tools {
		a, ok := analyze.Get(id)
		if !ok {
			var ids []string
			for _, x := range all {
				ids = append(ids, x.ID())
			}
			return nil, fmt.Errorf("unknown tool %q (have %s)", id, strings.Join(ids, ", "))
		}
		out = append(out, a)
	}
	return out, nil
}

// sessionMetrics computes metrics for every main session (with its subagents
// folded in) and every subagent session on its own.
func sessionMetrics(ds *analyze.Dataset) []analyze.SessionMetrics {
	children := map[analyze.SessionKey][]*analyze.Session{}
	for _, s := range ds.Sessions {
		if s.Parent != nil {
			children[*s.Parent] = append(children[*s.Parent], s)
		}
	}
	ms := make([]analyze.SessionMetrics, 0, len(ds.Sessions))
	for _, s := range ds.Sessions {
		if kids := children[s.Key]; len(kids) > 0 {
			ms = append(ms, metrics.WithChildren(s, kids, ds.Window))
		} else {
			ms = append(ms, metrics.Session(s, ds.Window))
		}
	}
	return ms
}

// previous returns archived metrics of sessions whose last event fell in
// [since, until).
func previous(home string, since, until time.Time) ([]analyze.SessionMetrics, error) {
	entries, err := state.LoadArchive(home, since)
	if err != nil {
		return nil, err
	}
	var out []analyze.SessionMetrics
	for _, e := range entries {
		v, ok := e.Metrics.Values[metrics.LastEventAt]
		if !ok {
			continue
		}
		at := time.Unix(int64(v.N), 0).UTC()
		if !at.Before(since) && at.Before(until) {
			out = append(out, e.Metrics)
		}
	}
	slices.SortFunc(out, func(a, b analyze.SessionMetrics) int { return strings.Compare(a.Session.String(), b.Session.String()) })
	return out, nil
}

// FindSession collects every session of one adapter (all time) and returns
// the one with the given key "<adapter>/<store>/<native-id>".
func FindSession(home, key string, now time.Time) (*analyze.Session, error) {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("session %q: want <tool>/<store>/<id> as summary.md and anchors.jsonl print it", key)
	}
	a, ok := analyze.Get(parts[0])
	if !ok {
		return nil, fmt.Errorf("session %q: unknown tool %q", key, parts[0])
	}
	cfg, err := analyze.LoadConfig(home)
	if err != nil {
		return nil, err
	}
	w := analyze.Window{Since: time.Unix(0, 0).UTC(), Until: now.Add(time.Hour)}
	ds, err := collect.Collect(cfg, home, w, collect.Options{Adapters: []analyze.Adapter{a}})
	if err != nil {
		return nil, err
	}
	for _, s := range ds.Sessions {
		if s.Key.String() == key {
			return s, nil
		}
	}
	return nil, fmt.Errorf("session %q not found (its transcript may have been deleted)", key)
}
