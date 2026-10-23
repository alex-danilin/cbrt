package claude

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/analyzetest"
)

// Fixtures are written by go run ./scripts/synth-transcripts/claude.
var fixtureRoot = filepath.Join("..", "..", "..", "..", "testdata", "analyze", "claude", "projects")

const (
	s1    = "00000000-0000-4000-8000-000000000001"
	s2    = "00000000-0000-4000-8000-000000000002"
	s3    = "00000000-0000-4000-8000-000000000003"
	s4    = "00000000-0000-4000-8000-000000000004"
	s5    = "00000000-0000-4000-8000-000000000005"
	agent = "a00000000000000f1"
)

func run(t *testing.T) analyzetest.Result {
	t.Helper()
	res := analyzetest.Run(t, Adapter{}, []string{fixtureRoot, filepath.Join(fixtureRoot, "missing")}, 1)
	if len(res.Sources) != 5 {
		t.Fatalf("got %d sources, want 5", len(res.Sources))
	}
	return res
}

func TestRegistered(t *testing.T) {
	if a, ok := analyze.Get("claude"); !ok || a.ID() != "claude" {
		t.Fatal("adapter not registered")
	}
}

func TestDiscover(t *testing.T) {
	res := run(t)
	for i, want := range []string{s1, s2, s3, s4, s5} {
		if got := res.Sources[i].NativeSessionID; got != want {
			t.Errorf("source %d = %s, want %s", i, got, want)
		}
	}
	src := res.Sources[2]
	if len(src.Members) != 2 || !strings.HasSuffix(filepath.ToSlash(src.Members[1].Path), "/subagents/agent-"+agent+".jsonl") {
		t.Errorf("session 3 members: %+v", src.Members)
	}
}

func eq(p *int64, want int64) bool { return p != nil && *p == want }

// Acceptance test 10: token sums computed by hand from the generator's usage
// numbers for session 1 (five requests, one split over three records).
func TestSession1TokensByHand(t *testing.T) {
	evs := run(t).Events[s1]
	sum, withUsage, requests := analyzetest.TokenSum(evs)
	if requests != 5 || withUsage != 5 {
		t.Fatalf("requests %d, with usage %d; want 5, 5", requests, withUsage)
	}
	// input 12+8+5+20+4, cache write 2048+300+0+500+0, cache read
	// 0+2048+2348+2348+2848, output 180+60+40+300+25 of which thinking
	// 120+0+10+100+0.
	if !eq(sum.InUncached, 49) || !eq(sum.InCacheWrite, 2848) || !eq(sum.InCacheRead, 9592) ||
		!eq(sum.OutVisible, 375) || !eq(sum.OutThinking, 230) {
		b, _ := json.Marshal(sum)
		t.Fatalf("sums %s", b)
	}
	for _, e := range evs {
		if e.Kind == analyze.KindModelRequest && (e.TokensCoverage != analyze.Measured || e.Model != "claude-opus-4-5" || len(e.TokensNative) == 0) {
			t.Errorf("request %s: coverage %s model %q", e.RequestID, e.TokensCoverage, e.Model)
		}
	}
}

func TestSplitRequestCountedOnce(t *testing.T) {
	evs := run(t).Events[s1]
	n, seq := 0, ""
	blocks := 0
	for _, e := range evs {
		if e.Kind == analyze.KindModelRequest && e.RequestID == "req_fake_s1_a" {
			n++
			seq = e.Seq
		}
		if e.Kind == analyze.KindThinking || e.Kind == analyze.KindAssistantText || e.Kind == analyze.KindToolCall {
			blocks++
		}
	}
	if n != 1 {
		t.Fatalf("req_fake_s1_a counted %d times", n)
	}
	// Emitted on the last record of the request, which also holds the Read
	// call, so the Seq carries an index suffix.
	if !strings.HasSuffix(seq, "#1") {
		t.Errorf("model_request Seq %q", seq)
	}
	if blocks != 8 {
		t.Errorf("%d block events, want 8", blocks)
	}
}

func TestKinds(t *testing.T) {
	res := run(t)
	want := map[string]map[analyze.Kind]int{
		s1: {analyze.KindUserPrompt: 2, analyze.KindSystemContext: 1, analyze.KindThinking: 2, analyze.KindAssistantText: 3,
			analyze.KindToolCall: 3, analyze.KindToolResult: 3, analyze.KindModelRequest: 5},
		s2: {analyze.KindUserPrompt: 4, analyze.KindSystemContext: 3, analyze.KindCompaction: 1, analyze.KindAssistantText: 2,
			analyze.KindModelSwitch: 1, analyze.KindError: 2, analyze.KindInterrupt: 1, analyze.KindModelRequest: 2},
		s4: {analyze.KindUserPrompt: 1, analyze.KindToolCall: 3, analyze.KindToolResult: 3, analyze.KindPermissionDeny: 1,
			analyze.KindModelRequest: 3},
	}
	for sid, w := range want {
		got := analyzetest.Kinds(res.Events[sid])
		for k, n := range w {
			if got[k] != n {
				t.Errorf("%s: %s = %d, want %d", sid, k, got[k], n)
			}
		}
		total := 0
		for _, n := range w {
			total += n
		}
		if len(res.Events[sid]) != total {
			t.Errorf("%s: %d events, want %d (%v)", sid, len(res.Events[sid]), total, got)
		}
	}
	for _, e := range res.Events[s2] {
		if e.Kind == analyze.KindModelSwitch && e.Model != "claude-haiku-4-5" {
			t.Errorf("model switch to %q", e.Model)
		}
		if e.Origin != analyze.OriginIDE {
			t.Errorf("session 2 origin %q", e.Origin)
		}
	}
}

func TestBranchPerEvent(t *testing.T) {
	evs := run(t).Events[s1]
	seen := map[string]int{}
	switched := false
	for _, e := range evs {
		if e.BranchCoverage != analyze.Measured {
			t.Errorf("%s: branch coverage %s", e.Seq, e.BranchCoverage)
		}
		if e.Kind == analyze.KindUserPrompt && seen["main"] > 0 {
			switched = true
		}
		want := "main"
		if switched {
			want = "feature/word-count"
		}
		if e.Branch != want {
			t.Errorf("%s (%s): branch %q, want %q", e.Seq, e.Kind, e.Branch, want)
		}
		seen[e.Branch]++
	}
	if seen["main"] == 0 || seen["feature/word-count"] == 0 {
		t.Errorf("branches %v", seen)
	}
	for _, e := range run(t).Events[s4] {
		if e.Branch != "" || e.BranchCoverage != analyze.Unknown {
			t.Errorf("session 4: branch %q coverage %s", e.Branch, e.BranchCoverage)
		}
	}
}

func TestToolCallsAndResults(t *testing.T) {
	res := run(t)
	type got struct {
		name    string
		ok      bool
		ms      int64
		files   []string
		hasHash bool
	}
	results := map[string]got{}
	calls := map[string]got{}
	for _, sid := range []string{s1, s4} {
		for _, e := range res.Events[sid] {
			switch e.Kind {
			case analyze.KindToolCall:
				calls[e.CorrelationID] = got{name: e.ToolName, files: e.Files, hasHash: len(e.ToolArgsHash) == 40}
			case analyze.KindToolResult:
				g := got{name: e.ToolName, ok: e.ToolSuccess != nil && *e.ToolSuccess}
				if e.DurationMS != nil && e.DurationCoverage == analyze.Measured {
					g.ms = *e.DurationMS
				}
				results[e.CorrelationID] = g
			}
		}
	}
	want := map[string]got{
		"toolu_fake_s1_01": {name: analyze.ToolReadFile, ok: true, ms: 2500},
		"toolu_fake_s1_02": {name: analyze.ToolShell, ok: false, ms: 4000},
		"toolu_fake_s1_03": {name: analyze.ToolEdit, ok: true, ms: 400},
		"toolu_fake_s4_01": {name: analyze.ToolMCP, ok: true, ms: 600},
		"toolu_fake_s4_02": {name: analyze.ToolWeb, ok: true, ms: 1500},
		"toolu_fake_s4_03": {name: analyze.ToolShell, ok: false, ms: 8000},
	}
	for id, w := range want {
		r := results[id]
		if r.name != w.name || r.ok != w.ok || r.ms != w.ms {
			t.Errorf("%s result: %+v, want %+v", id, r, w)
		}
		if c := calls[id]; c.name != w.name || !c.hasHash {
			t.Errorf("%s call: %+v", id, c)
		}
	}
	if f := calls["toolu_fake_s1_01"].files; len(f) != 1 || f[0] != "/work/inkwell/cmd/inkwell/main.go" {
		t.Errorf("Read files %v", f)
	}
	for _, e := range res.Events[s4] {
		if e.Kind == analyze.KindPermissionDeny && e.CorrelationID != "toolu_fake_s4_03" {
			t.Errorf("denial correlation %q", e.CorrelationID)
		}
		if e.Origin != analyze.OriginSDK {
			t.Errorf("session 4 origin %q", e.Origin)
		}
	}
}

func TestSubagent(t *testing.T) {
	evs := run(t).Events[s3]
	var main, sub []analyze.Event
	for _, e := range evs {
		switch {
		case e.NativeSessionID == s3 && e.ParentSessionID == "":
			main = append(main, e)
		case e.NativeSessionID == agent && e.ParentSessionID == s3:
			sub = append(sub, e)
		default:
			t.Errorf("event in session %q parent %q", e.NativeSessionID, e.ParentSessionID)
		}
	}
	if k := analyzetest.Kinds(sub); k[analyze.KindSystemContext] != 1 || k[analyze.KindUserPrompt] != 0 ||
		k[analyze.KindModelRequest] != 2 || k[analyze.KindToolResult] != 1 {
		t.Errorf("subagent kinds %v", k)
	}
	// Streaming snapshots: only the final usage counts.
	sum, _, n := analyzetest.TokenSum(sub)
	if n != 2 || !eq(sum.InUncached, 12) || !eq(sum.InCacheWrite, 800) || !eq(sum.InCacheRead, 800) ||
		!eq(sum.OutVisible, 75) || !eq(sum.OutThinking, 30) {
		b, _ := json.Marshal(sum)
		t.Errorf("subagent sums %s over %d requests", b, n)
	}
	for _, e := range main {
		if e.Kind == analyze.KindToolCall && e.ToolName != analyze.ToolSubagent {
			t.Errorf("main tool %q", e.ToolName)
		}
	}
}

func TestPartialTailIgnored(t *testing.T) {
	for _, e := range run(t).Events[s4] {
		if strings.HasPrefix(e.Seq, "00000000-0000-4000-a000-000400000099") {
			t.Fatal("event from the trailing partial line")
		}
	}
}

// Bodies carry the text TextVolumeUnits measured; an image block is a
// placeholder, never its data.
func TestBodies(t *testing.T) {
	src := run(t).Sources[4]
	var results []analyze.Event
	err := Adapter{}.Bodies(src, func(e analyze.Event) error {
		if strings.Contains(e.Body, "iVBORw0KGgo") {
			t.Errorf("%s %s: image data in Body", e.Seq, e.Kind)
		}
		if e.Kind == analyze.KindToolResult {
			results = append(results, e)
		}
		return nil
	})
	if err != nil || len(results) != 2 {
		t.Fatalf("Bodies: %v, %d results", err, len(results))
	}
	if !strings.HasPrefix(results[0].Body, "the quick brown fox") || results[0].TextVolumeUnits < 6000 {
		t.Errorf("large result: %d units, body starts %.20q", results[0].TextVolumeUnits, results[0].Body)
	}
	if results[1].Body != "Captured."+imagePlaceholder {
		t.Errorf("image result body %q", results[1].Body)
	}
}

func TestNormalizeHook(t *testing.T) {
	at := time.Date(2026, 3, 2, 12, 0, 0, 0, time.FixedZone("x", 7200))
	git := &analyze.GitInfo{Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}
	cases := []struct {
		event, stdin string
		git          *analyze.GitInfo
		kind         analyze.Kind
	}{
		{"SessionStart", `{"session_id":"` + s1 + `","cwd":"/work/inkwell","source":"startup","model":"claude-opus-4-5"}`, git, analyze.KindSessionStart},
		{"SessionEnd", `{"session_id":"` + s1 + `","reason":"exit"}`, git, analyze.KindSessionEnd},
		{"Stop", `{"session_id":"` + s1 + `","stop_hook_active":false}`, nil, analyze.KindTurnEnd},
		{"PermissionRequest", `{"session_id":"` + s1 + `","tool_name":"Bash","tool_input":{"command":"go test ./..."},"tool_use_id":"toolu_fake_s1_02"}`, nil, analyze.KindPermissionReq},
		{"PreCompact", `{"session_id":"` + s1 + `","trigger":"auto"}`, git, analyze.KindCompaction},
		{"PostModelSwitch", `{"session_id":"` + s1 + `","model":{"id":"claude-haiku-4-5"}}`, nil, analyze.KindModelSwitch},
	}
	for _, c := range cases {
		env := analyze.Envelope{V: 1, AdapterID: "claude", Event: c.event, RecordedAt: at, Git: c.git,
			Stdin: c.stdin, File: filepath.Join("spool", "0000-"+c.event+".json")}
		evs, err := Adapter{}.NormalizeHook(env)
		if err != nil || len(evs) != 1 {
			t.Fatalf("%s: %v %v", c.event, evs, err)
		}
		e := evs[0]
		if e.Kind != c.kind || e.ObservedBy != analyze.FromRegistry || e.NativeSessionID != s1 || e.Tool != "claude" ||
			e.Store != "default" || e.Seq != "registry:0000-"+c.event+".json" || !e.TS.Equal(at) || e.TS.Location() != time.UTC {
			t.Errorf("%s: %+v", c.event, e)
		}
		if c.git != nil && (e.Branch != "main" || e.Commit != git.Commit || e.BranchCoverage != analyze.Measured) {
			t.Errorf("%s: branch %q %q %s", c.event, e.Branch, e.Commit, e.BranchCoverage)
		}
		if c.git == nil && (e.Branch != "" || e.BranchCoverage != analyze.Unknown) {
			t.Errorf("%s: branch %q %s without git", c.event, e.Branch, e.BranchCoverage)
		}
		switch c.kind {
		case analyze.KindPermissionReq:
			if e.CorrelationID != "toolu_fake_s1_02" || e.ToolNameRaw != "Bash" || e.ToolArgsHash == "" || e.ToolName != analyze.ToolShell {
				t.Errorf("permission request: %+v", e)
			}
		case analyze.KindModelSwitch:
			if e.Model != "claude-haiku-4-5" {
				t.Errorf("model switch model %q", e.Model)
			}
		case analyze.KindSessionStart:
			if e.Model != "claude-opus-4-5" {
				t.Errorf("session start model %q", e.Model)
			}
		}
	}
	if evs, err := (Adapter{}).NormalizeHook(analyze.Envelope{Event: "PreToolUse", RecordedAt: at, Stdin: `{}`}); err != nil || evs != nil {
		t.Errorf("unregistered event: %v %v", evs, err)
	}
	if _, err := (Adapter{}).NormalizeHook(analyze.Envelope{Event: "Stop", RecordedAt: at, Stdin: `{"cwd":"/x"}`}); err == nil {
		t.Error("missing session_id accepted")
	}
	if _, err := (Adapter{}).NormalizeHook(analyze.Envelope{Event: "Stop", RecordedAt: at, Stdin: `not json`}); err == nil {
		t.Error("bad stdin accepted")
	}
}

func TestNormalizeTool(t *testing.T) {
	for raw, want := range map[string]string{
		"Read": analyze.ToolReadFile, "Grep": analyze.ToolSearch, "Glob": analyze.ToolSearch,
		"Bash": analyze.ToolShell, "PowerShell": analyze.ToolShell, "Edit": analyze.ToolEdit,
		"Write": analyze.ToolEdit, "NotebookEdit": analyze.ToolEdit, "WebFetch": analyze.ToolWeb,
		"WebSearch": analyze.ToolWeb, "Agent": analyze.ToolSubagent, "Task": analyze.ToolSubagent,
		"AskUserQuestion": analyze.ToolAskUser, "mcp__notes__search": analyze.ToolMCP, "Skill": analyze.ToolOther,
	} {
		if got := normalizeTool(raw); got != want {
			t.Errorf("%s -> %s, want %s", raw, got, want)
		}
	}
}

// Text carries the words only on prompts, answers and shell commands; Cwd is
// the record's cwd; a model_request correlates to the assistant message id.
func TestTextCwdAndAnswerID(t *testing.T) {
	res := run(t)
	for sid, evs := range res.Events {
		for _, e := range evs {
			want := e.Kind == analyze.KindUserPrompt || e.Kind == analyze.KindAssistantText ||
				e.Kind == analyze.KindToolCall && e.ToolName == analyze.ToolShell
			if (e.Text != "") != want {
				t.Errorf("%s %s (%s %s): Text %q", sid, e.Seq, e.Kind, e.ToolNameRaw, e.Text)
			}
			if e.Cwd != "/work/inkwell" {
				t.Errorf("%s %s: Cwd %q", sid, e.Seq, e.Cwd)
			}
			if e.Kind == analyze.KindModelRequest && e.CorrelationID != "msg_"+e.RequestID {
				t.Errorf("%s: request %s correlation %q", sid, e.RequestID, e.CorrelationID)
			}
		}
	}
	got := map[string]bool{}
	for _, e := range res.Events[s1] {
		if e.Text != "" {
			got[string(e.Kind)+": "+e.Text] = true
		}
	}
	for _, w := range []string{
		"user_prompt: Add a --words flag to inkwell that prints the word count of a file.",
		"assistant_text: Done: countWords now uses strings.Fields.",
		"tool_call: go test ./...",
	} {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}
