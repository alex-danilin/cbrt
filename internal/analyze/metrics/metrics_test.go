package metrics

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

var t0 = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

// builder makes a synthetic session; every event gets a fresh Seq.
type builder struct {
	s *analyze.Session
}

func newSession(id string) *builder {
	return &builder{s: &analyze.Session{
		Key:          analyze.SessionKey{AdapterID: "fake", Store: "cli", NativeID: id},
		Project:      "/proj",
		Capabilities: analyze.Capabilities{PermissionSignal: "request"},
	}}
}

func (b *builder) add(min float64, e analyze.Event) *builder {
	e.TS = t0.Add(time.Duration(min * float64(time.Minute)))
	e.Seq = fmt.Sprint(len(b.s.Events) + 1)
	if e.ObservedBy == "" {
		e.ObservedBy = analyze.FromTranscript
	}
	b.s.Events = append(b.s.Events, e)
	return b
}

func (b *builder) prompt(min float64, text string) *builder {
	return b.add(min, analyze.Event{Kind: analyze.KindUserPrompt, Text: text})
}
func (b *builder) say(min float64, text string) *builder {
	return b.add(min, analyze.Event{Kind: analyze.KindAssistantText, Text: text})
}
func (b *builder) call(min float64, tool, id, text string, files ...string) *builder {
	return b.add(min, analyze.Event{Kind: analyze.KindToolCall, ToolName: tool, CorrelationID: id, Text: text,
		Files: files, ToolArgsHash: "h-" + tool + text + fmt.Sprint(files), TextVolumeUnits: 10})
}
func (b *builder) result(min float64, id string, ok bool) *builder {
	return b.add(min, analyze.Event{Kind: analyze.KindToolResult, CorrelationID: id, ToolSuccess: analyze.Bool(ok), TextVolumeUnits: 5})
}
func (b *builder) kind(min float64, k analyze.Kind) *builder {
	return b.add(min, analyze.Event{Kind: k})
}
func (b *builder) req(min float64, cov analyze.Coverage, tok *analyze.Tokens) *builder {
	return b.add(min, analyze.Event{Kind: analyze.KindModelRequest, Tokens: tok, TokensCoverage: cov})
}

var all = analyze.Window{Since: t0.Add(-time.Hour), Until: t0.Add(100 * 24 * time.Hour)}

func win(fromMin, toMin float64) analyze.Window {
	return analyze.Window{Since: t0.Add(time.Duration(fromMin * float64(time.Minute))), Until: t0.Add(time.Duration(toMin * float64(time.Minute)))}
}

func want(t *testing.T, m analyze.SessionMetrics, name string, n float64, cov analyze.Coverage) {
	t.Helper()
	v, ok := m.Values[name]
	if !ok {
		t.Fatalf("%s: missing", name)
	}
	if v.Cov != cov || (cov != analyze.Unknown && math.Abs(v.N-n) > 1e-9) || (cov == analyze.Unknown && v.N != 0) {
		t.Errorf("%s = %v %s, want %v %s", name, v.N, v.Cov, n, cov)
	}
}

func wantFrac(t *testing.T, m analyze.SessionMetrics, name string, f float64) {
	t.Helper()
	v := m.Values[name]
	if v.Frac == nil || math.Abs(*v.Frac-f) > 1e-9 {
		t.Errorf("%s frac = %v, want %v", name, v.Frac, f)
	}
}

func TestClaimedComplete(t *testing.T) {
	m := Session(newSession("a").prompt(0, "add a widget").say(1, "Working on it.").say(2, "Done: the widget is implemented.").s, all)
	want(t, m, ClaimedComplete, 1, analyze.Inferred)

	m = Session(newSession("a").prompt(0, "add a widget").say(1, "I am done reading.").say(2, "Which color should it be?").s, all)
	want(t, m, ClaimedComplete, 0, analyze.Inferred)

	m = Session(newSession("a").prompt(0, "x").kind(1, analyze.KindAssistantText).s, all)
	want(t, m, ClaimedComplete, 0, analyze.Unknown)

	m = Session(newSession("a").prompt(0, "x").kind(1, analyze.KindClaimComplete).s, all)
	want(t, m, ClaimedComplete, 1, analyze.Inferred)

	// Window: only the earlier, non-claiming text is inside.
	m = Session(newSession("a").prompt(0, "x").say(1, "Question?").say(10, "All tests pass.").s, win(0, 5))
	want(t, m, ClaimedComplete, 0, analyze.Inferred)
}

func TestEvidenceComplete(t *testing.T) {
	ok := newSession("a").prompt(0, "fix").
		call(1, analyze.ToolShell, "c1", "go build ./...").result(2, "c1", false).
		call(3, analyze.ToolEdit, "c2", "", "a.go").result(4, "c2", true).
		call(5, analyze.ToolShell, "c3", "go test ./internal/...").result(6, "c3", true).s
	want(t, Session(ok, all), EvidenceComplete, 1, analyze.Inferred)

	// The successful test run is before the last error.
	late := newSession("a").prompt(0, "fix").
		call(1, analyze.ToolShell, "c1", "npm test").result(2, "c1", true).
		add(3, analyze.Event{Kind: analyze.KindError}).s
	want(t, Session(late, all), EvidenceComplete, 0, analyze.Inferred)

	commit := newSession("a").call(1, analyze.ToolShell, "c1", `git commit -m "x"`).result(2, "c1", true).s
	want(t, Session(commit, all), EvidenceComplete, 1, analyze.Inferred)

	notTest := newSession("a").call(1, analyze.ToolShell, "c1", "ls -la").result(2, "c1", true).s
	want(t, Session(notTest, all), EvidenceComplete, 0, analyze.Inferred)

	tr := newSession("a").add(1, analyze.Event{Kind: analyze.KindTestResult, ToolSuccess: analyze.Bool(true)}).s
	want(t, Session(tr, all), EvidenceComplete, 1, analyze.Inferred)

	// Shell calls without recorded commands: cannot tell.
	blind := newSession("a").call(1, analyze.ToolShell, "c1", "").result(2, "c1", true).s
	want(t, Session(blind, all), EvidenceComplete, 0, analyze.Unknown)
}

func TestCorrections(t *testing.T) {
	s := newSession("a").
		prompt(0, "No, start by adding the parser module"). // first prompt never counts
		prompt(5, "add the tokenizer for numbers").
		prompt(10, "no, that is the wrong file").
		prompt(15, "There is no need for logging").           // "no" not at the start
		prompt(20, "please add tokenizer for numbers again"). // repeats prompt 2's terms
		prompt(25, "Actually use tabs").s
	want(t, Session(s, all), Corrections, 3, analyze.Inferred)
	want(t, Session(s, win(9, 16)), Corrections, 1, analyze.Inferred)

	blind := newSession("a").prompt(0, "").prompt(5, "").s
	want(t, Session(blind, all), Corrections, 0, analyze.Unknown)
}

func TestRetries(t *testing.T) {
	s := newSession("a").
		call(0, analyze.ToolReadFile, "1", "", "a.go").
		call(1, analyze.ToolReadFile, "2", "", "a.go"). // exact retry
		call(2, analyze.ToolEdit, "3", "", "b.go").result(3, "3", false).
		call(4, analyze.ToolEdit, "4", "v2", "b.go"). // after error, same file
		call(5, analyze.ToolShell, "5", "make").result(6, "5", false).
		call(7, analyze.ToolShell, "6", "make"). // exact and after error: counted once, as exact
		call(8, analyze.ToolShell, "7", "date").
		call(9, analyze.ToolSearch, "8", "x").call(10, analyze.ToolSearch, "9", "y").
		call(11, analyze.ToolSearch, "10", "z").call(12, analyze.ToolSearch, "11", "w").
		call(13, analyze.ToolSearch, "12", "v").
		call(14, analyze.ToolShell, "13", "date").s // 6 events back: not a retry
	m := Session(s, all)
	want(t, m, RetriesExact, 2, analyze.Measured)
	want(t, m, RetriesAfterError, 1, analyze.Inferred)
	want(t, m, Retries, 3, analyze.Inferred)

	m = Session(s, win(0.5, 2))
	want(t, m, RetriesExact, 1, analyze.Measured)
	want(t, m, Retries, 1, analyze.Measured)

	nohash := newSession("a").add(0, analyze.Event{Kind: analyze.KindToolCall, ToolName: analyze.ToolReadFile}).s
	want(t, Session(nohash, all), RetriesExact, 0, analyze.Unknown)
	want(t, Session(nohash, all), Retries, 0, analyze.Unknown)
}

func TestExplorationAndReads(t *testing.T) {
	s := newSession("a").
		call(0, analyze.ToolReadFile, "1", "", "a.go").result(1, "1", true).
		call(2, analyze.ToolSearch, "2", "q").result(3, "2", true).
		call(4, analyze.ToolWeb, "3", "w").
		call(5, analyze.ToolReadFile, "4", "", "a.go").
		call(6, analyze.ToolEdit, "5", "", "a.go").
		call(7, analyze.ToolReadFile, "6", "", "b.go").
		call(8, analyze.ToolReadFile, "7", "", "a.go").s
	m := Session(s, all)
	want(t, m, ExplorationBeforeFirstEdit, 3, analyze.Measured)
	want(t, m, ExplorationBeforeFirstEditVolume, 3*10+2*5, analyze.Estimated)
	want(t, m, RepeatedReads, 1, analyze.Measured)
	want(t, m, ToolCalls, 7, analyze.Measured)
	want(t, m, ToolCallsPrefix+analyze.ToolReadFile, 4, analyze.Measured)

	m = Session(s, win(1.5, 100))
	want(t, m, ExplorationBeforeFirstEdit, 2, analyze.Measured)
	want(t, m, RepeatedReads, 1, analyze.Measured)
	m = Session(s, win(5.5, 100)) // first edit is before... at 6: nothing before it in the window
	want(t, m, ExplorationBeforeFirstEdit, 0, analyze.Measured)
	want(t, m, RepeatedReads, 0, analyze.Measured)
}

func TestIdleTimes(t *testing.T) {
	h := 60.0
	s := newSession("a").
		prompt(0, "one").say(2, "a"). // model time 2 min
		add(3, analyze.Event{Kind: analyze.KindTurnEnd}).
		prompt(13, "two").say(14, "b"). // user 10 min, model 1 min
		prompt(14+9*h, "three").        // parked (9 h)
		say(14+9*h+3, "c").             // model 3 min
		prompt(14+9*h+3+7.5*h, "four"). // user 7.5 h (under the cap)
		say(14+9*h+3+7.5*h+1, "d").s
	m := Session(s, all)
	want(t, m, InferredUserTime, (10+7.5*h)*60, analyze.Inferred)
	want(t, m, SessionParked, 1, analyze.Inferred)
	want(t, m, InferredModelOrPermissionTime, (2+1+3+1)*60, analyze.Inferred)

	span := (14 + 9*h + 3 + 7.5*h + 1) * 60
	want(t, m, Span, span, analyze.Measured)
	want(t, m, ActiveSpan, span-9*h*60, analyze.Inferred)
	want(t, m, LastEventAt, float64(t0.Unix())+span, analyze.Measured)
	want(t, Session(s, win(-60, -30)), LastEventAt, 0, analyze.Unknown)

	// Window starting mid-gap clips the user time to the window start.
	m = Session(s, win(8, 20))
	want(t, m, InferredUserTime, 5*60, analyze.Inferred)
	want(t, m, InferredModelOrPermissionTime, 60, analyze.Inferred)
}

func TestPermissionWait(t *testing.T) {
	s := newSession("a").
		call(0, analyze.ToolShell, "c1", "rm x").
		add(0.5, analyze.Event{Kind: analyze.KindPermissionReq, CorrelationID: "c1", ObservedBy: analyze.FromRegistry}).
		result(3, "c1", true).
		add(4, analyze.Event{Kind: analyze.KindPermissionDeny}).s
	m := Session(s, all)
	want(t, m, PermissionWaitUpperBound, 150, analyze.Inferred)
	want(t, m, PermissionRequests, 1, analyze.Measured)
	want(t, m, PermissionDenials, 1, analyze.Measured)

	s.Capabilities.PermissionSignal = "denial_only"
	m = Session(s, all)
	want(t, m, PermissionWaitUpperBound, 0, analyze.Unknown)
	want(t, m, PermissionDenials, 1, analyze.Measured)
	s.Capabilities.PermissionSignal = "none"
	m = Session(s, all)
	want(t, m, PermissionWaitUpperBound, 0, analyze.Unknown)
	want(t, m, PermissionDenials, 0, analyze.Unknown)
}

func TestLinkedRegistryDuplicatesIgnored(t *testing.T) {
	b := newSession("a").prompt(0, "x").
		add(0, analyze.Event{Kind: analyze.KindUserPrompt, Text: "x", ObservedBy: analyze.FromRegistry}).
		kind(1, analyze.KindCompaction)
	b.s.LinkedTo = map[string]string{"2": "1"}
	m := Session(b.s, all)
	want(t, m, Turns, 1, analyze.Measured)
	want(t, m, Compactions, 1, analyze.Measured)
}

func tok(in, cr, cw, out, th int64, nils ...int) *analyze.Tokens {
	t := &analyze.Tokens{InUncached: analyze.Int64(in), InCacheRead: analyze.Int64(cr), InCacheWrite: analyze.Int64(cw),
		OutVisible: analyze.Int64(out), OutThinking: analyze.Int64(th)}
	for _, n := range nils {
		switch n {
		case 2:
			t.InCacheWrite = nil
		case 4:
			t.OutThinking = nil
		}
	}
	return t
}

func TestTokens(t *testing.T) {
	s := newSession("a").
		req(0, analyze.Measured, tok(100, 1000, 50, 20, 10)).
		req(1, analyze.Partial, tok(10, 3000, 0, 5, 0, 2)).
		req(2, analyze.Unknown, nil).
		req(3, analyze.Measured, tok(5, 2000, 0, 1, 1)).s
	m := Session(s, all)
	want(t, m, ModelRequests, 4, analyze.Measured)
	want(t, m, TokensInUncached, 115, analyze.Partial)
	wantFrac(t, m, TokensInUncached, 0.75)
	want(t, m, TokensInCacheWrite, 50, analyze.Partial)
	want(t, m, TokensConsumed, 100+1000+50+20+10+10+3000+5+0+5+2000+1+1, analyze.Partial)
	wantFrac(t, m, TokensConsumed, 0.75)
	want(t, m, PeakRequestContext, 3010, analyze.Partial)
	want(t, m, ContextSizeFirst, 1150, analyze.Partial)
	want(t, m, ContextSizeLast, 2005, analyze.Partial)

	m = Session(s, win(-1, 0.5))
	want(t, m, TokensConsumed, 1180, analyze.Measured)
	wantFrac(t, m, TokensConsumed, 1)
	want(t, m, PeakRequestContext, 1150, analyze.Measured)

	m = Session(s, win(1.5, 2.5)) // only the unrecorded request
	want(t, m, TokensConsumed, 0, analyze.Unknown)
	wantFrac(t, m, TokensConsumed, 0)
	want(t, m, PeakRequestContext, 0, analyze.Unknown)

	none := newSession("a").prompt(0, "x").s
	m = Session(none, all)
	want(t, m, TokensConsumed, 0, analyze.Unknown)
	want(t, m, TokensOutThinking, 0, analyze.Unknown)
	want(t, m, ModelRequests, 0, analyze.Measured)
	want(t, m, SubagentTokenShare, 0, analyze.Unknown)
}

func TestCountsAndSubagents(t *testing.T) {
	parent := newSession("p").
		prompt(0, "x").kind(1, analyze.KindCompaction).kind(2, analyze.KindModelSwitch).
		kind(3, analyze.KindInterrupt).kind(4, analyze.KindError).
		call(5, analyze.ToolSubagent, "s1", "").call(6, analyze.ToolSubagent, "s2", "").
		req(7, analyze.Measured, tok(100, 0, 0, 0, 0)).s
	c1 := newSession("c1").req(8, analyze.Measured, tok(250, 0, 0, 50, 0)).s
	c2 := newSession("c2").req(9, analyze.Measured, tok(100, 0, 0, 0, 0)).s
	m := WithChildren(parent, []*analyze.Session{c1, c2}, all)
	for _, n := range []string{Compactions, ModelSwitches, Interrupts, Errors, Turns} {
		want(t, m, n, 1, analyze.Measured)
	}
	want(t, m, SubagentCount, 2, analyze.Measured)
	want(t, m, SubagentSessions, 2, analyze.Measured)
	want(t, m, SubagentTokenShare, 0.8, analyze.Measured)
	wantFrac(t, m, SubagentTokenShare, 1)

	blind := newSession("c3").req(8, analyze.Unknown, nil).s
	m = WithChildren(newSession("p").s, []*analyze.Session{blind}, all)
	want(t, m, SubagentTokenShare, 0, analyze.Unknown)
	if m.MetricsVersion != Version {
		t.Errorf("version %q", m.MetricsVersion)
	}
}

func TestProjectFiles(t *testing.T) {
	var ss []*analyze.Session
	for i := range 4 {
		b := newSession(fmt.Sprint(i)).call(float64(i), analyze.ToolReadFile, "r", "", "shared.go")
		if i < 2 {
			b.call(float64(i), analyze.ToolReadFile, "r2", "", "pair.go")
		}
		if i == 3 {
			b.s.Project = "/other"
		}
		ss = append(ss, b.s)
	}
	got := ProjectFiles(ss, all)
	if len(got) != 1 || got["/proj"]["shared.go"] != 3 || len(got["/proj"]) != 1 {
		t.Errorf("ProjectFiles = %v", got)
	}
	if got := ProjectFiles(ss, win(0.5, 100)); len(got) != 0 {
		t.Errorf("windowed ProjectFiles = %v", got)
	}
}
