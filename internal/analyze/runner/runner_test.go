package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	_ "github.com/alex-danilin/cbrt/internal/analyze/adapters/all"
	"github.com/alex-danilin/cbrt/internal/analyze/collect"
	"github.com/alex-danilin/cbrt/internal/analyze/report"
	"github.com/alex-danilin/cbrt/internal/analyze/state"
)

// fixtureHome is an analyzer home whose config points every adapter at the
// synthetic fixtures (and at no real store, no cbrt data).
func fixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	abs := func(p string) string {
		a, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "analyze", p))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	cfg := map[string]any{
		"v": 1, "cbrt_data_dirs": []string{filepath.Join(home, "no-cbrt")},
		"tools": map[string]any{
			"claude":      map[string]any{"roots": []string{abs("claude/projects")}},
			"codex":       map[string]any{"roots": []string{abs("codex/sessions")}},
			"antigravity": map[string]any{"roots": []string{abs("antigravity/antigravity/brain"), abs("antigravity/antigravity-cli/brain")}},
			"fixturetool": map[string]any{"roots": []string{home}},
		},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestRunEndToEnd(t *testing.T) {
	home := fixtureHome(t)
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	res, err := Run(home, Options{Since: "120d", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions == 0 || res.Events == 0 {
		t.Fatalf("nothing collected: %+v", res)
	}
	for _, f := range []string{"summary.md", "anchors.jsonl", "excerpt_candidates.jsonl", "projects.jsonl"} {
		if st, err := os.Stat(filepath.Join(res.OutDir, f)); err != nil || st.Size() == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, f := range []string{"RUBRIC.md", "recommendations.md", "runs.jsonl"} {
		if _, err := os.Stat(filepath.Join(home, f)); err != nil {
			if _, err2 := os.Stat(filepath.Join(home, "state", f)); err2 != nil {
				t.Errorf("%s not written: %v", f, err)
			}
		}
	}
	runs, err := state.LoadRuns(home)
	if err != nil || len(runs) != 1 || runs[0].RunID != res.RunID || runs[0].MetricsVersion == "" {
		t.Fatalf("runs: %v %+v", err, runs)
	}
	ids, err := state.LoadIdentity(home)
	if err != nil || len(ids.ByFound) == 0 {
		t.Fatalf("task identity not saved: %v", err)
	}
	if entries, err := state.LoadArchive(home, time.Time{}); err != nil || len(entries) == 0 {
		t.Fatalf("archive: %v, %d entries", err, len(entries))
	}

	// The next run starts where the last one ended.
	later := now.Add(48 * time.Hour)
	res2, err := Run(home, Options{Now: func() time.Time { return later }})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Window.Since.Equal(now) || !res2.Window.Until.Equal(later) {
		t.Errorf("second window %+v, want %v..%v", res2.Window, now, later)
	}
}

func TestRunOptions(t *testing.T) {
	home := fixtureHome(t)
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	at := func() time.Time { return now }
	if _, err := Run(home, Options{Tools: []string{"nope"}, Now: at}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("unknown tool: %v", err)
	}
	if _, err := Run(home, Options{Since: "soon", Now: at}); err == nil {
		t.Error("bad --since accepted")
	}
	res, err := Run(home, Options{Since: "120d", Tools: []string{"codex"}, Now: at})
	if err != nil || res.Sessions == 0 {
		t.Fatalf("codex only: %v %+v", err, res)
	}
	// A partial run never touches the archive.
	if entries, _ := state.LoadArchive(home, time.Time{}); len(entries) != 0 {
		t.Errorf("a --tools run wrote %d archive entries", len(entries))
	}
}

// A default run starts at the end of the last full run: --tools and
// --project runs and a windowless review note line in between do not move
// the cursor, so sessions from before them are still read.
func TestCursorSkipsFilteredRuns(t *testing.T) {
	home := fixtureHome(t)
	day := func(d int) time.Time { return time.Date(2026, 3, d, 0, 0, 0, 0, time.UTC) }
	run := func(o Options, at time.Time) Result {
		t.Helper()
		o.Now = func() time.Time { return at }
		res, err := Run(home, o)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// The fixtures have sessions on March 2, after the full run's end.
	full := run(Options{Since: "3d"}, day(2))
	run(Options{Tools: []string{"codex"}}, day(3))
	run(Options{Project: "no-such-project"}, day(4))
	if err := state.AppendRun(home, state.Run{RunID: full.RunID, RanAt: day(4), ReviewTokensSpent: 100, Notes: "review"}); err != nil {
		t.Fatal(err)
	}
	runs, err := state.LoadRuns(home)
	if err != nil || len(runs) != 4 || len(runs[1].Tools) != 1 || runs[2].Project != "no-such-project" || runs[0].Tools != nil {
		t.Fatalf("runs = %+v, %v", runs, err)
	}

	res := run(Options{}, day(5))
	if !res.Window.Since.Equal(full.Window.Until) {
		t.Fatalf("default run starts %v, want the last full run's end %v", res.Window.Since, full.Window.Until)
	}
	// The same window from a fresh home reads the same sessions.
	want, err := Run(fixtureHome(t), Options{Since: full.Window.Until.Format(time.RFC3339), Now: func() time.Time { return day(5) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions == 0 || res.Sessions != want.Sessions {
		t.Errorf("default run read %d sessions, a fresh run over the same window %d", res.Sessions, want.Sessions)
	}
}

func TestFindSession(t *testing.T) {
	home := fixtureHome(t)
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	ds := mustRunDataset(t, home, now)
	key := ds.Sessions[0].Key.String()
	s, err := FindSession(home, key, now)
	if err != nil || s.Key.String() != key {
		t.Fatalf("FindSession(%s): %v", key, err)
	}
	if _, err := FindSession(home, "claude/default/missing", now); err == nil {
		t.Error("missing session found")
	}
	if _, err := FindSession(home, "nonsense", now); err == nil {
		t.Error("malformed key accepted")
	}
}

func mustRunDataset(t *testing.T, home string, now time.Time) *analyze.Dataset {
	t.Helper()
	cfg, err := analyze.LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := analyze.Get("claude")
	w := analyze.Window{Since: time.Unix(0, 0).UTC(), Until: now}
	ds, err := collect.Collect(cfg, home, w, collect.Options{Adapters: []analyze.Adapter{a}, NoCache: true})
	if err != nil || len(ds.Sessions) == 0 {
		t.Fatalf("collect: %v", err)
	}
	return ds
}

// Every candidate's est_tokens predicts the size of the excerpt the reviewer
// gets for it (default context) within 25%; samples start at a user prompt
// of a top-level session; every listed hotspot says whether it got a range.
func TestCandidatesMatchExcerpts(t *testing.T) {
	home := fixtureHome(t)
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	res, err := Run(home, Options{Since: "120d", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(res.OutDir, report.CandidatesFile))
	if err != nil {
		t.Fatal(err)
	}
	red, err := report.LoadRedactor(home)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var c report.Candidate
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		kinds[c.Kind]++
		s, err := FindSession(home, c.Session, now)
		if err != nil {
			t.Fatal(err)
		}
		out, err := report.Excerpt(s, c.FromSeq, c.ToSeq, red, report.DefaultContext)
		if err != nil {
			t.Fatalf("%s %s..%s: %v", c.Session, c.FromSeq, c.ToSeq, err)
		}
		got := analyze.TextVolume(out)
		if diff := got - c.EstTokens; diff*4 > got || -diff*4 > got {
			t.Errorf("%s %s %s..%s: est_tokens %d, excerpt %d", c.Kind, c.Session, c.FromSeq, c.ToSeq, c.EstTokens, got)
		}
		if strings.Contains(out, "iVBORw0KGgo") || strings.Contains(out, "skill-fake") {
			t.Errorf("%s: excerpt prints image data or injected context", c.Session)
		}
		if strings.HasPrefix(c.Kind, "sample") {
			if s.Parent != nil {
				t.Errorf("%s: a subagent session was sampled", c.Session)
			}
			for _, e := range s.Events {
				if e.Seq == c.FromSeq && e.Kind != analyze.KindUserPrompt {
					t.Errorf("%s: sample starts at %s, not a user prompt", c.Session, e.Kind)
				}
			}
		}
	}
	t.Logf("candidates by kind: %v", kinds)
	if kinds["sample_ordinary"] == 0 || kinds["hotspot"] == 0 {
		t.Errorf("candidate kinds %v: the fixtures should yield hotspots and ordinary samples", kinds)
	}

	sum, err := os.ReadFile(filepath.Join(res.OutDir, report.SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	_, hot, _ := strings.Cut(string(sum), "## Hotspots\n\n")
	hot, _, _ = strings.Cut(hot, "\n## ")
	for _, l := range strings.Split(strings.TrimSpace(hot), "\n") {
		if strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "- …") && !strings.Contains(l, ". excerpt ") && !strings.Contains(l, ". no excerpt: ") {
			t.Errorf("hotspot without excerpt status: %s", l)
		}
	}
}
