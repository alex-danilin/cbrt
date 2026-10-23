package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Chat Completions response types (non-stream and stream).

type chatUsage struct {
	PromptTokens        int  `json:"prompt_tokens"`
	CompletionTokens    int  `json:"completion_tokens"`
	TotalTokens         *int `json:"total_tokens,omitempty"`
	PromptTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
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

// Responses output types.

type outUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

type outResponse struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Error             any                `json:"error"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
	Model             string             `json:"model"`
	Output            []any              `json:"output"`
	ParallelToolCalls bool               `json:"parallel_tool_calls"`
	ToolChoice        string             `json:"tool_choice"`
	Tools             []any              `json:"tools"`
	Usage             *outUsage          `json:"usage"`
}

type summaryText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type reasoningItem struct {
	Type    string        `json:"type"`
	ID      string        `json:"id"`
	Summary []summaryText `json:"summary"`
}

type outputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type refusalPart struct {
	Type    string `json:"type"`
	Refusal string `json:"refusal"`
}

type messageItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Status  string `json:"status"`
	Role    string `json:"role"`
	Content []any  `json:"content"` // outputText and refusalPart
}

// newPart returns a message content part of kind partText or partRefusal.
func newPart(kind, text string) any {
	if kind == partRefusal {
		return refusalPart{Type: "refusal", Refusal: text}
	}
	return outputText{Type: "output_text", Text: text, Annotations: []any{}}
}

type functionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

type customCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Input     string `json:"input"`
	Status    string `json:"status"`
}

func newResponse(id, model string, created int64) outResponse {
	return outResponse{
		ID: id, Object: "response", CreatedAt: created, Status: "in_progress", Model: model,
		Output: []any{}, ParallelToolCalls: true, ToolChoice: "auto", Tools: []any{},
	}
}

// finishStatus maps a Chat finish_reason to the response status.
func finishStatus(finish string) (string, *incompleteDetails) {
	switch finish {
	case "length":
		return "incomplete", &incompleteDetails{"max_output_tokens"}
	case "content_filter":
		return "incomplete", &incompleteDetails{"content_filter"}
	}
	return "completed", nil
}

// newMessage returns a completed message with a text part (also when both
// are empty) and a refusal part if refusal is set.
func newMessage(id, text, refusal string) messageItem {
	var content []any
	if text != "" || refusal == "" {
		content = append(content, newPart(partText, text))
	}
	if refusal != "" {
		content = append(content, newPart(partRefusal, refusal))
	}
	return messageItem{Type: "message", ID: id, Status: "completed", Role: "assistant", Content: content}
}

func newReasoning(id, text string) reasoningItem {
	return reasoningItem{Type: "reasoning", ID: id, Summary: []summaryText{{"summary_text", text}}}
}

// toolItem builds the output item for a completed tool call.
func toolItem(tools ToolMap, callID, chatName, args string) (any, error) {
	ref := tools.lookup(chatName)
	if callID == "" {
		callID = randomID("call_")
	}
	if ref.custom {
		input, err := customInput(ref.name, args)
		if err != nil {
			return nil, err
		}
		return customCallItem{Type: "custom_tool_call", ID: randomID("ctc_"), CallID: callID,
			Name: ref.name, Namespace: ref.namespace, Input: input, Status: "completed"}, nil
	}
	a, err := functionArgs(ref.name, args)
	if err != nil {
		return nil, err
	}
	return functionCallItem{Type: "function_call", ID: randomID("fc_"), CallID: callID,
		Name: ref.name, Namespace: ref.namespace, Arguments: a, Status: "completed"}, nil
}

// EncodeResponse converts a non-streaming Chat Completions response body into
// a Responses response object. It returns an error if the body is malformed
// or a tool call's arguments are invalid.
func EncodeResponse(chat []byte, model string, tools ToolMap, exposeReasoning bool) ([]byte, error) {
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

	out := newResponse(randomID("resp_"), model, time.Now().Unix())
	if exposeReasoning && reasoning != "" {
		out.Output = append(out.Output, newReasoning(randomID("rs_"), reasoning))
	}
	refusal := ""
	if msg.Refusal != nil {
		refusal = *msg.Refusal
	}
	if text != "" || refusal != "" {
		out.Output = append(out.Output, newMessage(randomID("msg_"), text, refusal))
	}
	size := len(text) + len(reasoning) + len(refusal)
	for _, tc := range msg.ToolCalls {
		args, err := argString(tc.Function.Arguments)
		if err != nil {
			return nil, err
		}
		if tc.Function.Name == "" {
			return nil, errors.New("upstream tool call has no function name")
		}
		size += len(args)
		item, err := toolItem(tools, tc.ID, tc.Function.Name, args)
		if err != nil {
			return nil, err
		}
		out.Output = append(out.Output, item)
	}
	if text == "" && refusal == "" && len(msg.ToolCalls) == 0 {
		out.Output = append(out.Output, newMessage(randomID("msg_"), "", ""))
	}

	finish := ""
	if ch.FinishReason != nil {
		finish = *ch.FinishReason
	}
	out.Status, out.IncompleteDetails = finishStatus(finish)
	u := convertUsage(resp.Usage, 0, estimateTokens(size))
	out.Usage = &u
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

// functionArgs validates function arguments as a JSON object; empty means {}.
func functionArgs(name, args string) (string, error) {
	if strings.TrimSpace(args) == "" {
		return "{}", nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err != nil || obj == nil {
		return "", fmt.Errorf("tool call %q: arguments are not a valid JSON object", name)
	}
	return args, nil
}

// customInput unwraps the {"input": "..."} arguments of a flattened custom
// tool. A JSON string, or text that is not a JSON object, is taken as the
// input itself (small models sometimes skip the wrapper).
func customInput(name, args string) (string, error) {
	t := strings.TrimSpace(args)
	if t == "" {
		return "", fmt.Errorf("custom tool call %q: missing input", name)
	}
	if t[0] == '{' {
		var obj struct {
			Input *string `json:"input"`
		}
		if err := json.Unmarshal([]byte(t), &obj); err != nil || obj.Input == nil {
			return "", fmt.Errorf("custom tool call %q: arguments must be a JSON object with a string \"input\"", name)
		}
		return *obj.Input, nil
	}
	var s string
	if t[0] == '"' && json.Unmarshal([]byte(t), &s) == nil {
		return s, nil
	}
	return args, nil
}

// convertUsage maps Chat usage to Responses usage. As in the Responses API,
// input_tokens includes cached tokens.
func convertUsage(u *chatUsage, inputEstimate, outputEstimate int) outUsage {
	var out outUsage
	if u == nil {
		out.InputTokens, out.OutputTokens = inputEstimate, outputEstimate
		out.TotalTokens = inputEstimate + outputEstimate
		return out
	}
	out.InputTokens, out.OutputTokens = u.PromptTokens, u.CompletionTokens
	if d := u.PromptTokensDetails; d != nil && d.CachedTokens != nil {
		out.InputTokensDetails.CachedTokens = min(max(*d.CachedTokens, 0), u.PromptTokens)
	}
	if d := u.CompletionTokensDetails; d != nil && d.ReasoningTokens != nil {
		out.OutputTokensDetails.ReasoningTokens = max(*d.ReasoningTokens, 0)
	}
	out.TotalTokens = u.PromptTokens + u.CompletionTokens
	if u.TotalTokens != nil && *u.TotalTokens > 0 {
		out.TotalTokens = *u.TotalTokens
	}
	return out
}

func estimateTokens(bytes int) int { return (bytes + 3) / 4 }
