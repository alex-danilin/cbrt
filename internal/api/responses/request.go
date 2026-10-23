package responses

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/alex-danilin/cbrt/internal/jsonscan"
)

// RequestInfo describes a decoded Responses request.
type RequestInfo struct {
	Model           string   // model as requested by the client
	Stream          bool     // client asked for SSE
	Downgraded      []string // what was dropped/approximated, deduplicated, in first-seen order
	ReasoningEffort string   // reasoning.effort if present, else ""
	Tools           ToolMap  // maps chat tool calls back to Responses tools
}

// maxGrammar caps the custom-tool grammar copied into a tool description.
const maxGrammar = 4 << 10

// customInputSchema is the parameters schema of a flattened custom tool.
const customInputSchema = `{"type":"object","properties":{"input":{"type":"string","description":"The raw input for this tool, as plain text (not JSON-encoded)."}},"required":["input"]}`

// emptyParams is the parameters schema of a function tool without one.
const emptyParams = `{"type":"object","properties":{}}`

// builtinCalls are built-in tool call history items; they cannot be
// represented in Chat Completions and are rejected.
var builtinCalls = map[string]bool{
	"web_search_call": true, "file_search_call": true, "computer_call": true, "computer_call_output": true,
	"local_shell_call": true, "local_shell_call_output": true, "image_generation_call": true,
	"code_interpreter_call": true, "mcp_call": true, "mcp_list_tools": true, "mcp_approval_request": true,
	"mcp_approval_response": true, "shell_call": true, "shell_call_output": true,
	"apply_patch_call": true, "apply_patch_call_output": true, "tool_search_call": true, "tool_search_output": true,
}

// The decoder validates the body once and then works on it without copying
// or re-parsing: every []byte field below is a slice of the body holding one
// raw JSON value (string fields keep their quoted literal), and the Chat
// Completions body is appended directly to an output buffer. Large values
// (tool parameter schemas, message and tool output texts) are copied to the
// output as they are and never decoded. Objects are matched with
// encoding/json's rules (see package jsonscan), so the result is the same as
// decoding into the equivalent structs.

// inTool is a Responses tool definition.
type inTool struct {
	typ, name string
	desc      []byte // string literal or nil
	params    []byte // any JSON value or nil
	format    []byte // any JSON value or nil
	tools     []inTool
	strict    bool // "strict": true (dropped: free providers reject it)
}

func decodeTool(x *jsonscan.Index, t *inTool, v []byte) bool {
	if v[0] != '{' {
		return false
	}
	var kb [32]byte
	it := jsonscan.ObjectMembers(x, v)
	for {
		k, val, more := it.Next()
		if !more {
			return true
		}
		ok := true
		switch string(jsonscan.FoldKey(&kb, k)) {
		case "type":
			ok = fieldEnum(val, &t.typ)
		case "name":
			ok = jsonscan.FieldStr(val, &t.name)
		case "description":
			ok = jsonscan.FieldRawStr(val, &t.desc)
		case "parameters":
			t.params = val
		case "format":
			t.format = val
		case "strict":
			t.strict = string(val) == "true"
		case "tools":
			if jsonscan.IsNull(val) {
				t.tools = nil
			} else {
				t.tools, ok = jsonscan.DecodeSlice(x, t.tools, val, false, decodeTool)
			}
		}
		if !ok {
			return false
		}
	}
}

// inItem is a Responses input item.
type inItem struct {
	typ, role, callID, name, namespace string
	content, arguments, output         []byte // any JSON value or nil
	input                              []byte // string literal, or nil if absent/null
}

func decodeItem(x *jsonscan.Index, it *inItem, v []byte) bool {
	switch v[0] {
	case 'n':
		return true
	case '{':
	default:
		return false
	}
	var kb [32]byte
	m := jsonscan.ObjectMembers(x, v)
	for {
		k, val, more := m.Next()
		if !more {
			return true
		}
		ok := true
		switch string(jsonscan.FoldKey(&kb, k)) {
		case "type":
			ok = fieldEnum(val, &it.typ)
		case "role":
			ok = fieldEnum(val, &it.role)
		case "call_id":
			ok = jsonscan.FieldStr(val, &it.callID)
		case "name":
			ok = jsonscan.FieldStr(val, &it.name)
		case "namespace":
			ok = jsonscan.FieldStr(val, &it.namespace)
		case "content":
			it.content = val
		case "arguments":
			it.arguments = val
		case "output":
			it.output = val
		case "input":
			ok = jsonscan.FieldPtrStr(val, &it.input)
		}
		if !ok {
			return false
		}
	}
}

// inPart is a content part.
type inPart struct {
	typ      string
	text     []byte // string literal or nil
	refusal  []byte // string literal or nil
	imageURL []byte // string literal, or nil if absent/null
	fileID   []byte // string literal or nil
	detail   string
}

func decodePart(x *jsonscan.Index, p *inPart, v []byte) bool {
	if v[0] != '{' {
		return false
	}
	var kb [32]byte
	m := jsonscan.ObjectMembers(x, v)
	for {
		k, val, more := m.Next()
		if !more {
			return true
		}
		ok := true
		switch string(jsonscan.FoldKey(&kb, k)) {
		case "type":
			ok = fieldEnum(val, &p.typ)
		case "text":
			ok = jsonscan.FieldRawStr(val, &p.text)
		case "refusal":
			ok = jsonscan.FieldRawStr(val, &p.refusal)
		case "image_url":
			ok = jsonscan.FieldPtrStr(val, &p.imageURL)
		case "file_id":
			ok = jsonscan.FieldRawStr(val, &p.fileID)
		case "detail":
			ok = fieldEnum(val, &p.detail)
		}
		if !ok {
			return false
		}
	}
}

// emptyStr reports whether the string literal s (nil is "") is empty.
func emptyStr(s []byte) bool { return len(s) <= 2 }

// topLevel holds the members of the request object with the semantics of
// map[string]json.RawMessage (exact keys, the last duplicate wins), in the
// order of first occurrence. Small objects are searched linearly; past
// linearTop members a key index keeps decoding linear in the member count.
type topLevel struct {
	members []topMember
	index   map[string]int // key -> position in members; nil while small
}

type topMember struct {
	key string
	val []byte
}

// linearTop is the member count up to which keys are matched by a scan.
const linearTop = 16

func splitTop(x *jsonscan.Index, v []byte) topLevel {
	top := topLevel{members: make([]topMember, 0, linearTop)}
	it := jsonscan.ObjectMembers(x, v)
	for {
		k, val, ok := it.Next()
		if !ok {
			return top
		}
		top.set(jsonscan.Unquote(k), val)
	}
}

func (t *topLevel) set(key string, val []byte) {
	if i, ok := t.find(key); ok {
		t.members[i].val = val
		return
	}
	if t.index != nil {
		t.index[key] = len(t.members)
	}
	t.members = append(t.members, topMember{key, val})
	if t.index == nil && len(t.members) > linearTop {
		t.index = make(map[string]int, 2*len(t.members))
		for i, m := range t.members {
			t.index[m.key] = i
		}
	}
}

func (t *topLevel) find(key string) (int, bool) {
	if t.index != nil {
		i, ok := t.index[key]
		return i, ok
	}
	for i := range t.members {
		if t.members[i].key == key {
			return i, true
		}
	}
	return 0, false
}

func (t *topLevel) get(key string) []byte {
	if i, ok := t.find(key); ok {
		return t.members[i].val
	}
	return nil
}

// null reports whether the member is absent or null.
func (t *topLevel) null(key string) bool { v := t.get(key); return v == nil || jsonscan.IsNull(v) }

type decoder struct {
	info RequestInfo
	seen map[string]bool
	x    *jsonscan.Index // of the request body

	out    []byte // the chat body being written
	nMsgs  int    // messages written to out
	open   bool   // the last message is an assistant message still taking tool calls
	nCalls int    // tool calls written to the open assistant message

	parts []inPart   // scratch
	texts [][]byte   // scratch
	segs  []chatPart // scratch
}

func (d *decoder) downgrade(what string) {
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	if !d.seen[what] {
		d.seen[what] = true
		d.info.Downgraded = append(d.info.Downgraded, what)
	}
}

// DecodeRequest converts an OpenAI Responses request body into an OpenAI
// Chat Completions request body. It returns *Error for content that cannot
// be represented faithfully.
func DecodeRequest(body []byte) ([]byte, RequestInfo, error) {
	d := &decoder{x: jsonscan.NewIndex(body)}
	v := jsonscan.TrimSpace(body)
	if d.x == nil || v[0] != '{' {
		return nil, d.info, errorf("invalid request body: must be a JSON object")
	}
	top := splitTop(d.x, v)

	model, ok := unmarshalString(top.get("model"))
	if !ok || model == "" {
		return nil, d.info, errorf("model: must be a non-empty string")
	}
	d.info.Model = model

	if !top.null("stream") {
		if !jsonscan.FieldBool(top.get("stream"), &d.info.Stream) {
			return nil, d.info, errorf("stream: must be a boolean")
		}
	}

	if !top.null("previous_response_id") {
		if id, ok := unmarshalString(top.get("previous_response_id")); !ok || id != "" {
			return nil, d.info, errorf("previous_response_id is not supported: this gateway is stateless, send the full conversation in input")
		}
	}

	maxTokens := -1
	if !top.null("max_output_tokens") {
		n, ok := jsonscan.ParseFloat(top.get("max_output_tokens"))
		if !ok || n < 1 || n != math.Trunc(n) || n > math.MaxInt32 {
			return nil, d.info, errorf("max_output_tokens: must be a positive integer")
		}
		maxTokens = int(n)
	}
	var sampling [2][]byte // temperature, top_p (valid numbers, written as is)
	for i, k := range [...]string{"temperature", "top_p"} {
		if top.null(k) {
			continue
		}
		raw := top.get(k)
		if _, ok := jsonscan.ParseFloat(raw); !ok {
			return nil, d.info, errorf("%s: must be a number", k)
		}
		sampling[i] = raw
	}

	// The chat body is written into one buffer, tools before messages (JSON
	// member order is not significant). It is about as large as the tools
	// and input it is made of.
	size := len(model) + len(top.get("tools")) + len(top.get("instructions")) + len(top.get("input"))
	d.out = make([]byte, 0, size+size/8+512)
	d.out = append(d.out, `{"model":`...)
	d.out = jsonscan.AppendQuoted(d.out, model)

	// Tools first: history items need the name mapping.
	nTools := 0
	if !top.null("tools") {
		var err error
		if nTools, err = d.tools(top.get("tools")); err != nil {
			return nil, d.info, err
		}
	}
	d.out = append(d.out, `,"messages":`...)
	msgsAt := len(d.out)
	d.out = append(d.out, '[')

	if !top.null("instructions") {
		raw := top.get("instructions")
		if raw[0] != '"' {
			return nil, d.info, errorf("instructions: must be a string")
		}
		if !emptyStr(raw) {
			d.beginMsg("system")
			d.out = jsonscan.AppendStrLit(d.out, raw)
			d.endMsg()
		}
	}

	if top.null("input") {
		return nil, d.info, errorf("input: is required")
	}
	switch raw := top.get("input"); raw[0] {
	case '"':
		if emptyStr(raw) {
			return nil, d.info, errorf("input: must not be empty")
		}
		d.beginMsg("user")
		d.out = jsonscan.AppendStrLit(d.out, raw)
		d.endMsg()
	case '[':
		if err := d.items(raw); err != nil {
			return nil, d.info, err
		}
	default:
		return nil, d.info, errorf("input: must be a string or an array of input items")
	}
	d.closeMsg()

	var toolChoice any
	var parallel []byte
	if nTools > 0 {
		if !top.null("tool_choice") {
			choice, err := d.toolChoice(top.get("tool_choice"))
			if err != nil {
				return nil, d.info, err
			}
			toolChoice = choice
		}
		if !top.null("parallel_tool_calls") {
			var b bool
			if parallel = top.get("parallel_tool_calls"); !jsonscan.FieldBool(parallel, &b) {
				return nil, d.info, errorf("parallel_tool_calls: must be a boolean")
			}
		}
	}

	if string(top.get("store")) == "true" {
		d.downgrade("store")
	}
	if !top.null("reasoning") {
		d.downgrade("reasoning")
		var r struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(top.get("reasoning"), &r) == nil {
			d.info.ReasoningEffort = r.Effort
		}
	}
	var responseFormat json.RawMessage
	if !top.null("text") {
		rf, err := d.text(top.get("text"))
		if err != nil {
			return nil, d.info, err
		}
		responseFormat = rf
	}

	// include, prompt_cache_key, client_metadata, metadata, truncation,
	// service_tier, user, top_logprobs, background, and anything
	// unrecognized are dropped and reported by name.
	var unknown []string
	for _, m := range top.members {
		switch m.key {
		case "model", "stream", "previous_response_id", "max_output_tokens", "temperature", "top_p",
			"tools", "instructions", "input", "tool_choice", "parallel_tool_calls", "store", "reasoning", "text":
			continue
		}
		if !jsonscan.IsNull(m.val) {
			unknown = append(unknown, m.key)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		d.downgrade(k)
	}

	// The rest of the chat body.
	out := append(d.out, ']')
	if d.nMsgs == 0 {
		out = append(out[:msgsAt], "null"...) // as the nil slice encoded before
	}
	out = append(out, `,"stream":`...)
	out = strconv.AppendBool(out, d.info.Stream)
	if maxTokens >= 0 {
		out = append(out, `,"max_tokens":`...)
		out = strconv.AppendInt(out, int64(maxTokens), 10)
	}
	if sampling[0] != nil {
		out = append(out, `,"temperature":`...)
		out = append(out, sampling[0]...)
	}
	if sampling[1] != nil {
		out = append(out, `,"top_p":`...)
		out = append(out, sampling[1]...)
	}
	if toolChoice != nil {
		out = append(out, `,"tool_choice":`...)
		out = append(out, marshal(toolChoice)...)
	}
	if parallel != nil {
		out = append(out, `,"parallel_tool_calls":`...)
		out = append(out, parallel...)
	}
	if len(responseFormat) > 0 {
		out = append(out, `,"response_format":`...)
		out = append(out, responseFormat...)
	}
	out = append(out, '}')
	return out, d.info, nil
}

// unmarshalString decodes a raw value like json.Unmarshal into a string:
// absent is an error, null is "".
func unmarshalString(v []byte) (string, bool) {
	switch {
	case v == nil:
		return "", false
	case v[0] == '"':
		return jsonscan.Unquote(v), true
	case jsonscan.IsNull(v):
		return "", true
	}
	return "", false
}

// Message writing. The assistant message that tool calls merge into is
// always the last one written, so it is left open until the next message.

// beginMsg starts a message; its content value must follow.
func (d *decoder) beginMsg(role string) {
	d.closeMsg()
	if d.nMsgs > 0 {
		d.out = append(d.out, ',')
	}
	d.nMsgs++
	d.out = append(d.out, `{"role":"`...)
	d.out = append(d.out, role...)
	d.out = append(d.out, `","content":`...)
}

func (d *decoder) endMsg() { d.out = append(d.out, '}') }

// openAssistant ends a message started with beginMsg("assistant") without
// closing it, so that tool calls can follow.
func (d *decoder) openAssistant() { d.open, d.nCalls = true, 0 }

func (d *decoder) closeMsg() {
	if !d.open {
		return
	}
	if d.nCalls > 0 {
		d.out = append(d.out, ']')
	}
	d.out = append(d.out, '}')
	d.open = false
}

// text maps text.format to response_format; verbosity is downgraded.
func (d *decoder) text(raw json.RawMessage) (json.RawMessage, error) {
	var t map[string]json.RawMessage
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, errorf("text: must be an object")
	}
	var rf json.RawMessage
	for _, k := range sortedKeys(t) {
		if isNull(t[k]) {
			continue
		}
		if k != "format" {
			d.downgrade("text." + k)
			continue
		}
		var f struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict,omitempty"`
		}
		if err := json.Unmarshal(t[k], &f); err != nil {
			return nil, errorf("text.format: must be an object")
		}
		switch f.Type {
		case "", "text":
		case "json_object":
			rf = json.RawMessage(`{"type":"json_object"}`)
		case "json_schema":
			if isNull(f.Schema) {
				return nil, errorf("text.format: json_schema requires schema")
			}
			name := f.Name
			if name == "" {
				name = "response"
			}
			rf = marshal(map[string]any{"type": "json_schema", "json_schema": struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Schema      json.RawMessage `json:"schema"`
				Strict      *bool           `json:"strict,omitempty"`
			}{name, f.Description, compactJSON(f.Schema), f.Strict}})
		default:
			return nil, errorf("text.format: unsupported type %q", f.Type)
		}
	}
	return rf, nil
}

// tools flattens function, custom and namespace tools into chat function
// tools and drops built-in tools. Top-level names are registered before
// namespaced ones so that they keep their names on collision. It returns
// the number of chat tools, which are appended to d.out as the "tools"
// member (omitted if there are none).
func (d *decoder) tools(raw []byte) (int, error) {
	list, ok := jsonscan.DecodeSlice[inTool](d.x, nil, raw, true, decodeTool)
	if !ok {
		return 0, errorf("tools: must be an array of tool definitions")
	}
	names := make([][]string, len(list))
	hint := 0 // number of tools to register
	for i := range list {
		hint++
		if list[i].typ == "namespace" {
			hint += len(list[i].tools)
		}
	}
	for i, t := range list {
		if t.typ != "function" && t.typ != "custom" {
			continue
		}
		if t.name == "" {
			return 0, errorf("tools[%d]: name is required", i)
		}
		n := d.addTool(hint, toolRef{name: t.name, custom: t.typ == "custom"})
		if n == "" {
			return 0, errorf("tools[%d]: duplicate tool name %q", i, t.name)
		}
		names[i] = []string{n}
	}
	for i, t := range list {
		if t.typ != "namespace" {
			continue
		}
		if t.name == "" {
			return 0, errorf("tools[%d]: namespace name is required", i)
		}
		names[i] = make([]string, 0, len(t.tools))
		for j, nt := range t.tools {
			if nt.typ != "function" && nt.typ != "custom" {
				d.downgrade("tool:" + nt.typ)
				names[i] = append(names[i], "")
				continue
			}
			if nt.name == "" {
				return 0, errorf("tools[%d].tools[%d]: name is required", i, j)
			}
			n := d.addTool(hint, toolRef{namespace: t.name, name: nt.name, custom: nt.typ == "custom"})
			if n == "" {
				return 0, errorf("tools[%d].tools[%d]: duplicate tool name %q in namespace %q", i, j, nt.name, t.name)
			}
			names[i] = append(names[i], n)
		}
	}

	mark := len(d.out)
	out := append(d.out, `,"tools":[`...)
	count := 0
	for i := range list {
		t := &list[i]
		switch t.typ {
		case "function", "custom":
			var err error
			if out, err = d.tool(out, count, t, names[i][0], "", i, -1); err != nil {
				return 0, err
			}
			count++
		case "namespace":
			for j := range t.tools {
				if names[i][j] == "" {
					continue
				}
				// The namespace description is given once, on its first tool,
				// to keep large app namespaces from multiplying prompt size.
				desc := ""
				if j == 0 && !emptyStr(t.desc) {
					desc = "(" + t.name + ") " + jsonscan.Unquote(t.desc)
				}
				var err error
				if out, err = d.tool(out, count, &t.tools[j], names[i][j], desc, i, j); err != nil {
					return 0, err
				}
				count++
			}
		default:
			d.downgrade("tool:" + t.typ)
		}
	}
	if count == 0 {
		out = out[:mark]
	} else {
		out = append(out, ']')
	}
	d.out = out
	return count, nil
}

// toolWhere names tools[i] or tools[i].tools[j] (j >= 0) in errors.
func toolWhere(i, j int) string {
	if j < 0 {
		return fmt.Sprintf("tools[%d]", i)
	}
	return fmt.Sprintf("tools[%d].tools[%d]", i, j)
}

// tool appends one function or custom tool as the n-th chat tool. nsDesc,
// if set, is the description of the enclosing namespace and is prepended.
func (d *decoder) tool(out []byte, n int, t *inTool, name, nsDesc string, i, j int) ([]byte, error) {
	if t.typ == "function" && t.strict {
		d.downgrade("tools.strict")
	}
	// The description stays the original literal unless it is rewritten.
	desc, rawDesc := "", true
	if nsDesc != "" {
		desc, rawDesc = strings.TrimSpace(nsDesc+"\n\n"+unquoteOrEmpty(t.desc)), false
	}
	params := []byte(customInputSchema)
	if t.typ == "function" {
		params = []byte(emptyParams)
		if t.params != nil && !jsonscan.IsNull(t.params) {
			if t.params[0] != '{' {
				return out, errorf("%s: parameters must be a JSON Schema object", toolWhere(i, j))
			}
			params = t.params
		}
	} else if t.format != nil && !jsonscan.IsNull(t.format) {
		var f struct {
			Type       string `json:"type"`
			Syntax     string `json:"syntax"`
			Definition string `json:"definition"`
		}
		if err := json.Unmarshal(t.format, &f); err != nil {
			return out, errorf("%s: format must be an object", toolWhere(i, j))
		}
		switch f.Type {
		case "", "text":
		case "grammar":
			d.downgrade("custom_tool_grammar")
			def := f.Definition
			if len(def) > maxGrammar {
				def = truncateUTF8(def, maxGrammar) + "\n…(truncated)"
			}
			syntax := f.Syntax
			if syntax == "" {
				syntax = "grammar"
			}
			if rawDesc {
				desc, rawDesc = unquoteOrEmpty(t.desc), false
			}
			desc = strings.TrimSpace(desc + "\n\nThe `input` argument is raw text that must conform to this " +
				syntax + " grammar:\n```\n" + def + "\n```")
		default:
			d.downgrade("custom_tool_format")
		}
	}

	if n > 0 {
		out = append(out, ',')
	}
	out = append(out, `{"type":"function","function":{"name":`...)
	out = jsonscan.AppendQuoted(out, name)
	switch {
	case rawDesc && !emptyStr(t.desc):
		out = append(out, `,"description":`...)
		out = jsonscan.AppendStrLit(out, t.desc)
	case !rawDesc && desc != "":
		out = append(out, `,"description":`...)
		out = jsonscan.AppendQuoted(out, desc)
	}
	out = append(out, `,"parameters":`...)
	out = d.x.AppendCompact(out, params)
	return append(out, "}}"...), nil
}

func unquoteOrEmpty(s []byte) string {
	if s == nil {
		return ""
	}
	return jsonscan.Unquote(s)
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// toolChoice maps a Responses tool_choice; unsupported forms are downgraded
// (nil result).
func (d *decoder) toolChoice(raw json.RawMessage) (any, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return s, nil
		}
		return nil, errorf("tool_choice: unsupported value %q", s)
	}
	var tc struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, errorf("tool_choice: must be a string or an object")
	}
	switch tc.Type {
	case "function", "custom":
		if tc.Name == "" {
			return nil, errorf("tool_choice: name is required for type %q", tc.Type)
		}
		name := d.info.Tools.chatName(tc.Namespace, tc.Name)
		if _, ok := d.info.Tools.byChat[name]; !ok {
			return nil, errorf("tool_choice: tool %q is not among the offered tools", tc.Name)
		}
		return map[string]any{"type": "function", "function": map[string]string{"name": name}}, nil
	}
	d.downgrade("tool_choice")
	return nil, nil
}

// items converts the input items (an array) to chat messages in order.
func (d *decoder) items(raw []byte) error {
	it := jsonscan.ArrayElements(d.x, raw)
	i := 0
	for ; ; i++ {
		v, more := it.Next()
		if !more {
			break
		}
		var item inItem
		if !decodeItem(d.x, &item, v) {
			return errorf("input[%d]: must be an input item object", i)
		}
		if err := d.item(&item, i); err != nil {
			return err
		}
	}
	if i == 0 {
		return errorf("input: must not be empty")
	}
	return nil
}

func (d *decoder) item(it *inItem, i int) error {
	typ := it.typ
	if typ == "" && it.role != "" {
		typ = "message"
	}
	switch typ {
	case "message":
		return d.message(it, i)
	case "function_call", "custom_tool_call":
		if it.callID == "" || it.name == "" {
			return errorf("input[%d]: %s requires call_id and name", i, typ)
		}
		name := d.info.Tools.chatName(it.namespace, it.name)
		if !d.open {
			d.beginMsg("assistant")
			d.out = append(d.out, "null"...)
			d.openAssistant()
		}
		if d.nCalls == 0 {
			d.out = append(d.out, `,"tool_calls":[`...)
		} else {
			d.out = append(d.out, ',')
		}
		d.nCalls++
		d.out = append(d.out, `{"id":`...)
		d.out = jsonscan.AppendQuoted(d.out, it.callID)
		d.out = append(d.out, `,"type":"function","function":{"name":`...)
		d.out = jsonscan.AppendQuoted(d.out, name)
		d.out = append(d.out, `,"arguments":`...)
		if typ == "function_call" {
			var err error
			if d.out, err = appendHistoryArgs(d.x, d.out, it.arguments); err != nil {
				return errorf("input[%d]: %v", i, err)
			}
		} else {
			if it.input == nil {
				return errorf("input[%d]: custom_tool_call requires input", i)
			}
			// Byte-identical to marshal(map[string]string{"input": ...}).
			args := `{"input":` + string(marshal(jsonscan.Unquote(it.input))) + `}`
			d.out = jsonscan.AppendQuoted(d.out, args)
		}
		d.out = append(d.out, "}}"...)
	case "function_call_output", "custom_tool_call_output":
		if it.callID == "" {
			return errorf("input[%d]: %s requires call_id", i, typ)
		}
		texts, err := d.toolOutputText(it.output, i)
		if err != nil {
			return err
		}
		d.beginMsg("tool")
		d.out = jsonscan.AppendJoined(d.out, texts, "\n")
		d.out = append(d.out, `,"tool_call_id":`...)
		d.out = jsonscan.AppendQuoted(d.out, it.callID)
		d.endMsg()
	case "reasoning":
		d.downgrade("reasoning_items")
	default:
		if builtinCalls[typ] {
			return errorf("input[%d]: built-in tool call history item %q is not supported", i, typ)
		}
		return errorf("input[%d]: unsupported input item type %q", i, typ)
	}
	return nil
}

// appendHistoryArgs appends the arguments string of a function_call given
// as a JSON string (standard) or an object.
func appendHistoryArgs(x *jsonscan.Index, out, raw []byte) ([]byte, error) {
	if raw == nil || jsonscan.IsNull(raw) {
		return append(out, `"{}"`...), nil
	}
	switch raw[0] {
	case '"':
		if blankStr(raw) {
			return append(out, `"{}"`...), nil
		}
		return jsonscan.AppendStrLit(out, raw), nil
	case '{':
		return jsonscan.AppendQuoted(out, string(x.AppendCompact(nil, raw))), nil
	}
	return out, fmt.Errorf("function_call arguments must be a string")
}

// blankStr reports whether the string literal s decodes to white space only.
func blankStr(s []byte) bool {
	for _, c := range s[1 : len(s)-1] {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '\\':
		default:
			if c < 0x80 {
				return false
			}
		}
		// An escape or a non-ASCII byte: decide on the decoded text.
		return strings.TrimSpace(jsonscan.Unquote(s)) == ""
	}
	return true
}

// chatPart is a chat content part; a text part joins d.texts[t0:t1].
type chatPart struct {
	image  bool
	t0, t1 int
	url    []byte // image_url string literal
	detail string
}

// message converts a message item.
func (d *decoder) message(it *inItem, i int) error {
	role := it.role
	switch role {
	case "user", "assistant", "system":
	case "developer":
		role = "system"
	case "":
		return errorf("input[%d]: message requires role", i)
	default:
		return errorf("input[%d]: unsupported role %q", i, it.role)
	}
	parts, err := d.contentParts(it.content, i)
	if err != nil {
		return err
	}
	d.texts, d.segs = d.texts[:0], d.segs[:0]
	for k := range parts {
		p := &parts[k]
		switch p.typ {
		case "input_text", "output_text", "text":
			d.appendText(p.text)
		case "refusal":
			d.appendText(p.refusal)
		case "input_image":
			if role != "user" {
				return errorf("input[%d]: input_image is only supported in user messages", i)
			}
			if emptyStr(p.imageURL) {
				if !emptyStr(p.fileID) {
					return errorf("input[%d]: input_image with file_id is not supported; send image_url (URL or data URL)", i)
				}
				return errorf("input[%d]: input_image requires image_url", i)
			}
			detail := p.detail
			if detail == "auto" {
				detail = ""
			}
			d.segs = append(d.segs, chatPart{image: true, url: p.imageURL, detail: detail})
		case "input_file":
			return errorf("input[%d]: input_file content is not supported", i)
		default:
			return errorf("input[%d]: unsupported content part type %q", i, p.typ)
		}
	}

	d.beginMsg(role)
	segs := d.segs
	switch {
	case len(segs) == 0:
		d.out = append(d.out, `""`...)
	case len(segs) == 1 && !segs[0].image:
		d.out = jsonscan.AppendJoined(d.out, d.texts[segs[0].t0:segs[0].t1], "\n\n")
	default:
		d.out = append(d.out, '[')
		for k, s := range segs {
			if k > 0 {
				d.out = append(d.out, ',')
			}
			if !s.image {
				d.out = append(d.out, `{"type":"text","text":`...)
				d.out = jsonscan.AppendJoined(d.out, d.texts[s.t0:s.t1], "\n\n")
				d.out = append(d.out, '}')
				continue
			}
			d.out = append(d.out, `{"type":"image_url","image_url":{"url":`...)
			d.out = jsonscan.AppendStrLit(d.out, s.url)
			if s.detail != "" {
				d.out = append(d.out, `,"detail":`...)
				d.out = jsonscan.AppendQuoted(d.out, s.detail)
			}
			d.out = append(d.out, "}}"...)
		}
		d.out = append(d.out, ']')
	}
	if role == "assistant" {
		d.openAssistant()
	} else {
		d.endMsg()
	}
	return nil
}

// appendText adds a text, joining it to a directly preceding text part.
func (d *decoder) appendText(text []byte) {
	d.texts = append(d.texts, text)
	if n := len(d.segs); n > 0 && !d.segs[n-1].image {
		d.segs[n-1].t1 = len(d.texts)
		return
	}
	d.segs = append(d.segs, chatPart{t0: len(d.texts) - 1, t1: len(d.texts)})
}

// contentParts accepts a string or an array of content parts. The result
// is scratch space valid until the next call.
func (d *decoder) contentParts(raw []byte, i int) ([]inPart, error) {
	switch {
	case raw == nil || jsonscan.IsNull(raw):
		return nil, nil
	case raw[0] == '"':
		d.parts = append(d.parts[:0], inPart{typ: "input_text", text: raw})
		return d.parts, nil
	}
	parts, ok := jsonscan.DecodeSlice(d.x, d.parts, raw, true, decodePart)
	if !ok {
		return nil, errorf("input[%d]: content must be a string or an array of content parts", i)
	}
	d.parts = parts
	return parts, nil
}

// toolOutputText returns the texts of a tool output (string or text parts),
// to be joined by newlines.
func (d *decoder) toolOutputText(raw []byte, i int) ([][]byte, error) {
	switch {
	case raw == nil || jsonscan.IsNull(raw):
		return nil, nil
	case raw[0] == '"':
		return append(d.texts[:0], raw), nil
	}
	parts, ok := jsonscan.DecodeSlice(d.x, d.parts, raw, true, decodePart)
	if !ok {
		return nil, errorf("input[%d]: output must be a string or an array of content parts", i)
	}
	d.parts = parts
	texts := d.texts[:0]
	for k := range parts {
		switch parts[k].typ {
		case "input_text", "output_text", "text":
			texts = append(texts, parts[k].text)
		default:
			return nil, errorf("input[%d]: unsupported content part type %q in tool output", i, parts[k].typ)
		}
	}
	d.texts = texts
	return texts, nil
}

// fieldEnum is jsonscan.FieldStr for fields with a small set of common values,
// which are returned without allocating.
func fieldEnum(v []byte, dst *string) bool {
	switch string(v) {
	case `"function"`:
		*dst = "function"
	case `"custom"`:
		*dst = "custom"
	case `"namespace"`:
		*dst = "namespace"
	case `"message"`:
		*dst = "message"
	case `"user"`:
		*dst = "user"
	case `"assistant"`:
		*dst = "assistant"
	case `"developer"`:
		*dst = "developer"
	case `"system"`:
		*dst = "system"
	case `"input_text"`:
		*dst = "input_text"
	case `"output_text"`:
		*dst = "output_text"
	case `"function_call"`:
		*dst = "function_call"
	case `"function_call_output"`:
		*dst = "function_call_output"
	case `"custom_tool_call"`:
		*dst = "custom_tool_call"
	case `"custom_tool_call_output"`:
		*dst = "custom_tool_call_output"
	case `"reasoning"`:
		*dst = "reasoning"
	case `"input_image"`:
		*dst = "input_image"
	case `"auto"`:
		*dst = "auto"
	default:
		return jsonscan.FieldStr(v, dst)
	}
	return true
}

// addTool registers a tool in d.info.Tools, sizing its maps for hint tools
// when they are created.
func (d *decoder) addTool(hint int, ref toolRef) string {
	if d.info.Tools.byChat == nil {
		d.info.Tools.byChat = make(map[string]toolRef, hint)
		d.info.Tools.byOrig = make(map[toolKey]string, hint)
	}
	return d.info.Tools.add(ref)
}
