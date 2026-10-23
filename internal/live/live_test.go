//go:build live

// Package live holds opt-in smoke tests against real providers.
//
//	go test -tags live -count=1 -v ./internal/live
//
// NVIDIA tests need NVIDIA_API_KEY (read from the environment or the repo's
// .env); Ollama tests need an Ollama at OLLAMA_URL (default
// http://127.0.0.1:11434) with the model minicpm-v4.6-32k. Each test skips
// cleanly when its provider is unavailable. Token use is kept tiny.
package live

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/gateway"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/server"
	"github.com/alex-danilin/cbrt/internal/usage"
)

const (
	nvidiaModel      = "openai/gpt-oss-20b"
	unavailableModel = "moonshotai/kimi-k2.6" // 404 "not found for account" on the free tier
	ollamaModel      = "minicpm-v4.6-32k"
)

func init() {
	// Existing environment variables win over the file.
	config.LoadEnvFile("../../.env")
}

func ollamaURL() string {
	if u := os.Getenv("OLLAMA_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://127.0.0.1:11434"
}

func needNvidia(t *testing.T) {
	t.Helper()
	if os.Getenv("NVIDIA_API_KEY") == "" {
		t.Skip("NVIDIA_API_KEY not set")
	}
}

func needOllama(t *testing.T) {
	t.Helper()
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(ollamaURL() + "/api/tags")
	if err != nil {
		t.Skipf("Ollama not reachable at %s: %v", ollamaURL(), err)
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&tags)
	for _, m := range tags.Models {
		if m.Name == ollamaModel || m.Name == ollamaModel+":latest" {
			return
		}
	}
	t.Skipf("Ollama at %s has no model %s", ollamaURL(), ollamaModel)
}

// newGateway serves the real providers through an in-process gateway and returns its URL.
func newGateway(t *testing.T) string {
	t.Helper()
	nv := fmt.Sprintf(`{"provider": "nvidia", "model": %q, "first_byte_timeout": "60s", "capabilities": ["tools", "json"], "max_context": 131072}`, nvidiaModel)
	doc := fmt.Sprintf(`{
	  "timeouts": {"idle": "120s"},
	  "providers": [
	    {"id": "nvidia", "type": "openai", "location": "cloud", "base_url": "https://integrate.api.nvidia.com/v1",
	     "api_key": "${NVIDIA_API_KEY}", "auth_patterns": ["authorization failed"], "enabled": %t},
	    {"id": "ollama", "type": "ollama", "location": "local", "base_url": %q}
	  ],
	  "routes": {
	    "nv": [%s],
	    "claude-*": [%s],
	    "failover": [{"provider": "nvidia", "model": %q, "first_byte_timeout": "30s"}, %s],
	    "local": [{"provider": "ollama", "model": %q, "first_byte_timeout": "300s", "max_context": 32768,
	               "token_param": "max_tokens", "tool_choice_required": false}]
	  }
	}`, os.Getenv("NVIDIA_API_KEY") != "", ollamaURL()+"/v1", nv, nv, unavailableModel, nv, ollamaModel)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	store, err := usage.Open(usage.Options{Dir: t.TempDir(), RetentionMonths: 6})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := events.NewHub(100, log, nil)
	gw, err := gateway.New(cfg, router.NewHealth(time.Now), hub, store, log)
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Options{Gateway: gw, Store: store, Hub: hub, Logger: log})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func post(t *testing.T, url string, body any, timeout time.Duration) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	c := &http.Client{Timeout: timeout}
	t0 := time.Now()
	resp, err := c.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s -> %d in %s; provider=%s model=%s attempts=%s", url[strings.LastIndex(url, "/v1"):], resp.StatusCode,
		time.Since(t0).Round(time.Millisecond), resp.Header.Get("X-Cbrt-Provider"),
		resp.Header.Get("X-Cbrt-Model"), resp.Header.Get("X-Cbrt-Attempts"))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %.1000s", resp.StatusCode, out)
	}
	return resp, out
}

type sseEvent struct {
	event string
	data  string
}

func parseSSE(body []byte) []sseEvent {
	var evs []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.data != "" || cur.event != "" {
				evs = append(evs, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "event:"):
			cur.event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			cur.data += strings.TrimSpace(line[len("data:"):])
		}
	}
	if cur.data != "" || cur.event != "" {
		evs = append(evs, cur)
	}
	return evs
}

type chatMessage struct {
	Content          string `json:"content"`
	Reasoning        string `json:"reasoning"`
	ReasoningContent string `json:"reasoning_content"`
}

func chatBody(model string, stream bool) map[string]any {
	return map[string]any{
		"model":      model,
		"stream":     stream,
		"max_tokens": 300,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with exactly one word: pong"}},
	}
}

func TestNvidiaChat(t *testing.T) {
	needNvidia(t)
	url := newGateway(t) + "/v1/chat/completions"
	resp, out := post(t, url, chatBody("nv", false), 90*time.Second)
	if resp.Header.Get("X-Cbrt-Provider") != "nvidia" || resp.Header.Get("X-Cbrt-Model") != nvidiaModel {
		t.Errorf("served by %s/%s", resp.Header.Get("X-Cbrt-Provider"), resp.Header.Get("X-Cbrt-Model"))
	}
	var r struct {
		Choices []struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &r); err != nil || len(r.Choices) == 0 {
		t.Fatalf("decode: %v %.500s", err, out)
	}
	m := r.Choices[0].Message
	t.Logf("content=%q finish=%s usage=%+v", m.Content, r.Choices[0].FinishReason, r.Usage)
	if m.Content == "" && m.Reasoning == "" && m.ReasoningContent == "" {
		t.Error("empty answer")
	}
}

func TestNvidiaChatStream(t *testing.T) {
	needNvidia(t)
	url := newGateway(t) + "/v1/chat/completions"
	resp, out := post(t, url, chatBody("nv", true), 90*time.Second)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type %q", ct)
	}
	evs := parseSSE(out)
	if len(evs) == 0 || evs[len(evs)-1].data != "[DONE]" {
		t.Fatalf("stream does not end with [DONE]: %.500s", out)
	}
	var text, reasoning strings.Builder
	chunks := 0
	for _, ev := range evs[:len(evs)-1] {
		var c struct {
			Choices []struct {
				Delta chatMessage `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(ev.data), &c) != nil {
			continue // SSE comments or metadata
		}
		chunks++
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
			reasoning.WriteString(ch.Delta.Reasoning + ch.Delta.ReasoningContent)
		}
	}
	t.Logf("%d chunks, content=%q, %d reasoning chars", chunks, text.String(), reasoning.Len())
	if text.Len() == 0 && reasoning.Len() == 0 {
		t.Error("empty stream")
	}
}

func TestNvidiaMessagesStream(t *testing.T) {
	needNvidia(t)
	url := newGateway(t) + "/v1/messages"
	resp, out := post(t, url, map[string]any{
		"model":      "claude-sonnet-4-5",
		"stream":     true,
		"max_tokens": 300,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with exactly one word: pong"}},
	}, 90*time.Second)
	if resp.Header.Get("X-Cbrt-Route") != "claude-*" {
		t.Errorf("route %q", resp.Header.Get("X-Cbrt-Route"))
	}
	var types []string
	var text strings.Builder
	for _, ev := range parseSSE(out) {
		if ev.event == "" {
			continue
		}
		types = append(types, ev.event)
		var d struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		json.Unmarshal([]byte(ev.data), &d)
		if d.Delta.Type == "text_delta" {
			text.WriteString(d.Delta.Text)
		}
	}
	t.Logf("%d events, text=%q", len(types), text.String())
	if len(types) < 3 || types[0] != "message_start" || types[len(types)-1] != "message_stop" {
		t.Fatalf("event sequence %v", types)
	}
	if !slices.Contains(types, "content_block_delta") {
		t.Errorf("no content_block_delta in %v", types)
	}
}

func TestNvidiaResponsesToolCall(t *testing.T) {
	needNvidia(t)
	url := newGateway(t) + "/v1/responses"
	_, out := post(t, url, map[string]any{
		"model":             "nv",
		"stream":            true,
		"max_output_tokens": 300,
		"instructions":      "You are a weather assistant. Always answer weather questions by calling get_weather.",
		"input":             "What is the weather in Paris right now?",
		"tool_choice":       "required",
		"tools": []any{map[string]any{
			"type": "function", "name": "get_weather", "description": "Get the current weather for a city.",
			"parameters": map[string]any{
				"type": "object", "required": []string{"city"},
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
			},
		}},
	}, 90*time.Second)
	var terminal string
	var calls []map[string]any
	for _, ev := range parseSSE(out) {
		var d struct {
			Type string         `json:"type"`
			Item map[string]any `json:"item"`
		}
		if json.Unmarshal([]byte(ev.data), &d) != nil {
			continue
		}
		switch d.Type {
		case "response.completed", "response.incomplete", "response.failed":
			terminal = d.Type
		case "response.output_item.done":
			if d.Item["type"] == "function_call" {
				calls = append(calls, d.Item)
			}
		}
	}
	t.Logf("terminal=%s function_calls=%v", terminal, calls)
	if terminal != "response.completed" {
		t.Errorf("terminal event %q", terminal)
	}
	if len(calls) == 0 {
		t.Fatalf("no function_call item: %.2000s", out)
	}
	if calls[0]["name"] != "get_weather" {
		t.Errorf("called %v", calls[0]["name"])
	}
	var args map[string]any
	if s, _ := calls[0]["arguments"].(string); json.Unmarshal([]byte(s), &args) != nil || args["city"] == nil {
		t.Errorf("arguments %v", calls[0]["arguments"])
	}
}

func TestOllamaChat(t *testing.T) {
	needOllama(t)
	url := newGateway(t) + "/v1/chat/completions"
	body := chatBody("local", false)
	body["max_tokens"] = 256 // minicpm-v may think first; leave room for the answer
	resp, out := post(t, url, body, 10*time.Minute)
	if resp.Header.Get("X-Cbrt-Provider") != "ollama" {
		t.Errorf("provider %q", resp.Header.Get("X-Cbrt-Provider"))
	}
	var r struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &r); err != nil || len(r.Choices) == 0 {
		t.Fatalf("decode: %v %.500s", err, out)
	}
	m := r.Choices[0].Message
	t.Logf("content=%q, %d reasoning chars", m.Content, len(m.Reasoning+m.ReasoningContent))
	if m.Content == "" && m.Reasoning == "" && m.ReasoningContent == "" {
		t.Error("empty answer")
	}
}

func TestNvidiaFailoverOnUnavailableModel(t *testing.T) {
	needNvidia(t)
	url := newGateway(t) + "/v1/chat/completions"
	resp, _ := post(t, url, chatBody("failover", false), 120*time.Second)
	attempts := resp.Header.Get("X-Cbrt-Attempts")
	want := "nvidia:" + unavailableModel + ":model_unavailable,nvidia:" + nvidiaModel + ":ok"
	if attempts != want {
		t.Errorf("attempts %q, want %q", attempts, want)
	}
	if resp.Header.Get("X-Cbrt-Model") != nvidiaModel || resp.Header.Get("X-Cbrt-Fallback") != "true" {
		t.Errorf("model %q fallback %q", resp.Header.Get("X-Cbrt-Model"), resp.Header.Get("X-Cbrt-Fallback"))
	}
}
