package anthropic

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

const captureDir = "../../../testdata/clients/claude-code-2.1.284"

func loadCapture(t *testing.T, prefix string) []byte {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(captureDir, prefix+"-*.json"))
	if len(matches) != 1 {
		t.Fatalf("capture %s: found %d files", prefix, len(matches))
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		RequestBody json.RawMessage `json:"request_body"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c.RequestBody
}

// decoded is the chat body as generic JSON.
type decoded struct {
	Model             string           `json:"model"`
	Messages          []map[string]any `json:"messages"`
	Stream            bool             `json:"stream"`
	MaxTokens         int              `json:"max_tokens"`
	Stop              []string         `json:"stop"`
	Tools             []map[string]any `json:"tools"`
	ToolChoice        any              `json:"tool_choice"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls"`
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

func TestDecodeCaptures(t *testing.T) {
	allowedTop := []string{"model", "messages", "stream", "max_tokens", "temperature", "top_p", "stop", "tools", "tool_choice", "parallel_tool_calls"}
	tests := []struct {
		capture    string
		roles      []string
		tools      int
		downgraded []string
	}{
		{"0003", []string{"system", "user", "system"}, 29,
			[]string{"billing_header", "cache_control", "context_management", "metadata", "output_config", "thinking"}},
		{"0015", []string{"system", "user", "system"}, 2,
			[]string{"billing_header", "cache_control", "context_management", "metadata", "output_config", "thinking"}},
		{"0016", []string{"system", "user", "system", "assistant", "tool", "tool", "tool", "system"}, 2,
			[]string{"billing_header", "cache_control", "context_management", "metadata", "output_config", "thinking"}},
	}
	for _, tt := range tests {
		t.Run(tt.capture, func(t *testing.T) {
			body := loadCapture(t, tt.capture)
			d, info, generic := decode(t, body)

			for k := range generic {
				if !slices.Contains(allowedTop, k) {
					t.Errorf("unexpected top-level field %q", k)
				}
			}
			if got := roles(d.Messages); !reflect.DeepEqual(got, tt.roles) {
				t.Errorf("roles = %v, want %v", got, tt.roles)
			}
			if len(d.Tools) != tt.tools {
				t.Errorf("tools = %d, want %d", len(d.Tools), tt.tools)
			}
			for _, tool := range d.Tools {
				fn := tool["function"].(map[string]any)
				if tool["type"] != "function" || fn["name"] == "" || fn["parameters"] == nil || fn["description"] == "" {
					t.Errorf("bad tool %v", tool["function"])
				}
			}
			if d.MaxTokens != 32000 || !d.Stream || d.Model != info.Model || !info.Stream {
				t.Errorf("max_tokens=%d stream=%v model=%q", d.MaxTokens, d.Stream, d.Model)
			}
			got := slices.Clone(info.Downgraded)
			slices.Sort(got)
			if !reflect.DeepEqual(got, tt.downgraded) {
				t.Errorf("downgraded = %v, want %v", got, tt.downgraded)
			}
			if info.ReasoningEffort != "high" {
				t.Errorf("effort = %q", info.ReasoningEffort)
			}
			// The billing block is dropped, so the capture's second system block leads.
			var blocks struct {
				System []struct{ Text string } `json:"system"`
			}
			json.Unmarshal([]byte(body), &blocks)
			sys := d.Messages[0]["content"].(string)
			if len(blocks.System) < 2 || strings.Contains(sys, billingPrefix) || !strings.HasPrefix(sys, blocks.System[1].Text[:40]) {
				t.Errorf("leading system = %.80q", sys)
			}
			chat, _, _ := DecodeRequest(body)
			for _, bad := range []string{"cache_control", "input_schema", "tool_use_id", `"thinking"`, "billing-header"} {
				if strings.Contains(string(chat), bad) {
					t.Errorf("chat body still contains %s", bad)
				}
			}
			// Every tool message answers a preceding assistant tool call.
			ids := map[string]bool{}
			for _, m := range d.Messages {
				if calls, ok := m["tool_calls"].([]any); ok {
					for _, c := range calls {
						cm := c.(map[string]any)
						args := cm["function"].(map[string]any)["arguments"].(string)
						if !json.Valid([]byte(args)) {
							t.Errorf("invalid arguments %q", args)
						}
						ids[cm["id"].(string)] = true
					}
				}
				if m["role"] == "tool" && !ids[m["tool_call_id"].(string)] {
					t.Errorf("tool message %v has no matching call", m["tool_call_id"])
				}
			}
		})
	}
}

func TestDecode0016Details(t *testing.T) {
	d, _, _ := decode(t, loadCapture(t, "0016"))
	a := d.Messages[3]
	if a["content"] != nil {
		t.Errorf("assistant content = %v, want null (thinking dropped)", a["content"])
	}
	calls := a["tool_calls"].([]any)
	wantIDs := []string{"toolu_synth_01", "toolu_synth_02", "toolu_synth_03"}
	for i, c := range calls {
		cm := c.(map[string]any)
		if cm["id"] != wantIDs[i] || d.Messages[4+i]["tool_call_id"] != wantIDs[i] {
			t.Errorf("call %d id mismatch: %v", i, cm["id"])
		}
	}
	if got := calls[1].(map[string]any)["function"].(map[string]any)["arguments"]; got != `{"target_file":"notes.md"}` {
		t.Errorf("arguments = %v", got)
	}
	if got := d.Messages[5]["content"]; got != "1\t# Tasky\n2\tRelease codename: Heron.\n3\t" {
		t.Errorf("tool content = %q", got)
	}
	if got := d.Messages[7]["content"].(string); !strings.HasPrefix(got, "<total_tokens>") {
		t.Errorf("trailing system = %q", got)
	}
}

func TestDecodeMapping(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"stop_sequences":["END"],"temperature":0.5,"top_p":0.9,"top_k":5,
	"tools":[{"name":"a","description":"d","input_schema":{"type":"object"},"defer_loading":true},
	         {"type":"web_search_20250305","name":"web_search"},{"type":"custom","name":"b","input_schema":{}}],
	"tool_choice":{"type":"any","disable_parallel_tool_use":true},
	"system":"sys","messages":[{"role":"user","content":"hi"}]}`
	d, info, generic := decode(t, []byte(body))
	if !reflect.DeepEqual(d.Stop, []string{"END"}) || generic["temperature"] != 0.5 || generic["top_p"] != 0.9 {
		t.Errorf("sampling fields: %v", generic)
	}
	if len(d.Tools) != 2 || d.ToolChoice != "required" || d.ParallelToolCalls == nil || *d.ParallelToolCalls {
		t.Errorf("tools=%d choice=%v parallel=%v", len(d.Tools), d.ToolChoice, d.ParallelToolCalls)
	}
	if !slices.Contains(info.Downgraded, "tool:web_search_20250305") || !slices.Contains(info.Downgraded, "top_k") {
		t.Errorf("downgraded = %v", info.Downgraded)
	}
	if !reflect.DeepEqual(roles(d.Messages), []string{"system", "user"}) || d.Messages[1]["content"] != "hi" {
		t.Errorf("messages = %v", d.Messages)
	}
}

func TestToolChoice(t *testing.T) {
	tests := []struct {
		in   string
		want any
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"any"}`, "required"},
		{`{"type":"none"}`, "none"},
		{`{"type":"tool","name":"x"}`, map[string]any{"type": "function", "function": map[string]any{"name": "x"}}},
	}
	for _, tt := range tests {
		body := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"x","input_schema":{}}],"tool_choice":` + tt.in + `}`
		d, _, _ := decode(t, []byte(body))
		if !reflect.DeepEqual(d.ToolChoice, tt.want) {
			t.Errorf("%s -> %v, want %v", tt.in, d.ToolChoice, tt.want)
		}
		if d.ParallelToolCalls != nil {
			t.Errorf("%s: unexpected parallel_tool_calls", tt.in)
		}
	}
}

func TestDecodeRejections(t *testing.T) {
	const ok = `"model":"m","max_tokens":5`
	tests := []struct {
		name, body, want string
	}{
		{"document", `{` + ok + `,"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`, `"document"`},
		{"unknown block", `{` + ok + `,"messages":[{"role":"user","content":[{"type":"hologram"}]}]}`, `"hologram"`},
		{"server tool use", `{` + ok + `,"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"x","name":"web_search","input":{}}]}]}`, `"server_tool_use"`},
		{"search result in tool_result", `{` + ok + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"search_result"}]}]}]}`, `"search_result"`},
		{"bad role", `{` + ok + `,"messages":[{"role":"robot","content":"x"}]}`, `"robot"`},
		{"missing max_tokens", `{"model":"m","messages":[{"role":"user","content":"x"}]}`, "max_tokens"},
		{"zero max_tokens", `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`, "max_tokens"},
		{"fractional max_tokens", `{"model":"m","max_tokens":1.5,"messages":[{"role":"user","content":"x"}]}`, "max_tokens"},
		{"empty messages", `{` + ok + `,"messages":[]}`, "messages"},
		{"missing model", `{"max_tokens":5,"messages":[{"role":"user","content":"x"}]}`, "model"},
		{"not json", `{`, "invalid request body"},
		{"bad tool_choice", `{` + ok + `,"tool_choice":{"type":"sometimes"},"messages":[{"role":"user","content":"x"}]}`, "tool_choice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := DecodeRequest([]byte(tt.body))
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if !strings.Contains(e.Msg, tt.want) {
				t.Errorf("msg %q does not mention %s", e.Msg, tt.want)
			}
		})
	}
}

func TestToolResultSplit(t *testing.T) {
	body := `{"model":"m","max_tokens":5,"messages":[
	 {"role":"user","content":"go"},
	 {"role":"assistant","content":[{"type":"text","text":"calling"},
	   {"type":"tool_use","id":"t1","name":"a","input":{"x":1}},
	   {"type":"tool_use","id":"t2","name":"b","input":{}}]},
	 {"role":"user","content":[
	   {"type":"text","text":"before"},
	   {"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"l1"},{"type":"text","text":"l2"},
	     {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
	   {"type":"image","source":{"type":"url","url":"https://x/y.png"}},
	   {"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"boom"},
	   {"type":"text","text":"after"}]}]}`
	d, info, _ := decode(t, []byte(body))
	if got := roles(d.Messages); !reflect.DeepEqual(got, []string{"user", "assistant", "tool", "tool", "user"}) {
		t.Fatalf("roles = %v", got)
	}
	if d.Messages[1]["content"] != "calling" {
		t.Errorf("assistant content = %v", d.Messages[1]["content"])
	}
	if d.Messages[2]["tool_call_id"] != "t1" || d.Messages[2]["content"] != "l1\nl2" {
		t.Errorf("tool 1 = %v", d.Messages[2])
	}
	if d.Messages[3]["tool_call_id"] != "t2" || d.Messages[3]["content"] != "Error: boom" {
		t.Errorf("tool 2 = %v", d.Messages[3])
	}
	want := []any{
		map[string]any{"type": "text", "text": "before"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/y.png"}},
		map[string]any{"type": "text", "text": "after"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
	}
	if !reflect.DeepEqual(d.Messages[4]["content"], want) {
		t.Errorf("trailing user = %v", d.Messages[4]["content"])
	}
	if len(info.Downgraded) != 0 {
		t.Errorf("downgraded = %v", info.Downgraded)
	}
}

func TestToolResultOnly(t *testing.T) {
	body := `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[
	  {"type":"tool_result","tool_use_id":"t1"},{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`
	d, _, _ := decode(t, []byte(body))
	if got := roles(d.Messages); !reflect.DeepEqual(got, []string{"tool", "user"}) {
		t.Fatalf("roles = %v", got)
	}
	if d.Messages[0]["content"] != "" || d.Messages[1]["content"] != "a\n\nb" {
		t.Errorf("messages = %v", d.Messages)
	}
	body = `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"r"}]}]}`
	d, _, _ = decode(t, []byte(body))
	if got := roles(d.Messages); !reflect.DeepEqual(got, []string{"tool"}) {
		t.Errorf("roles = %v", got)
	}
}
