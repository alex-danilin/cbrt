// Package collect builds the analyzer's Dataset: it discovers every adapter's
// sources, parses them (through the optional parse cache), adds the hook
// registry's events, groups events into sessions, applies the reporting
// window and joins cbrt's usage records to model requests (§3.2.1, §3.5).
package collect

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/hook"
	"github.com/alex-danilin/cbrt/internal/usage"
)

// Session flags.
const (
	FlagBranchDrift     = "branch_drift"
	FlagBranchPartial   = "branch_partial"
	FlagModelUnverified = "model_unverified"
)

// Options adjusts Collect.
type Options struct {
	// Adapters to read; nil means every registered adapter (analyze.Adapters).
	Adapters []analyze.Adapter
	// NoCache neither reads nor writes the parse cache.
	NoCache bool
	// Now fills a zero Window.Until (and, with it, a zero Window.Since as
	// Until minus cfg.WindowDays). Default time.Now.
	Now func() time.Time
}

// Collect reads every source and registry envelope and returns the sessions
// with at least one event inside w, each with its full event history. Problems
// (unreadable sources, bad spool files) name sources and files, never content.
// The returned error is reserved for failures to read the analyzer home itself.
func Collect(cfg analyze.Config, home string, w analyze.Window, opts Options) (*analyze.Dataset, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	adapters := opts.Adapters
	if adapters == nil {
		adapters = analyze.Adapters()
	}
	w = effectiveWindow(cfg, w, opts.Now)

	c := &collector{
		cfg:      cfg,
		home:     home,
		noCache:  opts.NoCache,
		adapters: map[string]analyze.Adapter{},
		sessions: map[analyze.SessionKey]*building{},
	}
	for _, a := range adapters {
		if cfg.Tools[a.ID()].Disabled {
			continue
		}
		c.adapters[a.ID()] = a
		c.order = append(c.order, a)
	}
	for _, a := range c.order {
		c.readAdapter(a)
	}
	if err := c.readRegistry(); err != nil {
		return nil, err
	}

	ds := &analyze.Dataset{Window: w}
	for _, b := range c.sessions {
		s := b.finish()
		if len(s.Source.Members) > 0 {
			ds.Present = append(ds.Present, s.Key) // its transcript still exists
		}
		if inWindow(s, w) {
			ds.Sessions = append(ds.Sessions, s)
		}
	}
	sort.Slice(ds.Sessions, func(i, j int) bool {
		return ds.Sessions[i].Key.String() < ds.Sessions[j].Key.String()
	})
	sort.Slice(ds.Present, func(i, j int) bool { return ds.Present[i].String() < ds.Present[j].String() })

	idx, bySession, problems := readCbrt(cfg.CbrtDataDirs)
	c.problems = append(c.problems, problems...)
	for _, s := range ds.Sessions {
		joinServed(s, idx, bySession)
	}

	sort.Strings(c.problems)
	ds.Problems = slices.Compact(c.problems)
	return ds, nil
}

func effectiveWindow(cfg analyze.Config, w analyze.Window, now func() time.Time) analyze.Window {
	if w.Until.IsZero() {
		w.Until = now().UTC()
	}
	if w.Since.IsZero() {
		days := cfg.WindowDays
		if days <= 0 {
			days = 7
		}
		w.Since = w.Until.AddDate(0, 0, -days)
	}
	return w
}

type collector struct {
	cfg      analyze.Config
	home     string
	noCache  bool
	adapters map[string]analyze.Adapter
	order    []analyze.Adapter
	sessions map[analyze.SessionKey]*building
	problems []string
}

// building is a session while events are still being added.
type building struct {
	key    analyze.SessionKey
	parent string // ParentSessionID, "" for a main session
	caps   analyze.Capabilities
	source analyze.Source
	seen   map[string]bool // Seq
	events []analyze.Event
}

func keyOf(e analyze.Event) analyze.SessionKey {
	return analyze.SessionKey{AdapterID: e.Tool, Store: e.Store, NativeID: e.NativeSessionID}
}

func (c *collector) session(a analyze.Adapter, key analyze.SessionKey, src analyze.Source) *building {
	b := c.sessions[key]
	if b == nil {
		b = &building{key: key, caps: a.Capabilities(), source: src, seen: map[string]bool{}}
		c.sessions[key] = b
	}
	return b
}

// add appends e unless the session already has an event with its Seq (the
// first one wins: duplicates by natural key are dropped).
func (b *building) add(e analyze.Event) {
	if b.seen[e.Seq] {
		return
	}
	b.seen[e.Seq] = true
	if b.parent == "" && e.ParentSessionID != "" {
		b.parent = e.ParentSessionID
	}
	b.events = append(b.events, e)
}

func sourceName(src analyze.Source) string {
	return src.AdapterID + "/" + src.Store + "/" + src.NativeSessionID
}

// problemReason is err's message for the problem list, with file locations
// reduced to base names: a path error says what failed on which file name,
// and any of paths quoted in a message is cut to its base name.
func problemReason(err error, paths ...string) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + " " + baseName(pe.Path) + ": " + pe.Err.Error()
	}
	msg := err.Error()
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) }) // longest first
	for _, p := range paths {
		if p != "" {
			msg = strings.ReplaceAll(msg, p, baseName(p))
		}
	}
	return msg
}

// baseName is the last element of a path written with either separator.
func baseName(p string) string {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return p
	}
	return parts[len(parts)-1]
}

func (c *collector) readAdapter(a analyze.Adapter) {
	roots := c.cfg.Roots(a)
	srcs, err := a.Discover(roots)
	if err != nil {
		c.problems = append(c.problems, a.ID()+": discovery failed: "+problemReason(err, roots...))
		// Discover may still have returned what it found before the error.
	}
	for _, src := range srcs {
		evs, err := c.sourceEvents(a, src)
		if err != nil {
			paths := []string{src.Locator}
			for _, m := range src.Members {
				paths = append(paths, m.Path)
			}
			c.problems = append(c.problems, "source "+sourceName(src)+": unreadable: "+problemReason(err, paths...))
			continue
		}
		for _, e := range evs {
			c.session(a, keyOf(e), src).add(e)
		}
	}
}

// readRegistry routes every spooled envelope to its adapter and attaches the
// resulting events to their sessions, creating registry-only sessions for
// sessions no transcript has.
func (c *collector) readRegistry() error {
	envs, problems, err := hook.ReadSpool(c.home)
	if err != nil {
		return fmt.Errorf("reading the hook registry: %w", err)
	}
	for _, p := range problems {
		c.problems = append(c.problems, "registry: "+p)
	}
	for _, env := range envs {
		name := hook.SpoolName(env)
		a, ok := c.adapters[env.AdapterID]
		if !ok {
			if _, registered := analyze.Get(env.AdapterID); !registered {
				c.problems = append(c.problems, fmt.Sprintf("registry: %s: unknown adapter %q", name, env.AdapterID))
			}
			continue // a disabled or deselected adapter
		}
		evs, err := a.NormalizeHook(env)
		if err != nil {
			c.problems = append(c.problems, "registry: "+name+": not understood by "+a.ID())
			continue
		}
		for _, e := range evs {
			key := keyOf(e)
			src := analyze.Source{
				AdapterID: key.AdapterID, Store: key.Store, NativeSessionID: key.NativeID,
				Locator: "registry:" + key.NativeID,
			}
			c.session(a, key, src).add(e)
		}
	}
	return nil
}

// finish sorts the events and derives the session's fields.
func (b *building) finish() *analyze.Session {
	sort.SliceStable(b.events, func(i, j int) bool { return b.events[i].TS.Before(b.events[j].TS) })
	s := &analyze.Session{
		Key:          b.key,
		Origin:       origin(b.events),
		Project:      project(b.events),
		Events:       b.events,
		Capabilities: b.caps,
		Source:       b.source,
	}
	if b.parent != "" {
		s.Parent = &analyze.SessionKey{AdapterID: b.key.AdapterID, Store: b.key.Store, NativeID: b.parent}
	}
	s.LinkedTo = link(b.events)
	s.Flags = branchFlags(b.events)
	return s
}

// origin is the first known origin among the events, else unknown.
func origin(evs []analyze.Event) string {
	for _, e := range evs {
		if e.Origin != "" && e.Origin != analyze.OriginUnknown {
			return e.Origin
		}
	}
	return analyze.OriginUnknown
}

// project is the most frequent non-empty Cwd, normalized; ties go to the
// lexically smallest.
func project(evs []analyze.Event) string {
	counts := map[string]int{}
	for _, e := range evs {
		if e.Cwd != "" {
			counts[normPath(e.Cwd)]++
		}
	}
	best, n := "", 0
	for p, k := range counts {
		if k > n || (k == n && p < best) {
			best, n = p, k
		}
	}
	return best
}

func normPath(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// link maps each registry event to the transcript event with the same
// CorrelationID: the first one in event order (a tool call before its
// result). Never by time.
func link(evs []analyze.Event) map[string]string {
	first := map[string]string{}
	for _, e := range evs {
		if e.ObservedBy != analyze.FromRegistry && e.CorrelationID != "" {
			if _, ok := first[e.CorrelationID]; !ok {
				first[e.CorrelationID] = e.Seq
			}
		}
	}
	var out map[string]string
	for _, e := range evs {
		if e.ObservedBy != analyze.FromRegistry || e.CorrelationID == "" {
			continue
		}
		if seq, ok := first[e.CorrelationID]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[e.Seq] = seq
		}
	}
	return out
}

// branchFlags counts branch observations: every Measured event is one, but
// MeasuredAtStart events all repeat the one observation made at session
// start (Codex), so together they count once.
func branchFlags(evs []analyze.Event) []string {
	n := 0
	atStart := false
	branches := map[string]bool{}
	for _, e := range evs {
		if e.Branch == "" || (e.BranchCoverage != analyze.Measured && e.BranchCoverage != analyze.MeasuredAtStart) {
			continue
		}
		if e.BranchCoverage == analyze.MeasuredAtStart {
			if atStart {
				continue
			}
			atStart = true
		}
		n++
		branches[e.Branch] = true
	}
	switch {
	case len(branches) > 1:
		return []string{FlagBranchDrift}
	case n < 2:
		return []string{FlagBranchPartial}
	}
	return nil
}

func inWindow(s *analyze.Session, w analyze.Window) bool {
	for _, e := range s.Events {
		if w.Contains(e.TS) {
			return true
		}
	}
	return false
}

// readCbrt indexes every attempts-*.jsonl record under dirs by ResponseID
// (the first record of an id wins, files in name order) and collects the
// sessions cbrt saw. Missing directories are fine.
func readCbrt(dirs []string) (byResponse map[string]usage.Record, sessions map[string]bool, problems []string) {
	byResponse = map[string]usage.Record{}
	sessions = map[string]bool{}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "attempts-*.jsonl"))
		if err != nil {
			continue
		}
		sort.Strings(files)
		for _, f := range files {
			bad, err := readAttempts(f, byResponse, sessions)
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case err != nil:
				problems = append(problems, "cbrt: "+filepath.Base(f)+": unreadable")
			case bad > 0:
				problems = append(problems, fmt.Sprintf("cbrt: %s: %d malformed lines", filepath.Base(f), bad))
			}
		}
	}
	return byResponse, sessions, problems
}

func readAttempts(path string, byResponse map[string]usage.Record, sessions map[string]bool) (bad int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), len(data)+1)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r usage.Record
		if json.Unmarshal(line, &r) != nil {
			bad++
			continue
		}
		if r.Session != "" {
			sessions[r.Session] = true
		}
		if r.ResponseID == "" {
			continue
		}
		if _, dup := byResponse[r.ResponseID]; !dup {
			byResponse[r.ResponseID] = r
		}
	}
	return bad, sc.Err()
}

// joinServed fills s.Served from the records whose ResponseID is a
// model_request's CorrelationID, and flags model_unverified when cbrt saw
// the session but some model request has no record.
func joinServed(s *analyze.Session, idx map[string]usage.Record, cbrtSessions map[string]bool) {
	unmatched := false
	for _, e := range s.Events {
		if e.Kind != analyze.KindModelRequest {
			continue
		}
		r, ok := idx[e.CorrelationID]
		if e.CorrelationID == "" || !ok {
			unmatched = true
			continue
		}
		if s.Served == nil {
			s.Served = map[string]analyze.Served{}
		}
		s.Served[e.Seq] = analyze.Served{
			ResponseID: r.ResponseID,
			Provider:   r.Provider,
			Model:      r.Model,
			Route:      r.Route,
			Location:   r.Location,
			Fallback:   r.AttemptNo > 1,
			Outcome:    string(r.Outcome),
			TS:         r.TS.UTC(),
		}
	}
	if unmatched && cbrtSessions[s.Key.NativeID] {
		s.Flags = append(s.Flags, FlagModelUnverified)
		sort.Strings(s.Flags)
	}
}
