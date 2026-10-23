package responses

// This file is a verbatim copy (identifiers prefixed with "ref") of the
// request decoder as it was before the allocation-free rewrite. It is the
// oracle for the equivalence tests and FuzzDecodeEquivalence; do not
// change its behaviour.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// refMaxGrammar caps the custom-tool grammar copied into a tool description.
const refMaxGrammar = 4 << 10

// refCustomInputSchema is the parameters schema of a flattened custom tool.
const refCustomInputSchema = `{"type":"object","properties":{"input":{"type":"string","description":"The raw input for this tool, as plain text (not JSON-encoded)."}},"required":["input"]}`

// refBuiltinCalls are built-in tool call history items; they cannot be
// represented in Chat Completions and are rejected.
var refBuiltinCalls = map[string]bool{
	"web_search_call": true, "file_search_call": true, "computer_call": true, "computer_call_output": true,
	"local_shell_call": true, "local_shell_call_output": true, "image_generation_call": true,
	"code_interpreter_call": true, "mcp_call": true, "mcp_list_tools": true, "mcp_approval_request": true,
	"mcp_approval_response": true, "shell_call": true, "shell_call_output": true,
	"apply_patch_call": true, "apply_patch_call_output": true, "tool_search_call": true, "tool_search_output": true,
}

// Chat Completions request types.

type refChatRequest struct {
	Model             string            `json:"model"`
	Messages          []*refChatMessage `json:"messages"`
	Stream            bool              `json:"stream"`
	MaxTokens         *int              `json:"max_tokens,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	Tools             []refChatTool     `json:"tools,omitempty"`
	ToolChoice        any               `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    json.RawMessage   `json:"response_format,omitempty"`
}

type refChatMessage struct {
	Role       string            `json:"role"`
	Content    any               `json:"content"` // string, []refChatPart or nil
	ToolCalls  []refChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

type refChatPart struct {
	Type     string       `json:"type"`
	Text     *string      `json:"text,omitempty"`
	ImageURL *refImageURL `json:"image_url,omitempty"`
}

type refImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type refChatToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function refChatFunction `json:"function"`
}

type refChatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type refChatTool struct {
	Type     string         `json:"type"`
	Function refChatToolDef `json:"function"`
}

type refChatToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Responses request types.

type refInItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"`
	Input     *string         `json:"input"`
	Output    json.RawMessage `json:"output"`
}

type refInPart struct {
	Type     string  `json:"type"`
	Text     string  `json:"text"`
	Refusal  string  `json:"refusal"`
	ImageURL *string `json:"image_url"`
	FileID   string  `json:"file_id"`
	Detail   string  `json:"detail"`
}

type refInTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Format      json.RawMessage `json:"format"`
	Tools       []refInTool     `json:"tools"`
	Strict      json.RawMessage `json:"strict"` // behaviour change: strict:true is reported

}

type refDecoder struct {
	info RequestInfo
	seen map[string]bool
}

func (d *refDecoder) downgrade(what string) {
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	if !d.seen[what] {
		d.seen[what] = true
		d.info.Downgraded = append(d.info.Downgraded, what)
	}
}

// refDecodeRequest converts an OpenAI Responses request body into an OpenAI
// Chat Completions request body. It returns *Error for content that cannot
// be represented faithfully.
func refDecodeRequest(body []byte) ([]byte, RequestInfo, error) {
	d := &refDecoder{}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, d.info, errorf("invalid request body: must be a JSON object")
	}
	req := refChatRequest{}

	if err := json.Unmarshal(top["model"], &req.Model); err != nil || req.Model == "" {
		return nil, d.info, errorf("model: must be a non-empty string")
	}
	d.info.Model = req.Model

	if !isNull(top["stream"]) {
		if err := json.Unmarshal(top["stream"], &req.Stream); err != nil {
			return nil, d.info, errorf("stream: must be a boolean")
		}
	}
	d.info.Stream = req.Stream

	if !isNull(top["previous_response_id"]) {
		var id string
		if err := json.Unmarshal(top["previous_response_id"], &id); err != nil || id != "" {
			return nil, d.info, errorf("previous_response_id is not supported: this gateway is stateless, send the full conversation in input")
		}
	}

	if raw := top["max_output_tokens"]; !isNull(raw) {
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n < 1 || n != math.Trunc(n) || n > math.MaxInt32 {
			return nil, d.info, errorf("max_output_tokens: must be a positive integer")
		}
		v := int(n)
		req.MaxTokens = &v
	}
	for _, k := range []string{"temperature", "top_p"} {
		if isNull(top[k]) {
			continue
		}
		var f float64
		if err := json.Unmarshal(top[k], &f); err != nil {
			return nil, d.info, errorf("%s: must be a number", k)
		}
		if k == "temperature" {
			req.Temperature = &f
		} else {
			req.TopP = &f
		}
	}

	// Tools first: history items need the name mapping.
	if !isNull(top["tools"]) {
		tools, err := d.tools(top["tools"])
		if err != nil {
			return nil, d.info, err
		}
		req.Tools = tools
	}

	if raw := top["instructions"]; !isNull(raw) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, d.info, errorf("instructions: must be a string")
		}
		if s != "" {
			req.Messages = append(req.Messages, &refChatMessage{Role: "system", Content: s})
		}
	}

	raw := top["input"]
	if isNull(raw) {
		return nil, d.info, errorf("input: is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, d.info, errorf("input: must not be empty")
		}
		req.Messages = append(req.Messages, &refChatMessage{Role: "user", Content: s})
	} else {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, d.info, errorf("input: must be a string or an array of input items")
		}
		if len(items) == 0 {
			return nil, d.info, errorf("input: must not be empty")
		}
		msgs, err := d.items(items)
		if err != nil {
			return nil, d.info, err
		}
		req.Messages = append(req.Messages, msgs...)
	}

	if len(req.Tools) > 0 {
		if !isNull(top["tool_choice"]) {
			choice, err := d.toolChoice(top["tool_choice"])
			if err != nil {
				return nil, d.info, err
			}
			req.ToolChoice = choice
		}
		if !isNull(top["parallel_tool_calls"]) {
			var b bool
			if err := json.Unmarshal(top["parallel_tool_calls"], &b); err != nil {
				return nil, d.info, errorf("parallel_tool_calls: must be a boolean")
			}
			req.ParallelToolCalls = &b
		}
	}

	if raw := top["store"]; !isNull(raw) {
		var b bool
		if json.Unmarshal(raw, &b) == nil && b {
			d.downgrade("store")
		}
	}
	if raw := top["reasoning"]; !isNull(raw) {
		d.downgrade("reasoning")
		var r struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(raw, &r) == nil {
			d.info.ReasoningEffort = r.Effort
		}
	}
	if raw := top["text"]; !isNull(raw) {
		rf, err := d.text(raw)
		if err != nil {
			return nil, d.info, err
		}
		req.ResponseFormat = rf
	}

	handled := map[string]bool{
		"model": true, "stream": true, "previous_response_id": true, "max_output_tokens": true,
		"temperature": true, "top_p": true, "tools": true, "instructions": true, "input": true,
		"tool_choice": true, "parallel_tool_calls": true, "store": true, "reasoning": true, "text": true,
	}
	for _, k := range sortedKeys(top) {
		if handled[k] || isNull(top[k]) {
			continue
		}
		// include, prompt_cache_key, client_metadata, metadata, truncation,
		// service_tier, user, top_logprobs, background, and anything
		// unrecognized are dropped and reported by name.
		d.downgrade(k)
	}

	return marshal(req), d.info, nil
}

// text maps text.format to response_format; verbosity is downgraded.
func (d *refDecoder) text(raw json.RawMessage) (json.RawMessage, error) {
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
// namespaced ones so that they keep their names on collision.
func (d *refDecoder) tools(raw json.RawMessage) ([]refChatTool, error) {
	var list []refInTool
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errorf("tools: must be an array of tool definitions")
	}
	names := make([][]string, len(list))
	for i, t := range list {
		if t.Type != "function" && t.Type != "custom" {
			continue
		}
		if t.Name == "" {
			return nil, errorf("tools[%d]: name is required", i)
		}
		n := d.info.Tools.add(toolRef{name: t.Name, custom: t.Type == "custom"})
		if n == "" {
			return nil, errorf("tools[%d]: duplicate tool name %q", i, t.Name)
		}
		names[i] = []string{n}
	}
	for i, t := range list {
		if t.Type != "namespace" {
			continue
		}
		if t.Name == "" {
			return nil, errorf("tools[%d]: namespace name is required", i)
		}
		for j, nt := range t.Tools {
			if nt.Type != "function" && nt.Type != "custom" {
				d.downgrade("tool:" + nt.Type)
				names[i] = append(names[i], "")
				continue
			}
			if nt.Name == "" {
				return nil, errorf("tools[%d].tools[%d]: name is required", i, j)
			}
			n := d.info.Tools.add(toolRef{namespace: t.Name, name: nt.Name, custom: nt.Type == "custom"})
			if n == "" {
				return nil, errorf("tools[%d].tools[%d]: duplicate tool name %q in namespace %q", i, j, nt.Name, t.Name)
			}
			names[i] = append(names[i], n)
		}
	}

	var out []refChatTool
	for i, t := range list {
		switch t.Type {
		case "function", "custom":
			ct, err := d.tool(t, names[i][0], "", fmt.Sprintf("tools[%d]", i))
			if err != nil {
				return nil, err
			}
			out = append(out, ct)
		case "namespace":
			for j, nt := range t.Tools {
				if names[i][j] == "" {
					continue
				}
				// The namespace description is given once, on its first tool,
				// to keep large app namespaces from multiplying prompt size.
				desc := ""
				if j == 0 && t.Description != "" {
					desc = "(" + t.Name + ") " + t.Description
				}
				ct, err := d.tool(nt, names[i][j], desc, fmt.Sprintf("tools[%d].tools[%d]", i, j))
				if err != nil {
					return nil, err
				}
				out = append(out, ct)
			}
		default:
			d.downgrade("tool:" + t.Type)
		}
	}
	return out, nil
}

// tool converts one function or custom tool. nsDesc, if set, is the
// description of the enclosing namespace and is prepended.
func (d *refDecoder) tool(t refInTool, name, nsDesc, where string) (refChatTool, error) {
	if t.Type == "function" && string(t.Strict) == "true" {
		d.downgrade("tools.strict") // intentional change after the rewrite (review #11)
	}
	desc := t.Description
	if nsDesc != "" {
		desc = strings.TrimSpace(nsDesc + "\n\n" + desc)
	}
	if t.Type == "function" {
		params := json.RawMessage(`{"type":"object","properties":{}}`)
		if !isNull(t.Parameters) {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(t.Parameters, &obj); err != nil {
				return refChatTool{}, errorf("%s: parameters must be a JSON Schema object", where)
			}
			params = compactJSON(t.Parameters)
		}
		return refChatTool{Type: "function", Function: refChatToolDef{Name: name, Description: desc, Parameters: params}}, nil
	}
	// custom
	if !isNull(t.Format) {
		var f struct {
			Type       string `json:"type"`
			Syntax     string `json:"syntax"`
			Definition string `json:"definition"`
		}
		if err := json.Unmarshal(t.Format, &f); err != nil {
			return refChatTool{}, errorf("%s: format must be an object", where)
		}
		switch f.Type {
		case "", "text":
		case "grammar":
			d.downgrade("custom_tool_grammar")
			def := f.Definition
			if len(def) > refMaxGrammar {
				def = refTruncateUTF8(def, refMaxGrammar) + "\n…(truncated)"
			}
			syntax := f.Syntax
			if syntax == "" {
				syntax = "grammar"
			}
			desc = strings.TrimSpace(desc + "\n\nThe `input` argument is raw text that must conform to this " +
				syntax + " grammar:\n```\n" + def + "\n```")
		default:
			d.downgrade("custom_tool_format")
		}
	}
	return refChatTool{Type: "function", Function: refChatToolDef{Name: name, Description: desc, Parameters: json.RawMessage(refCustomInputSchema)}}, nil
}

func refTruncateUTF8(s string, n int) string {
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
func (d *refDecoder) toolChoice(raw json.RawMessage) (any, error) {
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

// items converts input items to chat messages in order.
func (d *refDecoder) items(items []json.RawMessage) ([]*refChatMessage, error) {
	var out []*refChatMessage
	// lastAssistant is the assistant message tool calls merge into; it is
	// reset by any other message.
	var lastAssistant *refChatMessage
	for i, raw := range items {
		where := fmt.Sprintf("input[%d]", i)
		var it refInItem
		if err := json.Unmarshal(raw, &it); err != nil {
			return nil, errorf("%s: must be an input item object", where)
		}
		typ := it.Type
		if typ == "" && it.Role != "" {
			typ = "message"
		}
		switch typ {
		case "message":
			m, err := d.message(it, where)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
			lastAssistant = nil
			if m.Role == "assistant" {
				lastAssistant = m
			}
		case "function_call", "custom_tool_call":
			if it.CallID == "" || it.Name == "" {
				return nil, errorf("%s: %s requires call_id and name", where, typ)
			}
			name := d.info.Tools.chatName(it.Namespace, it.Name)
			var args string
			if typ == "function_call" {
				a, err := refHistoryArgs(it.Arguments)
				if err != nil {
					return nil, errorf("%s: %v", where, err)
				}
				args = a
			} else {
				if it.Input == nil {
					return nil, errorf("%s: custom_tool_call requires input", where)
				}
				args = string(marshal(map[string]string{"input": *it.Input}))
			}
			if lastAssistant == nil {
				lastAssistant = &refChatMessage{Role: "assistant"}
				out = append(out, lastAssistant)
			}
			lastAssistant.ToolCalls = append(lastAssistant.ToolCalls, refChatToolCall{
				ID: it.CallID, Type: "function", Function: refChatFunction{Name: name, Arguments: args},
			})
		case "function_call_output", "custom_tool_call_output":
			if it.CallID == "" {
				return nil, errorf("%s: %s requires call_id", where, typ)
			}
			text, err := refToolOutputText(it.Output, where)
			if err != nil {
				return nil, err
			}
			out = append(out, &refChatMessage{Role: "tool", ToolCallID: it.CallID, Content: text})
			lastAssistant = nil
		case "reasoning":
			d.downgrade("reasoning_items")
		default:
			if refBuiltinCalls[typ] {
				return nil, errorf("%s: built-in tool call history item %q is not supported", where, typ)
			}
			return nil, errorf("%s: unsupported input item type %q", where, typ)
		}
	}
	return out, nil
}

// refHistoryArgs accepts arguments as a JSON string (standard) or an object.
func refHistoryArgs(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "{}", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "{}", nil
		}
		return s, nil
	}
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '{' {
		return string(compactJSON(t)), nil
	}
	return "", fmt.Errorf("function_call arguments must be a string")
}

// message converts a message item.
func (d *refDecoder) message(it refInItem, where string) (*refChatMessage, error) {
	role := it.Role
	switch role {
	case "user", "assistant", "system":
	case "developer":
		role = "system"
	case "":
		return nil, errorf("%s: message requires role", where)
	default:
		return nil, errorf("%s: unsupported role %q", where, it.Role)
	}
	parts, err := refContentParts(it.Content, where)
	if err != nil {
		return nil, err
	}
	var out []refChatPart
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			out = refAppendText(out, p.Text)
		case "refusal":
			out = refAppendText(out, p.Refusal)
		case "input_image":
			if role != "user" {
				return nil, errorf("%s: input_image is only supported in user messages", where)
			}
			if p.ImageURL == nil || *p.ImageURL == "" {
				if p.FileID != "" {
					return nil, errorf("%s: input_image with file_id is not supported; send image_url (URL or data URL)", where)
				}
				return nil, errorf("%s: input_image requires image_url", where)
			}
			detail := p.Detail
			if detail == "auto" {
				detail = ""
			}
			out = append(out, refChatPart{Type: "image_url", ImageURL: &refImageURL{URL: *p.ImageURL, Detail: detail}})
		case "input_file":
			return nil, errorf("%s: input_file content is not supported", where)
		default:
			return nil, errorf("%s: unsupported content part type %q", where, p.Type)
		}
	}
	return &refChatMessage{Role: role, Content: refPartsContent(out)}, nil
}

// refContentParts accepts a string or an array of content parts.
func refContentParts(raw json.RawMessage, where string) ([]refInPart, error) {
	if isNull(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []refInPart{{Type: "input_text", Text: s}}, nil
	}
	var parts []refInPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errorf("%s: content must be a string or an array of content parts", where)
	}
	return parts, nil
}

// refToolOutputText flattens a tool output (string or text parts).
func refToolOutputText(raw json.RawMessage, where string) (string, error) {
	if isNull(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []refInPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errorf("%s: output must be a string or an array of content parts", where)
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			texts = append(texts, p.Text)
		default:
			return "", errorf("%s: unsupported content part type %q in tool output", where, p.Type)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// refAppendText adds text, joining it to a directly preceding text part.
func refAppendText(parts []refChatPart, text string) []refChatPart {
	if n := len(parts); n > 0 && parts[n-1].Type == "text" {
		joined := *parts[n-1].Text + "\n\n" + text
		parts[n-1].Text = &joined
		return parts
	}
	return append(parts, refChatPart{Type: "text", Text: &text})
}

// refPartsContent uses a plain string when there are no images.
func refPartsContent(parts []refChatPart) any {
	switch {
	case len(parts) == 0:
		return ""
	case len(parts) == 1 && parts[0].Type == "text":
		return *parts[0].Text
	}
	return parts
}
