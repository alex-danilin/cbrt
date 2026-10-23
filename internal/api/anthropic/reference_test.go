package anthropic

// This file is a verbatim copy (identifiers prefixed with "ref") of the
// request decoder as it was before the allocation-free rewrite. It is the
// oracle for the equivalence tests and FuzzDecodeEquivalence; do not
// change its behaviour.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// refBillingPrefix marks Claude Code's pseudo system block.
const refBillingPrefix = "x-anthropic-billing-header:"

// Chat Completions request types.

type refChatRequest struct {
	Model             string           `json:"model"`
	Messages          []refChatMessage `json:"messages"`
	Stream            bool             `json:"stream"`
	MaxTokens         int              `json:"max_tokens"`
	Temperature       *float64         `json:"temperature,omitempty"`
	TopP              *float64         `json:"top_p,omitempty"`
	Stop              []string         `json:"stop,omitempty"`
	Tools             []refChatTool    `json:"tools,omitempty"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
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
	URL string `json:"url"`
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

// Anthropic request types.

type refInBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Source       *refImageSource `json:"source"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Input        json.RawMessage `json:"input"`
	ToolUseID    string          `json:"tool_use_id"`
	Content      json.RawMessage `json:"content"`
	IsError      bool            `json:"is_error"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type refImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
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

func (d *refDecoder) noteCache(raw json.RawMessage) {
	if !isNull(raw) {
		d.downgrade("cache_control")
	}
}

// refDecodeRequest converts an Anthropic Messages request body into an OpenAI
// Chat Completions request body. It returns *Error for content that cannot
// be represented faithfully.
func refDecodeRequest(body []byte) ([]byte, RequestInfo, error) {
	d := &refDecoder{}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, d.info, errorf("invalid request body: %v", err)
	}
	req := refChatRequest{}

	if err := json.Unmarshal(top["model"], &req.Model); err != nil || req.Model == "" {
		return nil, d.info, errorf("model: must be a non-empty string")
	}
	d.info.Model = req.Model

	var maxTokens float64
	if err := json.Unmarshal(top["max_tokens"], &maxTokens); err != nil || isNull(top["max_tokens"]) ||
		maxTokens < 1 || maxTokens != math.Trunc(maxTokens) || maxTokens > math.MaxInt32 {
		return nil, d.info, errorf("max_tokens: must be a positive integer")
	}
	req.MaxTokens = int(maxTokens)

	if !isNull(top["stream"]) {
		if err := json.Unmarshal(top["stream"], &req.Stream); err != nil {
			return nil, d.info, errorf("stream: must be a boolean")
		}
	}
	d.info.Stream = req.Stream

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
	if !isNull(top["stop_sequences"]) {
		if err := json.Unmarshal(top["stop_sequences"], &req.Stop); err != nil {
			return nil, d.info, errorf("stop_sequences: must be an array of strings")
		}
	}

	var msgs []inMessage
	if err := json.Unmarshal(top["messages"], &msgs); err != nil || len(msgs) == 0 {
		return nil, d.info, errorf("messages: must be a non-empty array")
	}

	if !isNull(top["system"]) {
		text, err := d.systemText(top["system"], "system")
		if err != nil {
			return nil, d.info, err
		}
		if text != "" {
			req.Messages = append(req.Messages, refChatMessage{Role: "system", Content: text})
		}
	}

	for i, m := range msgs {
		where := fmt.Sprintf("messages[%d]", i)
		var out []refChatMessage
		var err error
		switch m.Role {
		case "system":
			var text string
			text, err = d.systemText(m.Content, where)
			out = []refChatMessage{{Role: "system", Content: text}}
		case "user":
			out, err = d.userMessage(m.Content, where)
		case "assistant":
			out, err = d.assistantMessage(m.Content, where)
		default:
			err = errorf("%s: unsupported role %q", where, m.Role)
		}
		if err != nil {
			return nil, d.info, err
		}
		req.Messages = append(req.Messages, out...)
	}

	if !isNull(top["tools"]) {
		tools, err := d.tools(top["tools"])
		if err != nil {
			return nil, d.info, err
		}
		req.Tools = tools
	}
	if !isNull(top["tool_choice"]) {
		choice, noParallel, err := refToolChoice(top["tool_choice"])
		if err != nil {
			return nil, d.info, err
		}
		// A tool_choice without tools is rejected by most upstreams.
		if len(req.Tools) > 0 {
			req.ToolChoice = choice
			if noParallel {
				f := false
				req.ParallelToolCalls = &f
			}
		}
	}

	if raw, ok := top["output_config"]; ok && !isNull(raw) {
		d.downgrade("output_config")
		var oc struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(raw, &oc) == nil {
			d.info.ReasoningEffort = oc.Effort
		}
	}

	handled := map[string]bool{
		"model": true, "max_tokens": true, "stream": true, "temperature": true, "top_p": true,
		"stop_sequences": true, "messages": true, "system": true, "tools": true,
		"tool_choice": true, "output_config": true,
	}
	for _, k := range sortedKeys(top) {
		if handled[k] || isNull(top[k]) {
			continue
		}
		// Anthropic-only fields (thinking, metadata, context_management, top_k,
		// service_tier, container, mcp_servers, ...) and anything unrecognized
		// are dropped and reported by name.
		d.downgrade(k)
	}

	return marshal(req), d.info, nil
}

// systemText flattens a system prompt (string or text blocks), dropping
// Claude Code's billing pseudo-header block.
func (d *refDecoder) systemText(raw json.RawMessage, where string) (string, error) {
	blocks, err := refParseContent(raw, where)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, b := range blocks {
		d.noteCache(b.CacheControl)
		if b.Type != "text" {
			return "", errorf("%s: unsupported content block type %q in system content", where, b.Type)
		}
		if strings.HasPrefix(b.Text, refBillingPrefix) {
			d.downgrade("billing_header")
			continue
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n"), nil
}

// refParseContent accepts a string or an array of blocks.
func refParseContent(raw json.RawMessage, where string) ([]refInBlock, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []refInBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []refInBlock
	if err := json.Unmarshal(raw, &blocks); err != nil || blocks == nil {
		return nil, errorf("%s: content must be a string or an array of content blocks", where)
	}
	return blocks, nil
}

// userMessage splits a user message 1:N: one tool message per tool_result,
// then one user message with the remaining text and images.
func (d *refDecoder) userMessage(raw json.RawMessage, where string) ([]refChatMessage, error) {
	blocks, err := refParseContent(raw, where)
	if err != nil {
		return nil, err
	}
	var out []refChatMessage
	var parts, trailing []refChatPart
	for _, b := range blocks {
		d.noteCache(b.CacheControl)
		switch b.Type {
		case "text":
			parts = refAppendText(parts, b.Text)
		case "image":
			p, err := refImagePart(b.Source, where)
			if err != nil {
				return nil, err
			}
			parts = append(parts, p)
		case "tool_result":
			if b.ToolUseID == "" {
				return nil, errorf("%s: tool_result.tool_use_id is required", where)
			}
			text, imgs, err := d.toolResult(b, where)
			if err != nil {
				return nil, err
			}
			out = append(out, refChatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: text})
			trailing = append(trailing, imgs...)
		case "thinking", "redacted_thinking":
			d.downgrade("thinking")
		default:
			return nil, errorf("%s: unsupported content block type %q in user message", where, b.Type)
		}
	}
	parts = append(parts, trailing...)
	if len(parts) > 0 || len(out) == 0 {
		out = append(out, refChatMessage{Role: "user", Content: refPartsContent(parts)})
	}
	return out, nil
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

func refImagePart(src *refImageSource, where string) (refChatPart, error) {
	if src == nil {
		return refChatPart{}, errorf("%s: image.source is required", where)
	}
	switch src.Type {
	case "base64":
		if src.MediaType == "" || src.Data == "" {
			return refChatPart{}, errorf("%s: base64 image requires media_type and data", where)
		}
		return refChatPart{Type: "image_url", ImageURL: &refImageURL{URL: "data:" + src.MediaType + ";base64," + src.Data}}, nil
	case "url":
		if src.URL == "" {
			return refChatPart{}, errorf("%s: url image requires url", where)
		}
		return refChatPart{Type: "image_url", ImageURL: &refImageURL{URL: src.URL}}, nil
	}
	return refChatPart{}, errorf("%s: unsupported image source type %q", where, src.Type)
}

// toolResult flattens tool_result content into text plus image parts.
func (d *refDecoder) toolResult(b refInBlock, where string) (string, []refChatPart, error) {
	var texts []string
	var imgs []refChatPart
	if !isNull(b.Content) {
		blocks, err := refParseContent(b.Content, where)
		if err != nil {
			return "", nil, err
		}
		for _, c := range blocks {
			d.noteCache(c.CacheControl)
			switch c.Type {
			case "text":
				texts = append(texts, c.Text)
			case "image":
				p, err := refImagePart(c.Source, where)
				if err != nil {
					return "", nil, err
				}
				imgs = append(imgs, p)
			default:
				return "", nil, errorf("%s: unsupported content block type %q in tool_result", where, c.Type)
			}
		}
	}
	text := strings.Join(texts, "\n")
	if b.IsError {
		text = "Error: " + text
	}
	return text, imgs, nil
}

// assistantMessage maps text and tool_use blocks to one assistant message.
func (d *refDecoder) assistantMessage(raw json.RawMessage, where string) ([]refChatMessage, error) {
	blocks, err := refParseContent(raw, where)
	if err != nil {
		return nil, err
	}
	var texts []string
	var calls []refChatToolCall
	for _, b := range blocks {
		d.noteCache(b.CacheControl)
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			if b.ID == "" || b.Name == "" {
				return nil, errorf("%s: tool_use requires id and name", where)
			}
			args := "{}"
			if !isNull(b.Input) {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(b.Input, &obj); err != nil {
					return nil, errorf("%s: tool_use.input must be an object", where)
				}
				args = string(compactJSON(b.Input))
			}
			calls = append(calls, refChatToolCall{ID: b.ID, Type: "function", Function: refChatFunction{Name: b.Name, Arguments: args}})
		case "thinking", "redacted_thinking":
			d.downgrade("thinking")
		default:
			return nil, errorf("%s: unsupported content block type %q in assistant message", where, b.Type)
		}
	}
	msg := refChatMessage{Role: "assistant", ToolCalls: calls}
	switch {
	case len(texts) > 0:
		msg.Content = strings.Join(texts, "\n\n")
	case len(calls) == 0:
		msg.Content = ""
	}
	return []refChatMessage{msg}, nil
}

// tools keeps custom tools and drops Anthropic built-in tools.
func (d *refDecoder) tools(raw json.RawMessage) ([]refChatTool, error) {
	var list []struct {
		Type         string          `json:"type"`
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		InputSchema  json.RawMessage `json:"input_schema"`
		CacheControl json.RawMessage `json:"cache_control"`
		Strict       json.RawMessage `json:"strict"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errorf("tools: must be an array of tool definitions")
	}
	var out []refChatTool
	for i, t := range list {
		d.noteCache(t.CacheControl)
		if t.Type != "" && t.Type != "custom" {
			d.downgrade("tool:" + t.Type)
			continue
		}
		if string(t.Strict) == "true" {
			d.downgrade("tools.strict") // intentional change after the rewrite (review #11)
		}
		if t.Name == "" {
			return nil, errorf("tools[%d]: name is required", i)
		}
		if isNull(t.InputSchema) {
			return nil, errorf("tools[%d]: input_schema is required", i)
		}
		out = append(out, refChatTool{Type: "function", Function: refChatToolDef{
			Name: t.Name, Description: t.Description, Parameters: compactJSON(t.InputSchema),
		}})
	}
	return out, nil
}

// refToolChoice maps an Anthropic tool_choice; noParallel reports
// disable_parallel_tool_use.
func refToolChoice(raw json.RawMessage) (choice any, noParallel bool, err error) {
	var tc struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, false, errorf("tool_choice: must be an object")
	}
	switch tc.Type {
	case "auto":
		choice = "auto"
	case "any":
		choice = "required"
	case "none":
		choice = "none"
	case "tool":
		if tc.Name == "" {
			return nil, false, errorf("tool_choice: name is required for type \"tool\"")
		}
		choice = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
	default:
		return nil, false, errorf("tool_choice: unsupported type %q", tc.Type)
	}
	return choice, tc.DisableParallelToolUse, nil
}

// refImageTokens is the fixed per-image estimate used by refCountTokens.
const refImageTokens = 1500

// refCountTokens estimates the input tokens of an Anthropic Messages request
// (for POST /v1/messages/count_tokens) as about 4 bytes per token over the
// system prompt, tool definitions and message contents, plus a fixed
// estimate per image.
func refCountTokens(body []byte) (int, error) {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []inMessage     `json:"messages"`
		Tools    []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 0, errorf("invalid request body: %v", err)
	}
	var c refCounter
	c.content(req.System)
	for _, t := range req.Tools {
		c.bytes += len(t.Name) + len(t.Description) + len(compactJSON(t.InputSchema))
	}
	for _, m := range req.Messages {
		c.bytes += 16 // role and message framing
		c.content(m.Content)
	}
	return estimateTokens(c.bytes) + c.images*refImageTokens, nil
}

type refCounter struct{ bytes, images int }

// content counts a string or an array of blocks; unknown shapes are ignored.
func (c *refCounter) content(raw json.RawMessage) {
	if isNull(raw) {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		c.bytes += len(s)
		return
	}
	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		Content  json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "image":
			c.images++
		case "thinking", "redacted_thinking":
			// Dropped before the request goes upstream.
		default:
			c.bytes += len(b.Text) + len(b.Name)
			if !isNull(b.Input) {
				c.bytes += len(compactJSON(b.Input))
			}
			c.content(b.Content)
		}
	}
}
