package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

var t0 = time.Date(2026, 10, 8, 12, 15, 0, 0, time.UTC)

func f64(v float64) *float64 { return &v }

func validRec() Recommendation {
	return Recommendation{
		CreatedRunID:   "20261008T121500Z",
		Title:          "Add a test command to AGENTS.md",
		Category:       CatInstructions,
		EvidenceRefs:   []string{"claude/projects/s1#12"},
		Cause:          "agent guesses the test command",
		Alternatives:   "the command changed this week",
		Change:         "document go test -count=1 ./...",
		CostMinutes:    5,
		Confidence:     ConfMed,
		Scope:          Scope{Tools: []string{"claude"}},
		Baseline:       Baseline{Metric: "failed_test_runs", Value: f64(0)},
		CheckMethod:    "failed_test_runs drops by half",
		MinEvidence:    ">= 5 sessions in scope",
		EvalWindowDays: 7,
	}
}

func TestWriteJSONAtomicKeepsOldFileWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	if err := WriteJSONAtomic(p, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(p)

	boom := errors.New("rename failed")
	rename = func(string, string) error { return boom }
	defer func() { rename = os.Rename }()
	if err := WriteJSONAtomic(p, map[string]int{"a": 2}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want rename failure", err)
	}
	now, _ := os.ReadFile(p)
	if string(now) != string(old) {
		t.Fatalf("file changed: %s", now)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
}

func TestRecLifecycle(t *testing.T) {
	home := t.TempDir()
	r1, err := AddRec(home, validRec(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if r1.RecID != "r_1" || r1.ImplStatus != ImplProposed || r1.EvalStatus != EvalPending {
		t.Fatalf("r1 = %+v", r1)
	}
	in := validRec()
	in.RecID, in.ImplStatus, in.EvalStatus = "r_99", ImplApplied, EvalImproved // ignored
	r2, err := AddRec(home, in, t0)
	if err != nil || r2.RecID != "r_2" || r2.ImplStatus != ImplProposed || r2.EvalStatus != EvalPending {
		t.Fatalf("r2 = %+v, %v", r2, err)
	}

	if err := SetEval(home, "r_1", EvalImproved, "", "run"); err == nil {
		t.Fatal("SetEval on a proposed rec must fail")
	}
	if err := SetImpl(home, "r_1", ImplReverted, "", t0); err == nil {
		t.Fatal("reverting a proposed rec must fail")
	}
	if err := SetImpl(home, "r_1", "done", "", t0); err == nil {
		t.Fatal("unknown impl status must fail")
	}
	if err := SetImpl(home, "r_9", ImplApplied, "", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown rec: %v", err)
	}
	if err := SetImpl(home, "r_1", ImplApplied, "added to AGENTS.md", t0); err != nil {
		t.Fatal(err)
	}
	if err := SetImpl(home, "r_2", ImplDeclined, "not worth it", t0); err != nil {
		t.Fatal(err)
	}
	if err := SetEval(home, "r_2", EvalNoChange, "", "run"); err == nil {
		t.Fatal("SetEval on a declined rec must fail")
	}
	if err := SetEval(home, "r_1", EvalPending, "", "run"); err == nil {
		t.Fatal("pending is not a verdict")
	}
	if err := SetEval(home, "r_1", EvalImproved, "", ""); err == nil {
		t.Fatal("verdict without run id must fail")
	}
	if err := SetEval(home, "r_1", EvalImproved, "fewer failed runs", "20261015T000000Z"); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRecs(home)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Items[0]
	if got.ImplStatus != ImplApplied || got.ImplAt == nil || !got.ImplAt.Equal(t0) || got.EvalStatus != EvalImproved || got.EvalRunID != "20261015T000000Z" {
		t.Fatalf("r_1 = %+v", got)
	}
	// Revert, then re-apply: evaluation restarts.
	if err := SetImpl(home, "r_1", ImplReverted, "", t0); err != nil {
		t.Fatal(err)
	}
	if err := SetImpl(home, "r_1", ImplApplied, "", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r, _ = LoadRecs(home)
	if r.Items[0].EvalStatus != EvalPending || r.Items[0].EvalRunID != "" {
		t.Fatalf("re-applied rec keeps old verdict: %+v", r.Items[0])
	}

	md := ExportMarkdown(r)
	if !strings.Contains(md, "### r_1: Add a test command") || !strings.Contains(md, "- r_2: Add a test command to AGENTS.md (instructions, declined): not worth it") {
		t.Fatalf("export:\n%s", md)
	}
	if err := WriteExport(home, r); err != nil {
		t.Fatal(err)
	}
}

func TestAddRecValidation(t *testing.T) {
	cases := map[string]func(*Recommendation){
		"title":            func(r *Recommendation) { r.Title = " " },
		"category":         func(r *Recommendation) { r.Category = "docs" },
		"evidence_refs":    func(r *Recommendation) { r.EvidenceRefs = []string{""} },
		"cause":            func(r *Recommendation) { r.Cause = "" },
		"alternatives":     func(r *Recommendation) { r.Alternatives = "" },
		"change":           func(r *Recommendation) { r.Change = "" },
		"cost_minutes":     func(r *Recommendation) { r.CostMinutes = 0 },
		"confidence":       func(r *Recommendation) { r.Confidence = "medium" },
		"baseline.metric":  func(r *Recommendation) { r.Baseline.Metric = "" },
		"baseline.value":   func(r *Recommendation) { r.Baseline.Value = nil },
		"check_method":     func(r *Recommendation) { r.CheckMethod = "" },
		"min_evidence":     func(r *Recommendation) { r.MinEvidence = "" },
		"eval_window_days": func(r *Recommendation) { r.EvalWindowDays = -1 },
	}
	home := t.TempDir()
	for field, mut := range cases {
		r := validRec()
		mut(&r)
		_, err := AddRec(home, r, t0)
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	if _, err := os.Stat(recsPath(home)); !os.IsNotExist(err) {
		t.Fatal("invalid recs must not create the file")
	}
}

func TestDueForEval(t *testing.T) {
	at := t0
	mk := func(id, impl, eval string, days int) Recommendation {
		r := validRec()
		r.RecID, r.ImplStatus, r.EvalStatus, r.EvalWindowDays = id, impl, eval, days
		if impl != ImplProposed {
			r.ImplAt = &at
		}
		return r
	}
	recs := Recs{Items: []Recommendation{
		mk("r_1", ImplApplied, EvalPending, 7),       // due exactly at 7 days
		mk("r_2", ImplApplied, EvalPending, 8),       // not yet
		mk("r_3", ImplApplied, EvalNoChange, 1),      // already evaluated
		mk("r_4", ImplProposed, EvalPending, 1),      // not applied
		mk("r_5", ImplDeclined, EvalPending, 1),      // declined
		mk("r_6", ImplApplied, EvalPending, 1),       // due
		mk("r_7", ImplReverted, EvalInconclusive, 1), // reverted
	}}
	var ids []string
	for _, r := range DueForEval(recs, t0.AddDate(0, 0, 7)) {
		ids = append(ids, r.RecID)
	}
	if !reflect.DeepEqual(ids, []string{"r_1", "r_6"}) {
		t.Fatalf("due = %v", ids)
	}
}

func sm(native string, until time.Time, n float64) analyze.SessionMetrics {
	return analyze.SessionMetrics{
		Session:        analyze.SessionKey{AdapterID: "claude", Store: "projects", NativeID: native},
		Window:         analyze.Window{Since: until.AddDate(0, 0, -7), Until: until},
		MetricsVersion: "1",
		Values:         map[string]analyze.Value{"tool_calls": {N: n, Cov: analyze.Measured}},
	}
}

func TestArchiveReplaceThenFreeze(t *testing.T) {
	home := t.TempDir()
	a, b := sm("a", t0, 1), sm("b", t0, 1)
	both := map[analyze.SessionKey]bool{a.Session: true, b.Session: true}
	if err := UpsertArchive(home, []analyze.SessionMetrics{a, b}, both, t0); err != nil {
		t.Fatal(err)
	}
	// Second run: a still present with new numbers -> replaced.
	a2 := sm("a", t0, 5)
	b2 := sm("b", t0, 9)
	if err := UpsertArchive(home, []analyze.SessionMetrics{a2, b2}, both, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Third run: b's transcript is gone -> frozen with its last numbers.
	onlyA := map[analyze.SessionKey]bool{a.Session: true}
	if err := UpsertArchive(home, []analyze.SessionMetrics{sm("a", t0, 6)}, onlyA, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Fourth run: b reappears in ms but frozen entries are never replaced.
	if err := UpsertArchive(home, []analyze.SessionMetrics{sm("b", t0, 100)}, both, t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	es, err := LoadArchive(home, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("entries = %d", len(es))
	}
	ea, eb := es[0], es[1]
	if ea.Frozen || ea.Metrics.Values["tool_calls"].N != 6 {
		t.Fatalf("a = %+v", ea)
	}
	if !eb.Frozen || eb.Metrics.Values["tool_calls"].N != 9 || !eb.LastUpdated.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("b = %+v", eb)
	}
	if _, err := os.Stat(archivePath(home, "2026-10")); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveMonthsAndMove(t *testing.T) {
	home := t.TempDir()
	sep := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) // exclusive end: September
	a := sm("a", sep, 1)
	p := map[analyze.SessionKey]bool{a.Session: true}
	if err := UpsertArchive(home, []analyze.SessionMetrics{a}, p, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archivePath(home, "2026-09")); err != nil {
		t.Fatal(err)
	}
	// The session continues into October: it moves, it is not duplicated.
	a.Values[LastEventMetric] = analyze.Value{N: float64(t0.Unix()), Cov: analyze.Measured}
	if err := UpsertArchive(home, []analyze.SessionMetrics{a}, p, t0); err != nil {
		t.Fatal(err)
	}
	all, _ := LoadArchive(home, time.Time{})
	if len(all) != 1 {
		t.Fatalf("entries = %d", len(all))
	}
	recent, _ := LoadArchive(home, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if len(recent) != 1 {
		t.Fatalf("since filter: %d", len(recent))
	}
	none, _ := LoadArchive(home, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if len(none) != 0 {
		t.Fatalf("since filter: %d", len(none))
	}
}

func TestRunIDsSortable(t *testing.T) {
	times := []time.Time{
		time.Date(2026, 10, 8, 12, 15, 0, 0, time.UTC),
		time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.FixedZone("x", 3*3600)),
		time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC),
	}
	var ids []string
	for _, tm := range times {
		ids = append(ids, NewRunID(tm))
	}
	if NewRunID(times[0]) != "20261008T121500Z" {
		t.Fatalf("id = %s", ids[0])
	}
	sort.Strings(ids)
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for i, tm := range times {
		if ids[i] != NewRunID(tm) {
			t.Fatalf("order mismatch at %d: %v", i, ids)
		}
		if back, err := ParseRunID(ids[i]); err != nil || !back.Equal(tm) {
			t.Fatalf("parse %s = %v, %v", ids[i], back, err)
		}
	}
}

func TestRunsAppendAndLoad(t *testing.T) {
	home := t.TempDir()
	r := Run{RunID: NewRunID(t0), RanAt: t0, Sessions: 3, AdapterVersions: map[string]string{"claude": "1"}, MetricsVersion: "1"}
	if err := AppendRun(home, r); err != nil {
		t.Fatal(err)
	}
	// Simulate a torn write, then a later append.
	f, _ := os.OpenFile(runsPath(home), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"v":1,"run_id":"2026`)
	f.Close()
	r2 := Run{RunID: r.RunID, ReviewTokensSpent: 1234}
	if err := AppendRun(home, r2); err != nil {
		t.Fatal(err)
	}
	runs, err := LoadRuns(home)
	if err != nil || len(runs) != 2 || runs[0].Sessions != 3 || runs[1].ReviewTokensSpent != 1234 || !runs[0].RanAt.Equal(t0) {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if err := AppendRun(home, Run{}); err == nil {
		t.Fatal("run without id must fail")
	}
	os.WriteFile(runsPath(home), []byte(`{"v":2,"run_id":"x"}`+"\n"), 0o600)
	if _, err := LoadRuns(home); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer line: %v", err)
	}
}

// The run scope round-trips; lines without one (older versions) are full
// runs; filtered runs and windowless note lines are no cursor.
func TestRunScopeAndCursor(t *testing.T) {
	home := t.TempDir()
	w := analyze.Window{Since: t0.Add(-time.Hour), Until: t0}
	old := `{"v":1,"run_id":"20261008T121500Z","ran_at":"2026-10-08T12:15:00Z","window":{"since":"2026-10-08T11:15:00Z","until":"2026-10-08T12:15:00Z"},"sources":1,"sessions":1,"events":2,"metrics_version":"1"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(runsPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsPath(home), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Run{
		{RunID: "a", Window: w, Tools: []string{"codex"}},
		{RunID: "b", Window: w, Project: "proj"},
		{RunID: "b", ReviewTokensSpent: 10, Notes: "review"},
	} {
		if err := AppendRun(home, r); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(runsPath(home))
	if lines := strings.Split(string(b), "\n"); strings.Contains(lines[3], "tools") || strings.Contains(lines[3], "project") {
		t.Errorf("an unfiltered line carries scope keys: %s", lines[3])
	}
	runs, err := LoadRuns(home)
	if err != nil || len(runs) != 4 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if !runs[0].Cursor() || runs[0].Tools != nil || runs[0].Project != "" || !runs[0].Window.Until.Equal(t0) {
		t.Errorf("old-format line: %+v, cursor %v", runs[0], runs[0].Cursor())
	}
	if runs[1].Cursor() || len(runs[1].Tools) != 1 || runs[1].Tools[0] != "codex" {
		t.Errorf("tool-filtered line: %+v", runs[1])
	}
	if runs[2].Cursor() || runs[2].Project != "proj" {
		t.Errorf("project-filtered line: %+v", runs[2])
	}
	if runs[3].Cursor() {
		t.Errorf("windowless note line is a cursor: %+v", runs[3])
	}
}

func TestIdentityRoundTrip(t *testing.T) {
	home := t.TempDir()
	id, err := LoadIdentity(home)
	if err != nil || id.ByFound == nil || id.Aliases == nil || id.V != 1 {
		t.Fatalf("empty identity = %+v, %v", id, err)
	}
	id.ByFound["claude/projects/s1#1"] = "t_1"
	id.Aliases["t_2"] = "t_1"
	if err := SaveIdentity(home, id); err != nil {
		t.Fatal(err)
	}
	got, err := LoadIdentity(home)
	if err != nil || !reflect.DeepEqual(got, id) {
		t.Fatalf("got %+v, %v", got, err)
	}
	os.WriteFile(identPath(home), []byte(`{"v":7}`), 0o600)
	if _, err := LoadIdentity(home); err == nil {
		t.Fatal("newer identity must fail")
	}
}

func TestPruneKeepsState(t *testing.T) {
	home := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(home, rel)
		os.MkdirAll(p, 0o700)
		os.WriteFile(filepath.Join(p, "summary.md"), []byte("x"), 0o600)
		return p
	}
	oldRun := mk(filepath.Join(OutDir, NewRunID(t0.AddDate(0, 0, -91))))
	newRun := mk(filepath.Join(OutDir, NewRunID(t0.AddDate(0, 0, -89))))
	oddOld := mk(filepath.Join(OutDir, "manual"))
	os.Chtimes(oddOld, t0.AddDate(-1, 0, 0), t0.AddDate(-1, 0, 0))
	if _, err := AddRec(home, validRec(), t0.AddDate(-1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	a := sm("a", t0.AddDate(-1, 0, 0), 1)
	if err := UpsertArchive(home, []analyze.SessionMetrics{a}, nil, t0.AddDate(-1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := Prune(home, t0, 90); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{oldRun: false, oddOld: false, newRun: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", p, err == nil, want)
		}
	}
	if r, err := LoadRecs(home); err != nil || len(r.Items) != 1 {
		t.Fatalf("recs after prune: %+v, %v", r, err)
	}
	if es, err := LoadArchive(home, time.Time{}); err != nil || len(es) != 1 || !es[0].Frozen {
		t.Fatalf("archive after prune: %+v, %v", es, err)
	}
	if err := Prune(home, t0, 0); err == nil {
		t.Fatal("zero retention must fail")
	}
}

// TestRecsV0Upgrade loads a hand-written v0 file and requires the same result
// as the equivalent current-version file.
func TestRecsV0Upgrade(t *testing.T) {
	load := func(fixture string) Recs {
		t.Helper()
		home := t.TempDir()
		b, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(home, StateDir), 0o700)
		os.WriteFile(recsPath(home), b, 0o600)
		r, err := LoadRecs(home)
		if err != nil {
			t.Fatalf("%s: %v", fixture, err)
		}
		// The next write stores the current version.
		if err := SaveRecs(home, r); err != nil {
			t.Fatal(err)
		}
		var head struct{ V int }
		out, _ := os.ReadFile(recsPath(home))
		json.Unmarshal(out, &head)
		if head.V != 1 {
			t.Fatalf("saved v = %d", head.V)
		}
		return r
	}
	v0, v1 := load("recommendations_v0.json"), load("recommendations_v1.json")
	if len(v1.Items) != 3 {
		t.Fatalf("v1 items = %d", len(v1.Items))
	}
	if !reflect.DeepEqual(v0, v1) {
		a, _ := json.MarshalIndent(v0, "", " ")
		b, _ := json.MarshalIndent(v1, "", " ")
		t.Fatalf("v0 upgrade differs:\n%s\nwant\n%s", a, b)
	}
}

// context_checked is required with project scope; recommendations saved
// before it existed still load and re-save byte for byte.
func TestContextChecked(t *testing.T) {
	ok := func(cc []string, projects ...string) error {
		r := validRec()
		r.Scope.Projects, r.ContextChecked = projects, cc
		return r.Validate()
	}
	good := []struct {
		cc       []string
		projects []string
	}{
		{nil, nil},
		{[]string{"AGENTS.md", "docs/deploy.md", `scripts\build.go`}, []string{"~/work/p"}},
		{[]string{"unavailable: project folder no longer exists"}, []string{"~/work/p"}},
		{[]string{"README.md"}, nil},
	}
	for _, c := range good {
		if err := ok(c.cc, c.projects...); err != nil {
			t.Errorf("%q with projects %q: %v", c.cc, c.projects, err)
		}
	}
	for _, cc := range [][]string{nil, {}, {"/abs/AGENTS.md"}, {`C:\p\AGENTS.md`}, {"~/p/README.md"}, {"../other/AGENTS.md"},
		{"unavailable:"}, {"unavailable: gone", "AGENTS.md"}, {"AGENTS.md", "unavailable: gone"}, {" "}} {
		if err := ok(cc, "~/work/p"); err == nil || !strings.Contains(err.Error(), "context_checked") {
			t.Errorf("%q accepted with project scope (err %v)", cc, err)
		}
	}

	home := t.TempDir()
	orig, err := os.ReadFile(filepath.Join("testdata", "recommendations_v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(home, StateDir), 0o700)
	os.WriteFile(recsPath(home), orig, 0o600)
	r, err := LoadRecs(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRecs(home, r); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(recsPath(home))
	if strings.Contains(string(again), "context_checked") {
		t.Error("re-saving an old file added context_checked")
	}
	r2, err := LoadRecs(home)
	if err != nil || !reflect.DeepEqual(r, r2) {
		t.Errorf("old recommendations changed on a load/save round trip: %v", err)
	}
}

func TestNewerVersionIsAnError(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, StateDir), 0o700)
	os.WriteFile(recsPath(home), []byte(`{"v":2,"items":[]}`), 0o600)
	if _, err := LoadRecs(home); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v", err)
	}
	if _, err := AddRec(home, validRec(), t0); err == nil {
		t.Fatal("AddRec must not overwrite a newer file")
	}
	os.MkdirAll(filepath.Join(home, ArchiveDir), 0o700)
	os.WriteFile(archivePath(home, "2026-10"), []byte(`{"v":3,"sessions":[]}`), 0o600)
	if _, err := LoadArchive(home, time.Time{}); err == nil {
		t.Fatal("newer archive must fail")
	}
	// Archive has no v0: a file without "v" has no upgrade path.
	os.WriteFile(archivePath(home, "2026-10"), []byte(`{"sessions":[]}`), 0o600)
	if _, err := LoadArchive(home, time.Time{}); err == nil {
		t.Fatal("unversioned archive must fail")
	}
}
