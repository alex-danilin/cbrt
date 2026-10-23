package responses

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const captureDir = "../../../testdata/clients/codex-0.154.0"

type capture struct {
	RequestBody  json.RawMessage `json:"request_body"`
	ResponseBody string          `json:"response_body"`
}

func loadCapture(t *testing.T, prefix string) capture {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(captureDir, prefix+"-*.json"))
	if len(matches) != 1 {
		t.Fatalf("capture %s: found %d files", prefix, len(matches))
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var c capture
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// decoded is the chat body as generic JSON.
type decoded struct {
	Model             string           `json:"model"`
	Messages          []map[string]any `json:"messages"`
	Stream            bool             `json:"stream"`
	MaxTokens         *int             `json:"max_tokens"`
	Tools             []map[string]any `json:"tools"`
	ToolChoice        any              `json:"tool_choice"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls"`
	ResponseFormat    map[string]any   `json:"response_format"`
}

func decode(t *testing.T, body []byte) (decoded, RequestInfo, map[string]any) {
	t.Helper()
	chat, info, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var d decoded
	var generic map[string]any
	if err := json.Unmarshal(chat, &d); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(chat, &generic)
	return d, info, generic
}

func roles(msgs []map[string]any) []string {
	var r []string
	for _, m := range msgs {
		r = append(r, m["role"].(string))
	}
	return r
}

func toolName(tool map[string]any) string {
	return tool["function"].(map[string]any)["name"].(string)
}

func TestDecodeCapture(t *testing.T) {
	c := loadCapture(t, "0003")
	var req struct {
		Instructions string `json:"instructions"`
		Tools        []struct {
			Type  string            `json:"type"`
			Name  string            `json:"name"`
			Tools []json.RawMessage `json:"tools"`
		} `json:"tools"`
	}
	mustJSON(t, c.RequestBody, &req)
	topFuncs, nested := 0, 0
	for _, tl := range req.Tools {
		switch tl.Type {
		case "function", "custom":
			topFuncs++
		case "namespace":
			nested += len(tl.Tools)
		}
	}
	if len(req.Tools) != 23 || nested != 183 {
		t.Fatalf("capture changed: %d top-level, %d nested tools", len(req.Tools), nested)
	}

	d, info, generic := decode(t, c.RequestBody)
	allowed := []string{"model", "messages", "stream", "max_tokens", "temperature", "top_p", "tools", "tool_choice", "parallel_tool_calls", "response_format"}
	for k := range generic {
		if !slices.Contains(allowed, k) {
			t.Errorf("unexpected chat field %q", k)
		}
	}
	if d.Model != "synth-coder-8b" || !d.Stream || !info.Stream || info.Model != d.Model || d.MaxTokens != nil {
		t.Errorf("envelope: model=%q stream=%v max_tokens=%v", d.Model, d.Stream, d.MaxTokens)
	}
	if got := roles(d.Messages); !reflect.DeepEqual(got, []string{"system", "system", "user", "user"}) {
		t.Errorf("roles = %v", got)
	}
	if d.Messages[0]["content"] != req.Instructions {
		t.Error("instructions are not the first system message")
	}
	if dev := d.Messages[1]["content"].(string); !strings.HasPrefix(dev, "<skills_instructions>") || !strings.Contains(dev, "\n\n<permissions instructions>") {
		t.Errorf("developer message = %.80q", dev)
	}
	if last := d.Messages[3]["content"]; last != "Look at notes.md and main.go here and tell me the release codename." {
		t.Errorf("user prompt = %v", last)
	}

	if len(d.Tools) != topFuncs+nested {
		t.Errorf("tools = %d, want %d", len(d.Tools), topFuncs+nested)
	}
	seen := map[string]bool{}
	for _, tl := range d.Tools {
		n := toolName(tl)
		if seen[n] || len(n) > 64 || !validToolName.MatchString(n) {
			t.Errorf("bad or duplicate tool name %q", n)
		}
		seen[n] = true
		fn := tl["function"].(map[string]any)
		if _, ok := fn["strict"]; ok || tl["type"] != "function" || fn["parameters"] == nil {
			t.Errorf("tool %s = %v", n, tl)
		}
	}
	// App namespaces in the synthetic capture are mcp__codex_apps__appNN with _tool_… nested tools.
	flattened := false
	for n := range seen {
		if strings.HasPrefix(n, "mcp__codex_apps__app") && strings.Contains(n, "_tool") {
			flattened = true
		}
	}
	if !seen["exec_command"] || !flattened {
		t.Error("expected exec_command and a flattened app tool (mcp__codex_apps__appNN…_toolNNN)")
	}
	// Every namespaced tool maps back to its namespace.
	for _, tl := range req.Tools {
		for _, raw := range tl.Tools {
			var nt struct{ Name string }
			mustJSON(t, raw, &nt)
			n := info.Tools.chatName(tl.Name, nt.Name)
			if !seen[n] || info.Tools.lookup(n) != (toolRef{namespace: tl.Name, name: nt.Name}) {
				t.Errorf("%s/%s -> %q -> %+v", tl.Name, nt.Name, n, info.Tools.lookup(n))
			}
		}
	}
	if d.ToolChoice != "auto" || d.ParallelToolCalls == nil || !*d.ParallelToolCalls {
		t.Errorf("tool_choice=%v parallel=%v", d.ToolChoice, d.ParallelToolCalls)
	}
	for _, want := range []string{"reasoning", "include", "prompt_cache_key", "client_metadata", "tool:web_search"} {
		if !slices.Contains(info.Downgraded, want) {
			t.Errorf("Downgraded %v lacks %q", info.Downgraded, want)
		}
	}
	if slices.Contains(info.Downgraded, "store") {
		t.Error("store:false must not be downgraded")
	}
	if info.ReasoningEffort != "medium" {
		t.Errorf("ReasoningEffort = %q", info.ReasoningEffort)
	}
}

func TestDecodeMultiTurn(t *testing.T) {
	body := `{"model":"m","max_output_tokens":500,"temperature":0.2,"top_p":0.9,"store":true,
	"instructions":"be good",
	"tools":[
	  {"type":"function","name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}},"strict":true},
	  {"type":"custom","name":"apply_patch","description":"patch files","format":{"type":"grammar","syntax":"lark","definition":"start: \"*** Begin Patch\""}},
	  {"type":"namespace","name":"mcp__app","description":"An app","tools":[{"type":"function","name":"_search","description":"find","parameters":{"type":"object"}}]},
	  {"type":"web_search"}],
	"tool_choice":{"type":"custom","name":"apply_patch"},
	"parallel_tool_calls":false,
	"input":[
	  {"role":"user","content":"hi"},
	  {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"x"},
	  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"let me look"}]},
	  {"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
	  {"type":"function_call","call_id":"c2","name":"_search","namespace":"mcp__app","arguments":"{}"},
	  {"type":"function_call_output","call_id":"c1","output":"a.txt"},
	  {"type":"function_call_output","call_id":"c2","output":[{"type":"input_text","text":"r1"},{"type":"output_text","text":"r2"}]},
	  {"type":"custom_tool_call","call_id":"c3","name":"apply_patch","input":"*** Begin Patch\n\"x\""},
	  {"type":"custom_tool_call_output","call_id":"c3","output":"done"},
	  {"type":"message","role":"developer","content":"mid system"},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"high"}]}
	]}`
	d, info, _ := decode(t, []byte(body))
	wantRoles := []string{"system", "user", "assistant", "tool", "tool", "assistant", "tool", "system", "user"}
	if got := roles(d.Messages); !reflect.DeepEqual(got, wantRoles) {
		t.Fatalf("roles = %v, want %v", got, wantRoles)
	}
	a := d.Messages[2]
	calls := a["tool_calls"].([]any)
	if a["content"] != "let me look" || len(calls) != 2 {
		t.Fatalf("assistant = %v", a)
	}
	c0 := calls[0].(map[string]any)
	c1 := calls[1].(map[string]any)
	if c0["id"] != "c1" || c0["function"].(map[string]any)["name"] != "shell" || c0["function"].(map[string]any)["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("call 0 = %v", c0)
	}
	if c1["id"] != "c2" || c1["function"].(map[string]any)["name"] != "mcp__app___search" {
		t.Errorf("call 1 = %v", c1)
	}
	if d.Messages[3]["tool_call_id"] != "c1" || d.Messages[3]["content"] != "a.txt" ||
		d.Messages[4]["tool_call_id"] != "c2" || d.Messages[4]["content"] != "r1\nr2" {
		t.Errorf("tool results = %v %v", d.Messages[3], d.Messages[4])
	}
	custom := d.Messages[5]["tool_calls"].([]any)[0].(map[string]any)
	fn := custom["function"].(map[string]any)
	var wrapped map[string]string
	mustJSON(t, []byte(fn["arguments"].(string)), &wrapped)
	if custom["id"] != "c3" || fn["name"] != "apply_patch" || !reflect.DeepEqual(wrapped, map[string]string{"input": "*** Begin Patch\n\"x\""}) ||
		d.Messages[5]["content"] != nil {
		t.Errorf("custom call = %v (%s)", d.Messages[5], fn["arguments"])
	}
	if d.Messages[6]["tool_call_id"] != "c3" || d.Messages[6]["content"] != "done" || d.Messages[7]["content"] != "mid system" {
		t.Errorf("messages = %v", d.Messages[6:8])
	}
	parts := d.Messages[8]["content"].([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAAA" ||
		parts[1].(map[string]any)["image_url"].(map[string]any)["detail"] != "high" {
		t.Errorf("user parts = %v", parts)
	}

	if len(d.Tools) != 3 {
		t.Fatalf("tools = %v", d.Tools)
	}
	cfn := d.Tools[1]["function"].(map[string]any)
	if cfn["name"] != "apply_patch" || !strings.Contains(cfn["description"].(string), "lark grammar") ||
		!strings.Contains(cfn["description"].(string), `start: "*** Begin Patch"`) {
		t.Errorf("custom tool = %v", cfn)
	}
	props := cfn["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["input"]; !ok {
		t.Errorf("custom params = %v", cfn["parameters"])
	}
	if toolName(d.Tools[2]) != "mcp__app___search" || !strings.Contains(d.Tools[2]["function"].(map[string]any)["description"].(string), "An app") {
		t.Errorf("namespaced tool = %v", d.Tools[2])
	}
	if !reflect.DeepEqual(d.ToolChoice, map[string]any{"type": "function", "function": map[string]any{"name": "apply_patch"}}) {
		t.Errorf("tool_choice = %v", d.ToolChoice)
	}
	if d.ParallelToolCalls == nil || *d.ParallelToolCalls || d.MaxTokens == nil || *d.MaxTokens != 500 {
		t.Errorf("parallel=%v max_tokens=%v", d.ParallelToolCalls, d.MaxTokens)
	}
	for _, want := range []string{"reasoning_items", "store", "custom_tool_grammar", "tool:web_search"} {
		if !slices.Contains(info.Downgraded, want) {
			t.Errorf("Downgraded %v lacks %q", info.Downgraded, want)
		}
	}
	if !info.Tools.lookup("apply_patch").custom || info.Tools.lookup("mcp__app___search").namespace != "mcp__app" {
		t.Errorf("tool map = %+v", info.Tools)
	}
}

func TestDecodeSimpleAndOptions(t *testing.T) {
	d, info, generic := decode(t, []byte(`{"model":"m","input":"hello","tool_choice":"required","parallel_tool_calls":true,
	  "text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"},"strict":true},"verbosity":"low"},
	  "truncation":"auto","metadata":{"a":"b"},"user":"u","service_tier":"auto","background":false,"some_new_field":1}`))
	if !reflect.DeepEqual(roles(d.Messages), []string{"user"}) || d.Messages[0]["content"] != "hello" {
		t.Errorf("messages = %v", d.Messages)
	}
	if _, ok := generic["tool_choice"]; ok {
		t.Error("tool_choice without tools")
	}
	if _, ok := generic["parallel_tool_calls"]; ok {
		t.Error("parallel_tool_calls without tools")
	}
	if d.ResponseFormat["type"] != "json_schema" || d.ResponseFormat["json_schema"].(map[string]any)["name"] != "out" {
		t.Errorf("response_format = %v", d.ResponseFormat)
	}
	want := []string{"text.verbosity", "background", "metadata", "service_tier", "some_new_field", "truncation", "user"}
	if !reflect.DeepEqual(info.Downgraded, want) {
		t.Errorf("Downgraded = %v, want %v", info.Downgraded, want)
	}
	if info.Stream || d.Stream {
		t.Error("stream defaults to false")
	}

	// Unsupported tool_choice forms are downgraded, not sent.
	_, info, generic = decode(t, []byte(`{"model":"m","input":"x","tools":[{"type":"function","name":"f"}],
	  "tool_choice":{"type":"allowed_tools","mode":"auto","tools":[]}}`))
	if _, ok := generic["tool_choice"]; ok || !slices.Contains(info.Downgraded, "tool_choice") {
		t.Errorf("tool_choice kept: %v %v", generic["tool_choice"], info.Downgraded)
	}
}

func TestDecodeRejects(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"not json", `[`, "invalid request body"},
		{"missing model", `{"input":"x"}`, "model"},
		{"empty model", `{"model":"","input":"x"}`, "model"},
		{"missing input", `{"model":"m"}`, "input"},
		{"empty input string", `{"model":"m","input":""}`, "input"},
		{"empty input array", `{"model":"m","input":[]}`, "input"},
		{"previous_response_id", `{"model":"m","input":"x","previous_response_id":"resp_1"}`, "previous_response_id"},
		{"web_search_call", `{"model":"m","input":[{"type":"web_search_call","id":"ws","status":"completed"}]}`, `"web_search_call"`},
		{"local_shell_call", `{"model":"m","input":[{"type":"local_shell_call","call_id":"x"}]}`, `"local_shell_call"`},
		{"input_file", `{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"f"}]}]}`, "input_file"},
		{"file_id image", `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","file_id":"f"}]}]}`, "file_id"},
		{"unknown item", `{"model":"m","input":[{"type":"item_reference","id":"x"}]}`, `"item_reference"`},
		{"unknown part", `{"model":"m","input":[{"role":"user","content":[{"type":"input_audio"}]}]}`, `"input_audio"`},
		{"bad role", `{"model":"m","input":[{"role":"tool","content":"x"}]}`, "role"},
		{"call without id", `{"model":"m","input":[{"type":"function_call","name":"f","arguments":"{}"}]}`, "call_id"},
		{"output image", `{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"u"}]}]}`, "input_image"},
		{"bad max_output_tokens", `{"model":"m","input":"x","max_output_tokens":0}`, "max_output_tokens"},
		{"unknown tool_choice tool", `{"model":"m","input":"x","tools":[{"type":"function","name":"f"}],"tool_choice":{"type":"function","name":"g"}}`, "tool_choice"},
		{"duplicate tool", `{"model":"m","input":"x","tools":[{"type":"function","name":"f"},{"type":"custom","name":"f"}]}`, "duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := DecodeRequest([]byte(tt.body))
			var e *Error
			if !errors.As(err, &e) || !strings.Contains(e.Msg, tt.want) {
				t.Errorf("err = %v, want *Error containing %q", err, tt.want)
			}
		})
	}
	// An empty previous_response_id is accepted.
	if _, _, err := DecodeRequest([]byte(`{"model":"m","input":"x","previous_response_id":""}`)); err != nil {
		t.Error(err)
	}
}

func TestFlatName(t *testing.T) {
	if got := flatName("ns", "tool", false); got != "ns__tool" {
		t.Errorf("flatName = %q", got)
	}
	long := flatName("mcp__codex_apps__acmecorp_web_tools", "_list_widget_inventory_detail_xxxxxxxxx", false)
	if len(long) != 64 || !validToolName.MatchString(long) || long != flatName("mcp__codex_apps__acmecorp_web_tools", "_list_widget_inventory_detail_xxxxxxxxx", false) {
		t.Errorf("long = %q", long)
	}
	if odd := flatName("a.b", "c d", false); !validToolName.MatchString(odd) || !strings.HasPrefix(odd, "a_b__c_d_") {
		t.Errorf("odd = %q", odd)
	}
	// A top-level name that equals a flattened name forces the hashed form.
	var m ToolMap
	m.add(toolRef{name: "ns__x"})
	n := m.add(toolRef{namespace: "ns", name: "x"})
	if n == "ns__x" || n == "" || m.lookup(n).namespace != "ns" || m.lookup("ns__x").namespace != "" {
		t.Errorf("collision: %q %+v", n, m)
	}
	if (ToolMap{}).lookup("f") != (toolRef{name: "f"}) {
		t.Error("zero ToolMap lookup")
	}
}
