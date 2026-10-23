package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/analyzetest"
)

var fixtureRoots = []string{
	filepath.Join("..", "..", "..", "..", "testdata", "analyze", "antigravity", "antigravity", "brain"),
	filepath.Join("..", "..", "..", "..", "testdata", "analyze", "antigravity", "antigravity-cli", "brain"),
}

const (
	convMain      = "00000000-0000-4000-8000-00000000a001"
	convInterrupt = "00000000-0000-4000-8000-00000000a002"
	convCompact   = "00000000-0000-4000-8000-00000000a003"
	convChunked   = "00000000-0000-4000-8000-00000000c001"
	convTools     = "00000000-0000-4000-8000-00000000c002"
)

func run(t *testing.T) analyzetest.Result {
	t.Helper()
	return analyzetest.Run(t, Adapter{}, fixtureRoots, 1)
}

func bySeq(evs []analyze.Event) map[string]analyze.Event {
	m := map[string]analyze.Event{}
	for _, e := range evs {
		m[e.Seq] = e
	}
	return m
}

func TestConformanceAndDiscovery(t *testing.T) {
	res := run(t)
	want := map[string]string{convMain: StoreIDE, convInterrupt: StoreIDE, convChunked: StoreCLI, convTools: StoreCLI}
	if len(res.Sources) != len(want) {
		t.Fatalf("%d sources, want %d", len(res.Sources), len(want))
	}
	for _, s := range res.Sources {
		if want[s.NativeSessionID] != s.Store {
			t.Errorf("%s: store %q, want %q", s.NativeSessionID, s.Store, want[s.NativeSessionID])
		}
		for _, m := range s.Members {
			if filepath.Base(m.Path) == "transcript.jsonl" || strings.Contains(filepath.ToSlash(m.Path), "chunks/transcript/") {
				t.Errorf("%s: compact transcript %s is a member", s.NativeSessionID, m.Path)
			}
		}
		for _, e := range res.Events[s.NativeSessionID] {
			wantOrigin := analyze.OriginIDE
			if s.Store == StoreCLI {
				wantOrigin = analyze.OriginCLI
			}
			if e.Origin != wantOrigin {
				t.Fatalf("%s: origin %q", s.NativeSessionID, e.Origin)
			}
		}
	}
	if _, ok := res.Events[convCompact]; ok {
		t.Error("compact-only conversation discovered")
	}
	ch := res.Sources[2]
	if ch.NativeSessionID != convChunked || len(ch.Members) != 3 || filepath.Base(ch.Members[0].Path) != "00000000.jsonl" ||
		filepath.Base(ch.Members[2].Path) != liveName {
		t.Errorf("chunked members out of order: %+v", ch.Members)
	}
}

func TestStoreForRoot(t *testing.T) {
	for root, want := range map[string]string{
		"/home/user/.gemini/antigravity/brain":     StoreIDE,
		"/home/user/.gemini/antigravity-cli/brain": StoreCLI,
		`D:\copies\antigravity-cli\brain`:          StoreCLI,
		"/srv/other/brain":                         StoreIDE,
	} {
		if got := storeForRoot(filepath.FromSlash(root)); got != want {
			t.Errorf("storeForRoot(%s) = %s, want %s", root, got, want)
		}
	}
	if storeForTranscript(`C:\x\.gemini\antigravity-cli\brain\id\.system_generated\logs\transcript.jsonl`) != StoreCLI ||
		storeForTranscript("/x/.gemini/antigravity/brain/id/.system_generated/logs/transcript.jsonl") != StoreIDE {
		t.Error("storeForTranscript")
	}
}

func stepSeqs(evs []analyze.Event) map[string]int {
	m := map[string]int{}
	for _, e := range evs {
		s, _, _ := strings.Cut(e.Seq, ":")
		m[s]++
	}
	return m
}

// Acceptance 6: a step in both a chunk and the live file counts once; a step
// only in chunks survives, also after the live file is replaced.
func TestChunkDedupe(t *testing.T) {
	res := run(t)
	evs := res.Events[convChunked]
	steps := stepSeqs(evs)
	if len(steps) != 25 {
		t.Fatalf("%d distinct steps, want 25", len(steps))
	}
	k := analyzetest.Kinds(evs)
	if k[analyze.KindModelRequest] != 12 || k[analyze.KindToolCall] != 12 || k[analyze.KindToolResult] != 12 {
		t.Errorf("kinds %v", k)
	}
	src := res.Sources[2]
	_, st, err := readSteps(src)
	if err != nil || st.Dups != 5 || st.Partial != 0 || st.Malformed != 0 {
		t.Errorf("stats %+v err %v, want 5 dups", st, err)
	}
	// Step 10 straddles the two chunks.
	if e, ok := bySeq(evs)["step-10"]; !ok || e.Kind != analyze.KindToolCall {
		t.Errorf("split record step 10 not parsed: %+v", e)
	}

	// Replace the live file in a copy with a shorter one holding only steps
	// 20-24: chunk-only steps 0-19 must survive.
	dir := t.TempDir()
	conv := filepath.Join(dir, "antigravity-cli", "brain", convChunked)
	if err := os.CopyFS(conv, os.DirFS(src.Locator)); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(logsDir(conv), liveName)
	data, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if err := os.WriteFile(live, []byte(strings.Join(lines[5:], "")), 0o644); err != nil {
		t.Fatal(err)
	}
	res2 := analyzetest.Run(t, Adapter{}, []string{filepath.Dir(conv)}, 1)
	steps2 := stepSeqs(res2.Events[convChunked])
	if len(steps2) != 25 || steps2["step-0"] == 0 || steps2["step-14"] == 0 {
		t.Errorf("after replacing the live file: %d steps", len(steps2))
	}
	// And with the live file gone entirely.
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	res3 := analyzetest.Run(t, Adapter{}, []string{filepath.Dir(conv)}, 1)
	if n := len(stepSeqs(res3.Events[convChunked])); n != 20 {
		t.Errorf("chunks only: %d steps, want 20", n)
	}
}

func TestTokenUsage(t *testing.T) {
	res := run(t)
	// Only the main conversation's last four planner steps carry usage.
	withUsage := map[string]string{
		"step-11": `{"input_tokens":5200,"cache_read_tokens":0,"output_tokens":310}`,
		"step-14": `{"input_tokens":640,"cache_read_tokens":5248,"output_tokens":455}`,
		"step-17": `{"input_tokens":410,"cache_read_tokens":5888,"output_tokens":120}`,
		"step-19": `{"input_tokens":220,"cache_read_tokens":6298,"output_tokens":95}`,
	}
	for id, evs := range res.Events {
		for _, e := range evs {
			if e.Kind != analyze.KindModelRequest {
				continue
			}
			if !strings.HasPrefix(e.RequestID, "step-") {
				t.Errorf("%s %s: request id %q", id, e.Seq, e.RequestID)
			}
			native, ok := withUsage[e.Seq]
			if id != convMain || !ok {
				if e.Tokens != nil || e.TokensCoverage != analyze.Unknown || len(e.TokensNative) != 0 {
					t.Errorf("%s %s: usage %v coverage %s native %s, want none", id, e.Seq, e.Tokens, e.TokensCoverage, e.TokensNative)
				}
				continue
			}
			if e.TokensCoverage != analyze.Partial || string(e.TokensNative) != native {
				t.Errorf("%s: coverage %s native %s", e.Seq, e.TokensCoverage, e.TokensNative)
			}
			if tk := e.Tokens; tk == nil || tk.InUncached == nil || tk.InCacheRead == nil || tk.OutVisible == nil ||
				tk.InCacheWrite != nil || tk.OutThinking != nil {
				t.Errorf("%s: buckets %+v", e.Seq, e.Tokens)
			}
		}
	}

	sum, n, requests := analyzetest.TokenSum(res.Events[convMain])
	if requests != 7 || n != 4 {
		t.Fatalf("main: %d of %d requests with usage, want 4 of 7", n, requests)
	}
	// Hand-computed: uncached 5200+640+410+220, cache read 0+5248+5888+6298.
	if sum.InUncached == nil || *sum.InUncached != 6470 || sum.InCacheRead == nil || *sum.InCacheRead != 17434 {
		t.Errorf("main: in_uncached %v in_cache_read %v, want 6470 and 17434", sum.InUncached, sum.InCacheRead)
	}
	// Output (thinking included, unsplittable) is all OutVisible: 120+310+455+95.
	if sum.OutVisible == nil || *sum.OutVisible != 980 {
		t.Errorf("main: out_visible %v, want 980", sum.OutVisible)
	}
	if sum.InCacheWrite != nil || sum.OutThinking != nil {
		t.Errorf("main: null buckets summed to %+v", sum)
	}
	if frac := float64(n) / float64(requests); frac >= 1 {
		t.Errorf("main: coverage fraction %v, want < 1", frac)
	}
	for _, id := range []string{convInterrupt, convChunked, convTools} {
		if _, n, r := analyzetest.TokenSum(res.Events[id]); n != 0 || r == 0 {
			t.Errorf("%s: %d of %d requests with usage, want 0 of some", id, n, r)
		}
	}
}

func TestUsageEdgeCases(t *testing.T) {
	raw := func(s string) json.RawMessage {
		if s == "" {
			return nil
		}
		return json.RawMessage(s)
	}
	for _, c := range []struct {
		in, cr, out string
		wantNil     bool
		native      string
	}{
		{"", "", "", true, ""},
		{"", "", "7", false, `{"output_tokens":7}`}, // output alone still counts (OutVisible)
		{"-1", "", "", true, `{"input_tokens":-1}`},
		{"12", "", "", false, `{"input_tokens":12}`},
		{"1.5", "3", "", false, `{"input_tokens":1.5,"cache_read_tokens":3}`},
	} {
		s := &step{InputTokens: raw(c.in), CacheReadTokens: raw(c.cr), OutputTokens: raw(c.out)}
		tk, native := usage(s)
		if (tk == nil) != c.wantNil || string(native) != c.native {
			t.Errorf("%+v: tokens %+v native %s", c, tk, native)
		}
		if tk != nil && (tk.OutThinking != nil || tk.InCacheWrite != nil) {
			t.Errorf("%+v: unexpected bucket %+v", c, tk)
		}
	}
}

func TestToolsAndDurations(t *testing.T) {
	res := run(t)
	m := bySeq(res.Events[convMain])
	for seq, want := range map[string]int64{"step-3:result": 1000, "step-4:result": 2000, "step-7:result": 2000, "step-12:result": 6000} {
		e := m[seq]
		if e.DurationMS == nil || *e.DurationMS != want || e.DurationCoverage != analyze.Measured {
			t.Errorf("%s: duration %v (%s), want %d measured", seq, e.DurationMS, e.DurationCoverage, want)
		}
	}
	// The invalid-tool-call error has a Created At but no Completed At.
	if e := m["step-13:result"]; e.Kind != analyze.KindToolResult || e.DurationMS != nil || e.DurationCoverage != analyze.Unknown || *e.ToolSuccess {
		t.Errorf("error-message result: %+v", e)
	}
	if m["step-13:error"].Kind != analyze.KindError || m["step-13"].ToolNameRaw != "view_file" {
		t.Error("ERROR_MESSAGE answering a call")
	}
	wantNames := map[string][2]string{
		"step-3":  {analyze.ToolReadFile, "view_file"},
		"step-4":  {analyze.ToolSearch, "grep_search"},
		"step-7":  {analyze.ToolEdit, "replace_file_content"},
		"step-9":  {analyze.ToolShell, "run_command"},
		"step-15": {analyze.ToolMCP, "call_mcp_tool"},
		"step-16": {analyze.ToolWeb, "search_web"},
		"step-18": {analyze.ToolOther, "manage_task"},
	}
	for seq, w := range wantNames {
		if e := m[seq]; e.ToolName != w[0] || e.ToolNameRaw != w[1] || e.CorrelationID != seq || m[seq+":result"].CorrelationID != seq {
			t.Errorf("%s: %s/%s corr %s", seq, e.ToolName, e.ToolNameRaw, e.CorrelationID)
		}
	}
	if e := m["step-12:result"]; e.ToolSuccess == nil || *e.ToolSuccess {
		t.Error("non-zero exit code counted as success")
	}
	if e := m["step-3:result"]; e.ToolSuccess == nil || !*e.ToolSuccess {
		t.Error("DONE step not a success")
	}
	if e := m["step-3"]; len(e.Files) != 1 || !strings.HasSuffix(e.Files[0], "report.go") {
		t.Errorf("files %v", e.Files)
	}
	// Same arguments, same hash: the two go test runs.
	if m["step-9"].ToolArgsHash != m["step-12"].ToolArgsHash {
		t.Error("identical run_command arguments hash differently")
	}
	tools := bySeq(res.Events[convTools])
	for seq, w := range map[string]string{"step-2": analyze.ToolSearch, "step-3": analyze.ToolSearch, "step-5": analyze.ToolEdit,
		"step-7": analyze.ToolWeb, "step-8": analyze.ToolSubagent, "step-10": analyze.ToolAskUser, "step-12": analyze.ToolOther} {
		if tools[seq].ToolName != w {
			t.Errorf("tools %s: %s, want %s", seq, tools[seq].ToolName, w)
		}
	}
	// Steps written out of order come back in step order.
	evs := res.Events[convTools]
	for i := 1; i < len(evs); i++ {
		if stepNum(evs[i].Seq) < stepNum(evs[i-1].Seq) {
			t.Fatalf("events out of step order at %d", i)
		}
	}
}

func stepNum(seq string) int {
	s, _, _ := strings.Cut(strings.TrimPrefix(seq, "step-"), ":")
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func TestInterruptsDenialsModel(t *testing.T) {
	res := run(t)
	main := res.Events[convMain]
	m := bySeq(main)
	if m["step-9:denied"].Kind != analyze.KindPermissionDeny || m["step-9:result"].ToolSuccess == nil || *m["step-9:result"].ToolSuccess {
		t.Error("permission denial not detected")
	}
	if m["step-18:error"].Kind != analyze.KindError {
		t.Error("ERROR generic step without an error event")
	}
	k := analyzetest.Kinds(main)
	if k[analyze.KindPermissionDeny] != 1 || k[analyze.KindInterrupt] != 0 || k[analyze.KindCheckpoint] != 1 || k[analyze.KindThinking] != 1 ||
		k[analyze.KindSystemContext] != 1 || k[analyze.KindUserPrompt] != 2 || k[analyze.KindModelSwitch] != 1 {
		t.Errorf("main kinds %v", k)
	}
	// The trailing partial line (step 20) is not read.
	if _, ok := m["step-20"]; ok {
		t.Error("trailing partial line parsed")
	}
	if m["step-2"].Model != "Gemini 3 Pro (High)" || m["step-11"].Model != "Gemini 3 Flash" || m["step-10:model"].Model != "Gemini 3 Flash" {
		t.Errorf("models %q %q", m["step-2"].Model, m["step-11"].Model)
	}
	if p := m["step-0"]; p.Kind != analyze.KindUserPrompt || p.TextLen != len("Add a total_weight field to the harvest report in orchard-ledger.") {
		t.Errorf("user prompt text length %d", p.TextLen)
	}

	in := bySeq(res.Events[convInterrupt])
	ki := analyzetest.Kinds(res.Events[convInterrupt])
	// Step 2 runs in the background while the model continues: no interrupt.
	// Step 4 is cut off by the user's prompt at step 5; step 6 is a
	// cancellation message.
	if ki[analyze.KindInterrupt] != 2 || in["step-5:interrupt"].Kind != analyze.KindInterrupt || in["step-6"].Kind != analyze.KindInterrupt {
		t.Errorf("interrupts %v", ki)
	}
	if _, ok := in["step-4:result"]; ok {
		t.Error("RUNNING step has a result")
	}
	if in["step-7"].Kind != analyze.KindSystemContext || in["step-1"].Model != "" {
		t.Error("plain system message or model without settings note")
	}
	for _, e := range res.Events[convTools] {
		if e.Model != "" {
			t.Fatalf("model %q without a settings note", e.Model)
		}
	}
}

func TestTextVolume(t *testing.T) {
	res := run(t)
	m := bySeq(res.Events[convMain])
	for _, seq := range []string{"step-0", "step-2:thinking", "step-5:text", "step-3:result"} {
		if m[seq].TextVolumeUnits == 0 {
			t.Errorf("%s: no text volume", seq)
		}
	}
	if m["step-2"].TextVolumeUnits != 0 {
		t.Error("model_request carries text volume")
	}
}

func TestNormalizeHook(t *testing.T) {
	at := time.Date(2026, 3, 2, 11, 0, 0, 0, time.FixedZone("x", 7200))
	stdin := func(n int, path string) string {
		b, _ := json.Marshal(map[string]any{"conversationId": "00000000-0000-4000-8000-00000000a001", "workspacePaths": []string{"/workspace/orchard-ledger"},
			"transcriptPath": path, "modelName": "Gemini 3 Pro", "invocationNum": n, "initialNumSteps": 0})
		return string(b)
	}
	cliPath := "/home/user/.gemini/antigravity-cli/brain/x/.system_generated/logs/transcript.jsonl"
	idePath := "/home/user/.gemini/antigravity/brain/x/.system_generated/logs/transcript.jsonl"
	cases := []struct {
		env    analyze.Envelope
		kind   analyze.Kind
		store  string
		branch analyze.Coverage
	}{
		{analyze.Envelope{Event: "PostInvocation", Stdin: stdin(1, cliPath), Git: &analyze.GitInfo{Branch: "main", Commit: "abc123"}}, analyze.KindSessionStart, StoreCLI, analyze.Measured},
		{analyze.Envelope{Event: "PostInvocation", Stdin: stdin(3, idePath)}, analyze.KindTurnEnd, StoreIDE, analyze.Unknown},
		{analyze.Envelope{Event: "Stop", Stdin: stdin(3, idePath), Git: &analyze.GitInfo{}}, analyze.KindSessionEnd, StoreIDE, analyze.Unknown},
	}
	for i, c := range cases {
		c.env.V, c.env.AdapterID, c.env.RecordedAt, c.env.File = 1, ID, at, filepath.Join("spool", "0000000"+string(rune('a'+i))+".json")
		evs, err := Adapter{}.NormalizeHook(c.env)
		if err != nil || len(evs) != 1 {
			t.Fatalf("case %d: %v %v", i, evs, err)
		}
		e := evs[0]
		if e.Kind != c.kind || e.Store != c.store || e.BranchCoverage != c.branch || e.ObservedBy != analyze.FromRegistry ||
			e.Model != "Gemini 3 Pro" || e.NativeSessionID != "00000000-0000-4000-8000-00000000a001" || e.TS.Location() != time.UTC ||
			!e.TS.Equal(at) || e.Seq != "registry:0000000"+string(rune('a'+i))+".json" || e.DurationCoverage != analyze.Unknown {
			t.Errorf("case %d: %+v", i, e)
		}
		if c.branch == analyze.Measured && (e.Branch != "main" || e.Commit != "abc123") {
			t.Errorf("case %d: branch %q commit %q", i, e.Branch, e.Commit)
		}
	}
	if evs, err := (Adapter{}).NormalizeHook(analyze.Envelope{Event: "PreToolUse", Stdin: "{}", RecordedAt: at}); evs != nil || err != nil {
		t.Error("unknown hook event produced events")
	}
	if _, err := (Adapter{}).NormalizeHook(analyze.Envelope{Event: "Stop", Stdin: "not json", RecordedAt: at}); err == nil {
		t.Error("bad stdin accepted")
	}
	if _, err := (Adapter{}).NormalizeHook(analyze.Envelope{Event: "Stop", Stdin: "{}", RecordedAt: at}); err == nil {
		t.Error("missing conversationId accepted")
	}
}

// Text carries the words only on prompts (the <USER_REQUEST> body), answers
// and run_command command lines; Cwd is set only where run_command records
// one; no model_request has an answer id (the format records none).
func TestTextCwdAndAnswerID(t *testing.T) {
	res := run(t)
	commands := 0
	for sid, evs := range res.Events {
		for _, e := range evs {
			isCmd := e.Kind == analyze.KindToolCall && e.ToolNameRaw == "run_command"
			want := e.Kind == analyze.KindUserPrompt || e.Kind == analyze.KindAssistantText || isCmd
			if (e.Text != "") != want {
				t.Errorf("%s %s (%s %s): Text %q", sid, e.Seq, e.Kind, e.ToolNameRaw, e.Text)
			}
			if strings.Contains(e.Text, "USER_REQUEST") || strings.Contains(e.Text, "ADDITIONAL_METADATA") {
				t.Errorf("%s %s: prompt Text keeps the wrapper: %q", sid, e.Seq, e.Text)
			}
			if isCmd {
				commands++
				if !strings.HasPrefix(e.Text, "go ") {
					t.Errorf("%s %s: command %q", sid, e.Seq, e.Text)
				}
			}
			wantCwd := ""
			if e.ToolNameRaw == "run_command" {
				wantCwd = "/workspace/orchard-ledger"
			}
			if e.Cwd != wantCwd {
				t.Errorf("%s %s (%s %s): Cwd %q, want %q", sid, e.Seq, e.Kind, e.ToolNameRaw, e.Cwd, wantCwd)
			}
			if e.Kind == analyze.KindModelRequest && e.CorrelationID != "" {
				t.Errorf("%s %s: model_request correlation %q", sid, e.Seq, e.CorrelationID)
			}
		}
	}
	if commands == 0 {
		t.Error("no run_command calls in the fixtures")
	}
}
