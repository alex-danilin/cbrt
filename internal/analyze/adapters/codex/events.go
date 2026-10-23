package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

type record struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Ordinal   *int64          `json:"ordinal"`
	Payload   json.RawMessage `json:"payload"`
}

// payload holds the payload fields the adapter reads, across record types.
type payload struct {
	Type string `json:"type"`

	// session_meta
	ID         string          `json:"id"`
	Originator string          `json:"originator"`
	Source     json.RawMessage `json:"source"`
	Git        *struct {
		Branch     string `json:"branch"`
		CommitHash string `json:"commit_hash"`
	} `json:"git"`

	// turn_context, task_started, task_complete, turn_aborted
	Cwd    string `json:"cwd"` // session_meta, turn_context
	TurnID string `json:"turn_id"`
	Model  string `json:"model"`
	Effort string `json:"effort"`

	// event_msg user_message / agent_message / error
	Message string `json:"message"`

	// token_count
	Info json.RawMessage `json:"info"`

	// token_usage_record
	ResponseID string `json:"response_id"`
	Usage      *usage `json:"usage"`

	// response_item message / reasoning
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Summary json.RawMessage `json:"summary"` // reasoning: parts; turn_context: a mode string

	// response_item tool calls and outputs; patch_apply_end
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Success   *bool           `json:"success"`
}

type usage struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
}

type tokenInfo struct {
	Last  *usage `json:"last_token_usage"`
	Total *usage `json:"total_token_usage"`
}

type pendingCall struct {
	ts   time.Time
	norm string
	raw  string
}

type turn struct {
	id        string
	n         int             // 1-based turn ordinal in the rollout (key of parser.merged)
	base      usage           // running total when the turn opened
	calls     int             // per-response model_requests emitted for this turn
	closing   json.RawMessage // last non-null token_count info of the turn
	closingTo *usage          // its total_token_usage
	lastSeq   string
	lastTS    time.Time
	lastLine  int
}

// parser walks one rollout and emits events as it goes.
type parser struct {
	src  analyze.Source
	path string
	emit func(analyze.Event) error

	origin         string
	branch, commit string
	hasGit         bool
	atStart        bool // no turn has opened yet: the branch is the measured start value
	model, effort  string
	cwd            string // latest session_meta / turn_context cwd

	seqs         map[string]bool
	reqIDs       map[string]bool
	turnN        int
	cur          *turn
	sawTaskStart bool
	cum          usage // latest running total seen
	// merged holds the turns (by turn.n) whose responses cannot be told apart;
	// a planning pass over the rollout fills it before the emitting pass.
	merged        map[int]bool
	pendingResp   string // response_id of a token_usage_record awaiting its token_count
	pendingUsage  usage  // that record's usage
	calls         map[string]pendingCall
	patchOK       map[string]bool
	hasUserEvents bool
	compacted     int // compacted records
	ctxCompacted  int // event_msg context_compacted
}

// Events reads one rollout. See the package comment for the format, and the
// method comments for each mapping.
func (a Adapter) Events(src analyze.Source, emit func(analyze.Event) error) error {
	return a.Bodies(src, func(e analyze.Event) error {
		e.Body = ""
		return emit(e)
	})
}

// Bodies emits the rollout's events like Events, with Body set.
func (Adapter) Bodies(src analyze.Source, emit func(analyze.Event) error) error {
	if len(src.Members) == 0 {
		return nil
	}
	path := src.Members[0].Path
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	recs := make([]*record, len(lines))
	bad := 0
	hasUserEvents := false
	for i, l := range lines {
		var r record
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		if json.Unmarshal(l, &r) != nil || r.Type == "" {
			bad++
			continue
		}
		recs[i] = &r
		if r.Type == "event_msg" && bytes.Contains(r.Payload, []byte(`"user_message"`)) {
			var pl payload
			if json.Unmarshal(r.Payload, &pl) == nil && pl.Type == "user_message" {
				hasUserEvents = true
			}
		}
	}
	// Two passes over the same records: the first only finds the turns whose
	// responses cannot be separated (parser.merged), so the second can emit
	// per-response requests as it reaches them, in order.
	merged := map[int]bool{}
	walk := func(emit func(analyze.Event) error) (malformed int, err error) {
		p := &parser{src: src, path: path, emit: emit, origin: analyze.OriginUnknown, atStart: true,
			seqs: map[string]bool{}, reqIDs: map[string]bool{}, calls: map[string]pendingCall{}, patchOK: map[string]bool{},
			hasUserEvents: hasUserEvents, merged: merged}
		for i, r := range recs {
			if r == nil {
				continue
			}
			if err := p.record(r, i+1); err != nil {
				if ee, ok := err.(emitError); ok {
					return malformed, ee.err
				}
				malformed++
			}
		}
		if p.cur != nil {
			// An open turn at the end: unknown, unless the rollout predates
			// task_started/task_complete and turns are never closed explicitly.
			if err := p.closeTurn(!p.sawTaskStart, p.cur.lastSeq, p.cur.lastTS, p.cur.lastLine); err != nil {
				return malformed, err.(emitError).err
			}
		}
		return malformed, nil
	}
	if _, err := walk(func(analyze.Event) error { return nil }); err != nil {
		return err
	}
	emitted := 0
	malformed, err := walk(func(e analyze.Event) error { emitted++; return emit(e) })
	if err != nil {
		return err
	}
	bad += malformed
	if emitted == 0 && bad > 0 {
		return fmt.Errorf("codex: %d malformed records and no usable events", bad)
	}
	return nil
}

// emitError carries an emit callback's error, which stops the walk.
type emitError struct{ err error }

func (e emitError) Error() string { return e.err.Error() }

// readLines returns the file's complete lines; a trailing line without a
// final newline (a write in progress) is dropped.
func readLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 256<<10)
	var out [][]byte
	for {
		l, err := br.ReadBytes('\n')
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, bytes.TrimRight(l, "\r\n"))
	}
}

func (p *parser) seqFor(r *record, line int) string {
	if r.Ordinal != nil {
		s := strconv.FormatInt(*r.Ordinal, 10)
		if !p.seqs[s] {
			p.seqs[s] = true
			return s
		}
	}
	s := "L" + strconv.Itoa(line)
	p.seqs[s] = true
	return s
}

func (p *parser) base(kind analyze.Kind, seq string, ts time.Time, line int) analyze.Event {
	e := analyze.Event{
		Tool:             ID,
		Store:            store,
		Origin:           p.origin,
		NativeSessionID:  p.src.NativeSessionID,
		Seq:              seq,
		TS:               ts,
		Kind:             kind,
		ObservedBy:       analyze.FromTranscript,
		Model:            p.model,
		Effort:           p.effort,
		Cwd:              p.cwd,
		BranchCoverage:   analyze.Unknown,
		DurationCoverage: analyze.Unknown,
	}
	if p.hasGit {
		e.Branch, e.Commit = p.branch, p.commit
		e.BranchCoverage = analyze.Inferred
		if p.atStart {
			e.BranchCoverage = analyze.MeasuredAtStart
		}
	}
	e.Ref, _ = json.Marshal(ref{Path: p.path, Line: line})
	return e
}

func (p *parser) out(e analyze.Event) error {
	if err := p.emit(e); err != nil {
		return emitError{err}
	}
	return nil
}

func withText(e analyze.Event, text string) analyze.Event {
	e.TextLen = len([]rune(text))
	e.TextVolumeUnits = analyze.TextVolume(text)
	e.Body = text
	return e
}

// record handles one line. A returned error that wraps errEmit stops the
// walk; any other error marks the record malformed.
func (p *parser) record(r *record, line int) error {
	t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
	if err != nil {
		return err
	}
	ts := t.UTC()
	var pl payload
	if len(r.Payload) > 0 {
		if err := json.Unmarshal(r.Payload, &pl); err != nil {
			return err
		}
	}
	seq := p.seqFor(r, line)
	if p.cur != nil {
		p.cur.lastSeq, p.cur.lastTS, p.cur.lastLine = seq, ts, line
	}
	switch r.Type {
	case "session_meta":
		return p.sessionMeta(pl, seq, ts, line)
	case "turn_context":
		if !p.sawTaskStart && (p.cur == nil || pl.TurnID == "" || pl.TurnID != p.cur.id) {
			// Legacy rollouts without task_started: turn_context opens a turn.
			if p.cur != nil {
				if err := p.closeTurn(true, seq, ts, line); err != nil {
					return err
				}
			}
			p.openTurn(pl.TurnID, seq, ts, line)
		} else if p.cur == nil {
			p.openTurn(pl.TurnID, seq, ts, line)
		}
		if pl.Model != "" {
			p.model = pl.Model
		}
		p.effort = pl.Effort
		if pl.Cwd != "" {
			p.cwd = pl.Cwd
		}
		return nil
	case "response_item":
		return p.responseItem(pl, seq, ts, line)
	case "event_msg":
		return p.eventMsg(pl, seq, ts, line)
	case "compacted":
		p.compacted++
		if p.compacted > p.ctxCompacted {
			return p.out(p.base(analyze.KindCompaction, seq, ts, line))
		}
		return nil
	case "world_state":
		// Context Codex injects (AGENTS.md, environment, instructions).
		return p.out(withText(p.base(analyze.KindSystemContext, seq, ts, line), string(r.Payload)))
	case "token_usage_record":
		// Names the response whose token_count follows (checked by usage there).
		p.pendingResp, p.pendingUsage = "", usage{}
		if pl.Usage != nil {
			p.pendingResp, p.pendingUsage = pl.ResponseID, *pl.Usage
		}
		return nil
	}
	return nil // unknown types carry nothing we emit
}

func (p *parser) sessionMeta(pl payload, seq string, ts time.Time, line int) error {
	p.origin = originOf(pl.Originator, pl.Source)
	p.cwd = pl.Cwd
	if pl.Git != nil && (pl.Git.Branch != "" || pl.Git.CommitHash != "") {
		p.hasGit, p.branch, p.commit = true, pl.Git.Branch, pl.Git.CommitHash
	}
	return p.out(p.base(analyze.KindSessionStart, seq, ts, line))
}

// originOf maps session_meta originator/source: VS Code (and other editor)
// sources → ide; an SDK originator, or source "mcp" → sdk; source "cli" or
// "exec" (`codex exec`, headless CLI) → cli; else unknown.
func originOf(originator string, source json.RawMessage) string {
	var src string
	_ = json.Unmarshal(source, &src)
	o := strings.ToLower(originator)
	switch {
	case src == "vscode" || strings.Contains(o, "vscode") || strings.Contains(o, "ide"):
		return analyze.OriginIDE
	case strings.Contains(o, "sdk") || src == "mcp":
		return analyze.OriginSDK
	case src == "cli" || src == "exec" || strings.HasPrefix(o, "codex"):
		return analyze.OriginCLI
	}
	return analyze.OriginUnknown
}

func (p *parser) openTurn(id, seq string, ts time.Time, line int) {
	p.turnN++
	if id == "" {
		id = "turn-" + strconv.Itoa(p.turnN)
	}
	id = p.uniqueReqID(id)
	p.cur = &turn{id: id, n: p.turnN, base: p.cum, lastSeq: seq, lastTS: ts, lastLine: line}
	p.atStart = false
}

// uniqueReqID returns id, or id with a "~<k>" suffix if a request or turn of
// this session already uses it, and reserves the result.
func (p *parser) uniqueReqID(id string) string {
	u := id
	for k := 2; p.reqIDs[u]; k++ {
		u = id + "~" + strconv.Itoa(k)
	}
	p.reqIDs[u] = true
	return u
}

// closeTurn emits the turn's model_request when its responses did not get
// their own: a merged turn (one request for the whole turn, RequestID =
// turn_id), or a turn without any recorded response (one Unknown request).
// measured is false when the turn was aborted or never closed: a merged
// turn's usage is then unknown even if some of its responses were counted.
func (p *parser) closeTurn(measured bool, seq string, ts time.Time, line int) error {
	t := p.cur
	p.cur = nil
	if t.calls > 0 && !p.merged[t.n] {
		return nil // every response already has its model_request
	}
	e := p.base(analyze.KindModelRequest, seq+".request", ts, line)
	e.RequestID = t.id // a merged turn spans several responses: no single answer id
	e.TokensCoverage = analyze.Unknown
	if len(t.closing) > 0 {
		e.TokensNative = t.closing
	}
	if measured && t.closingTo != nil {
		if e.Tokens = turnTokens(t.base, *t.closingTo); e.Tokens != nil {
			e.TokensCoverage = analyze.Partial
		}
	}
	return p.out(e)
}

// tokenCount handles one token_count. Rules (checked on real rollouts, see
// local_test.go):
//   - info null: no response recorded; skipped (Codex writes one before a
//     turn's first response).
//   - running total equal to the previous one: a repeat of the previous
//     response's event (sometimes with a different last_token_usage); skipped
//     so its usage counts once.
//   - otherwise one response: its own model_request, tokens from
//     last_token_usage, RequestID the response_id of the token_usage_record
//     just before it (when its usage matches), else "<turn_id>#<n>".
//   - a response whose last_token_usage is not the running total's increase
//     means the total also holds something no event reports: the whole turn
//     is merged into one request whose usage is the turn's total increase, so
//     the session sum still equals the rollout's final running total.
func (p *parser) tokenCount(pl payload, seq string, ts time.Time, line int) error {
	respID, respUsage := p.pendingResp, p.pendingUsage
	if len(pl.Info) == 0 || string(pl.Info) == "null" {
		return nil
	}
	var info tokenInfo
	if err := json.Unmarshal(pl.Info, &info); err != nil {
		return err
	}
	p.pendingResp = ""
	if info.Total == nil {
		return nil
	}
	prev, total := p.cum, *info.Total
	p.cum = total
	if total == prev {
		return nil // a repeat
	}
	t := p.cur
	if t == nil {
		return nil // outside any turn (not seen in real rollouts)
	}
	t.closing = append(json.RawMessage(nil), pl.Info...)
	t.closingTo = &total
	delta := usage{total.Input - prev.Input, total.Cached - prev.Cached, total.Output - prev.Output, total.Reasoning - prev.Reasoning}
	if info.Last == nil || *info.Last != delta {
		p.merged[t.n] = true
	}
	if p.merged[t.n] {
		return nil // closeTurn emits the turn's single request
	}
	t.calls++
	e := p.base(analyze.KindModelRequest, seq+".request", ts, line)
	id := t.id + "#" + strconv.Itoa(t.calls)
	if respID != "" && respUsage == delta {
		id = respID
		e.CorrelationID = respID // the answer id the client stored
	}
	e.RequestID = p.uniqueReqID(id)
	e.TokensNative = t.closing
	e.TokensCoverage = analyze.Unknown
	if e.Tokens = callTokens(delta); e.Tokens != nil {
		e.TokensCoverage = analyze.Partial
	}
	return p.out(e)
}

// turnTokens is a merged turn's usage: the running total at the turn's last
// token_count minus the running total when the turn opened.
func turnTokens(base, end usage) *analyze.Tokens {
	return callTokens(usage{end.Input - base.Input, end.Cached - base.Cached, end.Output - base.Output, end.Reasoning - base.Reasoning})
}

// callTokens maps one usage (a response's, or a merged turn's) to buckets.
// OpenAI convention: cached ⊂ input, reasoning ⊂ output. in_cache_write stays
// nil because whether cache_write_input_tokens is inside input_tokens is
// unverified (CacheWriteOverlap "unknown"), hence coverage Partial.
func callTokens(d usage) *analyze.Tokens {
	if d.Input < 0 || d.Cached < 0 || d.Output < 0 || d.Reasoning < 0 || d.Cached > d.Input || d.Reasoning > d.Output {
		return nil
	}
	return &analyze.Tokens{
		InUncached:  analyze.Int64(d.Input - d.Cached),
		InCacheRead: analyze.Int64(d.Cached),
		OutVisible:  analyze.Int64(d.Output - d.Reasoning),
		OutThinking: analyze.Int64(d.Reasoning),
	}
}

// injectedTag matches context Codex writes as a user-role message, e.g.
// <environment_context>, <user_instructions>, <permissions …>.
var injectedTag = regexp.MustCompile(`^<([a-z_]+)[\s>]`)

func injected(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "# AGENTS.md") {
		return true
	}
	m := injectedTag.FindStringSubmatch(t)
	return m != nil && m[1] != "image"
}

func (p *parser) responseItem(pl payload, seq string, ts time.Time, line int) error {
	switch pl.Type {
	case "message":
		var b strings.Builder
		for _, c := range pl.Content {
			b.WriteString(c.Text)
		}
		text := b.String()
		switch pl.Role {
		case "assistant":
			e := withText(p.base(analyze.KindAssistantText, seq, ts, line), text)
			e.Text = text
			return p.out(e)
		case "developer", "system":
			return p.out(withText(p.base(analyze.KindSystemContext, seq, ts, line), text))
		case "user":
			first := ""
			if len(pl.Content) > 0 {
				first = pl.Content[0].Text
			}
			if injected(first) {
				return p.out(withText(p.base(analyze.KindSystemContext, seq, ts, line), text))
			}
			if p.hasUserEvents {
				return nil // the user_message event already carries this prompt
			}
			e := withText(p.base(analyze.KindUserPrompt, seq, ts, line), text)
			e.Text = text
			return p.out(e)
		}
	case "reasoning":
		var b strings.Builder
		var summary []struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(pl.Summary, &summary)
		for _, s := range summary {
			b.WriteString(s.Text)
		}
		for _, c := range pl.Content {
			b.WriteString(c.Text)
		}
		e := withText(p.base(analyze.KindThinking, seq, ts, line), b.String())
		return p.out(e)
	case "function_call":
		tc := normalizeTool(pl.Namespace, pl.Name, []byte(pl.Arguments), false)
		return p.toolCall(tc, pl.CallID, pl.Arguments, seq, ts, line)
	case "custom_tool_call":
		tc := normalizeTool(pl.Namespace, pl.Name, []byte(pl.Input), true)
		return p.toolCall(tc, pl.CallID, pl.Input, seq, ts, line)
	case "local_shell_call":
		var act struct {
			Command []string `json:"command"`
		}
		_ = json.Unmarshal(pl.Action, &act)
		text := argvText(act.Command)
		tc := toolCall{raw: "local_shell", args: pl.Action, shellText: text}
		tc.normName, tc.files = classifyShell(text)
		if len(pl.Action) == 0 {
			tc.args = []byte("{}")
		}
		return p.toolCall(tc, pl.CallID, string(pl.Action), seq, ts, line)
	case "web_search_call":
		args := pl.Action
		if len(args) == 0 {
			args = []byte("{}")
		}
		tc := toolCall{raw: "web_search", normName: analyze.ToolWeb, args: args}
		return p.toolCall(tc, pl.CallID, string(pl.Action), seq, ts, line)
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		return p.toolResult(pl, seq, ts, line)
	}
	return nil
}

func (p *parser) toolCall(tc toolCall, callID, input, seq string, ts time.Time, line int) error {
	e := withText(p.base(analyze.KindToolCall, seq, ts, line), input)
	e.TextLen = 0 // tool input is not user/assistant text
	e.ToolName, e.ToolNameRaw = tc.normName, tc.raw
	e.ToolArgsHash = analyze.ArgsHash(tc.args)
	e.CorrelationID = callID
	e.Files = tc.files
	if tc.normName == analyze.ToolShell {
		e.Text = tc.shellText // the command line; "" for write_stdin and exec waits
	}
	if callID != "" {
		p.calls[callID] = pendingCall{ts: ts, norm: tc.normName, raw: tc.raw}
	}
	return p.out(e)
}

// toolResult pairs an output with its call by call_id: DurationMS is output
// time minus call time (Measured); ToolSuccess comes from the exit code the
// output records, or from patch_apply_end for apply_patch, else nil.
func (p *parser) toolResult(pl payload, seq string, ts time.Time, line int) error {
	text := outputText(pl.Output)
	e := withText(p.base(analyze.KindToolResult, seq, ts, line), text)
	e.TextLen = 0
	e.CorrelationID = pl.CallID
	call, ok := p.calls[pl.CallID]
	if ok {
		delete(p.calls, pl.CallID)
		e.ToolName, e.ToolNameRaw = call.norm, call.raw
		if d := ts.Sub(call.ts).Milliseconds(); d >= 0 {
			e.DurationMS, e.DurationCoverage = analyze.Int64(d), analyze.Measured
		}
	} else {
		e.ToolName, e.ToolNameRaw = analyze.ToolOther, pl.Type
		if pl.Name != "" {
			e.ToolNameRaw = pl.Name
		}
	}
	if code, ok := exitCode(text); ok {
		e.ToolSuccess = analyze.Bool(code == 0)
	} else if s, ok := p.patchOK[pl.CallID]; ok {
		e.ToolSuccess = analyze.Bool(s)
	}
	return p.out(e)
}

func (p *parser) eventMsg(pl payload, seq string, ts time.Time, line int) error {
	switch pl.Type {
	case "task_started":
		if p.cur != nil {
			// A new turn while one is open: the open one was cut off unrecorded.
			if err := p.closeTurn(false, seq, ts, line); err != nil {
				return err
			}
		}
		p.sawTaskStart = true
		p.openTurn(pl.TurnID, seq, ts, line)
	case "user_message":
		e := withText(p.base(analyze.KindUserPrompt, seq, ts, line), pl.Message)
		e.Text = pl.Message
		return p.out(e)
	case "token_count":
		return p.tokenCount(pl, seq, ts, line)
	case "task_complete":
		if err := p.out(p.base(analyze.KindTurnEnd, seq, ts, line)); err != nil {
			return err
		}
		if p.cur != nil {
			return p.closeTurn(true, seq, ts, line)
		}
	case "turn_aborted":
		if err := p.out(p.base(analyze.KindInterrupt, seq, ts, line)); err != nil {
			return err
		}
		if p.cur != nil {
			return p.closeTurn(false, seq, ts, line)
		}
	case "context_compacted":
		p.ctxCompacted++
		if p.ctxCompacted > p.compacted {
			return p.out(p.base(analyze.KindCompaction, seq, ts, line))
		}
	case "error", "stream_error":
		return p.out(withText(p.base(analyze.KindError, seq, ts, line), pl.Message))
	case "patch_apply_end":
		if pl.Success != nil && pl.CallID != "" {
			p.patchOK[pl.CallID] = *pl.Success
		}
	}
	return nil
}
