package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/analyzetest"
)

// Fixtures: go run ./scripts/synth-transcripts/codex (synthetic, see there).
var fixtureRoot = filepath.Join("..", "..", "..", "..", "testdata", "analyze", "codex", "sessions")

const (
	s1 = "00000000-0000-4000-8000-000000000001" // git, two measured turns, tools
	s2 = "00000000-0000-4000-8000-000000000002" // no git, info:null, interrupted
	s3 = "00000000-0000-4000-8000-000000000003" // ordinals, compaction, partial tail
	s4 = "00000000-0000-4000-8000-000000000004" // exec, web, wait, orphan output
	s5 = "00000000-0000-4000-8000-000000000005" // token_usage_record, merged turn
)

func run(t *testing.T) analyzetest.Result {
	t.Helper()
	return analyzetest.Run(t, Adapter{}, []string{fixtureRoot}, 1)
}

func ptr(n int64) *int64 { return &n }

func requests(evs []analyze.Event) []analyze.Event {
	var out []analyze.Event
	for _, e := range evs {
		if e.Kind == analyze.KindModelRequest {
			out = append(out, e)
		}
	}
	return out
}

func byCall(evs []analyze.Event, kind analyze.Kind, call string) analyze.Event {
	for _, e := range evs {
		if e.Kind == kind && e.CorrelationID == call {
			return e
		}
	}
	return analyze.Event{}
}

func TestRegistered(t *testing.T) {
	a, ok := analyze.Get(ID)
	if !ok || a.ID() != "codex" {
		t.Fatal("codex adapter not registered")
	}
	if err := a.Capabilities().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFixturesContract(t *testing.T) {
	res := run(t)
	var ids []string
	for _, s := range res.Sources {
		ids = append(ids, s.NativeSessionID)
		if s.Store != "default" || len(s.Members) != 1 || s.Locator != s.Members[0].Path {
			t.Errorf("source %s: %+v", s.NativeSessionID, s)
		}
	}
	if want := []string{s1, s2, s3, s4, s5}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("sources %v, want %v", ids, want)
	}
	srcs, err := Adapter{}.Discover([]string{filepath.Join(fixtureRoot, "missing")})
	if err != nil || len(srcs) != 0 {
		t.Errorf("missing root: %v %v", srcs, err)
	}
}

// tok builds buckets from (uncached, cache read, visible out, thinking).
func tok(unc, read, vis, think int64) analyze.Tokens {
	return analyze.Tokens{InUncached: ptr(unc), InCacheRead: ptr(read), OutVisible: ptr(vis), OutThinking: ptr(think)}
}

type reqWant struct {
	id     string
	tokens *analyze.Tokens // nil: Unknown
}

func checkRequests(t *testing.T, evs []analyze.Event, want []reqWant) {
	t.Helper()
	reqs := requests(evs)
	if len(reqs) != len(want) {
		t.Fatalf("%d model_request events, want %d", len(reqs), len(want))
	}
	for i, w := range want {
		r := reqs[i]
		if r.RequestID != w.id {
			t.Errorf("request %d: id %q, want %q", i+1, r.RequestID, w.id)
		}
		switch {
		case w.tokens == nil:
			if r.Tokens != nil || r.TokensCoverage != analyze.Unknown {
				t.Errorf("request %d: tokens %s (%s), want unknown", i+1, js(r.Tokens), r.TokensCoverage)
			}
		case r.Tokens == nil || !reflect.DeepEqual(*r.Tokens, *w.tokens) || r.TokensCoverage != analyze.Partial:
			t.Errorf("request %d: tokens %s (%s), want %s (partial)", i+1, js(r.Tokens), r.TokensCoverage, js(w.tokens))
		}
		if r.Tokens != nil && len(r.TokensNative) == 0 {
			t.Errorf("request %d: no TokensNative", i+1)
		}
	}
}

func tp(t analyze.Tokens) *analyze.Tokens { return &t }

// Hand-computed buckets per response: turn 1 has two responses (running total
// 0 → (1000, 200, 50, 20) → (2500, 1100, 170, 60)); turn 2's single response
// is written twice and counted once.
func TestCallTokens(t *testing.T) {
	evs := run(t).Events[s1]
	t1, t2 := "00000000-0000-4000-9000-000000000001", "00000000-0000-4000-9000-000000000002"
	checkRequests(t, evs, []reqWant{
		{t1 + "#1", tp(tok(800, 200, 30, 20))},
		{t1 + "#2", tp(tok(600, 900, 80, 40))},
		{t2 + "#1", tp(tok(600, 2400, 80, 0))},
	})
	reqs := requests(evs)
	if r := reqs[0]; r.Model != "gpt-5-codex" || r.Effort != "medium" || r.CorrelationID != "" {
		t.Errorf("request 1 model %q effort %q correlation %q", r.Model, r.Effort, r.CorrelationID)
	}
	if reqs[2].Effort != "high" || reqs[2].CorrelationID != "" {
		t.Errorf("turn 2 effort %q correlation %q", reqs[2].Effort, reqs[2].CorrelationID)
	}
	sum, with, n := analyzetest.TokenSum(evs)
	if want := tok(2000, 3500, 190, 60); !reflect.DeepEqual(sum, want) || with != 3 || n != 3 {
		t.Errorf("totals %s, %d/%d with usage", js(sum), with, n)
	}
}

// Response ids from token_usage_record, a repeated token_count, a merged
// (fallback) turn, an interrupted turn without usage, and a record whose
// usage does not match its response.
func TestResponseIDsAndFallback(t *testing.T) {
	evs := run(t).Events[s5]
	turn := func(n int) string { return "00000000-0000-4000-9000-0000000000" + strconv.Itoa(n) }
	checkRequests(t, evs, []reqWant{
		{"resp_fake_0001", tp(tok(1200, 0, 30, 10))},
		{"resp_fake_0002", tp(tok(400, 1100, 40, 20))},
		{"resp_fake_0003", tp(tok(400, 1400, 90, 0))},
		{turn(42), tp(tok(700, 1300, 55, 15))}, // merged: the turn's increase
		{turn(43), nil},                        // interrupted, no usage
		{turn(44) + "#1", tp(tok(200, 500, 20, 0))},
	})
	// The answer id is the response_id when the rollout records it for the
	// response; merged, interrupted and unmatched requests have none.
	for i, r := range requests(evs) {
		if want := map[bool]string{true: r.RequestID}[i < 3]; r.CorrelationID != want {
			t.Errorf("%s: correlation %q", r.RequestID, r.CorrelationID)
		}
	}
	if k := analyzetest.Kinds(evs); k[analyze.KindInterrupt] != 1 || k[analyze.KindTurnEnd] != 3 {
		t.Errorf("kinds %v", k)
	}
}

// finalTotal reads a rollout's last non-null total_token_usage.
func finalTotal(t *testing.T, path string) (u usage, ok bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range bytes.Split(b, []byte("\n")) {
		var r struct {
			Type    string `json:"type"`
			Payload struct {
				Type string     `json:"type"`
				Info *tokenInfo `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(l, &r) != nil || r.Type != "event_msg" || r.Payload.Type != "token_count" {
			continue
		}
		if r.Payload.Info != nil && r.Payload.Info.Total != nil {
			u, ok = *r.Payload.Info.Total, true
		}
	}
	return u, ok
}

// invariant compares the sum over a session's model_requests with the
// rollout's final running total: "holds" (equal), "excused" (below it, with
// Unknown requests that may hold the difference) or "fails".
func invariant(t *testing.T, src analyze.Source, evs []analyze.Event) string {
	t.Helper()
	final, ok := finalTotal(t, src.Members[0].Path)
	sum, with, n := analyzetest.TokenSum(evs)
	v := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	got := usage{v(sum.InUncached) + v(sum.InCacheRead), v(sum.InCacheRead), v(sum.OutVisible) + v(sum.OutThinking), v(sum.OutThinking)}
	switch {
	case got == final:
		return "holds"
	case ok && with < n && got.Input <= final.Input && got.Cached <= final.Cached && got.Output <= final.Output && got.Reasoning <= final.Reasoning:
		return "excused"
	}
	return "fails"
}

// Per session, the requests sum to the rollout's final running total.
func TestSessionSumInvariant(t *testing.T) {
	res := run(t)
	for _, src := range res.Sources {
		if got := invariant(t, src, res.Events[src.NativeSessionID]); got != "holds" {
			t.Errorf("%s: invariant %s", src.NativeSessionID, got)
		}
	}
}

// info:null → unknown; an interrupted turn's counted response is still its
// own request; the next turn's response is only its own usage.
func TestUnknownTurns(t *testing.T) {
	evs := run(t).Events[s2]
	checkRequests(t, evs, []reqWant{
		{"00000000-0000-4000-9000-000000000011", nil},
		{"00000000-0000-4000-9000-000000000012#1", tp(tok(400, 0, 20, 10))},
		{"00000000-0000-4000-9000-000000000013#1", tp(tok(200, 400, 35, 5))},
	})
	if _, with, n := analyzetest.TokenSum(evs); with != 2 || n != 3 {
		t.Errorf("coverage %d/%d", with, n)
	}
	k := analyzetest.Kinds(evs)
	if k[analyze.KindInterrupt] != 1 || k[analyze.KindTurnEnd] != 2 {
		t.Errorf("kinds %v", k)
	}
}

// Acceptance 11: branch measured at start, then inferred; unknown without git.
func TestBranch(t *testing.T) {
	res := run(t)
	evs := res.Events[s1]
	if e := evs[0]; e.Kind != analyze.KindSessionStart || e.BranchCoverage != analyze.MeasuredAtStart ||
		e.Branch != "feature/rain-gauge" || e.Commit != strings.Repeat("1", 40) {
		t.Errorf("first event %s %s %q", e.Kind, e.BranchCoverage, e.Branch)
	}
	for _, e := range evs {
		if e.Kind == analyze.KindUserPrompt && (e.BranchCoverage != analyze.Inferred || e.Branch != "feature/rain-gauge") {
			t.Errorf("prompt branch %q %s, want inferred", e.Branch, e.BranchCoverage)
		}
	}
	for _, e := range res.Events[s2] {
		if e.Branch != "" || e.BranchCoverage != analyze.Unknown {
			t.Fatalf("no-git session: branch %q %s", e.Branch, e.BranchCoverage)
		}
	}
}

func TestToolsAndDurations(t *testing.T) {
	evs := run(t).Events[s1]
	cases := []struct {
		call, name, raw string
		ms              int64
		ok              *bool
		files           []string
	}{
		{"call_fake_0001", analyze.ToolSearch, "shell_command", 1500, analyze.Bool(true), nil},
		{"call_fake_0002", analyze.ToolReadFile, "shell_command", 750, analyze.Bool(true), []string{"src/forecast/smoothing.go"}},
		{"call_fake_0003", analyze.ToolEdit, "apply_patch", 500, analyze.Bool(true), []string{"src/forecast/smoothing.go", "src/forecast/smoothing_test.go"}},
		{"call_fake_0004", analyze.ToolShell, "shell_command", 4250, analyze.Bool(false), nil},
	}
	for _, c := range cases {
		call, res := byCall(evs, analyze.KindToolCall, c.call), byCall(evs, analyze.KindToolResult, c.call)
		if call.ToolName != c.name || call.ToolNameRaw != c.raw || !reflect.DeepEqual(call.Files, c.files) || call.ToolArgsHash == "" {
			t.Errorf("%s call: %s/%s files %v", c.call, call.ToolName, call.ToolNameRaw, call.Files)
		}
		if res.ToolName != c.name || res.DurationMS == nil || *res.DurationMS != c.ms || res.DurationCoverage != analyze.Measured {
			t.Errorf("%s result: %s duration %s %s", c.call, res.ToolName, js(res.DurationMS), res.DurationCoverage)
		}
		if !reflect.DeepEqual(res.ToolSuccess, c.ok) {
			t.Errorf("%s success %s, want %s", c.call, js(res.ToolSuccess), js(c.ok))
		}
	}
	k := analyzetest.Kinds(evs)
	if k[analyze.KindUserPrompt] != 2 || k[analyze.KindSystemContext] != 3 || k[analyze.KindThinking] != 1 || k[analyze.KindAssistantText] != 2 {
		t.Errorf("kinds %v", k)
	}
}

func TestExecWebAndOrphans(t *testing.T) {
	evs := run(t).Events[s4]
	want := map[string][2]string{
		"call_fake_0041": {analyze.ToolWeb, "web.run"},
		"call_fake_0042": {analyze.ToolShell, "exec"},
		"call_fake_0043": {analyze.ToolShell, "wait"},
		"call_fake_0044": {analyze.ToolOther, "update_plan"},
		"call_fake_0045": {analyze.ToolAskUser, "request_user_input"},
	}
	for call, w := range want {
		e := byCall(evs, analyze.KindToolCall, call)
		if e.ToolName != w[0] || e.ToolNameRaw != w[1] {
			t.Errorf("%s: %s/%s, want %v", call, e.ToolName, e.ToolNameRaw, w)
		}
	}
	if r := byCall(evs, analyze.KindToolResult, "call_fake_0043"); r.ToolSuccess == nil || !*r.ToolSuccess {
		t.Errorf("wait result success %s", js(r.ToolSuccess))
	}
	if r := byCall(evs, analyze.KindToolResult, "call_fake_0042"); r.ToolSuccess != nil || r.DurationMS == nil {
		t.Errorf("exec result: success %s duration %s", js(r.ToolSuccess), js(r.DurationMS))
	}
	if r := byCall(evs, analyze.KindToolResult, "call_fake_0099"); r.ToolName != analyze.ToolOther || r.DurationMS != nil {
		t.Errorf("orphan output: %s %s", r.ToolName, js(r.DurationMS))
	}
	if evs[0].Origin != analyze.OriginCLI || analyzetest.Kinds(evs)[analyze.KindError] != 1 {
		t.Errorf("origin %s, kinds %v", evs[0].Origin, analyzetest.Kinds(evs))
	}
	if o := run(t).Events[s2][0].Origin; o != analyze.OriginIDE {
		t.Errorf("vscode origin %s", o)
	}
}

func TestCompactionOrdinalsPartialTail(t *testing.T) {
	evs := run(t).Events[s3]
	k := analyzetest.Kinds(evs)
	if k[analyze.KindCompaction] != 1 || k[analyze.KindModelRequest] != 2 || k[analyze.KindSystemContext] != 1 {
		t.Errorf("kinds %v", k)
	}
	if evs[0].Seq != "0" {
		t.Errorf("first Seq %q, want the record ordinal", evs[0].Seq)
	}
	for _, e := range evs {
		if strings.HasPrefix(e.Seq, "99") {
			t.Errorf("event from the partial trailing line: %s", e.Seq)
		}
	}
	t21 := "00000000-0000-4000-9000-000000000021"
	checkRequests(t, evs, []reqWant{{t21 + "#1", tp(tok(2000, 0, 100, 0))}, {t21 + "#2", tp(tok(200, 100, 20, 0))}})
	if r := requests(evs)[1]; r.BranchCoverage != analyze.Inferred || r.Branch != "main" {
		t.Errorf("branch %q %s", r.Branch, r.BranchCoverage)
	}
}

func TestNormalizeHook(t *testing.T) {
	at := time.Date(2026, 1, 5, 10, 0, 0, 0, time.FixedZone("x", 3600))
	env := func(ev, stdin string, git *analyze.GitInfo) analyze.Envelope {
		return analyze.Envelope{V: 1, AdapterID: ID, Event: ev, RecordedAt: at, Git: git, Stdin: stdin,
			File: filepath.Join("spool", "00000000-0000-4000-8000-0000000000aa.json")}
	}
	stdin := `{"session_id":"00000000-0000-4000-8000-000000000001","turn_id":"t-1","cwd":"/srv/projects/lighthouse","model":"gpt-5-codex"}`
	kinds := map[string]analyze.Kind{"SessionStart": analyze.KindSessionStart, "SessionEnd": analyze.KindSessionEnd,
		"Stop": analyze.KindTurnEnd, "PermissionRequest": analyze.KindPermissionReq,
		"Interrupt": analyze.KindInterrupt, "PreCompact": analyze.KindCompaction}
	for ev, kind := range kinds {
		got, err := Adapter{}.NormalizeHook(env(ev, stdin, &analyze.GitInfo{Branch: "main", Commit: "abc"}))
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %v %v", ev, got, err)
		}
		e := got[0]
		if e.Kind != kind || e.ObservedBy != analyze.FromRegistry || e.NativeSessionID != s1 || e.CorrelationID != "t-1" ||
			e.Seq != "registry:00000000-0000-4000-8000-0000000000aa.json" || !e.TS.Equal(at) || e.TS.Location() != time.UTC ||
			e.Branch != "main" || e.Commit != "abc" || e.BranchCoverage != analyze.Measured || e.Tool != ID || e.Store != "default" {
			t.Errorf("%s: %+v", ev, e)
		}
	}
	got, err := Adapter{}.NormalizeHook(env("Stop", stdin, nil))
	if err != nil || got[0].BranchCoverage != analyze.Unknown || got[0].Branch != "" {
		t.Errorf("no git: %+v %v", got, err)
	}
	if got, err := (Adapter{}).NormalizeHook(env("PreToolUse", stdin, nil)); got != nil || err != nil {
		t.Errorf("unregistered event: %v %v", got, err)
	}
	if _, err := (Adapter{}).NormalizeHook(env("Stop", "not json", nil)); err == nil {
		t.Error("bad stdin accepted")
	}
	if _, err := (Adapter{}).NormalizeHook(env("Stop", `{"turn_id":"t"}`, nil)); err == nil {
		t.Error("missing session_id accepted")
	}
}

func TestClassifyShell(t *testing.T) {
	cases := []struct {
		cmd, name string
		files     []string
	}{
		{"cat a.go b.go", analyze.ToolReadFile, []string{"a.go", "b.go"}},
		{"sed -n 1,20p 'x/y.go'", analyze.ToolReadFile, []string{"x/y.go"}},
		{"head -n 5 notes.md | wc -l", analyze.ToolReadFile, nil},
		{"Get-Content src/main.go", analyze.ToolReadFile, []string{"src/main.go"}},
		{"rg -n TODO", analyze.ToolSearch, nil},
		{"Get-ChildItem -Recurse", analyze.ToolSearch, nil},
		{"sed -i s/a/b/ f.go", analyze.ToolShell, nil},
		{"go test ./...", analyze.ToolShell, nil},
		{"", analyze.ToolShell, nil},
	}
	for _, c := range cases {
		name, files := classifyShell(c.cmd)
		if name != c.name || !reflect.DeepEqual(files, c.files) {
			t.Errorf("%q: %s %v, want %s %v", c.cmd, name, files, c.name, c.files)
		}
	}
	if got := shellCommand([]byte(`{"command":["bash","-lc","ls src"]}`)); got != "ls src" {
		t.Errorf("argv command %q", got)
	}
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Text carries the words only on prompts, answers and shell command lines
// (write_stdin and exec waits have none); Cwd comes from session_meta and
// turn_context and carries forward.
func TestTextAndCwd(t *testing.T) {
	res := run(t)
	got := map[string]bool{}
	for sid, evs := range res.Events {
		for _, e := range evs {
			switch {
			case e.Kind == analyze.KindUserPrompt, e.Kind == analyze.KindAssistantText,
				e.Kind == analyze.KindToolCall && e.ToolName == analyze.ToolShell && e.ToolNameRaw != "write_stdin" && e.ToolNameRaw != "wait":
				if e.Text == "" {
					t.Errorf("%s %s (%s %s): empty Text", sid, e.Seq, e.Kind, e.ToolNameRaw)
				}
				got[string(e.Kind)+": "+e.Text] = true
			case e.Text != "":
				t.Errorf("%s %s (%s %s): Text %q", sid, e.Seq, e.Kind, e.ToolNameRaw, e.Text)
			}
			if e.Cwd != "/srv/projects/lighthouse" {
				t.Errorf("%s %s (%s): Cwd %q", sid, e.Seq, e.Kind, e.Cwd)
			}
		}
	}
	if !got["tool_call: go test ./src/forecast/..."] {
		t.Errorf("shell command line missing: %v", got)
	}
}
