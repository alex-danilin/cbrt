package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Chat Completions response types (non-stream and stream).

type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type chatRespToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type chatRespMessage struct {
	Content          json.RawMessage    `json:"content"`
	Refusal          *string            `json:"refusal"`
	ReasoningContent *string            `json:"reasoning_content"`
	Reasoning        *string            `json:"reasoning"`
	ToolCalls        []chatRespToolCall `json:"tool_calls"`
}

type chatResponse struct {
	Choices []struct {
		Index        int             `json:"index"`
		Message      chatRespMessage `json:"message"`
		FinishReason *string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage      `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// Anthropic response types.

type outUsage struct {
	InputTokens  int  `json:"input_tokens"`
	CacheRead    *int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens int  `json:"output_tokens"`
}

type outMessage struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	Role         string   `json:"role"`
	Model        string   `json:"model"`
	Content      []any    `json:"content"`
	StopReason   *string  `json:"stop_reason"`
	StopSequence *string  `json:"stop_sequence"`
	Usage        outUsage `json:"usage"`
}

type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// EncodeResponse converts a non-streaming Chat Completions response body into
// an Anthropic Message JSON. It returns an error if the body is malformed or
// a tool call's arguments are not a valid JSON object.
func EncodeResponse(chat []byte, model string, exposeReasoning bool) ([]byte, error) {
	var resp chatResponse
	if err := json.Unmarshal(chat, &resp); err != nil {
		return nil, fmt.Errorf("invalid upstream response: %w", err)
	}
	if !isNull(resp.Error) {
		return nil, fmt.Errorf("upstream error: %s", compactJSON(resp.Error))
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("upstream response has no choices")
	}
	ch := resp.Choices[0]
	msg := ch.Message

	text, err := contentText(msg.Content)
	if err != nil {
		return nil, err
	}
	reasoning := pickReasoning(msg.ReasoningContent, msg.Reasoning)

	var content []any
	if exposeReasoning && reasoning != "" {
		content = append(content, thinkingBlock{"thinking", reasoning, ""})
	}
	if text != "" {
		content = append(content, textBlock{"text", text})
	}
	// Anthropic has no refusal block: an upstream refusal is sent as text.
	if msg.Refusal != nil && *msg.Refusal != "" {
		content = append(content, textBlock{"text", *msg.Refusal})
		text += *msg.Refusal
	}
	size := len(text) + len(reasoning)
	for _, tc := range msg.ToolCalls {
		args, err := argString(tc.Function.Arguments)
		if err != nil {
			return nil, err
		}
		input, err := parseArgs(tc.Function.Name, args)
		if err != nil {
			return nil, err
		}
		size += len(args)
		id := tc.ID
		if id == "" {
			id = randomID("toolu_")
		}
		content = append(content, toolUseBlock{"tool_use", id, tc.Function.Name, input})
	}
	hasTool := len(msg.ToolCalls) > 0
	if text == "" && !hasTool {
		content = append(content, textBlock{"text", ""})
	}

	finish := ""
	if ch.FinishReason != nil {
		finish = *ch.FinishReason
	}
	stop := stopReason(finish, hasTool)
	out := outMessage{
		ID: randomID("msg_"), Type: "message", Role: "assistant", Model: model,
		Content: content, StopReason: &stop,
		Usage: convertUsage(resp.Usage, 0, estimateTokens(size)),
	}
	return marshal(out), nil
}

// contentText accepts string, null, or an array of text parts.
func contentText(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("upstream message content is neither a string nor text parts")
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// pickReasoning prefers reasoning_content; some servers send both fields
// with the same text.
func pickReasoning(reasoningContent, reasoning *string) string {
	if reasoningContent != nil && *reasoningContent != "" {
		return *reasoningContent
	}
	if reasoning != nil {
		return *reasoning
	}
	return ""
}

// argString accepts arguments as a JSON string (standard) or an object.
func argString(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '{' {
		return string(t), nil
	}
	return "", errors.New("upstream tool call arguments are neither a string nor an object")
}

// parseArgs validates tool arguments as a JSON object; empty means {}.
func parseArgs(name, args string) (json.RawMessage, error) {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("tool call %q: arguments are not a valid JSON object", name)
	}
	return compactJSON([]byte(args)), nil
}

func stopReason(finish string, hasTool bool) string {
	switch {
	case hasTool || finish == "tool_calls" || finish == "function_call":
		return "tool_use"
	case finish == "length":
		return "max_tokens"
	}
	return "end_turn"
}

// convertUsage maps Chat usage to Anthropic usage. As in the Anthropic API,
// input_tokens excludes cached tokens, which are reported separately.
func convertUsage(u *chatUsage, inputEstimate, outputEstimate int) outUsage {
	if u == nil {
		return outUsage{InputTokens: inputEstimate, OutputTokens: outputEstimate}
	}
	out := outUsage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if d := u.PromptTokensDetails; d != nil && d.CachedTokens != nil {
		cached := min(max(*d.CachedTokens, 0), u.PromptTokens)
		out.CacheRead = &cached
		out.InputTokens -= cached
	}
	return out
}

func estimateTokens(bytes int) int { return (bytes + 3) / 4 }

// compactJSON compacts valid JSON; invalid input is returned unchanged.
func compactJSON(raw []byte) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return json.RawMessage(raw)
	}
	return json.RawMessage(buf.Bytes())
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
