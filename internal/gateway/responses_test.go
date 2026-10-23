package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/api/responses"
	"github.com/alex-danilin/cbrt/internal/events"
)

// responsesURL starts a second test server in front of the same gateway that
// serves the OpenAI Responses API.
func (e *env) responsesURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.gw.ServeResponses(w, r, b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// postWith posts body with extra request headers.
func postWith(t *testing.T, url, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// codexCapture returns the request body Codex 0.154.0 sent, with fields overridden.
func codexCapture(t *testing.T, set map[string]any) (string, map[string]json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/clients/codex-0.154.0/0003-POST-v1_responses.json")
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
	return string(b), capt.RequestBody
}

var chatToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// respStream is a reassembled Responses event stream.
type respStream struct {
	terminal  string           // last event name
	response  map[string]any   // the terminal event's response object
	items     []map[string]any // output_item.done items, in order
	text      string
	reasoning string
	args      map[string]string // function_call_arguments deltas by item id
	input     map[string]string // custom_tool_call_input deltas by item id
	names     []string          // all event names
}

var terminalEvents = map[string]bool{
	"response.completed": true, "response.incomplete": true, "response.failed": true,
}

// checkResponsesGrammar asserts the Responses streaming grammar: sequence
// numbers 0,1,2..., created then in_progress first, items opened and closed
// in order with matching ids, and exactly one terminal event, last. A failed
// stream may leave its last item open.
func checkResponsesGrammar(t *testing.T, evs []msgEvent) respStream {
	t.Helper()
	d := respStream{args: map[string]string{}, input: map[string]string{}}
	if len(evs) < 3 || evs[0].name != "response.created" || evs[1].name != "response.in_progress" {
		t.Fatalf("stream must start with response.created, response.in_progress: %+v", evs)
	}
	openIdx, openID, next := -1, "", 0
	for i, ev := range evs {
		d.names = append(d.names, ev.name)
		if ev.data["type"] != ev.name {
			t.Errorf("event %d: name %q but data.type %v", i, ev.name, ev.data["type"])
		}
		if seq, ok := ev.data["sequence_number"].(float64); !ok || int(seq) != i {
			t.Errorf("event %d (%s): sequence_number %v", i, ev.name, ev.data["sequence_number"])
		}
		if terminalEvents[ev.name] && i != len(evs)-1 {
			t.Errorf("terminal event %s at %d of %d", ev.name, i, len(evs))
		}
		idx := -1
		if f, ok := ev.data["output_index"].(float64); ok {
			idx = int(f)
		}
		itemID, _ := ev.data["item_id"].(string)
		switch ev.name {
		case "response.created", "response.in_progress":
			if i > 1 {
				t.Errorf("%s at %d", ev.name, i)
			}
		case "response.output_item.added":
			if openIdx != -1 || idx != next {
				t.Fatalf("event %d: item %d added while %d open (next %d)", i, idx, openIdx, next)
			}
			item := ev.data["item"].(map[string]any)
			openIdx, openID, next = idx, item["id"].(string), next+1
		case "response.output_item.done":
			item := ev.data["item"].(map[string]any)
			if idx != openIdx || item["id"] != openID {
				t.Fatalf("event %d: item %d/%v done while %d/%s open", i, idx, item["id"], openIdx, openID)
			}
			d.items = append(d.items, item)
			openIdx, openID = -1, ""
		case "response.output_text.delta", "response.reasoning_summary_text.delta",
			"response.function_call_arguments.delta", "response.custom_tool_call_input.delta",
			"response.content_part.added", "response.content_part.done", "response.output_text.done",
			"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
			"response.reasoning_summary_text.done", "response.function_call_arguments.done",
			"response.custom_tool_call_input.done":
			if idx != openIdx || itemID != openID {
				t.Fatalf("event %d (%s): for item %d/%s while %d/%s open", i, ev.name, idx, itemID, openIdx, openID)
			}
			delta, _ := ev.data["delta"].(string)
			switch ev.name {
			case "response.output_text.delta":
				d.text += delta
			case "response.reasoning_summary_text.delta":
				d.reasoning += delta
			case "response.function_call_arguments.delta":
				d.args[itemID] += delta
			case "response.custom_tool_call_input.delta":
				d.input[itemID] += delta
			}
		case "response.completed", "response.incomplete", "response.failed":
			d.terminal = ev.name
			d.response, _ = ev.data["response"].(map[string]any)
			if ev.name != "response.failed" && openIdx != -1 {
				t.Errorf("%s with item %d still open", ev.name, openIdx)
			}
		default:
			t.Errorf("unexpected event %q", ev.name)
		}
	}
	if d.terminal == "" {
		t.Errorf("stream has no terminal event; last %q", evs[len(evs)-1].name)
	}
	if d.terminal != "response.failed" && next == 0 {
		t.Error("response has no output items")
	}
	if d.response != nil && d.terminal != "response.failed" {
		if out, _ := d.response["output"].([]any); len(out) != len(d.items) {
			t.Errorf("final response has %d output items, stream closed %d", len(out), len(d.items))
		}
	}
	return d
}

// itemsOfType returns the items of one type.
func itemsOfType(items []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, it := range items {
		if it["type"] == typ {
			out = append(out, it)
		}
	}
	return out
}

func upstreamTools(t *testing.T, f *fake) []map[string]any {
	t.Helper()
	raw, _ := f.lastBody(t)["tools"].([]any)
	out := make([]map[string]any, len(raw))
	for i, x := range raw {
		out[i] = x.(map[string]any)
	}
	return out
}

func TestResponsesCodexCaptureStream(t *testing.T) {
	raw := fixture(t, "ollama/gemma-4-e2b-4_toolsstream.sse")
	a := newFake(t, sse(raw))
	e := newEnv(t, "", a, newFake(t, okJSON("unused")))
	body, orig := codexCapture(t, map[string]any{"model": "r"})
	resp, out := postWith(t, e.responsesURL(t), body, map[string]string{"Session-Id": "codex-sess"})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Cbrt-Provider") != "a" {
		t.Fatalf("%d %v\n%.2000s", resp.StatusCode, resp.Header, out)
	}
	if strings.Contains(out, "data: [DONE]") || strings.Contains(out, "chat.completion.chunk") {
		t.Error("upstream chat chunks leaked to the Responses client")
	}
	d := checkResponsesGrammar(t, parseSSE(t, out))
	if d.terminal != "response.completed" || d.response["status"] != "completed" || d.response["model"] != "r" {
		t.Errorf("terminal %s %v %v", d.terminal, d.response["status"], d.response["model"])
	}
	want := readUpstream(t, raw)
	calls := itemsOfType(d.items, "function_call")
	if len(calls) != len(want.args) || len(calls) == 0 {
		t.Fatalf("got %d function_call items, upstream had %d calls", len(calls), len(want.args))
	}
	for i, c := range calls {
		if c["name"] != "get_weather" || c["call_id"] == "" || !sameJSON(t, c["arguments"].(string), want.args[i]) {
			t.Errorf("call %d: %v, want %s", i, c, want.args[i])
		}
	}

	// The upstream request is plain Chat Completions.
	up := a.lastBody(t)
	if up["model"] != "m1" || up["stream"] != true {
		t.Errorf("upstream model/stream: %v %v", up["model"], up["stream"])
	}
	if so, _ := up["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("stream_options: %v", up["stream_options"])
	}
	for _, k := range []string{"input", "instructions", "reasoning", "include", "store", "prompt_cache_key", "client_metadata"} {
		if _, ok := up[k]; ok {
			t.Errorf("Responses field %q reached the upstream", k)
		}
	}
	if up["tool_choice"] != "auto" || up["parallel_tool_calls"] != true {
		t.Errorf("tool_choice/parallel_tool_calls: %v %v", up["tool_choice"], up["parallel_tool_calls"])
	}
	msgs := upstreamMessages(t, a)
	if len(msgs) < 2 || msgs[0]["role"] != "system" {
		t.Fatalf("upstream must start with a system message: %v", msgs)
	}
	var instr string
	json.Unmarshal(orig["instructions"], &instr)
	sys := msgs[0]["content"].(string)
	if !strings.HasPrefix(sys, instr[:200]) || !strings.Contains(sys, "<permissions instructions>") {
		t.Errorf("leading system should carry instructions and the developer message: %.300s", sys)
	}
	for i, m := range msgs[1:] {
		if m["role"] == "system" || m["role"] == "developer" {
			t.Errorf("system/developer message left at %d", i+1)
		}
	}
	if last := msgs[len(msgs)-1]; last["role"] != "user" || !strings.Contains(last["content"].(string), "release codename") {
		t.Errorf("last upstream message: %v", last)
	}

	// Tools: namespaces flattened, built-ins dropped, names valid.
	var src []struct {
		Type  string `json:"type"`
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	json.Unmarshal(orig["tools"], &src)
	wantTools := 0
	for _, st := range src {
		switch st.Type {
		case "function", "custom":
			wantTools++
		case "namespace":
			for _, nt := range st.Tools {
				if nt.Type == "function" || nt.Type == "custom" {
					wantTools++
				}
			}
		}
	}
	tools := upstreamTools(t, a)
	if len(tools) != wantTools || wantTools < 150 {
		t.Errorf("upstream has %d tools, want %d", len(tools), wantTools)
	}
	seen := map[string]bool{}
	for _, tl := range tools {
		fn, _ := tl["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if tl["type"] != "function" || !chatToolName.MatchString(name) || seen[name] {
			t.Errorf("invalid or duplicate upstream tool: %v %q", tl["type"], name)
		}
		seen[name] = true
		if _, ok := tl["tools"]; ok {
			t.Errorf("namespace structure leaked: %q", name)
		}
	}
	if seen["web_search"] || !seen["exec_command"] || !anyPrefix(seen, "mcp__codex_apps__app") {
		t.Errorf("expected exec_command and flattened app tools, no web_search: %d tools", len(seen))
	}

	if s, ok := e.gw.Session("codex-sess"); !ok || s.LastTarget != "a:m1" || s.Fallback {
		t.Errorf("session: %+v %v", s, ok)
	}
	recs := e.records(t)
	if len(recs) != 1 || recs[0]["client_api"] != "responses" || recs[0]["session"] != "codex-sess" || recs[0]["outcome"] != "completed" {
		t.Errorf("records: %v", recs)
	}
}

func anyPrefix(set map[string]bool, p string) bool {
	for k := range set {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

const (
	longNS   = "mcp__codex_apps__acmecorp_web_tools"
	longTool = "acmecorp_widget_catalog_list_related_widgets_xxxxxxxxxx"
	patch    = "*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** End Patch\n"
)

// codexToolsBody is a non-stream Codex-style request declaring a function,
// a namespaced function whose flattened name must be hashed, and a custom tool.
var codexToolsBody = fmt.Sprintf(`{"model":"r","stream":false,"instructions":"You are Codex.",
	"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]}],
	"tools":[
	  {"type":"function","name":"exec_command","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
	  {"type":"namespace","name":%q,"description":"demo","tools":[{"type":"function","name":%q,"description":"kw","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]},
	  {"type":"custom","name":"apply_patch","description":"patch files","format":{"type":"grammar","syntax":"lark","definition":"start: begin_patch"}}
	]}`, longNS, longTool)

func TestResponsesNonStreamToolCalls(t *testing.T) {
	// The upstream answers with calls to whatever names the gateway sent it.
	var upNames []string
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		upNames = nil
		for _, tl := range req.Tools {
			upNames = append(upNames, tl.Function.Name)
		}
		if len(upNames) != 3 {
			status(500, `{"error":{"message":"test: want 3 tools"}}`)(w, r)
			return
		}
		patchArgs, _ := json.Marshal(map[string]string{"input": patch})
		toolCallsJSON(
			[3]string{"call_1", upNames[0], `{"cmd":"ls"}`},
			[3]string{"call_2", upNames[1], `{"q":"go"}`},
			[3]string{"call_3", upNames[2], string(patchArgs)},
		)(w, r)
	})
	// fake reads the body before the step; re-read is empty, so capture via last.
	a.steps[0] = wrapLast(a, a.steps[0])
	e := newEnv(t, "", a, newFake(t, okJSON("unused")))
	resp, out := postWith(t, e.responsesURL(t), codexToolsBody, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	for k, want := range map[string]string{
		"X-Cbrt-Provider": "a", "X-Cbrt-Model": "m1", "X-Cbrt-Route": "r", "X-Cbrt-Attempts": "a:m1:ok",
		"Content-Type": "application/json",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if upNames[0] != "exec_command" || upNames[2] != "apply_patch" || !chatToolName.MatchString(upNames[1]) ||
		upNames[1] == longNS+"__"+longTool || !strings.HasPrefix(upNames[1], longNS+"__") {
		t.Errorf("upstream tool names: %q", upNames)
	}
	var r struct {
		Object, Status, Model string
		Output                []map[string]any
		Usage                 struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if r.Object != "response" || r.Status != "completed" || r.Model != "r" || len(r.Output) != 3 {
		t.Fatalf("response envelope: %s", out)
	}
	fc0, fc1, cc := r.Output[0], r.Output[1], r.Output[2]
	if fc0["type"] != "function_call" || fc0["call_id"] != "call_1" || fc0["name"] != "exec_command" ||
		!sameJSON(t, fc0["arguments"].(string), `{"cmd":"ls"}`) || fc0["namespace"] != nil {
		t.Errorf("exec_command call: %v", fc0)
	}
	if fc1["type"] != "function_call" || fc1["call_id"] != "call_2" || fc1["name"] != longTool || fc1["namespace"] != longNS ||
		!sameJSON(t, fc1["arguments"].(string), `{"q":"go"}`) {
		t.Errorf("namespaced call must come back with its original name and namespace: %v", fc1)
	}
	if cc["type"] != "custom_tool_call" || cc["call_id"] != "call_3" || cc["name"] != "apply_patch" || cc["input"] != patch {
		t.Errorf("custom tool call must come back unwrapped: %v", cc)
	}
	if r.Usage.InputTokens != 20 || r.Usage.OutputTokens != 9 {
		t.Errorf("usage: %+v", r.Usage)
	}
	st := e.store.Stats()
	if len(st.Models) != 1 || st.Models[0].Today.Completed != 1 || st.Models[0].TokensMonth.CompletionReported != 9 {
		t.Errorf("usage not recorded: %+v", st.Models)
	}
	recs := e.records(t)
	if len(recs) != 1 || recs[0]["client_api"] != "responses" || recs[0]["usage_source"] != "reported" {
		t.Errorf("records: %v", recs)
	}
}

// wrapLast replays the stored request body to h (fake consumed it already).
func wrapLast(f *fake, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(f.last.Load().(string)))
		h(w, r)
	}
}

func TestResponsesStreamFixtures(t *testing.T) {
	body := `{"model":"r","stream":true,"input":"weather in Paris and Rome?",
		"tools":[{"type":"function","name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	for _, fx := range []string{
		"nvidia/deepseek-v4.1-flash_stream.sse",
		"nvidia/deepseek-v4.1-flash_parallel-tools_stream.sse",
		"ollama/gemma-4-e2b-4_toolsstream.sse",
		"ollama/minicpm-v4.6_toolsstream.sse",
	} {
		for _, expose := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/expose=%v", fx, expose), func(t *testing.T) {
				raw := fixture(t, fx)
				extra := ""
				if !expose {
					extra = `, "visibility": {"expose_reasoning": false}`
				}
				a := newFake(t, sse(raw))
				e := newEnv(t, extra, a, newFake(t, okJSON("unused")))
				resp, out := postWith(t, e.responsesURL(t), body, nil)
				if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
					t.Fatalf("%d %v\n%s", resp.StatusCode, resp.Header, out)
				}
				d := checkResponsesGrammar(t, parseSSE(t, out))
				want := readUpstream(t, raw)
				if d.terminal != "response.completed" {
					t.Fatalf("terminal %s", d.terminal)
				}
				if d.text != want.text {
					t.Errorf("text %q, want %q", d.text, want.text)
				}
				msgs := itemsOfType(d.items, "message")
				if want.text != "" && (len(msgs) != 1 || msgs[0]["content"].([]any)[0].(map[string]any)["text"] != want.text) {
					t.Errorf("message items: %v", msgs)
				}
				calls := itemsOfType(d.items, "function_call")
				if len(calls) != len(want.args) {
					t.Fatalf("got %d function_call items, upstream had %d calls", len(calls), len(want.args))
				}
				for i, c := range calls {
					if c["name"] != "get_weather" || c["call_id"] == "" || c["arguments"] != want.args[i] ||
						d.args[c["id"].(string)] != want.args[i] {
						t.Errorf("call %d: %v (streamed %q), want %s", i, c, d.args[c["id"].(string)], want.args[i])
					}
				}
				rs := itemsOfType(d.items, "reasoning")
				if !expose || want.reasoning == "" {
					if len(rs) != 0 || strings.Contains(out, "reasoning_summary") {
						t.Errorf("reasoning must not be shown (expose=%v)", expose)
					}
				} else {
					if len(rs) != 1 || d.items[0]["type"] != "reasoning" || d.reasoning == "" || !strings.HasPrefix(want.reasoning, d.reasoning) {
						t.Fatalf("reasoning item: %d items, streamed %q, upstream %q", len(rs), d.reasoning, want.reasoning)
					}
					sum := rs[0]["summary"].([]any)[0].(map[string]any)
					if sum["type"] != "summary_text" || sum["text"] != d.reasoning {
						t.Errorf("reasoning summary: %v", sum)
					}
				}
				u, _ := d.response["usage"].(map[string]any)
				if u["output_tokens"] != want.completionTokens {
					t.Errorf("usage %v, want output_tokens %v", u, want.completionTokens)
				}
				recs := e.records(t)
				if len(recs) != 1 || recs[0]["client_api"] != "responses" || recs[0]["usage_source"] != "reported" {
					t.Errorf("records: %v", recs)
				}
			})
		}
	}
}

// TestResponsesCodexToolLoop scripts two Codex turns: provider a answers the
// first with an exec_command call, then starts failing, so b serves turn 2.
func TestResponsesCodexToolLoop(t *testing.T) {
	a := newFake(t,
		toolCallsJSON([3]string{"call_exec1", "exec_command", `{"cmd":"cat notes.md"}`}),
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			status(429, `{"error":{"message":"rate limit"}}`)(w, r)
		})
	b := newFake(t, sse("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"The codename is Heron.\"}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":6}}\n\ndata: [DONE]\n\n"))
	e := newEnv(t, "", a, b)
	url := e.responsesURL(t)
	hdr := map[string]string{"Session-Id": "codex-42", "Originator": "codex_exec"}

	tools := `[{"type":"function","name":"exec_command","description":"Run a command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}]`
	dev := `{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>read-only</permissions instructions>"}]}`
	user := `{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the release codename in notes.md?"}]}`
	turn1 := `{"model":"r","stream":false,"store":false,"instructions":"You are Codex.","tools":` + tools + `,"input":[` + dev + `,` + user + `]}`
	resp, out := postWith(t, url, turn1, hdr)
	if resp.StatusCode != 200 || resp.Header.Get("X-Cbrt-Provider") != "a" {
		t.Fatalf("turn 1: %d %v %s", resp.StatusCode, resp.Header, out)
	}
	var r1 struct {
		Output []json.RawMessage `json:"output"`
	}
	json.Unmarshal([]byte(out), &r1)
	var fc struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	}
	if len(r1.Output) != 1 || json.Unmarshal(r1.Output[0], &fc) != nil {
		t.Fatalf("turn 1 output: %s", out)
	}
	if fc.Type != "function_call" || fc.CallID != "call_exec1" || fc.Name != "exec_command" {
		t.Fatalf("turn 1 function_call: %s", r1.Output[0])
	}

	// Turn 2 as Codex builds it: full history, the function_call item echoed
	// back, its output, a mid-conversation developer message and a new user message.
	turn2 := fmt.Sprintf(`{"model":"r","stream":true,"store":false,"instructions":"You are Codex.","tools":%s,"input":[%s,%s,%s,
		{"type":"function_call_output","call_id":%q,"output":"Tasky notes\nThe release codename is Heron."},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"<turn_note>keep it short</turn_note>"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Answer now."}]}]}`,
		tools, dev, user, r1.Output[0], fc.CallID)
	resp, out = postWith(t, url, turn2, hdr)
	if resp.StatusCode != 200 || resp.Header.Get("X-Cbrt-Provider") != "b" || resp.Header.Get("X-Cbrt-Attempts") != "a:m1:rate_limited,b:m2:ok" {
		t.Fatalf("turn 2 should fail over to b: %d %v\n%s", resp.StatusCode, resp.Header, out)
	}
	d := checkResponsesGrammar(t, parseSSE(t, out))
	if d.text != "The codename is Heron." || d.terminal != "response.completed" {
		t.Errorf("turn 2 answer: %q %s", d.text, d.terminal)
	}

	msgs := upstreamMessages(t, b)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m["role"].(string))
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,user,user" {
		t.Fatalf("upstream roles %s", got)
	}
	if s := msgs[0]["content"].(string); !strings.Contains(s, "You are Codex.") || !strings.Contains(s, "read-only") {
		t.Errorf("leading system: %q", s)
	}
	tc := msgs[2]["tool_calls"].([]any)[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if tc["id"] != fc.CallID || fn["name"] != "exec_command" || !sameJSON(t, fn["arguments"].(string), `{"cmd":"cat notes.md"}`) {
		t.Errorf("assistant tool call: %v", tc)
	}
	if msgs[3]["tool_call_id"] != fc.CallID || !strings.Contains(msgs[3]["content"].(string), "Heron") {
		t.Errorf("tool message: %v", msgs[3])
	}
	if c := msgs[4]["content"].(string); c != "<system-reminder>\n<turn_note>keep it short</turn_note>\n</system-reminder>" {
		t.Errorf("late developer message should become a user reminder: %q", c)
	}
	if c := msgs[5]["content"].(string); c != "Answer now." {
		t.Errorf("last user message: %q", c)
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
	if s, ok := e.gw.Session("codex-42"); !ok || s.LastTarget != "b:m2" || !s.Fallback {
		t.Errorf("session state: %+v %v", s, ok)
	}
}

const respSimple = `{"model":"r","input":"hi"}`

type openAIError struct {
	Error struct {
		Type    string  `json:"type"`
		Code    *string `json:"code"`
		Message string  `json:"message"`
	} `json:"error"`
}

func decodeOpenAIError(t *testing.T, body string) openAIError {
	t.Helper()
	var eb openAIError
	if err := json.Unmarshal([]byte(body), &eb); err != nil || eb.Error.Type == "" {
		t.Fatalf("error body not OpenAI-shaped: %s", body)
	}
	return eb
}

func TestResponsesFailoverBeforeCommit(t *testing.T) {
	a := newFake(t, status(429, `{"error":{"message":"slow down"}}`))
	b := newFake(t, okJSON("from b"), status(500, `{"error":{"message":"boom"}}`))
	e := newEnv(t, "", a, b)
	url := e.responsesURL(t)

	resp, out := postTo(t, url, respSimple)
	if resp.StatusCode != 200 || !strings.Contains(out, `"text":"from b"`) || !strings.Contains(out, `"object":"response"`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if got := resp.Header.Get("X-Cbrt-Attempts"); got != "a:m1:rate_limited,b:m2:ok" || resp.Header.Get("X-Cbrt-Fallback") != "true" {
		t.Errorf("attempts %q", got)
	}

	// a is cooling down now, b fails: nothing can serve.
	resp, out = postTo(t, url, respSimple)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, out)
	}
	eb := decodeOpenAIError(t, out)
	if eb.Error.Type != "server_error" || eb.Error.Code == nil || *eb.Error.Code != "no_upstream_available" ||
		!strings.Contains(eb.Error.Message, "a:m1:skipped(rate_limited)") || !strings.Contains(eb.Error.Message, "b:m2:upstream_error") {
		t.Errorf("503 body: %s", out)
	}

	resp, out = postTo(t, url, `{"model":"nope","input":"hi"}`)
	if resp.StatusCode != 404 || decodeOpenAIError(t, out).Error.Type != "invalid_request_error" {
		t.Errorf("unknown model: %d %s", resp.StatusCode, out)
	}
}

func TestResponsesMidStreamFailure(t *testing.T) {
	a := newFake(t, sse("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\n"))
	b := newFake(t, okJSON("must not be used"))
	e := newEnv(t, "", a, b)
	resp, out := postTo(t, e.responsesURL(t), `{"model":"r","stream":true,"input":"hi"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	d := checkResponsesGrammar(t, parseSSE(t, out))
	if d.text != "par" {
		t.Errorf("partial content missing: %q", d.text)
	}
	if d.terminal != "response.failed" || strings.Contains(out, "response.completed") || d.response["status"] != "failed" {
		t.Errorf("want a terminal response.failed and no response.completed:\n%s", out)
	}
	if errObj, _ := d.response["error"].(map[string]any); errObj["code"] != "server_error" || !strings.Contains(errObj["message"].(string), "[DONE]") {
		t.Errorf("failure error: %v", d.response["error"])
	}
	if b.calls.Load() != 0 {
		t.Error("no failover after commit")
	}
	if st := e.store.Stats(); st.Models[0].Today.Partial != 1 {
		t.Errorf("outcome should be partial: %+v", st.Models[0].Today)
	}
}

func TestResponsesInvalidToolArgumentsFailOverWithoutCooldown(t *testing.T) {
	a := newFake(t, toolCallsJSON([3]string{"call_1", "exec_command", "{not json"}))
	b := newFake(t, okJSON("from b"))
	e := newEnv(t, "", a, b)
	url := e.responsesURL(t)
	body := `{"model":"r","input":"hi","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}}]}`
	for i := 1; i <= 2; i++ {
		resp, out := postTo(t, url, body)
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

func TestResponsesRejectionsAndDowngrades(t *testing.T) {
	a, b := newFake(t, okJSON("a")), newFake(t, okJSON("b"))
	e := newEnv(t, "", a, b)
	url := e.responsesURL(t)

	resp, out := postTo(t, url, `{"model":"r","previous_response_id":"resp_123","input":"continue"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("previous_response_id: %d %s", resp.StatusCode, out)
	}
	if eb := decodeOpenAIError(t, out); eb.Error.Type != "invalid_request_error" || !strings.Contains(eb.Error.Message, "previous_response_id") {
		t.Errorf("rejection body: %s", out)
	}
	if down, rej := e.gw.Counters(); rej != 1 || down != 0 {
		t.Errorf("counters after rejection: downgraded %d rejected %d", down, rej)
	}

	resp, out = postTo(t, url, `{"model":"r","input":[
		{"type":"message","role":"user","content":"search"},
		{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"x"}}]}`)
	if resp.StatusCode != 400 || !strings.Contains(decodeOpenAIError(t, out).Error.Message, "web_search_call") {
		t.Errorf("web_search_call history: %d %s", resp.StatusCode, out)
	}
	if _, rej := e.gw.Counters(); rej != 2 {
		t.Errorf("rejected counter %d", rej)
	}
	if a.calls.Load()+b.calls.Load() != 0 {
		t.Error("a rejected request must not reach an upstream")
	}

	// The capture is served, with its untranslatable parts counted.
	body, _ := codexCapture(t, map[string]any{"model": "r", "stream": false})
	_, info, err := responses.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reasoning", "include", "prompt_cache_key", "client_metadata", "tool:web_search"} {
		if !slices.Contains(info.Downgraded, want) {
			t.Errorf("downgrade %q not reported: %v", want, info.Downgraded)
		}
	}
	if slices.Contains(info.Downgraded, "store") {
		t.Error("store:false is not a downgrade")
	}
	resp, out = postTo(t, url, body)
	if resp.StatusCode != 200 {
		t.Fatalf("capture: %d %.500s", resp.StatusCode, out)
	}
	if down, rej := e.gw.Counters(); down != int64(len(info.Downgraded)) || rej != 2 {
		t.Errorf("counters after capture: downgraded %d (want %d) rejected %d", down, len(info.Downgraded), rej)
	}
}
