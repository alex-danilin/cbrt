package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/router"
)

// messagesURL starts a second test server in front of the same gateway that
// serves the Anthropic Messages API.
func (e *env) messagesURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.gw.ServeMessages(w, r, b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func postTo(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// toolCallsJSON is a non-stream Chat Completions answer carrying tool calls.
func toolCallsJSON(calls ...[3]string) http.HandlerFunc {
	var tcs []map[string]any
	for _, c := range calls {
		tcs = append(tcs, map[string]any{"id": c[0], "type": "function",
			"function": map[string]any{"name": c[1], "arguments": c[2]}})
	}
	b, _ := json.Marshal(map[string]any{
		"id": "x", "object": "chat.completion",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": tcs}}},
		"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 9, "total_tokens": 29},
	})
	return status(200, string(b))
}

// captureBody returns a Claude Code request body with fields overridden.
func captureBody(t *testing.T, name string, set map[string]any) string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/clients/claude-code-2.1.284/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var capt struct {
		RequestBody map[string]json.RawMessage `json:"request_body"`
	}
	if err := json.Unmarshal(raw, &capt); err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		capt.RequestBody[k], _ = json.Marshal(v)
	}
	b, _ := json.Marshal(capt.RequestBody)
	return string(b)
}

type msgEvent struct {
	name string
	data map[string]any
	raw  string
}

// parseSSE splits an Anthropic event stream; comment lines are skipped.
func parseSSE(t *testing.T, body string) []msgEvent {
	t.Helper()
	var out []msgEvent
	for _, block := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		var ev msgEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.raw = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev.name == "" && ev.raw == "" {
			continue
		}
		if err := json.Unmarshal([]byte(ev.raw), &ev.data); err != nil {
			t.Fatalf("event %q: data is not JSON: %q", ev.name, ev.raw)
		}
		out = append(out, ev)
	}
	return out
}

type toolUse struct {
	id, name string
	input    string
}

type decodedStream struct {
	text, thinking string
	tools          []toolUse
	stopReason     string
	outputTokens   float64
}

// checkGrammar asserts the Anthropic streaming grammar and reassembles the message.
func checkGrammar(t *testing.T, evs []msgEvent) decodedStream {
	t.Helper()
	var d decodedStream
	if len(evs) < 3 || evs[0].name != "message_start" {
		t.Fatalf("stream must start with message_start: %+v", evs)
	}
	if evs[len(evs)-1].name != "message_stop" || evs[len(evs)-2].name != "message_delta" {
		t.Fatalf("stream must end with message_delta, message_stop: %+v", evs[len(evs)-1])
	}
	next, open := 0, -1
	openType := ""
	for i, ev := range evs {
		if ev.data["type"] != ev.name {
			t.Errorf("event %d: name %q but data.type %v", i, ev.name, ev.data["type"])
		}
		idx := -1
		if f, ok := ev.data["index"].(float64); ok {
			idx = int(f)
		}
		switch ev.name {
		case "message_start":
			if i != 0 {
				t.Errorf("message_start at %d", i)
			}
		case "content_block_start":
			if open != -1 || idx != next {
				t.Fatalf("event %d: block %d started while %d open (next %d)", i, idx, open, next)
			}
			open, next = idx, next+1
			cb := ev.data["content_block"].(map[string]any)
			openType = cb["type"].(string)
			if openType == "tool_use" {
				d.tools = append(d.tools, toolUse{id: cb["id"].(string), name: cb["name"].(string)})
			}
		case "content_block_delta":
			if idx != open {
				t.Fatalf("event %d: delta for %d while %d open", i, idx, open)
			}
			delta := ev.data["delta"].(map[string]any)
			switch delta["type"] {
			case "text_delta":
				if openType != "text" {
					t.Errorf("text_delta in a %s block", openType)
				}
				d.text += delta["text"].(string)
			case "thinking_delta":
				if openType != "thinking" {
					t.Errorf("thinking_delta in a %s block", openType)
				}
				d.thinking += delta["thinking"].(string)
			case "input_json_delta":
				if openType != "tool_use" {
					t.Errorf("input_json_delta in a %s block", openType)
				}
				d.tools[len(d.tools)-1].input += delta["partial_json"].(string)
			default:
				t.Errorf("unknown delta %v", delta["type"])
			}
		case "content_block_stop":
			if idx != open {
				t.Fatalf("event %d: stop for %d while %d open", i, idx, open)
			}
			open = -1
		case "message_delta":
			if open != -1 {
				t.Errorf("message_delta with block %d still open", open)
			}
			d.stopReason, _ = ev.data["delta"].(map[string]any)["stop_reason"].(string)
			if u, ok := ev.data["usage"].(map[string]any); ok {
				d.outputTokens, _ = u["output_tokens"].(float64)
			}
		case "message_stop", "ping":
		default:
			t.Errorf("unexpected event %q", ev.name)
		}
	}
	if next == 0 {
		t.Error("message has no content blocks")
	}
	return d
}

type upstreamStream struct {
	text, reasoning  string
	args             map[int]string
	completionTokens float64
}

// readUpstream reassembles what an upstream fixture stream carries.
func readUpstream(t *testing.T, body string) upstreamStream {
	t.Helper()
	u := upstreamStream{args: map[int]string{}}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int `json:"index"`
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens float64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("fixture chunk: %v", err)
		}
		for _, ch := range c.Choices {
			u.text += ch.Delta.Content
			if ch.Delta.ReasoningContent != "" {
				u.reasoning += ch.Delta.ReasoningContent
			} else {
				u.reasoning += ch.Delta.Reasoning
			}
			for _, tc := range ch.Delta.ToolCalls {
				u.args[tc.Index] += tc.Function.Arguments
			}
		}
		if c.Usage != nil {
			u.completionTokens = c.Usage.CompletionTokens
		}
	}
	return u
}

func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// records reads the persisted attempt records (closes the store).
func (e *env) records(t *testing.T) []map[string]any {
	t.Helper()
	if err := e.store.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(e.dir, "attempts-*.jsonl"))
	var out []map[string]any
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil {
				out = append(out, m)
			}
		}
	}
	return out
}

func upstreamMessages(t *testing.T, f *fake) []map[string]any {
	t.Helper()
	raw, _ := f.lastBody(t)["messages"].([]any)
	out := make([]map[string]any, len(raw))
	for i, m := range raw {
		out[i] = m.(map[string]any)
	}
	return out
}

func TestMessagesNonStreamClaudeCodeCapture(t *testing.T) {
	a := newFake(t, okJSON("unused"))
	b := newFake(t, toolCallsJSON(
		[3]string{"call_1", "Glob", `{"pattern":"**/*"}`},
		[3]string{"call_2", "Read", `{"file_path":"notes.md"}`},
	))
	e := newEnv(t, "", a, b)
	body := captureBody(t, "0016-POST-v1_messages.json", map[string]any{"model": "claude-sonnet-4-5", "stream": false})
	resp, out := postTo(t, e.messagesURL(t), body)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	for k, want := range map[string]string{
		"X-Cbrt-Provider": "b", "X-Cbrt-Model": "m2", "X-Cbrt-Route": "claude-*", "X-Cbrt-Attempts": "b:m2:ok",
		"Content-Type": "application/json",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	var msg struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if msg.Type != "message" || msg.Role != "assistant" || msg.Model != "claude-sonnet-4-5" || msg.StopReason != "tool_use" {
		t.Errorf("message envelope: %s", out)
	}
	if len(msg.Content) != 2 || msg.Content[0].Type != "tool_use" || msg.Content[0].ID != "call_1" || msg.Content[0].Name != "Glob" ||
		!sameJSON(t, string(msg.Content[0].Input), `{"pattern":"**/*"}`) || msg.Content[1].Name != "Read" ||
		!sameJSON(t, string(msg.Content[1].Input), `{"file_path":"notes.md"}`) {
		t.Errorf("tool_use blocks: %s", out)
	}
	if msg.Usage.InputTokens != 20 || msg.Usage.OutputTokens != 9 {
		t.Errorf("usage: %+v", msg.Usage)
	}

	up := b.lastBody(t)
	if up["model"] != "m2" || up["stream"] != false {
		t.Errorf("upstream model/stream: %v %v", up["model"], up["stream"])
	}
	msgs := upstreamMessages(t, b)
	for i, m := range msgs {
		if m["role"] == "system" && i != 0 {
			t.Errorf("mid-conversation system message left at %d: %v", i, m)
		}
	}
	if msgs[0]["role"] != "system" || !strings.Contains(msgs[0]["content"].(string), "# Environment") /* the capture's pre-assistant system heading */ ||
		strings.Contains(msgs[0]["content"].(string), "x-anthropic-billing-header") {
		t.Errorf("leading system should carry the pre-assistant system message, not the billing header: %.200v", msgs[0]["content"])
	}
	last := msgs[len(msgs)-1]
	if last["role"] != "user" || !strings.Contains(last["content"].(string), "<system-reminder>\n<total_tokens>") {
		t.Errorf("late system message should become a user reminder: %v", last)
	}
	var tools int
	for _, m := range msgs {
		if m["role"] == "tool" {
			tools++
		}
	}
	if tools != 3 {
		t.Errorf("want 3 tool messages upstream, got %d", tools)
	}
	if a.calls.Load() != 0 {
		t.Error("claude-* route must only use b")
	}
}

func TestMessagesStreamFixtures(t *testing.T) {
	for _, fx := range []string{
		"nvidia/deepseek-v4.1-flash_stream.sse",
		"nvidia/deepseek-v4.1-flash_parallel-tools_stream.sse",
		"ollama/gemma-4-e2b-4_toolsstream.sse",
		"ollama/minicpm-v4.6_toolsstream.sse",
	} {
		t.Run(fx, func(t *testing.T) {
			raw := fixture(t, fx)
			b := newFake(t, sse(raw))
			e := newEnv(t, "", newFake(t, okJSON("unused")), b)
			body := `{"model":"claude-haiku-4-5","max_tokens":1024,"stream":true,
				"messages":[{"role":"user","content":"weather in Paris and Rome?"}],
				"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
			resp, out := postTo(t, e.messagesURL(t), body)
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Cbrt-Provider") != "b" {
				t.Fatalf("%d %v\n%s", resp.StatusCode, resp.Header, out)
			}
			if strings.Contains(out, "data: [DONE]") || strings.Contains(out, "chat.completion.chunk") {
				t.Error("upstream chat chunks leaked to the Anthropic client")
			}
			got := checkGrammar(t, parseSSE(t, out))
			want := readUpstream(t, raw)

			if got.text != want.text {
				t.Errorf("text %q, want %q", got.text, want.text)
			}
			if got.thinking != want.reasoning {
				t.Errorf("thinking %q, want %q", got.thinking, want.reasoning)
			}
			if len(got.tools) != len(want.args) {
				t.Fatalf("got %d tool_use blocks, upstream had %d calls", len(got.tools), len(want.args))
			}
			for i, tu := range got.tools {
				if tu.name != "get_weather" || tu.id == "" || !sameJSON(t, tu.input, want.args[i]) {
					t.Errorf("tool %d: %+v, want input %s", i, tu, want.args[i])
				}
			}
			wantStop := "end_turn"
			if len(want.args) > 0 {
				wantStop = "tool_use"
			}
			if got.stopReason != wantStop {
				t.Errorf("stop_reason %q, want %q", got.stopReason, wantStop)
			}
			if got.outputTokens != want.completionTokens {
				t.Errorf("message_delta output_tokens %v, want %v", got.outputTokens, want.completionTokens)
			}

			so, _ := b.lastBody(t)["stream_options"].(map[string]any)
			if so["include_usage"] != true || b.lastBody(t)["model"] != "m2" {
				t.Errorf("upstream request: %v", b.lastBody(t))
			}
			st := e.store.Stats()
			if len(st.Models) != 1 || st.Models[0].Today.Completed != 1 ||
				float64(st.Models[0].TokensMonth.CompletionReported) != want.completionTokens {
				t.Errorf("usage not recorded: %+v", st.Models)
			}
			recs := e.records(t)
			if len(recs) != 1 || recs[0]["client_api"] != "messages" || recs[0]["usage_source"] != "reported" {
				t.Errorf("records: %v", recs)
			}
		})
	}
}

const msgSimple = `{"model":"r","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

func TestMessagesFailoverBeforeCommit(t *testing.T) {
	a := newFake(t, status(429, `{"error":{"message":"slow down"}}`))
	b := newFake(t, okJSON("from b"), status(500, `{"error":{"message":"boom"}}`))
	e := newEnv(t, "", a, b)
	url := e.messagesURL(t)

	resp, out := postTo(t, url, msgSimple)
	if resp.StatusCode != 200 || !strings.Contains(out, `"text":"from b"`) || !strings.Contains(out, `"type":"message"`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if got := resp.Header.Get("X-Cbrt-Attempts"); got != "a:m1:rate_limited,b:m2:ok" || resp.Header.Get("X-Cbrt-Fallback") != "true" {
		t.Errorf("attempts %q", got)
	}

	// a is cooling down now, b fails: nothing can serve.
	resp, out = postTo(t, url, msgSimple)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, out)
	}
	var eb struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &eb); err != nil || eb.Type != "error" || eb.Error.Type != "overloaded_error" ||
		!strings.Contains(eb.Error.Message, "a:m1:skipped(rate_limited)") || !strings.Contains(eb.Error.Message, "b:m2:upstream_error") {
		t.Errorf("error body not Anthropic-shaped: %s", out)
	}
	if got := resp.Header.Get("X-Cbrt-Attempts"); got != "a:m1:skipped(rate_limited),b:m2:upstream_error" {
		t.Errorf("attempts %q", got)
	}

	resp, out = postTo(t, url, `{"model":"nope","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 404 || !strings.Contains(out, `"type":"not_found_error"`) {
		t.Errorf("unknown model: %d %s", resp.StatusCode, out)
	}
}

func TestMessagesMidStreamFailure(t *testing.T) {
	a := newFake(t, sse("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\n"))
	b := newFake(t, okJSON("must not be used"))
	e := newEnv(t, "", a, b)
	resp, out := postTo(t, e.messagesURL(t), `{"model":"r","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	evs := parseSSE(t, out)
	if evs[0].name != "message_start" || !strings.Contains(out, `"text":"par"`) {
		t.Errorf("partial content missing:\n%s", out)
	}
	lastEv := evs[len(evs)-1]
	if lastEv.name != "error" || lastEv.data["type"] != "error" || strings.Contains(out, "message_stop") {
		t.Errorf("want a terminal error event and no message_stop:\n%s", out)
	}
	if et := lastEv.data["error"].(map[string]any)["type"]; et != "api_error" {
		t.Errorf("error type %v", et)
	}
	if b.calls.Load() != 0 {
		t.Error("no failover after commit")
	}
	if st := e.store.Stats(); st.Models[0].Today.Partial != 1 {
		t.Errorf("outcome should be partial: %+v", st.Models[0].Today)
	}
}

func TestMessagesInvalidToolArgumentsFailOverWithoutCooldown(t *testing.T) {
	a := newFake(t, toolCallsJSON([3]string{"call_1", "Read", "{not json"}))
	b := newFake(t, okJSON("from b"))
	e := newEnv(t, "", a, b)
	url := e.messagesURL(t)
	for i := 1; i <= 2; i++ {
		resp, out := postTo(t, url, msgSimple)
		if resp.StatusCode != 200 || !strings.Contains(out, "from b") {
			t.Fatalf("request %d: %d %s", i, resp.StatusCode, out)
		}
		if got := resp.Header.Get("X-Cbrt-Attempts"); got != "a:m1:invalid_output,b:m2:ok" {
			t.Errorf("request %d: attempts %q (a must not be cooled down)", i, got)
		}
		if int(a.calls.Load()) != i {
			t.Errorf("request %d: a called %d times", i, a.calls.Load())
		}
	}
	for _, ev := range e.hub.Recent(10) {
		if ev.Kind == events.KindCooldown {
			t.Errorf("invalid output must not cool down: %+v", ev)
		}
	}
}

// A half-open probe that ends in invalid output must release the probe, or
// the target stays refused forever.
func TestMessagesInvalidOutputReleasesHalfOpenProbe(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	a := newFake(t,
		status(429, `{"error":{"message":"slow down"}}`),
		toolCallsJSON([3]string{"call_1", "Read", "{not json"}),
		okJSON("a again"))
	b := newFake(t, okJSON("from b"))
	e := newEnv(t, "", a, b)
	e.gw.health = router.NewHealth(clock)
	url := e.messagesURL(t)

	postTo(t, url, msgSimple) // a: 429 -> cooldown
	now = now.Add(10 * time.Minute)
	resp, _ := postTo(t, url, msgSimple) // a: half-open probe, invalid output
	if got := resp.Header.Get("X-Cbrt-Attempts"); got != "a:m1:invalid_output,b:m2:ok" {
		t.Fatalf("probe attempt: %q", got)
	}
	resp, out := postTo(t, url, msgSimple)
	if resp.Header.Get("X-Cbrt-Provider") != "a" || !strings.Contains(out, "a again") {
		t.Errorf("a must be probed again after an invalid answer, attempts %q", resp.Header.Get("X-Cbrt-Attempts"))
	}
}

func TestMessagesDecodeRejectionAndDowngrades(t *testing.T) {
	a, b := newFake(t, okJSON("a")), newFake(t, okJSON("b"))
	e := newEnv(t, "", a, b)
	url := e.messagesURL(t)

	resp, out := postTo(t, url, `{"model":"r","max_tokens":100,"messages":[{"role":"user","content":[
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}]}]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	var eb struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if json.Unmarshal([]byte(out), &eb) != nil || eb.Type != "error" || eb.Error.Type != "invalid_request_error" || !strings.Contains(eb.Error.Message, "document") {
		t.Errorf("rejection body: %s", out)
	}
	if down, rej := e.gw.Counters(); rej != 1 || down != 0 {
		t.Errorf("counters after rejection: downgraded %d rejected %d", down, rej)
	}
	if a.calls.Load()+b.calls.Load() != 0 {
		t.Error("a rejected request must not reach an upstream")
	}

	resp, out = postTo(t, url, `{"model":"r","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":1024},
		"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if down, rej := e.gw.Counters(); down < 2 || rej != 1 {
		t.Errorf("counters after downgrade: downgraded %d rejected %d", down, rej)
	}
	up := a.lastBody(t)
	if _, ok := up["thinking"]; ok {
		t.Errorf("thinking must not reach the upstream: %v", up)
	}

	resp, out = postTo(t, url, `{"model":"r","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 || !strings.Contains(out, "max_tokens") || !strings.Contains(out, `"invalid_request_error"`) {
		t.Errorf("missing max_tokens: %d %s", resp.StatusCode, out)
	}
}

// TestMessagesClaudeCodeToolLoop scripts two Claude Code turns: provider a
// answers the first with tool calls, then starts failing, so b serves turn 2.
func TestMessagesClaudeCodeToolLoop(t *testing.T) {
	a := newFake(t,
		toolCallsJSON([3]string{"call_abc", "Read", `{"file_path":"notes.md"}`}),
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			status(429, `{"error":{"message":"rate limit"}}`)(w, r)
		})
	b := newFake(t, sse("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"The codename is Heron.\"}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":6}}\n\ndata: [DONE]\n\n"))
	e := newEnv(t, "", a, b)
	url := e.messagesURL(t)

	tools := `[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]`
	turn1 := `{"model":"r","max_tokens":4096,"system":[{"type":"text","text":"You are Claude Code."}],"tools":` + tools + `,
		"messages":[{"role":"user","content":[{"type":"text","text":"What is the release codename in notes.md?"}]}]}`
	req, _ := http.NewRequest("POST", url, strings.NewReader(turn1))
	req.Header.Set("X-Claude-Code-Session-Id", "sess-42")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var msg1 struct {
		StopReason string            `json:"stop_reason"`
		Content    []json.RawMessage `json:"content"`
	}
	json.NewDecoder(resp.Body).Decode(&msg1)
	resp.Body.Close()
	if resp.Header.Get("X-Cbrt-Provider") != "a" || msg1.StopReason != "tool_use" || len(msg1.Content) != 1 {
		t.Fatalf("turn 1: %v %+v", resp.Header, msg1)
	}
	var tu struct{ Type, ID, Name string }
	json.Unmarshal(msg1.Content[0], &tu)
	if tu.Type != "tool_use" || tu.ID != "call_abc" || tu.Name != "Read" {
		t.Fatalf("turn 1 tool_use: %s", msg1.Content[0])
	}

	// Turn 2 as Claude Code builds it: history + assistant tool_use + user
	// tool_result + a trailing mid-conversation system message.
	turn2 := fmt.Sprintf(`{"model":"r","max_tokens":4096,"stream":true,"system":[{"type":"text","text":"You are Claude Code."}],"tools":%s,
		"messages":[
		  {"role":"user","content":[{"type":"text","text":"What is the release codename in notes.md?"}]},
		  {"role":"assistant","content":[%s]},
		  {"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"1\tTasky notes\n2\tThe release codename is Heron."}]},
		  {"role":"system","content":[{"type":"text","text":"<total_tokens>999 tokens left</total_tokens>"}]}
		]}`, tools, msg1.Content[0], tu.ID)
	req, _ = http.NewRequest("POST", url, strings.NewReader(turn2))
	req.Header.Set("X-Claude-Code-Session-Id", "sess-42")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("X-Cbrt-Provider") != "b" || resp.Header.Get("X-Cbrt-Attempts") != "a:m1:rate_limited,b:m2:ok" {
		t.Fatalf("turn 2 should fail over to b: %v", resp.Header)
	}
	d := checkGrammar(t, parseSSE(t, string(out)))
	if d.text != "The codename is Heron." || d.stopReason != "end_turn" {
		t.Errorf("turn 2 answer: %+v", d)
	}

	msgs := upstreamMessages(t, b)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m["role"].(string))
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,user" {
		t.Fatalf("upstream roles %s", got)
	}
	asst := msgs[2]["tool_calls"].([]any)[0].(map[string]any)
	fn := asst["function"].(map[string]any)
	if asst["id"] != "call_abc" || fn["name"] != "Read" || !sameJSON(t, fn["arguments"].(string), `{"file_path":"notes.md"}`) {
		t.Errorf("assistant tool call: %v", asst)
	}
	if msgs[3]["tool_call_id"] != "call_abc" || !strings.Contains(msgs[3]["content"].(string), "Heron") {
		t.Errorf("tool message: %v", msgs[3])
	}
	if c := msgs[4]["content"].(string); c != "<system-reminder>\n<total_tokens>999 tokens left</total_tokens>\n</system-reminder>" {
		t.Errorf("reminder message: %q", c)
	}

	var sw *events.Event
	for _, ev := range e.hub.Recent(20) {
		if ev.Kind == events.KindSwitch {
			ev := ev
			sw = &ev
		}
	}
	if sw == nil || sw.From != "a:m1" || sw.To != "b:m2" || sw.Route != "r" {
		t.Errorf("switch event: %+v", e.hub.Recent(20))
	}
	if s, ok := e.gw.Session("sess-42"); !ok || s.LastTarget != "b:m2" || !s.Fallback {
		t.Errorf("session state: %+v %v", s, ok)
	}
}
