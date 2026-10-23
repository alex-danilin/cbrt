package anthropic

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const upstreamDir = "../../../testdata/upstream/"

func encodeFixture(t *testing.T, name string, expose bool) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(upstreamDir + name)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeResponse(raw, "claude-x", expose)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var m map[string]any
	mustJSON(t, out, &m)
	return m
}

func blockTypes(m map[string]any) []string {
	var types []string
	for _, b := range m["content"].([]any) {
		types = append(types, b.(map[string]any)["type"].(string))
	}
	return types
}

func TestEncodeResponseFixtures(t *testing.T) {
	tests := []struct {
		file   string
		expose bool
		types  []string
		stop   string
		usage  map[string]any
	}{
		{"nvidia/deepseek-v4.1-flash_parallel-tools.json", true, []string{"thinking", "text", "tool_use", "tool_use"}, "tool_use",
			map[string]any{"input_tokens": 64.0, "cache_read_input_tokens": 256.0, "output_tokens": 88.0}},
		{"nvidia/deepseek-v4.1-flash_parallel-tools.json", false, []string{"text", "tool_use", "tool_use"}, "tool_use", nil},
		{"nvidia/deepseek-v4.1-flash_tool-choice-required.json", true, []string{"thinking", "text"}, "end_turn", nil},
		{"nvidia/gpt-oss-20b_reasoning-hits-max-tokens.json", false, []string{"text"}, "max_tokens",
			map[string]any{"input_tokens": 64.0, "output_tokens": 120.0}},
		{"nvidia/gpt-oss-20b_tools.json", true, []string{"thinking", "tool_use"}, "tool_use", nil},
		{"ollama/gemma-4-e2b-4_basic.json", true, []string{"thinking", "text"}, "end_turn", nil},
		{"ollama/gemma-4-e2b-4_tools.json", false, []string{"tool_use", "tool_use"}, "tool_use", nil},
		{"ollama/minicpm-v4.6_basic.json", false, []string{"text"}, "max_tokens", nil},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			m := encodeFixture(t, tt.file, tt.expose)
			if !regexp.MustCompile(`^msg_[0-9a-f]{24}$`).MatchString(m["id"].(string)) {
				t.Errorf("id = %v", m["id"])
			}
			if m["type"] != "message" || m["role"] != "assistant" || m["model"] != "claude-x" || m["stop_sequence"] != nil {
				t.Errorf("envelope = %v", m)
			}
			if got := blockTypes(m); !reflect.DeepEqual(got, tt.types) {
				t.Errorf("types = %v, want %v", got, tt.types)
			}
			if m["stop_reason"] != tt.stop {
				t.Errorf("stop_reason = %v", m["stop_reason"])
			}
			if tt.usage != nil && !reflect.DeepEqual(m["usage"], tt.usage) {
				t.Errorf("usage = %v, want %v", m["usage"], tt.usage)
			}
			for _, b := range blocksOf(m, "thinking") {
				if b["signature"] != "" || b["thinking"] == "" {
					t.Errorf("thinking block = %v", b)
				}
			}
			for _, b := range blocksOf(m, "tool_use") {
				if b["id"] == "" || b["name"] != "get_weather" || b["input"].(map[string]any)["city"] == nil {
					t.Errorf("tool_use = %v", b)
				}
			}
		})
	}
}

func TestEncodeResponseErrors(t *testing.T) {
	tests := []string{
		`not json`,
		`{"choices":[]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{bad"}}]}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"[1,2]"}}]}}]}`,
		`{"error":{"message":"boom"}}`,
	}
	for _, body := range tests {
		if _, err := EncodeResponse([]byte(body), "m", true); err == nil {
			t.Errorf("%s: want error", body)
		}
	}
}

func TestEncodeResponseEdgeCases(t *testing.T) {
	// Empty arguments become {}, missing ids are generated, object arguments are accepted.
	body := `{"choices":[{"message":{"content":null,"tool_calls":[
	  {"function":{"name":"f","arguments":""}},{"id":"b","function":{"name":"g","arguments":{"k":1}}}]},"finish_reason":"stop"}]}`
	out, err := EncodeResponse([]byte(body), "m", true)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	mustJSON(t, out, &m)
	tools := blocksOf(m, "tool_use")
	if len(tools) != 2 || !strings.HasPrefix(tools[0]["id"].(string), "toolu_") ||
		!reflect.DeepEqual(tools[0]["input"], map[string]any{}) || !reflect.DeepEqual(tools[1]["input"], map[string]any{"k": 1.0}) {
		t.Errorf("tools = %v", tools)
	}
	if m["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", m["stop_reason"])
	}
	if strings.Contains(string(out), `<`) {
		t.Error("HTML escaping in output")
	}
}

func TestCountTokens(t *testing.T) {
	// The capture file is pretty-printed; Claude Code sends compact JSON.
	body := []byte(compactJSON(loadCapture(t, "0003")))
	n, err := CountTokens(body)
	if err != nil {
		t.Fatal(err)
	}
	ref := float64(len(body)) / 4
	if n <= 0 || float64(n) < ref*0.7 || float64(n) > ref*1.3 {
		t.Errorf("CountTokens = %d, len/4 = %.0f", n, ref)
	}

	base := `{"messages":[{"role":"user","content":[{"type":"text","text":"hello there"}%s]}]}`
	img := `,{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`
	n0, _ := CountTokens([]byte(strings.Replace(base, "%s", "", 1)))
	n2, _ := CountTokens([]byte(strings.Replace(base, "%s", img+img, 1)))
	if n0 <= 0 || n2-n0 != 2*ImageTokens {
		t.Errorf("n0=%d n2=%d, want +2*ImageTokens for two images", n0, n2)
	}
	if _, err := CountTokens([]byte("{")); err == nil {
		t.Error("want error for invalid JSON")
	}
}

func TestErrorType(t *testing.T) {
	tests := map[int]string{
		400: "invalid_request_error", 401: "authentication_error", 403: "permission_error",
		404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error",
		503: "overloaded_error", 529: "overloaded_error", 500: "api_error", 502: "api_error", 418: "api_error",
	}
	for status, want := range tests {
		if got := ErrorType(status); got != want {
			t.Errorf("ErrorType(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestErrorJSON(t *testing.T) {
	tests := []struct{ typ, msg, want string }{
		{"api_error", "boom", `{"type":"error","error":{"type":"api_error","message":"boom"}}`},
		{"invalid_request_error", `bad "x" <y>`, `{"type":"error","error":{"type":"invalid_request_error","message":"bad \"x\" <y>"}}`},
	}
	for _, tt := range tests {
		got := ErrorJSON(tt.typ, tt.msg)
		if string(got) != tt.want || !json.Valid(got) {
			t.Errorf("ErrorJSON = %s, want %s", got, tt.want)
		}
	}
	if (&Error{Msg: "x"}).Error() != "x" {
		t.Error("Error.Error")
	}
}

// A count_tokens call shaped like Claude Code 2.1.285 sends (synthetic content): the
// body is {model, messages, tools: []} and is answered with {"input_tokens":N}.
func TestCountTokensCapture(t *testing.T) {
	raw, err := os.ReadFile("../../../testdata/clients/claude-code-2.1.285/0004-POST-v1_messages_count_tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Path         string          `json:"path"`
		RequestBody  json.RawMessage `json:"request_body"`
		ResponseBody map[string]any  `json:"response_body"`
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ex.Path, "/v1/messages/count_tokens") {
		t.Fatalf("path %q", ex.Path)
	}
	if _, ok := ex.ResponseBody["input_tokens"]; !ok || len(ex.ResponseBody) != 1 {
		t.Fatalf("recorded answer shape %v", ex.ResponseBody)
	}
	// The fixture is synthetic and its recorded answer is invented, so only the
	// estimate's plausibility is checked, not the recorded number.
	body := []byte(compactJSON(ex.RequestBody))
	n, err := CountTokens(body)
	if err != nil {
		t.Fatal(err)
	}
	if ref := len(body) / 4; n < ref/2 || n > ref*2 {
		t.Errorf("CountTokens = %d, len/4 = %d", n, ref)
	}
}
