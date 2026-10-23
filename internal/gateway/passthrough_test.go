package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/usage"
)

// canary stands in for the user's own login: it must reach the passthrough
// upstream and nothing else (no file, log line, event or status).
const canary = "sk-ant-oat01-CANARYcanaryCANARYcanary0123456789"

// seen is one request a fake Anthropic upstream received.
type seen struct {
	path, query string
	header      http.Header
	body        []byte
}

// anthropicFake is a scripted Anthropic upstream that records every request.
type anthropicFake struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []seen
	steps []http.HandlerFunc
}

func newAnthropicFake(t *testing.T, steps ...http.HandlerFunc) *anthropicFake {
	f := &anthropicFake{steps: steps}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, seen{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), b})
		step := f.steps[min(len(f.reqs), len(f.steps))-1]
		f.mu.Unlock()
		step(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *anthropicFake) setSteps(steps ...http.HandlerFunc) {
	f.mu.Lock()
	f.steps, f.reqs = steps, nil
	f.mu.Unlock()
}

func (f *anthropicFake) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *anthropicFake) last() seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

type passEnv struct {
	gw     *Gateway
	health *router.Health
	hub    *events.Hub
	store  *usage.Store
	dir    string
	logBuf *syncBuffer
	url    string // Messages endpoint in front of the gateway
	chat   string // Chat Completions endpoint in front of the gateway
	clock  *fakeClock
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newPassEnv routes claude-* to the passthrough provider first, then to the
// translated fake b.
func newPassEnv(t *testing.T, a *anthropicFake, b *fake) *passEnv {
	t.Helper()
	doc := fmt.Sprintf(`{
	  "timeouts": {"idle": "300ms", "first_byte_local": "2s", "first_byte_cloud": "2s"},
	  "providers": [
	    {"id": "anthropic", "type": "anthropic", "base_url": %q},
	    {"id": "b", "location": "local", "base_url": %q}
	  ],
	  "routes": {"claude-*": ["anthropic:*", "b:m2"]}
	}`, a.srv.URL, b.srv.URL+"/v1")
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := usage.Open(usage.Options{Dir: dir, RetentionMonths: 6})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hub := events.NewHub(100, log, nil)
	clock := &fakeClock{t: time.Now()}
	health := router.NewHealth(clock.now)
	gw, err := New(cfg, health, hub, store, log)
	if err != nil {
		t.Fatal(err)
	}
	gw.now = clock.now
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gw.ServeMessages(w, r, body)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gw.ServeChat(w, r, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &passEnv{gw: gw, health: health, hub: hub, store: store, dir: dir, logBuf: logBuf,
		url: srv.URL + "/v1/messages?beta=true", chat: srv.URL + "/v1/chat/completions", clock: clock}
}

// send posts body as Claude Code would, with the canary as its login.
func (e *passEnv) send(t *testing.T, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+canary)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20,some-future-beta-2027-01-01")
	req.Header.Set("Anthropic-Some-Future-Header", "x")
	req.Header.Set("X-Claude-Code-Session-Id", "sess-1")
	req.Header.Set("X-Claude-Code-Agent-Id", "agent-1")
	req.Header.Set("User-Agent", "claude-cli/2.1.285 (external, cli)")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("Cookie", "c=1")
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("X-Cbrt-Internal", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

const anthropicStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":2000,"output_tokens":1}}}` + "\n\n" +
	"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

func anthropicSSE() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Request-Id", "req_upstream_1")
		h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
		h.Set("Set-Cookie", "upstream=1")
		w.WriteHeader(200)
		io.WriteString(w, anthropicStream)
	}
}

func anthropicError(code int, body string, hdr ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

func claudeBody(t *testing.T) string {
	return captureBody(t, "0003-POST-v1_messages.json", map[string]any{"model": "claude-opus-4-8", "stream": true})
}

// claudeBodyJSON is a non-stream request, so the translated fallback can answer with okJSON.
func claudeBodyJSON(t *testing.T) string {
	return captureBody(t, "0003-POST-v1_messages.json", map[string]any{"model": "claude-opus-4-8", "stream": false})
}

func TestPassthroughRelaysUnchanged(t *testing.T) {
	a := newAnthropicFake(t, anthropicSSE())
	b := newFake(t, okJSON("translated"))
	e := newPassEnv(t, a, b)
	body := claudeBody(t)

	resp, got := e.send(t, body)
	if resp.StatusCode != 200 || string(got) != anthropicStream {
		t.Fatalf("status %d, body changed:\n%s", resp.StatusCode, got)
	}
	if b.calls.Load() != 0 {
		t.Fatal("translated target called")
	}
	s := a.last()
	if !bytes.Equal(s.body, []byte(body)) {
		t.Fatal("request body changed on the way to Anthropic")
	}
	if s.path != "/v1/messages" || s.query != "beta=true" {
		t.Fatalf("forwarded to %s?%s", s.path, s.query)
	}
	for k, want := range map[string]string{
		"Authorization":                "Bearer " + canary,
		"Anthropic-Version":            "2023-06-01",
		"Anthropic-Beta":               "oauth-2025-04-20,some-future-beta-2027-01-01",
		"Anthropic-Some-Future-Header": "x",
		"X-Claude-Code-Session-Id":     "sess-1",
		"X-Claude-Code-Agent-Id":       "agent-1",
		"User-Agent":                   "claude-cli/2.1.285 (external, cli)",
	} {
		if g := s.header.Get(k); g != want {
			t.Errorf("upstream %s = %q, want %q", k, g, want)
		}
	}
	for _, k := range []string{"X-Forwarded-For", "Cookie", "Origin", "X-Cbrt-Internal"} {
		if s.header.Get(k) != "" {
			t.Errorf("upstream got %s", k)
		}
	}
	for k, want := range map[string]string{
		"Request-Id":                                 "req_upstream_1",
		"Anthropic-Ratelimit-Unified-Status":         "allowed",
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.42",
		"Content-Type":                               "text/event-stream",
		"X-Cbrt-Provider":                            "anthropic",
		"X-Cbrt-Model":                               "claude-opus-4-8",
	} {
		if g := resp.Header.Get(k); g != want {
			t.Errorf("client %s = %q, want %q", k, g, want)
		}
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("upstream cookie relayed")
	}
	if srv, _ := e.gw.Session("sess-1"); srv.LastTarget != "anthropic:*" {
		t.Errorf("session served by %q", srv.LastTarget)
	}
}

func TestPassthroughPlanLimitFailsOverUntilReset(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour)
	a := newAnthropicFake(t,
		anthropicError(429, `{"type":"error","error":{"type":"rate_limit_error","message":"usage limit"}}`,
			"Anthropic-Ratelimit-Unified-Status", "rejected",
			"Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(reset.Unix(), 10)),
		anthropicSSE())
	b := newFake(t, okJSON("from the free model"))
	e := newPassEnv(t, a, b)
	body := claudeBodyJSON(t)

	resp, got := e.send(t, body)
	if resp.StatusCode != 200 || !strings.Contains(string(got), "from the free model") {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	if resp.Header.Get("X-Cbrt-Fallback") != "true" {
		t.Error("no fallback header")
	}
	chk := e.health.Check("anthropic", "anthropic:*")
	if chk.OK || chk.Reason != router.QuotaExhausted {
		t.Fatalf("anthropic not cooling for quota: %+v", chk)
	}
	if d := chk.Until.Sub(reset); d < -5*time.Second || d > 5*time.Second {
		t.Errorf("cooling until %v, want the reported reset %v", chk.Until, reset)
	}

	// The same session's next request goes straight to the free model.
	e.send(t, body)
	if a.calls() != 1 || b.calls.Load() != 2 {
		t.Fatalf("anthropic calls %d, free calls %d", a.calls(), b.calls.Load())
	}
	// After the reset it goes back to Anthropic.
	e.clock.add(2*time.Hour + time.Minute)
	resp, got = e.send(t, body)
	if a.calls() != 2 || string(got) != anthropicStream {
		t.Fatalf("after reset: anthropic calls %d, status %d", a.calls(), resp.StatusCode)
	}
}

func TestPassthroughThrottleCoolsBriefly(t *testing.T) {
	a := newAnthropicFake(t, anthropicError(429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "Retry-After", "7"))
	b := newFake(t, okJSON("free"))
	e := newPassEnv(t, a, b)
	resp, _ := e.send(t, claudeBodyJSON(t))
	if resp.StatusCode != 200 || b.calls.Load() != 1 {
		t.Fatalf("status %d, free calls %d", resp.StatusCode, b.calls.Load())
	}
	chk := e.health.Check("anthropic", "anthropic:*")
	if chk.Reason != router.RateLimited || chk.Until.Sub(e.clock.now()) > 8*time.Second {
		t.Fatalf("throttle cooldown: %+v", chk)
	}
}

// Answers that are the client's business reach it unchanged, with no failover
// and no cooldown: Claude Code refreshes its login on 401 and drops rejected
// thinking blocks on 400 by itself.
func TestPassthroughClientErrorsRelayed(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
	}{
		{401, `{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired"}}`},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: Invalid signature in thinking block"}}`},
		{413, `{"type":"error","error":{"type":"request_too_large","message":"too large"}}`},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			a := newAnthropicFake(t, anthropicError(tc.code, tc.body, "Request-Id", "req_e", "X-Should-Retry", "false"))
			b := newFake(t, okJSON("free"))
			e := newPassEnv(t, a, b)
			for i := 0; i < 2; i++ {
				resp, got := e.send(t, claudeBody(t))
				if resp.StatusCode != tc.code || string(got) != tc.body {
					t.Fatalf("got %d %s", resp.StatusCode, got)
				}
				if resp.Header.Get("Request-Id") != "req_e" || resp.Header.Get("X-Should-Retry") != "false" {
					t.Error("upstream headers not relayed")
				}
			}
			if a.calls() != 2 || b.calls.Load() != 0 {
				t.Fatalf("anthropic calls %d, free calls %d", a.calls(), b.calls.Load())
			}
		})
	}
}

func TestPassthroughServerErrorFailsOver(t *testing.T) {
	a := newAnthropicFake(t, anthropicError(529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	b := newFake(t, okJSON("free"))
	e := newPassEnv(t, a, b)
	if resp, got := e.send(t, claudeBodyJSON(t)); resp.StatusCode != 200 || !strings.Contains(string(got), "free") {
		t.Fatalf("got %d %s", resp.StatusCode, got)
	}
}

// Only Claude Code's Messages requests are relayed: a Chat Completions client
// on the same route never reaches the passthrough provider.
func TestPassthroughOnlyForMessagesClients(t *testing.T) {
	a := newAnthropicFake(t, anthropicSSE())
	b := newFake(t, okJSON("free"))
	e := newPassEnv(t, a, b)
	req, _ := http.NewRequest(http.MethodPost, e.chat, strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+canary)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || a.calls() != 0 {
		t.Fatalf("status %d, anthropic calls %d", resp.StatusCode, a.calls())
	}
	if !strings.Contains(resp.Header.Get("X-Cbrt-Attempts"), "anthropic:*:skipped(not_anthropic_client)") {
		t.Errorf("attempts %q", resp.Header.Get("X-Cbrt-Attempts"))
	}
}

// A request the translator can't handle is still relayed unchanged; without a
// passthrough target the client gets the translator's 400.
func TestPassthroughRelaysUntranslatable(t *testing.T) {
	body := `{"model":"claude-opus-4-8","max_tokens":10,"messages":[{"role":"user","content":[{"type":"some_future_block","x":1}]}]}`
	a := newAnthropicFake(t, anthropicSSE())
	b := newFake(t, okJSON("free"))
	e := newPassEnv(t, a, b)
	resp, got := e.send(t, body)
	if resp.StatusCode != 200 || string(got) != anthropicStream || !bytes.Equal(a.last().body, []byte(body)) {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	// With Anthropic unavailable, the free model can't take it either.
	a.setSteps(anthropicError(400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
	e.health.Fail("anthropic", "anthropic:*", router.Verdict{Class: router.QuotaExhausted, Scope: router.ScopeProvider, Cooldown: time.Hour})
	resp, _ = e.send(t, body)
	if resp.StatusCode != http.StatusServiceUnavailable || b.calls.Load() != 0 {
		t.Fatalf("cooling anthropic: status %d, free calls %d", resp.StatusCode, b.calls.Load())
	}
}

// cbrt never sends a passthrough provider a request of its own: model
// discovery skips it, so only client requests (with the client's login) reach it.
func TestPassthroughNeverOriginates(t *testing.T) {
	a := newAnthropicFake(t, anthropicSSE())
	b := newFake(t, okJSON("free"))
	e := newPassEnv(t, a, b)
	e.gw.Discover(context.Background())
	if a.calls() != 0 {
		t.Fatalf("discovery called the passthrough provider %d times", a.calls())
	}
}

// The client's login reaches the upstream and nowhere else, whatever happens.
func TestPassthroughCredentialNeverStored(t *testing.T) {
	steps := []http.HandlerFunc{
		anthropicSSE(),
		anthropicError(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key `+canary+`"}}`),
		anthropicError(429, `{"error":{"message":"limit for Bearer `+canary+`"}}`, "Anthropic-Ratelimit-Unified-Status", "rejected"),
		anthropicError(500, `{"error":{"message":"`+canary+`"}}`),
	}
	a := newAnthropicFake(t, steps...)
	b := newFake(t, okJSON("free"))
	e := newPassEnv(t, a, b)
	for i := range steps {
		e.clock.add(8 * 24 * time.Hour) // past any cooldown
		e.send(t, claudeBodyJSON(t))
		if a.calls() != i+1 {
			t.Fatalf("step %d not reached", i)
		}
	}
	e.store.Close()

	if strings.Contains(e.logBuf.String(), "CANARY") {
		t.Error("credential in the log")
	}
	for _, ev := range e.hub.Recent(100) {
		if strings.Contains(fmt.Sprintf("%+v", ev), "CANARY") {
			t.Errorf("credential in event %+v", ev)
		}
	}
	filepath.Walk(e.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("CANARY")) {
			t.Errorf("credential in %s", p)
		}
		return nil
	})
}

func TestUsageScanner(t *testing.T) {
	want := tokenCount{prompt: 2112, completion: 42, cached: 2000, reported: true}
	// any split of the stream into reads gives the same count
	for _, size := range []int{1, 2, 7, 64, len(anthropicStream)} {
		var s usageScanner
		for b := []byte(anthropicStream); len(b) > 0; {
			n := min(size, len(b))
			s.feed(b[:n])
			b = b[n:]
		}
		if got := s.count(); got != want {
			t.Fatalf("chunk %d: %+v, want %+v", size, got, want)
		}
	}
	var s usageScanner
	s.feed([]byte(`{"id":"msg_1","content":[{"type":"text","text":"the word \"usage\" in text"}],"usage":{"input_tokens":5,"cache_read_input_tokens":3,"output_tokens":9,"server_tool_use":{"web_search_requests":0}}}`))
	if got := s.count(); got != (tokenCount{prompt: 8, completion: 9, cached: 3, reported: true}) {
		t.Fatalf("json: %+v", got)
	}
	var none usageScanner
	none.feed([]byte(`{"type":"error"}`))
	if none.count().reported {
		t.Fatal("usage reported without any")
	}
}

// An untranslatable request that Anthropic fails on gets a retryable 503, not
// the translator's 400 (which Claude Code would not retry).
func TestPassthroughUntranslatableUpstreamFailureIs503(t *testing.T) {
	body := `{"model":"claude-opus-4-8","max_tokens":10,"messages":[{"role":"user","content":[{"type":"some_future_block"}]}]}`
	a := newAnthropicFake(t, anthropicError(529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	b := newFake(t, okJSON("free"))
	e := newPassEnv(t, a, b)
	resp, got := e.send(t, body)
	if resp.StatusCode != http.StatusServiceUnavailable || b.calls.Load() != 0 {
		t.Fatalf("status %d, free calls %d: %s", resp.StatusCode, b.calls.Load(), got)
	}
}

// A non-stream answer arrives (headers included) only when it is complete, so
// the first-byte budget for cloud targets doesn't apply to it.
func TestPassthroughSlowNonStreamAnswer(t *testing.T) {
	a := newAnthropicFake(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2500 * time.Millisecond) // past first_byte_cloud (2s) in newPassEnv
		anthropicError(200, `{"id":"msg_1","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)(w, r)
	})
	b := newFake(t, okJSON("free"))
	e := newPassEnv(t, a, b)
	resp, got := e.send(t, claudeBodyJSON(t))
	if resp.StatusCode != 200 || !strings.Contains(string(got), "msg_1") || b.calls.Load() != 0 {
		t.Fatalf("status %d, free calls %d: %s", resp.StatusCode, b.calls.Load(), got)
	}
}
