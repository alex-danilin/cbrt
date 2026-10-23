package gateway

import (
	"strings"
	"testing"
)

// gpt-oss sometimes leaks a chat-template token into a tool name; the gateway
// strips it (clients reject the name) and counts the repair as a downgrade.
func TestLeakedTemplateTokenStrippedFromToolName(t *testing.T) {
	const leaked = "exec_command<|channel|>commentary"
	// upstreams emit "<" literally or HTML-escaped as <
	escaped := strings.ReplaceAll(leaked, "<", string(rune(92))+"u003c")

	nonStream := `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"c1","type":"function","function":{"name":"` + leaked + `","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	streamBody := func(name string) string {
		return "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"" + name + "\",\"arguments\":\"\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n"
	}

	cases := []struct {
		name, req string
		up        string
		stream    bool
	}{
		{"non-stream", simple, nonStream, false},
		{"stream literal", streaming, streamBody(leaked), true},
		{"stream escaped", streaming, streamBody(escaped), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := status(200, tc.up)
			if tc.stream {
				up = sse(tc.up)
			}
			e := newEnv(t, "", newFake(t, up), newFake(t, okJSON("b")))
			resp, body := e.post(t, tc.req)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if !strings.Contains(body, `"name":"exec_command"`) || strings.Contains(body, "channel") {
				t.Fatalf("tool name not repaired:\n%s", body)
			}
			if d, _ := e.gw.Counters(); d != 1 {
				t.Errorf("downgraded = %d, want 1", d)
			}
		})
	}
}

func TestCleanToolNamesUntouched(t *testing.T) {
	e := newEnv(t, "", newFake(t, okJSON("text with <|token|> in content")), newFake(t, okJSON("b")))
	_, body := e.post(t, simple)
	if !strings.Contains(body, "<|token|>") {
		t.Fatalf("content must pass through unchanged: %s", body)
	}
	if d, _ := e.gw.Counters(); d != 0 {
		t.Errorf("downgraded = %d, want 0", d)
	}
}

// The repair runs on the upstream chat chunks, before the client API encoder,
// so Claude Code (Messages) and Codex (Responses) get the clean name too.
func TestLeakedToolNameRepairedForMessagesAndResponses(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"exec_command<|channel|>commentary\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	cases := []struct {
		name string
		url  func(*env, *testing.T) string
		body string
	}{
		{"messages", (*env).messagesURL, `{"model":"r","max_tokens":100,"stream":true,` +
			`"tools":[{"name":"exec_command","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", (*env).responsesURL, `{"model":"r","stream":true,"input":"hi",` +
			`"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "", newFake(t, sse(up)), newFake(t, okJSON("unused")))
			resp, out := postTo(t, tc.url(e, t), tc.body)
			if resp.StatusCode != 200 {
				t.Fatalf("%d %s", resp.StatusCode, out)
			}
			if !strings.Contains(out, `"name":"exec_command"`) || strings.Contains(out, "channel") {
				t.Fatalf("tool name not repaired:\n%s", out)
			}
		})
	}
}
