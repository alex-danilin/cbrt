package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// record is the subset of a transcript line the adapter reads. Record types
// with no uuid or timestamp (titles, modes, file-history, queue operations,
// cost state) produce no events.
type record struct {
	Type             string                 `json:"type"`
	Subtype          string                 `json:"subtype"`
	UUID             string                 `json:"uuid"`
	Timestamp        string                 `json:"timestamp"`
	AgentID          string                 `json:"agentId"`
	GitBranch        string                 `json:"gitBranch"`
	Cwd              string                 `json:"cwd"`
	Entrypoint       string                 `json:"entrypoint"`
	IsSidechain      bool                   `json:"isSidechain"`
	IsMeta           bool                   `json:"isMeta"`
	IsCompactSummary bool                   `json:"isCompactSummary"`
	IsAPIError       bool                   `json:"isApiErrorMessage"`
	RequestID        string                 `json:"requestId"`
	Effort           string                 `json:"effort"`
	Level            string                 `json:"level"`
	ToolDenialKind   string                 `json:"toolDenialKind"`
	PromptSource     string                 `json:"promptSource"`
	Origin           *struct{ Kind string } `json:"origin"`
	Message          *message               `json:"message"`
	Attachment       json.RawMessage        `json:"attachment"`
	Content          json.RawMessage        `json:"content"` // system records
}

type message struct {
	ID      string          `json:"id"` // Anthropic message id (assistant records)
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"` // string or []block
	Usage   json.RawMessage `json:"usage"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result: string or []block
	IsError   *bool           `json:"is_error"`
}

type usage struct {
	InputTokens         *int64 `json:"input_tokens"`
	CacheCreationTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     *int64 `json:"cache_read_input_tokens"`
	OutputTokens        *int64 `json:"output_tokens"`
	OutputTokensDetails *struct {
		ThinkingTokens *int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// line is one parsed record with its position.
type line struct {
	no  int // 1-based
	rec record
	ts  time.Time
}

// syntheticModel marks records Claude Code writes itself (API errors,
// interruptions); they are not model responses.
const syntheticModel = "<synthetic>"

// imagePlaceholder stands for an image block in a tool result's text (the
// image data itself is never read into an event).
const imagePlaceholder = "[image]"

// Events emits the main session's events, then each subagent's.
func (Adapter) Events(src analyze.Source, emit func(analyze.Event) error) error {
	return events(src, false, emit)
}

// events reads the members; Body is kept only when bodies is set.
func events(src analyze.Source, bodies bool, emit func(analyze.Event) error) error {
	if !bodies {
		next := emit
		emit = func(e analyze.Event) error {
			e.Body = ""
			return next(e)
		}
	}
	for i, m := range src.Members {
		sid, parent := src.NativeSessionID, ""
		if i > 0 {
			sid, parent = subagentID(m.Path), src.NativeSessionID
		}
		if err := memberEvents(m.Path, src.Store, sid, parent, i > 0, emit); err != nil {
			return err
		}
	}
	return nil
}

// readLines parses every complete line of a member. A trailing line without
// a final newline is still being written and is ignored. Unparseable lines
// are counted, not fatal.
func readLines(path string) (lines []line, bad int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	for no := 1; ; no++ {
		b, err := br.ReadBytes('\n')
		if err == io.EOF {
			return lines, bad, nil // b, if any, is the partial tail
		}
		if err != nil {
			return nil, 0, err
		}
		b = bytes.TrimSpace(b)
		if len(b) == 0 {
			continue
		}
		var r record
		if json.Unmarshal(b, &r) != nil {
			bad++
			continue
		}
		var ts time.Time
		if r.Timestamp != "" {
			if ts, err = time.Parse(time.RFC3339Nano, r.Timestamp); err != nil {
				bad++
				continue
			}
		}
		lines = append(lines, line{no: no, rec: r, ts: ts.UTC()})
	}
}

// memberEvents converts one transcript file. Pass 1 finds, per requestId,
// the record whose usage is final; pass 2 emits.
//
// Usage per requestId (local evidence): Claude Code writes one record per
// content block, all with the same requestId. In main transcripts every
// record of a request repeats the same final usage. In subagent transcripts
// the earlier records are streaming snapshots (stop_reason null, no
// output_tokens_details, output_tokens smaller) and output_tokens only grows,
// so the last record of the requestId always carries the final usage. The
// adapter therefore takes the last record with usage and emits the single
// model_request event at that record.
func memberEvents(path, st, sid, parent string, sidechain bool, emit func(analyze.Event) error) error {
	lines, bad, err := readLines(path)
	if err != nil {
		return err
	}
	if len(lines) == 0 && bad > 0 {
		return fmt.Errorf("claude: %d unparseable lines and no usable record", bad)
	}
	final := map[string]int{} // requestId -> index into lines
	for i, l := range lines {
		r := l.rec
		if r.Type == "assistant" && r.RequestID != "" && r.Message != nil && len(r.Message.Usage) > 0 &&
			r.Message.Model != syntheticModel && !r.IsAPIError && l.rec.UUID != "" && !l.ts.IsZero() {
			final[r.RequestID] = i
		}
	}
	p := &parser{path: path, store: st, sid: sid, parent: parent, sidechain: sidechain,
		calls: map[string]call{}, seenUUID: map[string]bool{}}
	for i, l := range lines {
		if l.rec.UUID == "" || l.ts.IsZero() || p.seenUUID[l.rec.UUID] {
			continue // no identity or time, or a duplicate key
		}
		p.seenUUID[l.rec.UUID] = true
		evs := p.convert(l)
		if fi, ok := final[l.rec.RequestID]; ok && fi == i {
			evs = append(evs, p.modelRequest(l))
		}
		for j := range evs {
			e := &evs[j]
			e.Seq = l.rec.UUID
			if len(evs) > 1 {
				e.Seq = fmt.Sprintf("%s#%d", l.rec.UUID, j)
			}
			if err := emit(*e); err != nil {
				return err
			}
		}
	}
	return nil
}

type call struct {
	name string
	ts   time.Time
}

type parser struct {
	path, store, sid, parent string
	sidechain                bool
	calls                    map[string]call // tool_use id -> call
	seenUUID                 map[string]bool
	model                    string // last real model seen, for switches
}

// base fills the fields every event of a record shares.
func (p *parser) base(l line, k analyze.Kind) analyze.Event {
	e := analyze.Event{
		Tool: adapterID, Store: p.store, Origin: origin(l.rec.Entrypoint),
		NativeSessionID: p.sid, ParentSessionID: p.parent,
		TS: l.ts, Kind: k, ObservedBy: analyze.FromTranscript,
		Effort: l.rec.Effort, Cwd: l.rec.Cwd,
		BranchCoverage: analyze.Unknown, DurationCoverage: analyze.Unknown,
	}
	if b := l.rec.GitBranch; b != "" {
		e.Branch, e.BranchCoverage = b, analyze.Measured
	}
	if m := l.rec.Message; m != nil && m.Model != "" && m.Model != syntheticModel {
		e.Model = m.Model
	}
	e.Ref, _ = json.Marshal(ref{Path: p.path, Line: l.no})
	return e
}

// origin maps the record's entrypoint to an Origin. No entrypoint (older
// transcripts) means the terminal CLI.
func origin(entry string) string {
	switch e := strings.ToLower(entry); {
	case e == "" || e == "cli":
		return analyze.OriginCLI
	case strings.Contains(e, "sdk"):
		return analyze.OriginSDK
	case strings.Contains(e, "desktop"), strings.Contains(e, "vscode"), strings.Contains(e, "jetbrains"), strings.Contains(e, "ide"):
		return analyze.OriginIDE
	}
	return analyze.OriginUnknown
}

func withText(e analyze.Event, s string, isText bool) analyze.Event {
	e.TextVolumeUnits = analyze.TextVolume(s)
	e.Body = s
	if isText {
		e.TextLen = utf8.RuneCountInString(s)
	}
	return e
}

func (p *parser) convert(l line) []analyze.Event {
	r := l.rec
	switch r.Type {
	case "user":
		return p.user(l)
	case "assistant":
		return p.assistant(l)
	case "attachment":
		var a struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(r.Attachment, &a)
		if a.Type == "total_tokens_reminder" {
			return nil // §3.2.2: ignored
		}
		s := a.Text
		if s == "" {
			s = string(r.Attachment)
		}
		return []analyze.Event{withText(p.base(l, analyze.KindSystemContext), s, false)}
	case "system":
		var s string
		_ = json.Unmarshal(r.Content, &s)
		switch {
		case r.Subtype == "compact_boundary":
			return []analyze.Event{p.base(l, analyze.KindCompaction)}
		case r.Subtype == "api_error" || r.Level == "error":
			return []analyze.Event{withText(p.base(l, analyze.KindError), s, false)}
		case r.Subtype == "local_command":
			return []analyze.Event{withText(p.base(l, analyze.KindSystemContext), s, false)}
		}
	}
	return nil
}

// contextTags open user-role text that Claude Code injected rather than the
// user typed.
var contextTags = []string{"<system-reminder>", "<local-command-caveat>", "<local-command-stdout>",
	"<local-command-stderr>", "<task-notification>", "<bash-stdout>", "<bash-stderr>"}

// userKind classifies user-role text. In a subagent transcript the prompts
// come from the parent agent, so they are context, not user prompts, unless
// the record says a human sent them.
func (p *parser) userKind(r record, text string) analyze.Kind {
	if strings.HasPrefix(text, "[Request interrupted") {
		return analyze.KindInterrupt
	}
	human := r.Origin != nil && r.Origin.Kind == "human"
	switch {
	case r.IsMeta, r.IsCompactSummary, r.PromptSource == "system",
		r.Origin != nil && r.Origin.Kind != "" && !human,
		p.sidechain && !human:
		return analyze.KindSystemContext
	}
	for _, t := range contextTags {
		if strings.HasPrefix(text, t) {
			return analyze.KindSystemContext
		}
	}
	return analyze.KindUserPrompt
}

// textEvent builds a text-bearing event. Event.Text keeps the words only on
// user prompts and assistant answers (the schema's rule).
func textEvent(p *parser, l line, k analyze.Kind, s string) analyze.Event {
	e := withText(p.base(l, k), s, k == analyze.KindUserPrompt || k == analyze.KindAssistantText || k == analyze.KindThinking)
	if k == analyze.KindUserPrompt || k == analyze.KindAssistantText {
		e.Text = s
	}
	return e
}

func (p *parser) user(l line) []analyze.Event {
	r := l.rec
	if r.Message == nil {
		return nil
	}
	var s string
	if json.Unmarshal(r.Message.Content, &s) == nil {
		return []analyze.Event{textEvent(p, l, p.userKind(r, s), s)}
	}
	var blocks []block
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		return nil
	}
	var out []analyze.Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, textEvent(p, l, p.userKind(r, b.Text), b.Text))
		case "tool_result":
			out = append(out, p.toolResult(l, b))
			if r.ToolDenialKind != "" {
				e := p.base(l, analyze.KindPermissionDeny)
				e.CorrelationID = b.ToolUseID
				out = append(out, e)
			}
		}
	}
	return out
}

func (p *parser) toolResult(l line, b block) analyze.Event {
	e := p.base(l, analyze.KindToolResult)
	e.CorrelationID = b.ToolUseID
	e.ToolNameRaw = "unknown" // the call is not in this file
	if c, ok := p.calls[b.ToolUseID]; ok {
		e.ToolNameRaw = c.name
		if d := l.ts.Sub(c.ts).Milliseconds(); d >= 0 {
			e.DurationMS, e.DurationCoverage = analyze.Int64(d), analyze.Measured
		}
	}
	e.ToolName = normalizeTool(e.ToolNameRaw)
	// is_error absent means success (the Messages API default).
	e.ToolSuccess = analyze.Bool(b.IsError == nil || !*b.IsError)
	return withText(e, resultText(b.Content), false)
}

// resultText is the text of a tool_result's content: a string, or the text
// blocks of an array, with an imagePlaceholder for each image block.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "image":
			sb.WriteString(imagePlaceholder)
		}
	}
	return sb.String()
}

func (p *parser) assistant(l line) []analyze.Event {
	r := l.rec
	if r.Message == nil {
		return nil
	}
	var out []analyze.Event
	if m := r.Message.Model; m != "" && m != syntheticModel && !r.IsAPIError {
		if p.model != "" && m != p.model {
			out = append(out, p.base(l, analyze.KindModelSwitch))
		}
		p.model = m
	}
	var blocks []block
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		var s string
		if json.Unmarshal(r.Message.Content, &s) == nil {
			blocks = []block{{Type: "text", Text: s}}
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			k := analyze.KindAssistantText
			if r.IsAPIError {
				k = analyze.KindError
			}
			out = append(out, textEvent(p, l, k, b.Text))
		case "thinking", "redacted_thinking":
			out = append(out, textEvent(p, l, analyze.KindThinking, b.Thinking))
		case "tool_use":
			e := p.base(l, analyze.KindToolCall)
			e.ToolNameRaw = b.Name
			if e.ToolNameRaw == "" {
				e.ToolNameRaw = "unknown"
			}
			e.ToolName = normalizeTool(b.Name)
			e.CorrelationID = b.ID
			args := []byte(b.Input)
			if len(args) == 0 {
				args = []byte("{}")
			}
			e.ToolArgsHash = analyze.ArgsHash(args)
			e.Files = argFiles(b.Input)
			if e.ToolName == analyze.ToolShell {
				e.Text = shellCommand(b.Input)
			}
			p.calls[b.ID] = call{name: e.ToolNameRaw, ts: l.ts}
			out = append(out, withText(e, string(b.Input), false))
		}
	}
	if r.IsAPIError && len(out) == 0 {
		out = append(out, p.base(l, analyze.KindError))
	}
	return out
}

// modelRequest builds the one model_request event of a requestId from its
// final record. Buckets (§3.2.2): input_tokens, cache reads and cache writes
// are disjoint; output_tokens includes thinking_tokens. A bucket the record
// lacks is nil and the coverage Partial. When output_tokens_details is
// missing (seen locally only on responses relayed from non-Anthropic models
// and on streaming snapshots, which are never final), out_visible takes all
// of output_tokens and out_thinking is nil.
func (p *parser) modelRequest(l line) analyze.Event {
	e := p.base(l, analyze.KindModelRequest)
	e.RequestID = l.rec.RequestID
	// The answer id the client stored: the assistant message id, the same
	// on every record of the request.
	e.CorrelationID = l.rec.Message.ID
	raw := l.rec.Message.Usage
	e.TokensNative = append(json.RawMessage(nil), raw...)
	var u usage
	if json.Unmarshal(raw, &u) != nil {
		e.TokensCoverage = analyze.Unknown
		return e
	}
	t := &analyze.Tokens{InUncached: u.InputTokens, InCacheRead: u.CacheReadTokens, InCacheWrite: u.CacheCreationTokens}
	if u.OutputTokens != nil {
		out := *u.OutputTokens
		if d := u.OutputTokensDetails; d != nil && d.ThinkingTokens != nil {
			th := min(*d.ThinkingTokens, out)
			t.OutThinking = analyze.Int64(th)
			out -= th
		}
		t.OutVisible = analyze.Int64(out)
	}
	e.Tokens, e.TokensCoverage = t, analyze.Measured
	for _, b := range []*int64{t.InUncached, t.InCacheRead, t.InCacheWrite, t.OutVisible, t.OutThinking} {
		if b == nil {
			e.TokensCoverage = analyze.Partial
		}
	}
	return e
}

// normalizeTool maps a Claude Code tool name to the core vocabulary.
func normalizeTool(name string) string {
	switch name {
	case "Read":
		return analyze.ToolReadFile
	case "Grep", "Glob":
		return analyze.ToolSearch
	case "Bash", "PowerShell":
		return analyze.ToolShell
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		return analyze.ToolEdit
	case "WebFetch", "WebSearch":
		return analyze.ToolWeb
	case "Agent", "Task":
		return analyze.ToolSubagent
	case "AskUserQuestion":
		return analyze.ToolAskUser
	}
	if strings.HasPrefix(name, "mcp__") {
		return analyze.ToolMCP
	}
	return analyze.ToolOther
}

// shellCommand is the command line of a Bash or PowerShell call ("" when the
// input has none).
func shellCommand(input json.RawMessage) string {
	var args struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &args)
	return args.Command
}

// argFiles returns the path-like arguments of a tool call.
func argFiles(input json.RawMessage) []string {
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		var s string
		if json.Unmarshal(args[k], &s) == nil && s != "" {
			out = append(out, s)
		}
	}
	return out
}

var errNoSession = errors.New("claude: hook stdin has no session_id")
