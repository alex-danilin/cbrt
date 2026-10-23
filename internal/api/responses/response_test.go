package responses

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const upstreamDir = "../../../testdata/upstream/"

func encodeFixture(t *testing.T, name string, tools ToolMap, expose bool) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(upstreamDir + name)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeResponse(raw, "codex-x", tools, expose)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var m map[string]any
	mustJSON(t, out, &m)
	return m
}

func itemTypes(m map[string]any) []string {
	var types []string
	for _, b := range m["output"].([]any) {
		types = append(types, b.(map[string]any)["type"].(string))
	}
	return types
}

func itemsOf(m map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, b := range m["output"].([]any) {
		if bm := b.(map[string]any); bm["type"] == typ {
			out = append(out, bm)
		}
	}
	return out
}

// toolsFor builds a ToolMap by decoding a request offering these tools.
func toolsFor(t *testing.T, tools string) ToolMap {
	t.Helper()
	_, info, err := DecodeRequest([]byte(`{"model":"m","input":"x","tools":` + tools + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return info.Tools
}

var idPattern = map[string]*regexp.Regexp{
	"reasoning":        regexp.MustCompile(`^rs_[0-9a-f]{24}$`),
	"message":          regexp.MustCompile(`^msg_[0-9a-f]{24}$`),
	"function_call":    regexp.MustCompile(`^fc_[0-9a-f]{24}$`),
	"custom_tool_call": regexp.MustCompile(`^ctc_[0-9a-f]{24}$`),
}

func TestEncodeResponseFixtures(t *testing.T) {
	tests := []struct {
		file   string
		expose bool
		types  []string
		status string
		usage  map[string]any
	}{
		{"nvidia/deepseek-v4.1-flash_parallel-tools.json", true, []string{"reasoning", "message", "function_call", "function_call"}, "completed",
			map[string]any{"input_tokens": 320.0, "input_tokens_details": map[string]any{"cached_tokens": 256.0},
				"output_tokens": 88.0, "output_tokens_details": map[string]any{"reasoning_tokens": 17.0}, "total_tokens": 408.0}},
		{"nvidia/deepseek-v4.1-flash_parallel-tools.json", false, []string{"message", "function_call", "function_call"}, "completed", nil},
		{"nvidia/deepseek-v4.1-flash_tool-choice-required.json", true, []string{"reasoning", "message"}, "completed", nil},
		{"nvidia/gpt-oss-20b_reasoning-hits-max-tokens.json", false, []string{"message"}, "incomplete",
			map[string]any{"input_tokens": 64.0, "input_tokens_details": map[string]any{"cached_tokens": 0.0},
				"output_tokens": 120.0, "output_tokens_details": map[string]any{"reasoning_tokens": 0.0}, "total_tokens": 184.0}},
		{"nvidia/gpt-oss-20b_reasoning-hits-max-tokens.json", true, []string{"reasoning", "message"}, "incomplete", nil},
		{"nvidia/gpt-oss-20b_tools.json", true, []string{"reasoning", "function_call"}, "completed", nil},
		{"ollama/gemma-4-e2b-4_basic.json", true, []string{"reasoning", "message"}, "completed", nil},
		{"ollama/gemma-4-e2b-4_tools.json", false, []string{"function_call", "function_call"}, "completed", nil},
		{"ollama/minicpm-v4.6_tools.json", false, []string{"function_call", "function_call"}, "completed", nil},
		{"ollama/minicpm-v4.6_basic.json", false, []string{"message"}, "incomplete", nil},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			m := encodeFixture(t, tt.file, ToolMap{}, tt.expose)
			if !regexp.MustCompile(`^resp_[0-9a-f]{24}$`).MatchString(m["id"].(string)) {
				t.Errorf("id = %v", m["id"])
			}
			if m["object"] != "response" || m["model"] != "codex-x" || m["error"] != nil || m["created_at"].(float64) <= 0 {
				t.Errorf("envelope = %v", m)
			}
			if got := itemTypes(m); !reflect.DeepEqual(got, tt.types) {
				t.Errorf("types = %v, want %v", got, tt.types)
			}
			if m["status"] != tt.status {
				t.Errorf("status = %v", m["status"])
			}
			if tt.status == "incomplete" {
				if !reflect.DeepEqual(m["incomplete_details"], map[string]any{"reason": "max_output_tokens"}) {
					t.Errorf("incomplete_details = %v", m["incomplete_details"])
				}
			} else if m["incomplete_details"] != nil {
				t.Errorf("incomplete_details = %v", m["incomplete_details"])
			}
			if tt.usage != nil && !reflect.DeepEqual(m["usage"], tt.usage) {
				t.Errorf("usage = %v, want %v", m["usage"], tt.usage)
			}
			for _, it := range m["output"].([]any) {
				item := it.(map[string]any)
				if !idPattern[item["type"].(string)].MatchString(item["id"].(string)) {
					t.Errorf("item id %v", item)
				}
			}
			for _, r := range itemsOf(m, "reasoning") {
				s := r["summary"].([]any)
				if len(s) != 1 || s[0].(map[string]any)["type"] != "summary_text" || s[0].(map[string]any)["text"] == "" {
					t.Errorf("reasoning = %v", r)
				}
			}
			for _, msg := range itemsOf(m, "message") {
				c := msg["content"].([]any)
				if msg["role"] != "assistant" || msg["status"] != "completed" || len(c) != 1 || c[0].(map[string]any)["type"] != "output_text" {
					t.Errorf("message = %v", msg)
				}
			}
			for _, fc := range itemsOf(m, "function_call") {
				var args map[string]any
				mustJSON(t, []byte(fc["arguments"].(string)), &args)
				if fc["call_id"] == "" || fc["name"] != "get_weather" || args["city"] == nil || fc["status"] != "completed" {
					t.Errorf("function_call = %v", fc)
				}
				if _, ok := fc["namespace"]; ok {
					t.Errorf("unexpected namespace: %v", fc)
				}
			}
		})
	}

	// Upstream call ids are kept.
	m := encodeFixture(t, "nvidia/deepseek-v4.1-flash_parallel-tools.json", ToolMap{}, false)
	fcs := itemsOf(m, "function_call")
	if fcs[0]["call_id"] != "chatcmpl-tool-synth000000000a1" || fcs[1]["call_id"] != "chatcmpl-tool-synth000000000a2" ||
		fcs[1]["arguments"] != `{"city": "Lima"}` {
		t.Errorf("function calls = %v", fcs)
	}
}

func TestEncodeResponseToolMapping(t *testing.T) {
	tools := toolsFor(t, `[{"type":"custom","name":"apply_patch"},
	  {"type":"namespace","name":"mcp__weather","tools":[{"type":"function","name":"get_weather"},{"type":"custom","name":"raw"}]}]`)

	// Namespaced function tools come back with their namespace.
	body := `{"choices":[{"message":{"tool_calls":[
	  {"id":"a","function":{"name":"mcp__weather__get_weather","arguments":"{\"city\":\"Paris\"}"}},
	  {"id":"b","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n\\\"q\\\"\"}"}},
	  {"id":"c","function":{"name":"mcp__weather__raw","arguments":"\"just text\""}},
	  {"function":{"name":"apply_patch","arguments":"plain text"}},
	  {"function":{"name":"unknown","arguments":""}}]},"finish_reason":"tool_calls"}]}`
	out, err := EncodeResponse([]byte(body), "m", tools, true)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	mustJSON(t, out, &m)
	if got := itemTypes(m); !reflect.DeepEqual(got, []string{"function_call", "custom_tool_call", "custom_tool_call", "custom_tool_call", "function_call"}) {
		t.Fatalf("types = %v", got)
	}
	o := m["output"].([]any)
	fc := o[0].(map[string]any)
	if fc["name"] != "get_weather" || fc["namespace"] != "mcp__weather" || fc["call_id"] != "a" || fc["arguments"] != `{"city":"Paris"}` {
		t.Errorf("namespaced call = %v", fc)
	}
	ctc := o[1].(map[string]any)
	if ctc["name"] != "apply_patch" || ctc["input"] != "*** Begin Patch\n\"q\"" || ctc["call_id"] != "b" || ctc["status"] != "completed" {
		t.Errorf("custom call = %v", ctc)
	}
	if _, ok := ctc["arguments"]; ok {
		t.Error("custom_tool_call has arguments")
	}
	if c := o[2].(map[string]any); c["input"] != "just text" || c["namespace"] != "mcp__weather" || c["name"] != "raw" {
		t.Errorf("namespaced custom call = %v", c)
	}
	if c := o[3].(map[string]any); c["input"] != "plain text" || !strings.HasPrefix(c["call_id"].(string), "call_") {
		t.Errorf("bare custom call = %v", c)
	}
	if c := o[4].(map[string]any); c["name"] != "unknown" || c["arguments"] != "{}" {
		t.Errorf("unknown call = %v", c)
	}
}

func TestEncodeResponseErrors(t *testing.T) {
	tools := toolsFor(t, `[{"type":"custom","name":"apply_patch"}]`)
	tests := []string{
		`not json`,
		`{"choices":[]}`,
		`{"error":{"message":"boom"}}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{bad"}}]}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"[1,2]"}}]}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"","arguments":"{}"}}]}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"apply_patch","arguments":"{\"patch\":\"x\"}"}}]}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"apply_patch","arguments":"{\"input\":1}"}}]}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"apply_patch","arguments":""}}]}}]}`,
	}
	for _, body := range tests {
		if _, err := EncodeResponse([]byte(body), "m", tools, true); err == nil {
			t.Errorf("%s: want error", body)
		}
	}
}

func TestEncodeResponseEmpty(t *testing.T) {
	out, err := EncodeResponse([]byte(`{"choices":[{"message":{"content":null},"finish_reason":"stop"}]}`), "m", ToolMap{}, true)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	mustJSON(t, out, &m)
	msgs := itemsOf(m, "message")
	if len(m["output"].([]any)) != 1 || len(msgs) != 1 || msgs[0]["content"].([]any)[0].(map[string]any)["text"] != "" {
		t.Errorf("output = %v", m["output"])
	}
	if m["status"] != "completed" || m["usage"] == nil {
		t.Errorf("response = %v", m)
	}
	if strings.Contains(string(out), `<`) {
		t.Error("HTML escaping in output")
	}
}

func TestErrorJSON(t *testing.T) {
	tests := []struct{ typ, code, msg, want string }{
		{"invalid_request_error", "unsupported_input", `bad "x" <y>`,
			`{"error":{"type":"invalid_request_error","code":"unsupported_input","message":"bad \"x\" <y>"}}`},
		{"server_error", "", "boom", `{"error":{"type":"server_error","code":null,"message":"boom"}}`},
	}
	for _, tt := range tests {
		got := ErrorJSON(tt.typ, tt.code, tt.msg)
		if string(got) != tt.want || !json.Valid(got) {
			t.Errorf("ErrorJSON = %s, want %s", got, tt.want)
		}
	}
	if (&Error{Msg: "x"}).Error() != "x" {
		t.Error("Error.Error")
	}
}
