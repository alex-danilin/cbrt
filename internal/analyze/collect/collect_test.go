package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	_ "github.com/alex-danilin/cbrt/internal/analyze/adapters/all"
	"github.com/alex-danilin/cbrt/internal/analyze/hook"
	"github.com/alex-danilin/cbrt/internal/usage"
)

var testdata = filepath.Join("..", "..", "..", "testdata", "analyze")

// allTime covers every fixture.
var allTime = analyze.Window{
	Since: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	Until: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
}

// Fixture session ids (synthetic).
const (
	s1 = "00000000-0000-4000-8000-000000000001"
	s2 = "00000000-0000-4000-8000-000000000002"
	s3 = "00000000-0000-4000-8000-000000000003"
)

// copyTree copies the committed Claude fixtures into a temp dir, so tests can
// grow, truncate and replace members.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// testConfig reads the fixtures (Claude from claudeRoot) and an empty cbrt
// data dir unless cbrtDir is set; it never touches the user's stores.
func testConfig(t *testing.T, claudeRoot, cbrtDir string) analyze.Config {
	t.Helper()
	if claudeRoot == "" {
		claudeRoot = filepath.Join(testdata, "claude", "projects")
	}
	if cbrtDir == "" {
		cbrtDir = t.TempDir()
	}
	return analyze.Config{
		V: analyze.ConfigVersion, WindowDays: 7,
		CbrtDataDirs: []string{cbrtDir, filepath.Join(t.TempDir(), "missing")},
		Tools: map[string]analyze.ToolConfig{
			"claude": {Roots: []string{claudeRoot}},
			"codex":  {Roots: []string{filepath.Join(testdata, "codex", "sessions")}},
			"antigravity": {Roots: []string{
				filepath.Join(testdata, "antigravity", "antigravity", "brain"),
				filepath.Join(testdata, "antigravity", "antigravity-cli", "brain"),
			}},
			"fixturetool": {Roots: []string{filepath.Join(t.TempDir(), "none")}},
		},
	}
}

// counting wraps an adapter and counts Events calls; version overrides
// Version when set.
type counting struct {
	analyze.Adapter
	n       *atomic.Int64
	version string
}

func (c counting) Events(src analyze.Source, emit func(analyze.Event) error) error {
	c.n.Add(1)
	return c.Adapter.Events(src, emit)
}

func (c counting) Version() string {
	if c.version != "" {
		return c.version
	}
	return c.Adapter.Version()
}

func claudeCounting(t *testing.T, version string) (analyze.Adapter, *atomic.Int64) {
	t.Helper()
	a, ok := analyze.Get("claude")
	if !ok {
		t.Fatal("claude adapter not registered")
	}
	n := &atomic.Int64{}
	return counting{Adapter: a, n: n, version: version}, n
}

func run(t *testing.T, cfg analyze.Config, home string, w analyze.Window, opts Options) *analyze.Dataset {
	t.Helper()
	ds, err := Collect(cfg, home, w, opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return ds
}

func asJSON(t *testing.T, ds *analyze.Dataset) []byte {
	t.Helper()
	b, err := json.Marshal(ds)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func find(ds *analyze.Dataset, adapter, native string) *analyze.Session {
	for _, s := range ds.Sessions {
		if s.Key.AdapterID == adapter && s.Key.NativeID == native {
			return s
		}
	}
	return nil
}

func mustFind(t *testing.T, ds *analyze.Dataset, adapter, native string) *analyze.Session {
	t.Helper()
	s := find(ds, adapter, native)
	if s == nil {
		t.Fatalf("session %s/%s not collected", adapter, native)
	}
	return s
}

func TestCacheHitMissInvalidation(t *testing.T) {
	root := copyTree(t, filepath.Join(testdata, "claude", "projects"))
	cfg := testConfig(t, root, "")
	home := t.TempDir()
	a, n := claudeCounting(t, "")
	opts := Options{Adapters: []analyze.Adapter{a}}
	reference := func() []byte {
		ref, _ := claudeCounting(t, "")
		return asJSON(t, run(t, cfg, t.TempDir(), allTime, Options{Adapters: []analyze.Adapter{ref}, NoCache: true}))
	}
	step := func(name string, wantParses int64) {
		t.Helper()
		n.Store(0)
		got := asJSON(t, run(t, cfg, home, allTime, opts))
		if n.Load() != wantParses {
			t.Errorf("%s: %d sources parsed, want %d", name, n.Load(), wantParses)
		}
		if !bytes.Equal(got, reference()) {
			t.Errorf("%s: output differs from an uncached run", name)
		}
	}

	step("cold", 5)
	step("warm", 0)

	main1 := filepath.Join(root, "-work-inkwell", s1+".jsonl")
	orig, err := os.ReadFile(main1)
	if err != nil {
		t.Fatal(err)
	}
	// Growth: the tool is still writing (a trailing partial line).
	if err := os.WriteFile(main1, append(slices.Clone(orig), []byte(`{"type":"user"`)...), 0o644); err != nil {
		t.Fatal(err)
	}
	step("growth", 1)
	step("warm after growth", 0)

	// Truncation: the last record goes away.
	trimmed := orig[:bytes.LastIndexByte(orig[:len(orig)-1], '\n')+1]
	if err := os.WriteFile(main1, trimmed, 0o644); err != nil {
		t.Fatal(err)
	}
	step("truncation", 1)

	// Replacement with the same size: only the head hash differs.
	replaced := bytes.Replace(trimmed, []byte("word count"), []byte("WORD COUNT"), 1)
	if bytes.Equal(replaced, trimmed) || len(replaced) != len(trimmed) {
		t.Fatal("fixture no longer contains the replaced phrase")
	}
	if err := os.WriteFile(main1, replaced, 0o644); err != nil {
		t.Fatal(err)
	}
	step("same-size replacement", 1)

	// A truncated subagent member re-parses its whole logical source.
	sub := filepath.Join(root, "-work-inkwell", s3, "subagents", "agent-a00000000000000f1.jsonl")
	b, err := os.ReadFile(sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, b[:bytes.LastIndexByte(b[:len(b)-1], '\n')+1], 0o644); err != nil {
		t.Fatal(err)
	}
	step("member truncation", 1)

	// A new adapter version invalidates every entry.
	v2, n2 := claudeCounting(t, "cache-test-version")
	run(t, cfg, home, allTime, Options{Adapters: []analyze.Adapter{v2}})
	if n2.Load() != 5 {
		t.Errorf("new adapter version: %d sources parsed, want 5", n2.Load())
	}
}

func TestCacheDeletionChangesNothing(t *testing.T) {
	cfg := testConfig(t, "", "")
	home := t.TempDir()
	cold := asJSON(t, run(t, cfg, home, allTime, Options{}))
	if _, err := os.Stat(filepath.Join(home, "cache", "claude")); err != nil {
		t.Fatalf("no cache written: %v", err)
	}
	warm := asJSON(t, run(t, cfg, home, allTime, Options{}))
	if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
		t.Fatal(err)
	}
	deleted := asJSON(t, run(t, cfg, home, allTime, Options{}))
	uncached := asJSON(t, run(t, cfg, t.TempDir(), allTime, Options{NoCache: true}))
	for name, b := range map[string][]byte{"warm": warm, "after deletion": deleted, "no cache": uncached} {
		if !bytes.Equal(b, cold) {
			t.Errorf("%s output differs from the cold run", name)
		}
	}
	// A corrupt entry is a miss, not an error.
	err := filepath.WalkDir(filepath.Join(home, "cache"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			err = os.WriteFile(p, []byte("not gzip"), 0o644)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if b := asJSON(t, run(t, cfg, home, allTime, Options{})); !bytes.Equal(b, cold) {
		t.Error("output with corrupt cache entries differs")
	}
}

type fakeGit struct{}

func (fakeGit) run(context.Context, string) (*analyze.GitInfo, error) {
	return &analyze.GitInfo{Branch: "main", Commit: "0000000000000000000000000000000000000000"}, nil
}

func record(t *testing.T, home, adapter, event string, at time.Time, stdin any) {
	t.Helper()
	var in []byte
	switch v := stdin.(type) {
	case string:
		in = []byte(v)
	default:
		var err error
		if in, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	opts := hook.Options{Now: func() time.Time { return at }, Git: fakeGit{}.run}
	if err := hook.Record(home, adapter, event, bytes.NewReader(in), opts); err != nil {
		t.Fatalf("hook.Record: %v", err)
	}
}

func TestRegistry(t *testing.T) {
	cfg := testConfig(t, "", "")
	base := run(t, cfg, t.TempDir(), allTime, Options{NoCache: true})
	sess := mustFind(t, base, "claude", s1)
	var call, other analyze.Event
	for _, e := range sess.Events {
		if e.Kind == analyze.KindToolCall && e.CorrelationID != "" {
			if call.Seq == "" {
				call = e
			} else if other.Seq == "" {
				other = e
			}
		}
	}
	if call.Seq == "" || other.Seq == "" {
		t.Fatal("fixture session has fewer than two tool calls")
	}

	home := t.TempDir()
	// Linked: same tool_use_id, recorded long after the call.
	record(t, home, "claude", "PermissionRequest", call.TS.Add(time.Hour),
		map[string]any{"session_id": s1, "tool_name": "Bash", "tool_use_id": call.CorrelationID})
	// Not linked: recorded at exactly another call's time, but with an id the
	// transcript does not have.
	record(t, home, "claude", "PermissionRequest", other.TS,
		map[string]any{"session_id": s1, "tool_name": "Bash", "tool_use_id": "toolu_fake_unknown"})
	// Not linked: a Stop at the same instant, without any id.
	record(t, home, "claude", "Stop", other.TS, map[string]any{"session_id": s1, "cwd": "/work/inkwell"})
	// A session no transcript has.
	record(t, home, "claude", "SessionStart", call.TS, map[string]any{"session_id": "fake-registry-only", "cwd": "/work/other"})
	// Problems: an unknown adapter, stdin the adapter rejects, a leftover .tmp.
	record(t, home, "nosuchtool", "Start", call.TS, map[string]any{"session": "x"})
	record(t, home, "claude", "Stop", call.TS, "not json")
	if err := os.WriteFile(filepath.Join(analyze.SpoolDir(home), "leftover.json.tmp"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	ds := run(t, cfg, home, allTime, Options{})
	got := mustFind(t, ds, "claude", s1)
	var reg []analyze.Event
	for _, e := range got.Events {
		if e.ObservedBy == analyze.FromRegistry {
			reg = append(reg, e)
		}
	}
	if len(reg) != 3 {
		t.Fatalf("%d registry events on the session, want 3", len(reg))
	}
	if len(got.Events) != len(sess.Events)+3 {
		t.Errorf("%d events, want %d", len(got.Events), len(sess.Events)+3)
	}
	if len(got.LinkedTo) != 1 {
		t.Errorf("LinkedTo has %d entries, want 1", len(got.LinkedTo))
	}
	for _, e := range reg {
		linked, ok := got.LinkedTo[e.Seq]
		switch {
		case e.CorrelationID == call.CorrelationID:
			if !ok || linked != call.Seq {
				t.Errorf("permission request linked to %q, want the tool call %q", linked, call.Seq)
			}
		case ok:
			t.Errorf("registry event %s (%s) linked to %q", e.Seq, e.Kind, linked)
		}
	}
	for i := 1; i < len(got.Events); i++ {
		if got.Events[i].TS.Before(got.Events[i-1].TS) {
			t.Fatalf("events not sorted by TS at %d", i)
		}
	}

	only := mustFind(t, ds, "claude", "fake-registry-only")
	if only.Source.Locator != "registry:fake-registry-only" || len(only.Source.Members) != 0 {
		t.Errorf("registry-only source = %+v", only.Source)
	}
	if len(only.Events) != 1 || only.Events[0].Kind != analyze.KindSessionStart {
		t.Errorf("registry-only session events = %d", len(only.Events))
	}

	want := []string{"leftover.json.tmp", "unknown adapter", "not understood by claude"}
	if len(ds.Problems) != len(want) {
		t.Errorf("problems = %q", ds.Problems)
	}
	for _, w := range want {
		if !slices.ContainsFunc(ds.Problems, func(p string) bool { return strings.Contains(p, w) }) {
			t.Errorf("no problem mentions %q: %q", w, ds.Problems)
		}
	}
	for _, p := range ds.Problems {
		if strings.Contains(p, home) || strings.Contains(p, string(filepath.Separator)) {
			t.Errorf("problem names a path: %q", p)
		}
	}
}

func TestWindow(t *testing.T) {
	cfg := testConfig(t, "", "")
	full := run(t, cfg, t.TempDir(), allTime, Options{NoCache: true})
	sess := mustFind(t, full, "claude", s1)
	first, last := sess.Events[0].TS, sess.Events[len(sess.Events)-1].TS
	// The window opens after the session started and closes before s2 starts.
	w := analyze.Window{Since: first.Add(time.Millisecond), Until: last.Add(time.Millisecond)}
	ds := run(t, cfg, t.TempDir(), w, Options{NoCache: true})
	if len(ds.Sessions) != 1 {
		var keys []string
		for _, s := range ds.Sessions {
			keys = append(keys, s.Key.String())
		}
		t.Fatalf("sessions in window: %v, want only %s", keys, s1)
	}
	got := ds.Sessions[0]
	if got.Key.NativeID != s1 || len(got.Events) != len(sess.Events) || !got.Events[0].TS.Equal(first) {
		t.Errorf("window session %s has %d events from %v, want the full history", got.Key, len(got.Events), got.Events[0].TS)
	}
	if !ds.Window.Since.Equal(w.Since) || !ds.Window.Until.Equal(w.Until) {
		t.Errorf("Dataset.Window = %+v", ds.Window)
	}
	// Present still lists every transcript-backed session, in or out of the window.
	if len(ds.Present) != len(full.Present) || len(full.Present) < 2 {
		t.Errorf("Present: %d in a narrow window, %d over all time", len(ds.Present), len(full.Present))
	}

	// A registry event alone brings a session into the window.
	home := t.TempDir()
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	record(t, home, "claude", "Stop", at, map[string]any{"session_id": s2})
	ds = run(t, cfg, home, analyze.Window{Since: at, Until: at.Add(time.Second)}, Options{NoCache: true})
	if len(ds.Sessions) != 1 || ds.Sessions[0].Key.NativeID != s2 || len(ds.Sessions[0].Events) < 2 {
		t.Errorf("a registry event in the window did not keep its session with full history")
	}

	// A zero window ends now and spans cfg.WindowDays.
	now := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	ds = run(t, cfg, t.TempDir(), analyze.Window{}, Options{NoCache: true, Now: func() time.Time { return now }})
	if !ds.Window.Until.Equal(now) || !ds.Window.Since.Equal(now.AddDate(0, 0, -7)) {
		t.Errorf("default window = %+v", ds.Window)
	}
	if find(ds, "claude", s1) == nil || find(ds, "codex", "00000000-0000-4000-8000-000000000001") != nil {
		t.Error("default window selected the wrong sessions")
	}
}

// fake is a synthetic adapter for what the fixtures do not exercise.
type fake struct {
	sources []analyze.Source
	events  map[string][]analyze.Event // by NativeSessionID
	fail    map[string]bool
}

func (fake) ID() string               { return "faketool" }
func (fake) Version() string          { return "1" }
func (fake) DiscoveryRoots() []string { return nil }
func (fake) Capabilities() analyze.Capabilities {
	return analyze.Capabilities{SourceKind: "none", Stores: []string{"default"}, Tokens: "unknown",
		TokenGranularity: "none", CacheWriteOverlap: "unknown", Branch: "per_event",
		ToolDuration: "none", PermissionSignal: "none"}
}
func (f fake) Discover([]string) ([]analyze.Source, error) { return f.sources, nil }
func (f fake) Events(src analyze.Source, emit func(analyze.Event) error) error {
	if f.fail[src.NativeSessionID] {
		return errors.New("fake: unreadable " + src.Locator)
	}
	for _, e := range f.events[src.NativeSessionID] {
		if err := emit(e); err != nil {
			return err
		}
	}
	return nil
}
func (fake) NormalizeHook(analyze.Envelope) ([]analyze.Event, error) { return nil, nil }
func (f fake) Bodies(src analyze.Source, emit func(analyze.Event) error) error {
	return f.Events(src, emit)
}

func fakeEvent(native, seq string, minute int) analyze.Event {
	return analyze.Event{
		Tool: "faketool", Store: "default", Origin: analyze.OriginCLI, NativeSessionID: native,
		Seq: seq, TS: time.Date(2026, 3, 2, 10, minute, 0, 0, time.UTC), Kind: analyze.KindToolCall,
		ObservedBy: analyze.FromTranscript, BranchCoverage: analyze.Unknown, DurationCoverage: analyze.Unknown,
	}
}

func fakeSource(native string) analyze.Source {
	return analyze.Source{AdapterID: "faketool", Store: "default", NativeSessionID: native, Locator: "fake:" + native}
}

func TestProjectBranchAndParent(t *testing.T) {
	a := filepath.Join(string(filepath.Separator)+"w", "Alpha")
	b := filepath.Join(string(filepath.Separator)+"w", "beta")
	f := fake{events: map[string][]analyze.Event{}, fail: map[string]bool{"broken": true}}
	add := func(native string, mut ...func(*analyze.Event)) {
		f.sources = append(f.sources, fakeSource(native))
		for i, m := range mut {
			e := fakeEvent(native, native+":"+string(rune('a'+i)), i)
			m(&e)
			f.events[native] = append(f.events[native], e)
		}
	}
	cwd := func(p string) func(*analyze.Event) { return func(e *analyze.Event) { e.Cwd = p } }
	br := func(name string, cov analyze.Coverage) func(*analyze.Event) {
		return func(e *analyze.Event) { e.Branch, e.BranchCoverage = name, cov }
	}
	// Alpha three times (once unclean), beta twice, once empty.
	add("proj", cwd(a), cwd(a+string(filepath.Separator)), cwd(filepath.Join(a, "sub", "..")), cwd(b), cwd(b), cwd(""))
	add("tie", cwd(b), cwd(a))
	add("drift", br("main", analyze.MeasuredAtStart), br("feature", analyze.Measured), br("main", analyze.Measured))
	add("steady", br("main", analyze.MeasuredAtStart), br("main", analyze.Measured), br("other", analyze.Inferred))
	add("partial", br("main", analyze.Measured), br("other", analyze.Inferred), br("", analyze.Measured))
	add("broken", cwd(a))
	// A subagent's events arrive through its parent's source.
	sub := fakeEvent("sub-1", "sub:a", 9)
	sub.ParentSessionID = "steady"
	f.events["steady"] = append(f.events["steady"], sub)

	cfg := testConfig(t, "", "")
	ds := run(t, cfg, t.TempDir(), allTime, Options{Adapters: []analyze.Adapter{f}, NoCache: true})

	norm := func(p string) string {
		p = filepath.Clean(p)
		if runtime.GOOS == "windows" {
			p = strings.ToLower(p)
		}
		return p
	}
	wantA := norm(a)
	if p := mustFind(t, ds, "faketool", "proj").Project; p != wantA {
		t.Errorf("project = %q, want %q", p, wantA)
	}
	tie := min(norm(a), norm(b)) // a tie goes to the lexically smallest
	if p := mustFind(t, ds, "faketool", "tie").Project; p != tie {
		t.Errorf("tied project = %q, want %q", p, tie)
	}
	if p := mustFind(t, ds, "faketool", "drift").Project; p != "" {
		t.Errorf("project without cwd = %q", p)
	}
	for native, want := range map[string][]string{
		"drift": {FlagBranchDrift}, "steady": nil, "partial": {FlagBranchPartial}, "proj": {FlagBranchPartial},
	} {
		if got := mustFind(t, ds, "faketool", native).Flags; !slices.Equal(got, want) {
			t.Errorf("%s flags = %v, want %v", native, got, want)
		}
	}
	s := mustFind(t, ds, "faketool", "sub-1")
	if s.Parent == nil || *s.Parent != (analyze.SessionKey{AdapterID: "faketool", Store: "default", NativeID: "steady"}) {
		t.Errorf("subagent parent = %v", s.Parent)
	}
	if s.Source.NativeSessionID != "steady" || s.Origin != analyze.OriginCLI {
		t.Errorf("subagent source %q origin %q", s.Source.NativeSessionID, s.Origin)
	}
	if mustFind(t, ds, "faketool", "steady").Parent != nil {
		t.Error("main session has a parent")
	}
	if find(ds, "faketool", "broken") != nil {
		t.Error("an unreadable source produced a session")
	}
	if !slices.Equal(ds.Problems, []string{"source faketool/default/broken: unreadable: fake: unreadable fake:broken"}) {
		t.Errorf("problems = %q", ds.Problems)
	}

	// Disabled adapters are not read.
	cfg.Tools["faketool"] = analyze.ToolConfig{Disabled: true}
	if ds := run(t, cfg, t.TempDir(), allTime, Options{Adapters: []analyze.Adapter{f}, NoCache: true}); len(ds.Sessions) != 0 {
		t.Errorf("a disabled adapter produced %d sessions", len(ds.Sessions))
	}
}

func writeAttempts(t *testing.T, dir string, recs []usage.Record, extra string) {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	buf.WriteString(extra)
	if err := os.WriteFile(filepath.Join(dir, "attempts-2026-03.jsonl"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func modelRequests(s *analyze.Session) []analyze.Event {
	var out []analyze.Event
	for _, e := range s.Events {
		if e.Kind == analyze.KindModelRequest {
			out = append(out, e)
		}
	}
	return out
}

func TestServedJoin(t *testing.T) {
	base := run(t, testConfig(t, "", ""), t.TempDir(), allTime, Options{NoCache: true})
	req1 := modelRequests(mustFind(t, base, "claude", s1))
	req2 := modelRequests(mustFind(t, base, "claude", s2))
	if len(req1) < 2 || len(req2) == 0 {
		t.Fatal("fixtures have too few model requests")
	}
	ts := time.Date(2026, 3, 2, 11, 0, 0, 0, time.UTC)
	recs := []usage.Record{
		// s1: only its first request went through cbrt, after a failover.
		{TS: ts, RequestID: "vx-1", AttemptNo: 1, Provider: "prov-a", Model: "model-a", Outcome: usage.OutcomeFailed, Session: s1},
		{TS: ts, RequestID: "vx-1", AttemptNo: 2, Route: "default", Provider: "prov-b", Location: "cloud",
			Model: "model-b", Outcome: usage.OutcomeCompleted, Session: s1, ResponseID: req1[0].CorrelationID},
		// A record of no known session.
		{TS: ts, RequestID: "vx-9", AttemptNo: 1, Provider: "prov-a", Model: "model-a", Outcome: usage.OutcomeCompleted, ResponseID: "fake-unmatched"},
	}
	for _, e := range req2 {
		recs = append(recs, usage.Record{TS: ts, RequestID: "vx-2", AttemptNo: 1, Provider: "prov-a",
			Location: "local", Model: "model-a", Outcome: usage.OutcomeCompleted, Session: s2, ResponseID: e.CorrelationID})
	}
	dir := t.TempDir()
	writeAttempts(t, dir, recs, "{not json\n")

	ds := run(t, testConfig(t, "", dir), t.TempDir(), allTime, Options{NoCache: true})
	got1 := mustFind(t, ds, "claude", s1)
	want := analyze.Served{ResponseID: req1[0].CorrelationID, Provider: "prov-b", Model: "model-b", Route: "default",
		Location: "cloud", Fallback: true, Outcome: "completed", TS: ts}
	if len(got1.Served) != 1 || got1.Served[req1[0].Seq] != want {
		t.Errorf("s1 served = %+v", got1.Served)
	}
	if !slices.Contains(got1.Flags, FlagModelUnverified) {
		t.Errorf("s1 flags %v lack %s", got1.Flags, FlagModelUnverified)
	}
	got2 := mustFind(t, ds, "claude", s2)
	if len(got2.Served) != len(req2) || slices.Contains(got2.Flags, FlagModelUnverified) {
		t.Errorf("s2: %d served of %d, flags %v", len(got2.Served), len(req2), got2.Flags)
	}
	for _, e := range req2 {
		if sv := got2.Served[e.Seq]; sv.Fallback || sv.Location != "local" {
			t.Errorf("s2 served %+v", sv)
		}
	}
	got3 := mustFind(t, ds, "claude", s3)
	if len(got3.Served) != 0 || slices.Contains(got3.Flags, FlagModelUnverified) {
		t.Errorf("s3 (no cbrt records): served %d, flags %v", len(got3.Served), got3.Flags)
	}
	if !slices.Equal(ds.Problems, []string{"cbrt: attempts-2026-03.jsonl: 1 malformed lines"}) {
		t.Errorf("problems = %q", ds.Problems)
	}
}

func TestDeterministic(t *testing.T) {
	dir := t.TempDir()
	base := run(t, testConfig(t, "", ""), t.TempDir(), allTime, Options{NoCache: true})
	var recs []usage.Record
	for _, s := range base.Sessions {
		for _, e := range modelRequests(s) {
			recs = append(recs, usage.Record{TS: e.TS, AttemptNo: 1, Provider: "prov-a", Model: "model-a",
				Outcome: usage.OutcomeCompleted, Session: s.Key.NativeID, ResponseID: e.CorrelationID})
			break
		}
	}
	writeAttempts(t, dir, recs, "")
	cfg := testConfig(t, "", dir)
	home := t.TempDir()
	at := time.Date(2026, 3, 2, 11, 0, 30, 0, time.UTC)
	record(t, home, "claude", "Stop", at, map[string]any{"session_id": s1, "cwd": "/work/inkwell"})
	record(t, home, "codex", "Stop", at, map[string]any{"session_id": "00000000-0000-4000-8000-000000000001", "turn_id": "fake-turn"})
	record(t, home, "fixturetool", "Start", at, map[string]any{"session": "fake-fixture-session"})

	first := asJSON(t, run(t, cfg, home, allTime, Options{}))
	second := asJSON(t, run(t, cfg, home, allTime, Options{}))
	third := asJSON(t, run(t, cfg, home, allTime, Options{NoCache: true}))
	if !bytes.Equal(first, second) || !bytes.Equal(first, third) {
		t.Fatal("two runs over the same inputs differ")
	}
	ds := run(t, cfg, home, allTime, Options{})
	for i := 1; i < len(ds.Sessions); i++ {
		if ds.Sessions[i-1].Key.String() >= ds.Sessions[i].Key.String() {
			t.Fatalf("sessions not sorted by key at %d", i)
		}
	}
	if find(ds, "fixturetool", "fake-fixture-session") == nil {
		t.Error("hooks-only tool session missing")
	}
}

// Problem reasons name files by base name only, whatever the separator.
func TestProblemReason(t *testing.T) {
	win := `D:\stores\proj\sess-1.jsonl`
	unix := "/srv/stores/proj/sess-2.jsonl"
	cases := []struct {
		err   error
		paths []string
		want  string
	}{
		{&fs.PathError{Op: "open", Path: win, Err: fs.ErrPermission}, nil, "open sess-1.jsonl: permission denied"},
		{&fs.PathError{Op: "read", Path: unix, Err: errors.New("i/o error")}, nil, "read sess-2.jsonl: i/o error"},
		{errors.New("tool: 3 bad lines in " + win), []string{`D:\stores\proj`, win}, "tool: 3 bad lines in sess-1.jsonl"},
		{errors.New("line 9 past the end of " + unix), []string{unix}, "line 9 past the end of sess-2.jsonl"},
	}
	for _, c := range cases {
		if got := problemReason(c.err, c.paths...); got != c.want {
			t.Errorf("problemReason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
