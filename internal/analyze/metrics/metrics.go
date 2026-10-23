// Package metrics computes one session's derived facts (architecture doc
// §3.2.3) over the reporting window. Only events inside the window count; the
// full history is used for context only (the session's first prompt, the
// first edit, the event before a prompt).
//
// Every Value carries a Coverage. A fact that cannot be computed for this
// session's tool is Value{Cov: Unknown}, never 0. Durations are seconds;
// token figures are tokens; "volume" figures are text_volume_units (a
// ceil(chars/4) proxy, never tokens).
//
// Registry events that duplicate a transcript event (Session.LinkedTo) are
// ignored everywhere, so a fact is never counted twice.
package metrics

import (
	"sort"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// Version is SessionMetrics.MetricsVersion. Bump it when any formula or
// pattern changes.
const Version = "1"

// Metric names (SessionMetrics.Values keys).
const (
	ClaimedComplete  = "claimed_complete"  // 1 when the last assistant text claims completion; Inferred
	EvidenceComplete = "evidence_complete" // 1 when a successful test run or commit follows the last error; Inferred

	Corrections = "corrections" // user prompts (not the first) that correct or repeat; Inferred

	Retries           = "retries"             // RetriesExact + RetriesAfterError
	RetriesExact      = "retries_exact"       // same tool and args hash within RetryWindow events; Measured
	RetriesAfterError = "retries_after_error" // call right after an error on the same file or command; Inferred

	ExplorationBeforeFirstEdit       = "exploration_before_first_edit"        // read/search/shell calls before the first edit; Measured
	ExplorationBeforeFirstEditVolume = "exploration_before_first_edit_volume" // their calls' and results' text volume; Estimated

	RepeatedReads = "repeated_reads" // distinct files read at least twice; Measured

	InferredUserTime              = "inferred_user_time"                // s, gaps from the agent's last event to the next prompt, each <= IdleCap; Inferred
	SessionParked                 = "session_parked"                    // count of such gaps > IdleCap (excluded from every time figure); Inferred
	InferredModelOrPermissionTime = "inferred_model_or_permission_time" // s, from a prompt to the agent's first event; Inferred
	PermissionWaitUpperBound      = "permission_wait_upper_bound"       // s, permission_request -> its tool_result; Inferred, Unknown unless PermissionSignal "request"
	PermissionRequests            = "permission_requests"               // Measured, Unknown unless PermissionSignal "request"
	PermissionDenials             = "permission_denials"                // Measured, Unknown when PermissionSignal "none"

	Compactions        = "compactions"
	ModelSwitches      = "model_switches"
	SubagentCount      = "subagent_count"       // subagent tool calls; Measured
	SubagentSessions   = "subagent_sessions"    // child sessions with events in the window (WithChildren only)
	SubagentTokenShare = "subagent_token_share" // children's consumed / (parent + children); Unknown without WithChildren

	ModelRequests      = "model_requests"
	TokensInUncached   = "tokens_in_uncached"
	TokensInCacheRead  = "tokens_in_cache_read"
	TokensInCacheWrite = "tokens_in_cache_write"
	TokensOutVisible   = "tokens_out_visible"
	TokensOutThinking  = "tokens_out_thinking"
	TokensConsumed     = "tokens_consumed" // sum of every recorded bucket

	PeakRequestContext = "peak_request_context" // context_size_trend: peak
	ContextSizeFirst   = "context_size_first"   // context_size_trend: first request in the window
	ContextSizeLast    = "context_size_last"    // context_size_trend: last request in the window
	TextVolumeUnits    = "text_volume_units"    // Estimated; the volume proxy for tools without tokens

	Turns           = "turns"      // user prompts
	ToolCalls       = "tool_calls" // all tool calls; per tool under ToolCallsPrefix + ToolName
	ToolCallsPrefix = "tool_calls."
	Errors          = "errors"        // error events
	ToolFailures    = "tool_failures" // tool results with success false
	Interrupts      = "interrupts"
	Span            = "span"        // s, first to last event in the window; Measured
	ActiveSpan      = "active_span" // s, Span minus inter-event gaps > IdleCap; Inferred
	// LastEventAt is the Unix time in seconds of the last event in the window
	// (the state archive files sessions by its month); Measured, Unknown when
	// the window has no event.
	LastEventAt = "last_event_at"
)

// Tunables of the formulas (bump Version when changing them).
const (
	IdleCap     = 8 * time.Hour // longer gaps are session_parked
	RetryWindow = 5             // events
)

// view is a session's deduplicated history plus the window.
type view struct {
	s   *analyze.Session
	w   analyze.Window
	ev  []analyze.Event // full history, linked registry duplicates removed
	in  []bool          // ev[i] in window
	idx map[string]int  // correlation id -> index of its tool_call
}

func newView(s *analyze.Session, w analyze.Window) *view {
	v := &view{s: s, w: w, idx: map[string]int{}}
	for _, e := range s.Events {
		if e.ObservedBy == analyze.FromRegistry && s.LinkedTo[e.Seq] != "" {
			continue
		}
		v.ev = append(v.ev, e)
		v.in = append(v.in, w.Contains(e.TS))
	}
	for i, e := range v.ev {
		if e.Kind == analyze.KindToolCall && e.CorrelationID != "" {
			if _, ok := v.idx[e.CorrelationID]; !ok {
				v.idx[e.CorrelationID] = i
			}
		}
	}
	return v
}

// callOf returns the tool_call a tool_result at i answers: by correlation
// id, else the nearest earlier tool_call. -1 when none.
func (v *view) callOf(i int) int {
	if id := v.ev[i].CorrelationID; id != "" {
		if j, ok := v.idx[id]; ok && j < i {
			return j
		}
		return -1
	}
	for j := i - 1; j >= 0; j-- {
		if v.ev[j].Kind == analyze.KindToolCall {
			return j
		}
	}
	return -1
}

func failed(e analyze.Event) bool {
	return e.Kind == analyze.KindError || (e.Kind == analyze.KindToolResult && e.ToolSuccess != nil && !*e.ToolSuccess)
}

func succeeded(e analyze.Event) bool { return e.ToolSuccess != nil && *e.ToolSuccess }

func measured(n float64) analyze.Value { return analyze.Value{N: n, Cov: analyze.Measured} }
func inferred(n float64) analyze.Value { return analyze.Value{N: n, Cov: analyze.Inferred} }
func unknown() analyze.Value           { return analyze.Value{Cov: analyze.Unknown} }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func frac(n, d int) *float64 {
	f := float64(n) / float64(d)
	return &f
}

// Session computes every per-session fact of s over w. SubagentTokenShare is
// Unknown here: it needs the child sessions (WithChildren).
func Session(s *analyze.Session, w analyze.Window) analyze.SessionMetrics {
	v := newView(s, w)
	m := analyze.SessionMetrics{
		Session:        s.Key,
		Project:        s.Project,
		Window:         w,
		MetricsVersion: Version,
		Values:         map[string]analyze.Value{},
		Flags:          append([]string(nil), s.Flags...),
	}
	put := func(name string, val analyze.Value) { m.Values[name] = val }

	put(ClaimedComplete, v.claimedComplete())
	put(EvidenceComplete, v.evidenceComplete())
	put(Corrections, v.corrections())
	exact, afterErr := v.retries()
	put(RetriesExact, exact)
	put(RetriesAfterError, afterErr)
	put(Retries, sumRetries(exact, afterErr))
	cnt, vol := v.explorationBeforeFirstEdit()
	put(ExplorationBeforeFirstEdit, cnt)
	put(ExplorationBeforeFirstEditVolume, vol)
	put(RepeatedReads, v.repeatedReads())
	user, parked, model := v.idle()
	put(InferredUserTime, user)
	put(SessionParked, parked)
	put(InferredModelOrPermissionTime, model)
	reqs, wait := v.permissionWait()
	put(PermissionRequests, reqs)
	put(PermissionWaitUpperBound, wait)
	put(PermissionDenials, v.permissionDenials())
	put(Compactions, measured(float64(v.count(analyze.KindCompaction))))
	put(ModelSwitches, measured(float64(v.count(analyze.KindModelSwitch))))
	put(Interrupts, measured(float64(v.count(analyze.KindInterrupt))))
	put(Errors, measured(float64(v.count(analyze.KindError))))
	put(Turns, measured(float64(v.count(analyze.KindUserPrompt))))
	put(SubagentCount, measured(float64(v.toolCalls()[analyze.ToolSubagent])))
	put(SubagentTokenShare, unknown())
	for name, val := range v.tokens().values() {
		put(name, val)
	}
	for name, val := range v.context() {
		put(name, val)
	}
	calls := v.toolCalls()
	total := 0
	for name, n := range calls {
		total += n
		put(ToolCallsPrefix+name, measured(float64(n)))
	}
	put(ToolCalls, measured(float64(total)))
	failures, volume := 0, 0
	for i, e := range v.ev {
		if !v.in[i] {
			continue
		}
		volume += e.TextVolumeUnits
		if e.Kind == analyze.KindToolResult && failed(e) {
			failures++
		}
	}
	put(ToolFailures, measured(float64(failures)))
	put(TextVolumeUnits, analyze.Value{N: float64(volume), Cov: analyze.Estimated})
	span, active := v.span()
	put(Span, span)
	put(ActiveSpan, active)
	put(LastEventAt, v.lastEventAt())
	return m
}

// lastEventAt: Unix seconds of the last event in the window.
func (v *view) lastEventAt() analyze.Value {
	var last time.Time
	for i, e := range v.ev {
		if v.in[i] && e.TS.After(last) {
			last = e.TS
		}
	}
	if last.IsZero() {
		return unknown()
	}
	return measured(float64(last.UnixNano()) / 1e9)
}

func sumRetries(a, b analyze.Value) analyze.Value {
	if a.Cov == analyze.Unknown {
		return unknown()
	}
	cov := analyze.Measured
	if b.N > 0 {
		cov = analyze.Inferred
	}
	return analyze.Value{N: a.N + b.N, Cov: cov}
}

func (v *view) count(k analyze.Kind) int {
	n := 0
	for i, e := range v.ev {
		if v.in[i] && e.Kind == k {
			n++
		}
	}
	return n
}

func (v *view) toolCalls() map[string]int {
	out := map[string]int{}
	for i, e := range v.ev {
		if v.in[i] && e.Kind == analyze.KindToolCall {
			name := e.ToolName
			if name == "" {
				name = analyze.ToolOther
			}
			out[name]++
		}
	}
	return out
}

// claimedComplete: the last assistant_text in the window with text matches
// CompletionPattern, or the adapter emitted task_claimed_complete after the
// window's last user prompt. Unknown when no assistant text was recorded.
func (v *view) claimedComplete() analyze.Value {
	lastText, lastPrompt, claim := -1, -1, -1
	for i, e := range v.ev {
		if !v.in[i] {
			continue
		}
		switch e.Kind {
		case analyze.KindAssistantText:
			if e.Text != "" {
				lastText = i
			}
		case analyze.KindUserPrompt:
			lastPrompt = i
		case analyze.KindClaimComplete:
			claim = i
		}
	}
	if claim >= 0 && claim > lastPrompt {
		return inferred(1)
	}
	if lastText < 0 {
		return unknown()
	}
	return inferred(b2f(CompletionPattern.MatchString(v.ev[lastText].Text)))
}

// evidenceComplete: after the last error in the window (error event or
// failed tool result), a successful test_result, or a successful shell
// tool_result whose call's command matches TestRunPattern or CommitPattern.
// Unknown when the window has shell calls but none recorded its command and
// there is no test_result.
func (v *view) evidenceComplete() analyze.Value {
	lastErr := -1
	shell, shellText, tests := 0, 0, 0
	for i, e := range v.ev {
		if !v.in[i] {
			continue
		}
		if failed(e) {
			lastErr = i
		}
		if e.Kind == analyze.KindToolCall && e.ToolName == analyze.ToolShell {
			shell++
			if e.Text != "" {
				shellText++
			}
		}
		if e.Kind == analyze.KindTestResult {
			tests++
		}
	}
	if shell > 0 && shellText == 0 && tests == 0 {
		return unknown()
	}
	for i := lastErr + 1; i < len(v.ev); i++ {
		e := v.ev[i]
		if !v.in[i] || !succeeded(e) {
			continue
		}
		if e.Kind == analyze.KindTestResult {
			return inferred(1)
		}
		if e.Kind != analyze.KindToolResult {
			continue
		}
		c := v.callOf(i)
		if c < 0 || v.ev[c].ToolName != analyze.ToolShell {
			continue
		}
		if t := v.ev[c].Text; TestRunPattern.MatchString(t) || CommitPattern.MatchString(t) {
			return inferred(1)
		}
	}
	return inferred(0)
}

// corrections: user prompts in the window, other than the session's first,
// that match CorrectionPattern or repeat an earlier prompt's key terms.
// Unknown when such prompts exist but none recorded its text.
func (v *view) corrections() analyze.Value {
	var earlier []map[string]bool
	first := true
	n, considered, withText := 0, 0, 0
	for i, e := range v.ev {
		if e.Kind != analyze.KindUserPrompt {
			continue
		}
		terms := keyTerms(e.Text)
		if !first && v.in[i] {
			considered++
			if e.Text != "" {
				withText++
				if CorrectionPattern.MatchString(e.Text) || repeats(terms, earlier) {
					n++
				}
			}
		}
		first = false
		earlier = append(earlier, terms)
	}
	if considered > 0 && withText == 0 {
		return unknown()
	}
	return inferred(float64(n))
}

// retries: a tool_call in the window with the same ToolName and ToolArgsHash
// as a tool_call among the RetryWindow events before it (exact, Measured); or
// the first tool_call after an error that targets one of the failed call's
// files or repeats its shell command (after error, Inferred). Exact retries
// are Unknown when no call in the window recorded an args hash.
func (v *view) retries() (exact, afterErr analyze.Value) {
	nExact, nAfter, calls, hashed := 0, 0, 0, 0
	var errCall *analyze.Event // the failed call awaiting its next call
	for i, e := range v.ev {
		if failed(e) {
			var c analyze.Event
			if e.Kind == analyze.KindToolResult {
				if j := v.callOf(i); j >= 0 {
					c = v.ev[j]
				}
			} else {
				c = e
				if len(c.Files) == 0 && c.Text == "" {
					for j := i - 1; j >= 0; j-- {
						if v.ev[j].Kind == analyze.KindToolCall {
							c = v.ev[j]
							break
						}
					}
				}
			}
			errCall = &c
			continue
		}
		if e.Kind != analyze.KindToolCall {
			continue
		}
		isExact := false
		if e.ToolArgsHash != "" {
			for j := i - 1; j >= 0 && j >= i-RetryWindow; j-- {
				p := v.ev[j]
				if p.Kind == analyze.KindToolCall && p.ToolName == e.ToolName && p.ToolArgsHash == e.ToolArgsHash {
					isExact = true
					break
				}
			}
		}
		after := errCall != nil && sameTarget(*errCall, e)
		errCall = nil
		if !v.in[i] {
			continue
		}
		calls++
		if e.ToolArgsHash != "" {
			hashed++
		}
		switch {
		case isExact:
			nExact++
		case after:
			nAfter++
		}
	}
	exact = measured(float64(nExact))
	if calls > 0 && hashed == 0 {
		exact = unknown()
	}
	return exact, inferred(float64(nAfter))
}

func sameTarget(failed, next analyze.Event) bool {
	for _, f := range failed.Files {
		for _, g := range next.Files {
			if f == g {
				return true
			}
		}
	}
	return failed.Text != "" && failed.ToolName == analyze.ToolShell &&
		next.ToolName == analyze.ToolShell && failed.Text == next.Text
}

func exploring(name string) bool {
	return name == analyze.ToolReadFile || name == analyze.ToolSearch || name == analyze.ToolShell
}

// explorationBeforeFirstEdit: read/search/shell tool calls in the window
// before the session's first edit call (all of them when it never edits),
// and the text volume of those calls and their results.
func (v *view) explorationBeforeFirstEdit() (count, volume analyze.Value) {
	n, vol := 0, 0
	calls := map[int]bool{}
	for i, e := range v.ev {
		if e.Kind == analyze.KindToolCall && e.ToolName == analyze.ToolEdit {
			break
		}
		switch {
		case e.Kind == analyze.KindToolCall && exploring(e.ToolName):
			calls[i] = true
			if v.in[i] {
				n++
				vol += e.TextVolumeUnits
			}
		case e.Kind == analyze.KindToolResult && v.in[i]:
			if c := v.callOf(i); c >= 0 && calls[c] {
				vol += e.TextVolumeUnits
			}
		}
	}
	return measured(float64(n)), analyze.Value{N: float64(vol), Cov: analyze.Estimated}
}

// ReadCounts returns how often each file was read (read_file calls) in the
// window.
func ReadCounts(s *analyze.Session, w analyze.Window) map[string]int {
	v := newView(s, w)
	out := map[string]int{}
	for i, e := range v.ev {
		if v.in[i] && e.Kind == analyze.KindToolCall && e.ToolName == analyze.ToolReadFile {
			for _, f := range e.Files {
				out[f]++
			}
		}
	}
	return out
}

func (v *view) repeatedReads() analyze.Value {
	n := 0
	for _, c := range ReadCounts(v.s, v.w) {
		if c >= 2 {
			n++
		}
	}
	return measured(float64(n))
}

func agentSide(k analyze.Kind) bool {
	switch k {
	case analyze.KindUserPrompt, analyze.KindSystemContext, analyze.KindSessionStart, analyze.KindSessionEnd:
		return false
	}
	return true
}

// idle splits waiting time. For each user prompt in the window (not the
// session's first event): the gap from the latest earlier agent-side event
// (clipped to the window start) is user time when <= IdleCap, else a parked
// session (counted, time excluded). From each prompt to the next agent-side
// event (clipped to the window end) is model-or-permission time, with the
// same cap.
func (v *view) idle() (user, parked, model analyze.Value) {
	var u, m time.Duration
	p := 0
	lastAgent := -1
	for i, e := range v.ev {
		if e.Kind != analyze.KindUserPrompt {
			if agentSide(e.Kind) {
				lastAgent = i
			}
			continue
		}
		if !v.in[i] {
			continue
		}
		if lastAgent >= 0 && (i == 0 || v.ev[i-1].Kind != analyze.KindUserPrompt) {
			start := v.ev[lastAgent].TS
			if start.Before(v.w.Since) {
				start = v.w.Since
			}
			if gap := e.TS.Sub(start); gap > IdleCap {
				p++
			} else if gap > 0 {
				u += gap
			}
		}
		for j := i + 1; j < len(v.ev); j++ {
			if v.ev[j].Kind == analyze.KindUserPrompt {
				break
			}
			if agentSide(v.ev[j].Kind) {
				end := v.ev[j].TS
				if !end.Before(v.w.Until) {
					end = v.w.Until
				}
				if gap := end.Sub(e.TS); gap <= IdleCap && gap > 0 {
					m += gap
				}
				break
			}
		}
	}
	return inferred(u.Seconds()), inferred(float64(p)), inferred(m.Seconds())
}

// permissionWait: for each permission_request in the window, the span to the
// next tool_result with the same correlation id (an upper bound: it includes
// the tool's run). Unknown unless the tool's PermissionSignal is "request".
func (v *view) permissionWait() (requests, wait analyze.Value) {
	if v.s.Capabilities.PermissionSignal != "request" {
		return unknown(), unknown()
	}
	n := 0
	var total time.Duration
	for i, e := range v.ev {
		if !v.in[i] || e.Kind != analyze.KindPermissionReq {
			continue
		}
		n++
		if e.CorrelationID == "" {
			continue
		}
		for j := i + 1; j < len(v.ev); j++ {
			r := v.ev[j]
			if r.Kind == analyze.KindToolResult && r.CorrelationID == e.CorrelationID {
				total += r.TS.Sub(e.TS)
				break
			}
		}
	}
	return measured(float64(n)), inferred(total.Seconds())
}

func (v *view) permissionDenials() analyze.Value {
	sig := v.s.Capabilities.PermissionSignal
	if sig != "request" && sig != "denial_only" {
		return unknown()
	}
	return measured(float64(v.count(analyze.KindPermissionDeny)))
}

// span: first to last event in the window; active excludes inter-event gaps
// longer than IdleCap.
func (v *view) span() (span, active analyze.Value) {
	var first, prev time.Time
	var parked time.Duration
	seen := false
	for i, e := range v.ev {
		if !v.in[i] {
			continue
		}
		if !seen {
			first, seen = e.TS, true
		} else if gap := e.TS.Sub(prev); gap > IdleCap {
			parked += gap
		}
		prev = e.TS
	}
	if !seen {
		return measured(0), inferred(0)
	}
	total := prev.Sub(first)
	return measured(total.Seconds()), inferred((total - parked).Seconds())
}

// tokenSums are the window's model_request totals.
type tokenSums struct {
	requests, recorded, complete int // all, with Tokens (measured or partial), TokensCoverage measured
	bucket                       [5]int64
	seen                         [5]int // requests that recorded each bucket
	total                        int64
}

var bucketNames = [5]string{TokensInUncached, TokensInCacheRead, TokensInCacheWrite, TokensOutVisible, TokensOutThinking}

func buckets(t *analyze.Tokens) [5]*int64 {
	return [5]*int64{t.InUncached, t.InCacheRead, t.InCacheWrite, t.OutVisible, t.OutThinking}
}

func (v *view) tokens() tokenSums {
	var ts tokenSums
	for i, e := range v.ev {
		if !v.in[i] || e.Kind != analyze.KindModelRequest {
			continue
		}
		ts.requests++
		if e.Tokens == nil || (e.TokensCoverage != analyze.Measured && e.TokensCoverage != analyze.Partial) {
			continue
		}
		ts.recorded++
		if e.TokensCoverage == analyze.Measured {
			ts.complete++
		}
		for k, p := range buckets(e.Tokens) {
			if p != nil {
				ts.bucket[k] += *p
				ts.seen[k]++
				ts.total += *p
			}
		}
	}
	return ts
}

// values: per bucket and consumed totals, each with Frac = recorded
// requests / requests. Unknown when the window has no request, or nothing
// recorded the bucket. Measured only when every request recorded it.
func (ts tokenSums) values() map[string]analyze.Value {
	out := map[string]analyze.Value{ModelRequests: measured(float64(ts.requests))}
	if ts.requests == 0 {
		for _, n := range bucketNames {
			out[n] = unknown()
		}
		out[TokensConsumed] = unknown()
		return out
	}
	f := frac(ts.recorded, ts.requests)
	for k, n := range bucketNames {
		val := analyze.Value{N: float64(ts.bucket[k]), Cov: analyze.Partial, Frac: f}
		switch ts.seen[k] {
		case 0:
			val = analyze.Value{Cov: analyze.Unknown, Frac: f}
		case ts.requests:
			val.Cov = analyze.Measured
		}
		out[n] = val
	}
	cons := analyze.Value{N: float64(ts.total), Cov: analyze.Partial, Frac: f}
	switch {
	case ts.recorded == 0:
		cons = analyze.Value{Cov: analyze.Unknown, Frac: f}
	case ts.complete == ts.requests:
		cons.Cov = analyze.Measured
	}
	out[TokensConsumed] = cons
	return out
}

// context: request_context (in_uncached + in_cache_read + in_cache_write) of
// each request in the window that recorded in_uncached: first, peak, last.
// Partial when a counted request lacked a cache bucket. Unknown when none.
func (v *view) context() map[string]analyze.Value {
	var ctx []int64
	cov := analyze.Measured
	for i, e := range v.ev {
		if !v.in[i] || e.Kind != analyze.KindModelRequest || e.Tokens == nil || e.Tokens.InUncached == nil {
			continue
		}
		c := *e.Tokens.InUncached
		for _, p := range []*int64{e.Tokens.InCacheRead, e.Tokens.InCacheWrite} {
			if p == nil {
				cov = analyze.Partial
			} else {
				c += *p
			}
		}
		ctx = append(ctx, c)
	}
	if len(ctx) == 0 {
		return map[string]analyze.Value{PeakRequestContext: unknown(), ContextSizeFirst: unknown(), ContextSizeLast: unknown()}
	}
	peak := ctx[0]
	for _, c := range ctx {
		peak = max(peak, c)
	}
	val := func(n int64) analyze.Value { return analyze.Value{N: float64(n), Cov: cov} }
	return map[string]analyze.Value{PeakRequestContext: val(peak), ContextSizeFirst: val(ctx[0]), ContextSizeLast: val(ctx[len(ctx)-1])}
}

// WithChildren is Session for a main session plus its subagent sessions
// (those whose Parent is parent.Key): it adds SubagentSessions (children
// with an event in w) and SubagentTokenShare = children's consumed tokens /
// (parent's + children's), with Frac = recorded requests / requests over all
// of them. The share is Unknown when no request in w recorded tokens.
func WithChildren(parent *analyze.Session, children []*analyze.Session, w analyze.Window) analyze.SessionMetrics {
	m := Session(parent, w)
	p := newView(parent, w).tokens()
	reqs, rec, complete := p.requests, p.recorded, p.complete
	var childTotal int64
	active := 0
	for _, c := range children {
		cv := newView(c, w)
		for i := range cv.ev {
			if cv.in[i] {
				active++
				break
			}
		}
		ct := cv.tokens()
		reqs += ct.requests
		rec += ct.recorded
		complete += ct.complete
		childTotal += ct.total
	}
	m.Values[SubagentSessions] = measured(float64(active))
	sum := p.total + childTotal
	if rec == 0 || sum == 0 {
		m.Values[SubagentTokenShare] = unknown()
		return m
	}
	cov := analyze.Partial
	if complete == reqs {
		cov = analyze.Measured
	}
	m.Values[SubagentTokenShare] = analyze.Value{N: float64(childTotal) / float64(sum), Cov: cov, Frac: frac(rec, reqs)}
	return m
}

// ProjectFiles returns, per project, the files read (read_file calls in w)
// in at least 3 different sessions of that project, with the number of
// sessions: the cross-session repeated-reads fact. Sessions without a
// project are skipped.
func ProjectFiles(sessions []*analyze.Session, w analyze.Window) map[string]map[string]int {
	counts := map[string]map[string]int{}
	for _, s := range sessions {
		if s.Project == "" {
			continue
		}
		for f := range ReadCounts(s, w) {
			if counts[s.Project] == nil {
				counts[s.Project] = map[string]int{}
			}
			counts[s.Project][f]++
		}
	}
	out := map[string]map[string]int{}
	for p, files := range counts {
		for f, n := range files {
			if n >= 3 {
				if out[p] == nil {
					out[p] = map[string]int{}
				}
				out[p][f] = n
			}
		}
	}
	return out
}

// Names returns the metric names of m in sorted order (for stable output).
func Names(m analyze.SessionMetrics) []string {
	out := make([]string, 0, len(m.Values))
	for n := range m.Values {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
