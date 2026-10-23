package antigravity

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

var (
	userRequestRe = regexp.MustCompile(`(?s)<USER_REQUEST>(.*?)</USER_REQUEST>`)
	settingsRe    = regexp.MustCompile(`(?s)<USER_SETTINGS_CHANGE>(.*?)</USER_SETTINGS_CHANGE>`)
	// The settings note reads: The user changed setting `Model Selection`
	// from <old> to <new>. <more text>
	modelSettingRe = regexp.MustCompile("changed setting `Model[^`]*` from .*? to (.+?)\\.(?:\\s|$)")
	createdAtRe    = regexp.MustCompile(`(?m)^[ \t]*Created At: (\S+)[ \t\r]*$`)
	completedAtRe  = regexp.MustCompile(`(?m)^[ \t]*Completed At: (\S+)[ \t\r]*$`)
)

// toolTypes are step types that are always a tool's result.
var toolTypes = map[string]string{
	"VIEW_FILE":        analyze.ToolReadFile,
	"GREP_SEARCH":      analyze.ToolSearch,
	"LIST_DIRECTORY":   analyze.ToolSearch,
	"RUN_COMMAND":      analyze.ToolShell,
	"CODE_ACTION":      analyze.ToolEdit,
	"READ_URL_CONTENT": analyze.ToolWeb,
	"SEARCH_WEB":       analyze.ToolWeb,
	"BROWSER_SUBAGENT": analyze.ToolSubagent,
	"ASK_QUESTION":     analyze.ToolAskUser,
	"GENERIC":          analyze.ToolOther, // named by its planner tool call
}

// callNames maps the planner's tool_calls[].name.
var callNames = map[string]string{
	"view_file":                  analyze.ToolReadFile,
	"grep_search":                analyze.ToolSearch,
	"find_by_name":               analyze.ToolSearch,
	"list_dir":                   analyze.ToolSearch,
	"run_command":                analyze.ToolShell,
	"command_status":             analyze.ToolShell,
	"send_command_input":         analyze.ToolShell,
	"replace_file_content":       analyze.ToolEdit,
	"multi_replace_file_content": analyze.ToolEdit,
	"write_to_file":              analyze.ToolEdit,
	"read_url_content":           analyze.ToolWeb,
	"search_web":                 analyze.ToolWeb,
	"browser_subagent":           analyze.ToolSubagent,
	"invoke_subagent":            analyze.ToolSubagent,
	"manage_subagents":           analyze.ToolSubagent,
	"ask_question":               analyze.ToolAskUser,
	"call_mcp_tool":              analyze.ToolMCP,
}

// normalizeTool names a tool step: by its step type when the type is
// specific, else by the paired planner tool call's name.
func normalizeTool(stepType, callName string) string {
	if n, ok := toolTypes[stepType]; ok && n != analyze.ToolOther {
		return n
	}
	if n, ok := callNames[callName]; ok {
		return n
	}
	if strings.HasPrefix(strings.ToLower(callName), "mcp") {
		return analyze.ToolMCP
	}
	return analyze.ToolOther
}

// Events emits a conversation's events in step order. Mapping:
//
//   - USER_INPUT from the user: user_prompt (the <USER_REQUEST> text when
//     present). Its <USER_SETTINGS_CHANGE> model note sets the model; in the
//     first user input it is the session's model, later it is a model_switch.
//     USER_INPUT from another source and CONVERSATION_HISTORY: system_context.
//   - PLANNER_RESPONSE: assistant_thinking (if any), assistant_text (if any)
//     and exactly one model_request (RequestID "step-<index>"). Its Tokens
//     come from the step's usage fields (see usage), coverage partial; a step
//     without them has Tokens nil, coverage unknown. Its tool_calls are
//     paired, in order, with the tool steps that follow.
//   - A tool step (a type in toolTypes, or any unknown MODEL-sourced type):
//     tool_call plus tool_result with CorrelationID "step-<index>"; the
//     duration comes from the embedded Created At / Completed At lines. ERROR
//     status (or a non-zero exit_code) is ToolSuccess false; an ERROR step
//     also yields permission_denied when its content says "denied
//     permission", else error. A RUNNING step has no result yet.
//   - ERROR_MESSAGE: error (or permission_denied); when it answers a pending
//     tool call, also that call's tool_call and a failed tool_result.
//   - SYSTEM_MESSAGE: interrupt when it reports a cancellation ("was
//     canceled"), else system_context. CHECKPOINT: checkpoint.
//   - Interrupt: a RUNNING tool step followed by the user's next USER_INPUT
//     with no MODEL step in between. A RUNNING step the model keeps working
//     past is a background command, not an interruption.
//
// Seq is "step-<index>" for a step's main event and "step-<index>:<role>"
// for the others.
//
// Text is the prompt (the <USER_REQUEST> body), the answer, or a
// run_command's CommandLine. Cwd is set only on run_command calls and their
// results (the call's Cwd argument): the transcript records no conversation
// workspace. The format records no answer id, so model_request events have
// no CorrelationID.
func (a Adapter) Events(src analyze.Source, emit func(analyze.Event) error) error {
	return a.Bodies(src, func(e analyze.Event) error {
		e.Body = ""
		return emit(e)
	})
}

// Bodies emits the source's events like Events, with Body set.
func (Adapter) Bodies(src analyze.Source, emit func(analyze.Event) error) error {
	steps, _, err := readSteps(src)
	if err != nil {
		return err
	}
	c := converter{src: src, origin: originFor(src.Store), emit: emit}
	for i := range steps {
		if err := c.step(&steps[i]); err != nil {
			return err
		}
	}
	return nil
}

type converter struct {
	src    analyze.Source
	origin string
	emit   func(analyze.Event) error

	model      string
	seenUser   bool
	lastTS     time.Time
	pending    []toolCall // planner tool calls not yet answered by a step
	hasRunning bool       // a RUNNING tool step with no MODEL step after it
}

func (c *converter) base(s *step, ts time.Time, suffix string, kind analyze.Kind) analyze.Event {
	seq := "step-" + strconv.Itoa(*s.Index)
	if suffix != "" {
		seq += ":" + suffix
	}
	r, _ := json.Marshal(s.ref)
	return analyze.Event{
		Tool: ID, Store: c.src.Store, Origin: c.origin, NativeSessionID: c.src.NativeSessionID,
		Seq: seq, TS: ts, Kind: kind, ObservedBy: analyze.FromTranscript,
		BranchCoverage: analyze.Unknown, DurationCoverage: analyze.Unknown, Ref: r,
	}
}

func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func (c *converter) step(s *step) error {
	ts, ok := parseTS(s.CreatedAt)
	if !ok {
		if c.lastTS.IsZero() {
			return nil // no usable time yet: TS must never be zero
		}
		ts = c.lastTS
	}
	c.lastTS = ts

	if s.Source == "MODEL" {
		c.hasRunning = false
	}
	switch {
	case s.Type == "USER_INPUT" && s.Source == "USER_EXPLICIT":
		return c.userInput(s, ts)
	case s.Type == "USER_INPUT" || s.Type == "CONVERSATION_HISTORY":
		return c.emit(c.base(s, ts, "", analyze.KindSystemContext))
	case s.Type == "PLANNER_RESPONSE":
		return c.planner(s, ts)
	case s.Type == "ERROR_MESSAGE":
		return c.errorMessage(s, ts)
	case s.Type == "SYSTEM_MESSAGE":
		if isCancellation(s.Content) {
			return c.emit(c.base(s, ts, "", analyze.KindInterrupt))
		}
		return c.emit(c.base(s, ts, "", analyze.KindSystemContext))
	case s.Type == "CHECKPOINT":
		return c.emit(c.base(s, ts, "", analyze.KindCheckpoint))
	case toolTypes[s.Type] != "" || s.Source == "MODEL":
		return c.tool(s, ts)
	default:
		return c.emit(c.base(s, ts, "", analyze.KindSystemContext))
	}
}

func isCancellation(content string) bool {
	l := strings.ToLower(content)
	return strings.Contains(l, "was canceled") || strings.Contains(l, "was cancelled")
}

func isDenial(s *step) bool {
	return strings.Contains(strings.ToLower(s.Content+"\n"+s.Error), "denied permission")
}

func (c *converter) userInput(s *step, ts time.Time) error {
	c.pending = nil
	if c.hasRunning {
		c.hasRunning = false
		if err := c.emit(c.base(s, ts, "interrupt", analyze.KindInterrupt)); err != nil {
			return err
		}
	}
	first := !c.seenUser
	c.seenUser = true
	if m := settingsModel(s.Content); m != "" {
		if first {
			c.model = m
		} else if m != c.model {
			c.model = m
			e := c.base(s, ts, "model", analyze.KindModelSwitch)
			e.Model = m
			if err := c.emit(e); err != nil {
				return err
			}
		}
	}
	text := s.Content
	if m := userRequestRe.FindStringSubmatch(text); m != nil {
		text = strings.TrimSpace(m[1])
	}
	e := c.base(s, ts, "", analyze.KindUserPrompt)
	e.Text = text
	e.TextLen = utf8.RuneCountInString(text)
	e.TextVolumeUnits, e.Body = analyze.TextVolume(text), text
	return c.emit(e)
}

// settingsModel returns the model a USER_INPUT's settings-change note names,
// or "".
func settingsModel(content string) string {
	model := ""
	for _, blk := range settingsRe.FindAllStringSubmatch(content, -1) {
		if m := modelSettingRe.FindStringSubmatch(blk[1]); m != nil {
			model = strings.TrimSpace(m[1])
		}
	}
	return model
}

func (c *converter) planner(s *step, ts time.Time) error {
	c.pending = append([]toolCall(nil), s.ToolCalls...)
	if s.Thinking != "" {
		e := c.base(s, ts, "thinking", analyze.KindThinking)
		e.Model = c.model
		e.TextVolumeUnits, e.Body = analyze.TextVolume(s.Thinking), s.Thinking
		if err := c.emit(e); err != nil {
			return err
		}
	}
	if strings.TrimSpace(s.Content) != "" {
		e := c.base(s, ts, "text", analyze.KindAssistantText)
		e.Text = s.Content
		e.Model = c.model
		e.TextLen = utf8.RuneCountInString(s.Content)
		e.TextVolumeUnits, e.Body = analyze.TextVolume(s.Content), s.Content
		if err := c.emit(e); err != nil {
			return err
		}
	}
	e := c.base(s, ts, "", analyze.KindModelRequest)
	e.Model = c.model
	e.RequestID = "step-" + strconv.Itoa(*s.Index)
	e.Tokens, e.TokensNative = usage(s)
	e.TokensCoverage = analyze.Unknown
	if e.Tokens != nil {
		e.TokensCoverage = analyze.Partial // in_cache_write and both output buckets are null
	}
	if err := c.emit(e); err != nil {
		return err
	}
	if s.Status == "ERROR" {
		return c.failure(s, ts)
	}
	return nil
}

// usage maps a planner step's usage fields to token buckets. Evidence from a
// real IDE store (299 planner steps with usage, 2026-10):
//
//   - input_tokens excludes cache reads: it is below cache_read_tokens in 266
//     of 299 steps, while input + cache read never shrank between consecutive
//     requests (296/296 pairs) and input alone did (89/296). So input_tokens
//     is in_uncached and cache_read_tokens is in_cache_read.
//   - output_tokens is per request (it fell in 165/296 pairs) and appears to
//     include thinking: it was at least the persisted text + tool arguments +
//     thinking volume in 92/92 steps with thinking. It cannot be split into
//     visible and thinking output, so out_visible and out_thinking stay null;
//     the raw value is kept in TokensNative.
//   - cache writes are not recorded: in_cache_write is null.
//
// The result is always Partial coverage. It returns nil Tokens when the step
// has no usable count (older versions and the CLI store write none).
func usage(s *step) (*analyze.Tokens, json.RawMessage) {
	fields := []struct {
		name string
		raw  json.RawMessage
	}{
		{"input_tokens", s.InputTokens}, {"cache_read_tokens", s.CacheReadTokens}, {"output_tokens", s.OutputTokens},
	}
	var native []byte
	for _, f := range fields {
		if len(f.raw) == 0 {
			continue
		}
		if native == nil {
			native = []byte{'{'}
		} else {
			native = append(native, ',')
		}
		native = strconv.AppendQuote(native, f.name)
		native = append(append(native, ':'), f.raw...)
	}
	if native == nil {
		return nil, nil
	}
	native = append(native, '}')
	in, okIn := count(s.InputTokens)
	cr, okCR := count(s.CacheReadTokens)
	out, okOut := count(s.OutputTokens)
	if !okIn && !okCR && !okOut {
		return nil, native
	}
	t := &analyze.Tokens{}
	if okIn {
		t.InUncached = analyze.Int64(in)
	}
	if okCR {
		t.InCacheRead = analyze.Int64(cr)
	}
	if okOut {
		// Output includes thinking and cannot be split: all of it is OutVisible
		// and OutThinking stays nil (see analyze.Tokens).
		t.OutVisible = analyze.Int64(out)
	}
	return t, native
}

// count parses a non-negative integer token count.
func count(raw json.RawMessage) (int64, bool) {
	var n int64
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// popCall takes the next planner tool call this step answers, if any.
func (c *converter) popCall() (toolCall, bool) {
	if len(c.pending) == 0 {
		return toolCall{}, false
	}
	tc := c.pending[0]
	c.pending = c.pending[1:]
	return tc, true
}

func (c *converter) tool(s *step, ts time.Time) error {
	tc, paired := c.popCall()
	return c.toolPair(s, ts, tc, paired, s.Status == "RUNNING")
}

func (c *converter) toolPair(s *step, ts time.Time, tc toolCall, paired, running bool) error {
	raw := s.Type
	args := []byte(s.Content)
	if paired {
		if tc.Name != "" {
			raw = tc.Name
		}
		if len(tc.Args) > 0 {
			args = tc.Args
		}
	}
	if raw == "" {
		raw = "UNKNOWN"
	}
	corr := "step-" + strconv.Itoa(*s.Index)
	name := normalizeTool(s.Type, tc.Name)

	var cmd runCommandArgs
	if paired && tc.Name == "run_command" {
		_ = json.Unmarshal(tc.Args, &cmd)
	}

	call := c.base(s, ts, "", analyze.KindToolCall)
	call.ToolName, call.ToolNameRaw, call.CorrelationID = name, raw, corr
	call.Cwd = cmd.Cwd
	if name == analyze.ToolShell {
		call.Text = cmd.CommandLine
	}
	call.ToolArgsHash = analyze.ArgsHash(args)
	if paired {
		call.TextVolumeUnits, call.Body = analyze.TextVolume(string(tc.Args)), string(tc.Args)
		call.Files = argFiles(tc.Args)
	}
	if err := c.emit(call); err != nil {
		return err
	}
	if running {
		c.hasRunning = true
		return nil
	}

	res := c.base(s, ts, "result", analyze.KindToolResult)
	res.ToolName, res.ToolNameRaw, res.CorrelationID = name, raw, corr
	res.Cwd = cmd.Cwd
	res.TextVolumeUnits, res.Body = analyze.TextVolume(s.Content), s.Content
	failed := s.Status == "ERROR" || s.Type == "ERROR_MESSAGE" || (s.ExitCode != nil && *s.ExitCode != 0)
	res.ToolSuccess = analyze.Bool(!failed)
	start, ok1 := embeddedTime(createdAtRe, s.Content)
	end, ok2 := embeddedTime(completedAtRe, s.Content)
	if ok2 {
		res.TS = end
	}
	if ok1 && ok2 && !end.Before(start) {
		res.DurationMS = analyze.Int64(end.Sub(start).Milliseconds())
		res.DurationCoverage = analyze.Measured
	}
	if err := c.emit(res); err != nil {
		return err
	}
	if s.Status == "ERROR" && s.Type != "ERROR_MESSAGE" { // errorMessage reports its own
		return c.failure(s, ts)
	}
	return nil
}

// runCommandArgs are the run_command arguments the adapter reads. Cwd is the
// only working directory an Antigravity transcript records: steps carry no
// conversation workspace, so other events have Cwd "".
type runCommandArgs struct {
	CommandLine string `json:"CommandLine"`
	Cwd         string `json:"Cwd"`
}

// failure emits permission_denied or error for a failed step.
func (c *converter) failure(s *step, ts time.Time) error {
	if isDenial(s) {
		return c.emit(c.base(s, ts, "denied", analyze.KindPermissionDeny))
	}
	return c.emit(c.base(s, ts, "error", analyze.KindError))
}

func (c *converter) errorMessage(s *step, ts time.Time) error {
	if tc, paired := c.popCall(); paired {
		if err := c.toolPair(s, ts, tc, true, false); err != nil {
			return err
		}
	}
	return c.failure(s, ts)
}

func embeddedTime(re *regexp.Regexp, content string) (time.Time, bool) {
	m := re.FindStringSubmatch(content)
	if m == nil {
		return time.Time{}, false
	}
	return parseTS(m[1])
}

// argFiles lists the paths a tool call names in its arguments.
func argFiles(args json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"AbsolutePath", "TargetFile", "DirectoryPath", "SearchPath", "SearchDirectory"} {
		if v, ok := m[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}
