package report

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/projects"
)

// Output file names under the run directory.
const (
	SummaryFile    = "summary.md"
	AnchorsFile    = "anchors.jsonl"
	CandidatesFile = "excerpt_candidates.jsonl"
	ProjectsFile   = "projects.jsonl"
)

// DefaultExcerptTokens is the excerpt budget when Input.ExcerptTokens is 0.
const DefaultExcerptTokens = 40000

// Input is everything one run hands to the report.
type Input struct {
	Dataset  *analyze.Dataset
	Metrics  []analyze.SessionMetrics
	Tasks    []analyze.Task
	Previous []analyze.SessionMetrics // last run's, for the trend
	RunID    string
	// ExcerptTokens is the budget for excerpt_candidates.jsonl (sum of
	// est_tokens); 0 means DefaultExcerptTokens.
	ExcerptTokens int
	// Projects is the project context inventory (projects.jsonl), and
	// ExcludedProjects how many excluded projects it leaves out.
	Projects         []projects.Project
	ExcludedProjects int
}

// view is one session prepared for reporting.
type view struct {
	s            *analyze.Session
	key          string
	caps         analyze.Capabilities
	excluded     bool
	registryOnly bool
	win          []int          // indices of in-window events
	ex           []int          // in-window transcript events with a Ref (excerptable)
	pos          map[string]int // Seq -> event index
	anchors      []Anchor       // nil for excluded sessions
	metrics      *analyze.SessionMetrics
	red          *Redactor
	estCache     []int // estimateEvent per event index, -1 until computed
}

func buildViews(in Input, red *Redactor, ex Exclusions) ([]*view, map[string]*view) {
	byKey := map[string]*view{}
	var views []*view
	if in.Dataset == nil {
		return nil, byKey
	}
	for _, s := range in.Dataset.Sessions {
		if s == nil {
			continue
		}
		v := &view{s: s, key: s.Key.String(), caps: caps(s), excluded: ex.Excluded(s), registryOnly: registryOnly(s), pos: map[string]int{}, red: red}
		for i := range s.Events {
			e := &s.Events[i]
			if _, dup := v.pos[e.Seq]; !dup {
				v.pos[e.Seq] = i
			}
			if !in.Dataset.Window.Contains(e.TS) {
				continue
			}
			v.win = append(v.win, i)
			if len(e.Ref) > 0 && e.ObservedBy != analyze.FromRegistry {
				v.ex = append(v.ex, i)
			}
		}
		if !v.excluded {
			v.anchors = sessionAnchors(v, red)
		}
		views = append(views, v)
		byKey[v.key] = v
	}
	sort.Slice(views, func(i, j int) bool { return views[i].key < views[j].key })
	for i := range in.Metrics {
		if v := byKey[in.Metrics[i].Session.String()]; v != nil {
			v.metrics = &in.Metrics[i]
		}
	}
	return views, byKey
}

// Write writes summary.md, anchors.jsonl and excerpt_candidates.jsonl to dir.
// Excluded sessions count in the summary's totals but never appear in
// anchors or candidates; all transcript-derived text is redacted.
func Write(dir string, in Input, red *Redactor, ex Exclusions) error {
	if in.Dataset == nil {
		return errors.New("report: no dataset")
	}
	if red == nil {
		return errors.New("report: no redactor (outputs are never unredacted)")
	}
	if in.ExcerptTokens <= 0 {
		in.ExcerptTokens = DefaultExcerptTokens
	}
	views, byKey := buildViews(in, red, ex)
	anchors := allAnchors(views, byKey, in.Tasks, red)
	hot := hotspots(views)
	cands, notes := candidates(views, hot, in.RunID, in.ExcerptTokens)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	summary := renderSummary(in, views, byKey, hot, notes, red)
	if err := writeFile(filepath.Join(dir, SummaryFile), func(w *bufio.Writer) error {
		_, err := w.WriteString(summary)
		return err
	}); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(dir, AnchorsFile), anchors); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(dir, CandidatesFile), cands); err != nil {
		return err
	}
	prs := make([]projects.Project, len(in.Projects))
	for i, p := range in.Projects {
		prs[i] = redactProject(p, red)
	}
	return writeJSONL(filepath.Join(dir, ProjectsFile), prs)
}

// allAnchors is every non-excluded session's anchors plus handoff points: the
// start of each task member range whose task also has members in another
// tool.
func allAnchors(views []*view, byKey map[string]*view, tasks []analyze.Task, red *Redactor) []Anchor {
	handoffs := map[string][]Anchor{}
	for _, t := range tasks {
		tools := map[string]bool{}
		for _, m := range t.Members {
			tools[m.Session.AdapterID] = true
		}
		if len(tools) < 2 {
			continue
		}
		for _, m := range t.Members {
			v := byKey[m.Session.String()]
			if v == nil || v.excluded {
				continue
			}
			var others []string
			for _, o := range t.Members {
				if o.Session.AdapterID == m.Session.AdapterID {
					continue
				}
				if ov := byKey[o.Session.String()]; ov != nil && ov.excluded {
					others = append(others, "(an excluded session)")
					continue
				}
				others = append(others, o.Session.String())
			}
			others = uniqSorted(others)
			i, ok := v.pos[m.FromSeq]
			if !ok {
				continue
			}
			text := "candidate task " + t.ID + " (" + t.Confidence + ") continues in other tools: " + joinComma(others)
			handoffs[v.key] = append(handoffs[v.key], Anchor{
				Session: v.key, Kind: AnchorHandoff, Seq: m.FromSeq,
				Text: red.Apply(text), idx: i,
			})
		}
	}
	var out []Anchor
	for _, v := range views {
		if v.excluded {
			continue
		}
		as := append(append([]Anchor(nil), v.anchors...), handoffs[v.key]...)
		sortAnchors(as)
		out = append(out, as...)
	}
	return out
}

func uniqSorted(ss []string) []string {
	sort.Strings(ss)
	out := ss[:0]
	for i, s := range ss {
		if i == 0 || s != ss[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func writeFile(path string, fill func(*bufio.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if err := fill(w); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeJSONL[T any](path string, rows []T) error {
	return writeFile(path, func(w *bufio.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		for i := range rows {
			if err := enc.Encode(rows[i]); err != nil {
				return err
			}
		}
		return nil
	})
}
