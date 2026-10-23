package gateway

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/api/anthropic"
	"github.com/alex-danilin/cbrt/internal/config"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseChatValidation(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       `[1,2]`,
		"no model":       `{"messages":[{"role":"user","content":"hi"}]}`,
		"empty model":    `{"model":"","messages":[{"role":"user","content":"hi"}]}`,
		"no messages":    `{"model":"m"}`,
		"empty messages": `{"model":"m","messages":[]}`,
		"bad stream":     `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":"yes"}`,
	} {
		if _, err := parseChat([]byte(body)); !errors.Is(err, errBadRequest) {
			t.Errorf("%s: want errBadRequest, got %v", name, err)
		}
	}
}

func TestParseChatDetectsNeeds(t *testing.T) {
	r, err := parseChat([]byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:"}}]}],
		"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"required",
		"response_format":{"type":"json_object"},"max_tokens":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Needs, []string{"tools", "vision", "json"}) || !r.ToolChoiceRequired || !r.Stream || !r.ClientWantsUsage || r.MaxOut != 100 {
		t.Errorf("parsed wrong: %+v", r)
	}
}

func TestUpstreamBody(t *testing.T) {
	r, _ := parseChat([]byte(`{"model":"default","stream":true,"stream_options":{"foo":1},"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":50,"x_unknown":{"keep":[1,2]}}`))
	b, err := r.upstreamBody(config.Target{Provider: "p", Model: "real/model", TokenParam: "max_tokens"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, b)
	if m["model"] != "real/model" {
		t.Errorf("model not rewritten: %v", m["model"])
	}
	if _, ok := m["max_completion_tokens"]; ok || m["max_tokens"] != 50.0 {
		t.Errorf("token param not renamed: %v", m)
	}
	so := m["stream_options"].(map[string]any)
	if so["include_usage"] != true || so["foo"] != 1.0 {
		t.Errorf("stream_options not merged: %v", so)
	}
	if !strings.Contains(string(b), `"x_unknown":{"keep":[1,2]}`) {
		t.Errorf("unknown field not preserved byte-for-byte: %s", b)
	}
	if r.field("model") == nil || string(r.field("model")) != `"default"` {
		t.Error("original request must not be mutated (needed for the next attempt)")
	}
}

func TestUpstreamBodyNoUsageAndClamp(t *testing.T) {
	no := false
	r, _ := parseChat([]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":32000}`))
	m := decode(t, must(r.upstreamBody(config.Target{Model: "x", StreamUsage: &no}, 4096)))
	if _, ok := m["stream_options"]; ok {
		t.Error("stream_options must not be injected when the profile disables stream usage")
	}
	if got := m["max_tokens"].(float64); got >= 4096 || got < 256 {
		t.Errorf("max_tokens not clamped to the context: %v", got)
	}
}

func TestSkipReason(t *testing.T) {
	no := false
	r, _ := parseChat([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{}],"tool_choice":"required"}`))
	cases := []struct {
		t    config.Target
		ctx  int
		want string
	}{
		{config.Target{}, 0, ""},
		{config.Target{Capabilities: []string{"vision"}}, 0, "no_tools"},
		{config.Target{ToolChoiceRequired: &no}, 0, "no_tool_choice_required"},
		{config.Target{}, 1, "context_too_small"},
	}
	for _, c := range cases {
		if got := r.skipReason(c.t, c.ctx); got != c.want {
			t.Errorf("skipReason(%+v, %d) = %q, want %q", c.t, c.ctx, got, c.want)
		}
	}
}

// The output cap is clamped to exactly what the context leaves, never above it.
func TestUpstreamBodyClampFitsContext(t *testing.T) {
	r, _ := parseChat([]byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 4000) + `"}],"max_tokens":32000}`))
	ctx := r.EstPromptTokens + 300
	if reason := r.skipReason(config.Target{}, ctx); reason != "" {
		t.Fatalf("skipped: %s", reason)
	}
	m := decode(t, must(r.upstreamBody(config.Target{Model: "x"}, ctx)))
	if got := m["max_tokens"].(float64); int(got)+r.EstPromptTokens != ctx {
		t.Errorf("max_tokens %v + prompt %d != context %d", got, r.EstPromptTokens, ctx)
	}
}

// A context that leaves less than minOutputTokens (or the client's smaller
// cap) for the answer is too small.
func TestSkipReasonMinOutput(t *testing.T) {
	r, _ := parseChat([]byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 4000) + `"}]}`))
	p := r.EstPromptTokens
	for _, c := range []struct {
		maxOut, ctx int
		want        string
	}{
		{0, p + minOutputTokens, ""},
		{0, p + minOutputTokens - 1, "context_too_small"},
		{0, p, "context_too_small"},
		{1000, p + minOutputTokens - 1, "context_too_small"},
		{50, p + 50, ""},
		{50, p + 49, "context_too_small"},
	} {
		r.MaxOut = c.maxOut
		if got := r.skipReason(config.Target{}, c.ctx); got != c.want {
			t.Errorf("maxOut %d ctx %d: %q, want %q", c.maxOut, c.ctx, got, c.want)
		}
	}
}

// Inline base64 images cost a fixed estimate, not their payload size.
func TestEstimateTokensImages(t *testing.T) {
	img := "data:image/png;base64," + strings.Repeat("A", 200000)
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},` +
		`{"type":"image_url","image_url":{"url":"` + img + `"}}]}]}`
	r, err := parseChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if r.EstPromptTokens > 2000 || r.EstPromptTokens < imageTokenEstimate {
		t.Fatalf("estimate %d", r.EstPromptTokens)
	}
	vision := config.Target{Capabilities: []string{"vision"}}
	if reason := r.skipReason(vision, 32768); reason != "" {
		t.Fatalf("32K vision target skipped: %s", reason)
	}
	// Responses-style input_image (URL string) and a remote URL.
	body = `{"model":"m","messages":[{"role":"user","content":[{"type":"input_image","image_url":"` + img + `"},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`
	r, _ = parseChat([]byte(body))
	if r.EstPromptTokens > 2*imageTokenEstimate+100 || r.EstPromptTokens < 2*imageTokenEstimate {
		t.Fatalf("estimate %d", r.EstPromptTokens)
	}
	// Anthropic Messages base64 images arrive as chat image_url data URLs.
	msg := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"image","source":` +
		`{"type":"base64","media_type":"image/png","data":"` + strings.Repeat("A", 200000) + `"}}]}]}`
	chat, _, err := anthropic.DecodeRequest([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if r, _ = parseChat(chat); r.EstPromptTokens > 2000 {
		t.Fatalf("messages estimate %d", r.EstPromptTokens)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestNormalizeSystem(t *testing.T) {
	const rem = `<system-reminder>\nR\n</system-reminder>` // JSON-escaped
	cases := []struct {
		name, in, want string // want "" = unchanged
	}{
		{"no system", `[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]`, ""},
		{"single leading only", `[{"role":"system","content":"S"},{"role":"user","content":"hi"}]`, ""},
		{"several leading joined", `[{"role":"system","content":"S"},{"role":"system","content":"S2"},{"role":"user","content":"hi"}]`,
			`[{"role":"system","content":"S\n\nS2"},{"role":"user","content":"hi"}]`},
		{"before assistant merges into leading",
			`[{"role":"system","content":"S"},{"role":"user","content":"hi"},{"role":"system","content":"E"},{"role":"user","content":"more"}]`,
			`[{"role":"system","content":"S\n\nE"},{"role":"user","content":"hi"},{"role":"user","content":"more"}]`},
		{"before assistant creates leading",
			`[{"role":"user","content":"hi"},{"role":"system","content":"E"}]`,
			`[{"role":"system","content":"E"},{"role":"user","content":"hi"}]`},
		{"multiple leading joined",
			`[{"role":"system","content":"A"},{"role":"system","content":"B"},{"role":"user","content":"hi"},{"role":"system","content":"C"}]`,
			`[{"role":"system","content":"A\n\nB\n\nC"},{"role":"user","content":"hi"}]`},
		{"after assistant appends to user string",
			`[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"u","name":"x"},{"role":"system","content":"R"}]`,
			`[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"u\n\n` + rem + `","name":"x"}]`},
		{"after assistant follows assistant",
			`[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"system","content":"R"}]`,
			`[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"` + rem + `"}]`},
		{"after tool message",
			`[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"r"},{"role":"system","content":"R"}]`,
			`[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"r"},{"role":"user","content":"` + rem + `"}]`},
		{"after user with array content",
			`[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]},{"role":"system","content":"R"}]`,
			`[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]},{"role":"user","content":"` + rem + `"}]`},
		{"array content flattened",
			`[{"role":"system","content":[{"type":"text","text":"S1"},{"type":"text","text":"S2"}]},{"role":"user","content":"q"},{"role":"system","content":[{"type":"text","text":"E1"},{"type":"text","text":"E2"}]},{"role":"assistant","content":"a"},{"role":"system","content":[{"type":"text","text":"R"}]}]`,
			`[{"role":"system","content":"S1\n\nS2\n\nE1\n\nE2"},{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"` + rem + `"}]`},
		{"not an array", `"oops"`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed := normalizeSystem(json.RawMessage(c.in))
			if c.want == "" {
				if changed || string(out) != c.in {
					t.Errorf("want unchanged, got changed=%v %s", changed, out)
				}
				return
			}
			var got, want any
			json.Unmarshal(out, &got)
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatal(err)
			}
			if !changed || !reflect.DeepEqual(got, want) {
				t.Errorf("changed=%v\n got %s\nwant %s", changed, out, c.want)
			}
		})
	}
}

func TestUpstreamBodyMidSystem(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"system","content":"R"}]}`
	r, err := parseChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	keep := decode(t, must(r.upstreamBody(config.Target{Model: "x", MidSystem: "keep"}, 0)))
	if msgs := keep["messages"].([]any); len(msgs) != 3 || msgs[2].(map[string]any)["role"] != "system" {
		t.Errorf("mid_system keep must leave messages untouched: %v", msgs)
	}
	if !strings.Contains(string(must(r.upstreamBody(config.Target{Model: "x", MidSystem: "keep"}, 0))),
		`"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"system","content":"R"}]`) {
		t.Error("mid_system keep must pass messages byte-for-byte")
	}
	for _, mode := range []string{"", "hoist_or_user"} {
		m := decode(t, must(r.upstreamBody(config.Target{Model: "x", MidSystem: mode}, 0)))
		msgs := m["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		if len(msgs) != 3 || last["role"] != "user" || !strings.Contains(last["content"].(string), "<system-reminder>\nR\n</system-reminder>") {
			t.Errorf("mode %q: %v", mode, msgs)
		}
	}
	if string(r.field("messages")) == "" || !strings.Contains(string(r.field("messages")), `"role":"system"`) {
		t.Error("original messages must not be mutated")
	}
}
