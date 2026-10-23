package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/usage"
)

// fake is a scripted upstream: each request pops the next handler (the last one repeats).
type fake struct {
	srv   *httptest.Server
	calls atomic.Int32
	steps []http.HandlerFunc
	last  atomic.Value // last request body
}

func newFake(t *testing.T, steps ...http.HandlerFunc) *fake {
	f := &fake{steps: steps}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.last.Store(string(b))
		n := int(f.calls.Add(1)) - 1
		if n >= len(f.steps) {
			n = len(f.steps) - 1
		}
		f.steps[n](w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) lastBody(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	json.Unmarshal([]byte(f.last.Load().(string)), &m)
	return m
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

func okJSON(content string) http.HandlerFunc {
	return status(200, fmt.Sprintf(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"completion_tokens_details":{"reasoning_tokens":3}}}`, content))
}

func sse(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}
}

func fixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/upstream/" + path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type env struct {
	gw    *Gateway
	hub   *events.Hub
	store *usage.Store
	srv   *httptest.Server
	dir   string // usage store directory
}

// newEnv builds a gateway whose providers "a" and "b" (and optional cloud "c") point at fakes.
func newEnv(t *testing.T, extra string, a, b *fake) *env {
	t.Helper()
	doc := fmt.Sprintf(`{
	  "timeouts": {"idle": "300ms", "first_byte_local": "2s", "first_byte_cloud": "2s"},
	  "providers": [
	    {"id": "a", "location": "local", "base_url": %q},
	    {"id": "b", "location": "local", "base_url": %q}
	  ],
	  "routes": {"r": ["a:m1", "b:m2"], "claude-*": ["b:m2"]}%s
	}`, a.srv.URL+"/v1", b.srv.URL+"/v1", extra)
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
	hub := events.NewHub(100, nil, nil)
	gw, err := New(cfg, router.NewHealth(time.Now), hub, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gw.ServeChat(w, r, b)
	}))
	t.Cleanup(srv.Close)
	return &env{gw: gw, hub: hub, store: store, srv: srv, dir: dir}
}

func (e *env) post(t *testing.T, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(e.srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const simple = `{"model":"r","messages":[{"role":"user","content":"hi"}]}`
const streaming = `{"model":"r","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestServesFromFirstTarget(t *testing.T) {
	a, b := newFake(t, okJSON("hello")), newFake(t, okJSON("unused"))
	e := newEnv(t, "", a, b)
	resp, body := e.post(t, simple)
	if resp.StatusCode != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Cbrt-Provider") != "a" || resp.Header.Get("X-Cbrt-Attempts") != "a:m1:ok" || resp.Header.Get("X-Cbrt-Fallback") != "" {
		t.Errorf("headers: %v", resp.Header)
	}
	if a.lastBody(t)["model"] != "m1" {
		t.Errorf("model not rewritten upstream: %v", a.lastBody(t)["model"])
	}
	if b.calls.Load() != 0 {
		t.Error("second target must not be called")
	}
	st := e.store.Stats()
	if len(st.Models) != 1 || st.Models[0].Today.Completed != 1 || st.Models[0].TokensMonth.CompletionReported != 7 {
		t.Errorf("usage not recorded: %+v", st.Models)
	}
}

func TestFailoverOnRateLimitThenSkipDuringCooldown(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		status(429, `{"error":{"message":"slow down"}}`)(w, r)
	})
	b := newFake(t, okJSON("from b"))
	e := newEnv(t, "", a, b)

	resp, body := e.post(t, simple)
	if resp.StatusCode != 200 || !strings.Contains(body, "from b") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Cbrt-Attempts"); got != "a:m1:rate_limited,b:m2:ok" || resp.Header.Get("X-Cbrt-Fallback") != "true" {
		t.Errorf("attempts header %q", got)
	}
	kinds := map[events.Kind]bool{}
	for _, ev := range e.hub.Recent(10) {
		kinds[ev.Kind] = true
	}
	if !kinds[events.KindCooldown] || !kinds[events.KindSwitch] {
		t.Errorf("want cooldown and switch events, got %+v", e.hub.Recent(10))
	}

	resp, _ = e.post(t, simple)
	if got := resp.Header.Get("X-Cbrt-Attempts"); got != "a:m1:skipped(rate_limited),b:m2:ok" {
		t.Errorf("second request should skip the cooling provider, got %q", got)
	}
	if a.calls.Load() != 1 {
		t.Errorf("provider a called %d times during cooldown", a.calls.Load())
	}
}

func TestQuotaAndAuthAreProminent(t *testing.T) {
	a := newFake(t, status(402, `{"error":{"message":"You are out of credits"}}`))
	b := newFake(t, status(403, `{"status":403,"title":"Forbidden","detail":"Authorization failed"}`))
	e := newEnv(t, "", a, b)
	resp, _ := e.post(t, simple)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 should carry Retry-After from the quota cooldown")
	}
	var sawQuota, sawDisabled bool
	for _, ev := range e.hub.Recent(10) {
		sawQuota = sawQuota || (ev.Kind == events.KindCooldown && ev.Reason == string(router.QuotaExhausted))
		sawDisabled = sawDisabled || ev.Kind == events.KindDisabled
	}
	if !sawQuota || !sawDisabled {
		t.Errorf("events: %+v", e.hub.Recent(10))
	}
}

func TestAllRejectedReturns400WithoutCooldown(t *testing.T) {
	a := newFake(t, status(400, `{"error":{"message":"bad tool schema"}}`))
	b := newFake(t, status(422, `{"detail":"extra fields not permitted"}`))
	e := newEnv(t, "", a, b)
	resp, body := e.post(t, simple)
	if resp.StatusCode != 422 && resp.StatusCode != 400 {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "invalid_request_error") {
		t.Errorf("body %s", body)
	}
	e.post(t, simple)
	if a.calls.Load() != 2 {
		t.Error("an upstream 400 must not put the target in cooldown")
	}
}

func TestStreamRelayStripsInjectedUsage(t *testing.T) {
	a := newFake(t, sse(fixture(t, "nvidia/deepseek-v4.1-flash_stream.sse")))
	b := newFake(t, okJSON("unused"))
	e := newEnv(t, "", a, b)
	resp, body := e.post(t, streaming)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	so, _ := a.lastBody(t)["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Error("gateway should request usage upstream")
	}
	if strings.Contains(body, `"usage"`) {
		t.Error("usage must be stripped when the client did not ask for it")
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("finish chunk or [DONE] lost:\n%s", body)
	}
	if !strings.Contains(body, `reasoning_content`) {
		t.Error("reasoning must be passed through to chat clients")
	}
	st := e.store.Stats()
	if st.Models[0].TokensMonth.CompletionReported != 44 {
		t.Errorf("reported usage not captured from the stream: %+v", st.Models[0].TokensMonth)
	}
}

func TestStreamKeepsUsageWhenClientAsks(t *testing.T) {
	a := newFake(t, sse(fixture(t, "ollama/gemma-4-e2b-4_stream.sse")))
	e := newEnv(t, "", a, newFake(t, okJSON("x")))
	_, body := e.post(t, `{"model":"r","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(body, `"usage"`) {
		t.Error("client asked for usage; it must be forwarded")
	}
}

func TestStreamToolCallLayouts(t *testing.T) {
	for _, fx := range []string{
		"nvidia/deepseek-v4.1-flash_parallel-tools_stream.sse",
		"ollama/gemma-4-e2b-4_toolsstream.sse",
		"ollama/minicpm-v4.6_toolsstream.sse",
	} {
		t.Run(fx, func(t *testing.T) {
			e := newEnv(t, "", newFake(t, sse(fixture(t, fx))), newFake(t, okJSON("x")))
			_, body := e.post(t, streaming)
			if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, "Lima") || !strings.Contains(body, "data: [DONE]") {
				t.Errorf("tool calls not relayed intact:\n%.500s", body)
			}
			if strings.Count(body, `"usage"`) != 0 {
				t.Error("usage leaked")
			}
		})
	}
}

func TestStreamEOFBeforeFirstEventFailsOver(t *testing.T) {
	a := newFake(t, sse(""))
	b := newFake(t, sse("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\ndata: [DONE]\n\n"))
	e := newEnv(t, "", a, b)
	resp, body := e.post(t, streaming)
	if resp.Header.Get("X-Cbrt-Provider") != "b" || !strings.Contains(body, `"content":"b"`) {
		t.Fatalf("no failover: %v %s", resp.Header, body)
	}
	if !strings.Contains(resp.Header.Get("X-Cbrt-Attempts"), "a:m1:upstream_error") {
		t.Errorf("attempts %q", resp.Header.Get("X-Cbrt-Attempts"))
	}
}

func TestStreamErrorPayloadBeforeCommitFailsOver(t *testing.T) {
	a := newFake(t, sse("data: {\"error\":{\"message\":\"model overloaded\"}}\n\n"))
	b := newFake(t, sse("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\ndata: [DONE]\n\n"))
	e := newEnv(t, "", a, b)
	resp, _ := e.post(t, streaming)
	if resp.Header.Get("X-Cbrt-Provider") != "b" {
		t.Fatalf("200-with-error must fail over: %v", resp.Header)
	}
}

func TestFirstByteTimeoutFailsOver(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	b := newFake(t, okJSON("fast"))
	e := newEnv(t, "", a, b)
	e.gw.rt.Load().cfg.Routes["r"][0].FirstByteTimeout = config.Duration(100 * time.Millisecond)
	start := time.Now()
	resp, body := e.post(t, simple)
	if !strings.Contains(body, "fast") || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout failover took %v: %s", time.Since(start), body)
	}
	if !strings.Contains(resp.Header.Get("X-Cbrt-Attempts"), "a:m1:timeout") {
		t.Errorf("attempts %q", resp.Header.Get("X-Cbrt-Attempts"))
	}
}

func TestMidStreamFailures(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"eof without done": sse("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"),
		"idle timeout": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			b := newFake(t, okJSON("must not be used"))
			e := newEnv(t, "", newFake(t, h), b)
			resp, body := e.post(t, streaming)
			if resp.StatusCode != 200 || !strings.Contains(body, `"content":"par"`) {
				t.Fatalf("partial content missing: %s", body)
			}
			if !strings.Contains(body, `"type":"upstream_error"`) || strings.Contains(body, "data: [DONE]") {
				t.Errorf("want a terminal error event and no [DONE]:\n%s", body)
			}
			if b.calls.Load() != 0 {
				t.Error("no failover after commit")
			}
			if st := e.store.Stats(); st.Models[0].Today.Partial != 1 {
				t.Errorf("outcome should be partial: %+v", st.Models[0].Today)
			}
		})
	}
}

// Invalid JSON after the first event is not forwarded: the stream ends with the
// error event instead.
func TestMidStreamMalformedChunk(t *testing.T) {
	b := newFake(t, okJSON("must not be used"))
	up := sse("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\ndata: not-json\n\ndata: [DONE]\n\n")
	e := newEnv(t, "", newFake(t, up), b)
	resp, body := e.post(t, streaming)
	if resp.StatusCode != 200 || !strings.Contains(body, `"content":"par"`) {
		t.Fatalf("partial content missing: %s", body)
	}
	if strings.Contains(body, "not-json") || strings.Contains(body, "data: [DONE]") {
		t.Errorf("malformed chunk forwarded or stream completed:\n%s", body)
	}
	if !strings.Contains(body, `"type":"upstream_error"`) || !strings.Contains(body, "malformed stream event") {
		t.Errorf("want a terminal error event:\n%s", body)
	}
	if b.calls.Load() != 0 {
		t.Error("no failover after commit")
	}
	if st := e.store.Stats(); st.Models[0].Today.Partial != 1 {
		t.Errorf("outcome should be partial: %+v", st.Models[0].Today)
	}
}

// A valid chunk whose fields don't fit the chunk struct (array content, as
// some providers send) is forwarded, not treated as malformed.
func TestMidStreamOddlyTypedChunkForwarded(t *testing.T) {
	odd := `{"choices":[{"delta":{"content":[{"type":"text","text":"odd"}]}}]}`
	up := sse("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\ndata: " + odd + "\n\ndata: [DONE]\n\n")
	e := newEnv(t, "", newFake(t, up), newFake(t, okJSON("b")))
	resp, body := e.post(t, streaming)
	if resp.StatusCode != 200 || !strings.Contains(body, odd) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("odd chunk not forwarded or stream not completed:\n%s", body)
	}
}

// failingWriter is a ResponseWriter whose body writes fail (a gone client).
type failingWriter struct {
	h      http.Header
	status int
}

func (w *failingWriter) Header() http.Header       { return w.h }
func (w *failingWriter) WriteHeader(code int)      { w.status = code }
func (w *failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestNonStreamWriteFailureIsCancelled(t *testing.T) {
	a := newFake(t, okJSON("hello"))
	e := newEnv(t, "", a, newFake(t, okJSON("b")))
	w := &failingWriter{h: http.Header{}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(simple))
	e.gw.ServeChat(w, r, []byte(simple))
	if w.status != 200 || a.calls.Load() != 1 {
		t.Fatalf("status %d calls %d", w.status, a.calls.Load())
	}
	recs := e.records(t)
	if len(recs) != 1 || recs[0]["outcome"] != string(usage.OutcomeCancelled) || recs[0]["class"] != string(router.ClientCancelled) {
		t.Fatalf("records %v", recs)
	}
	if recs[0]["prompt_tokens"] != 11.0 {
		t.Errorf("usage counts lost: %v", recs[0])
	}
	if !e.gw.health.Check("a", "a:m1").OK {
		t.Error("a write failure must not penalize the provider")
	}
}

func TestSkipsByCapabilityContextAndOffline(t *testing.T) {
	a, b := newFake(t, okJSON("a")), newFake(t, okJSON("b"))
	e := newEnv(t, "", a, b)
	rt := e.gw.rt.Load()
	rt.cfg.Routes["r"][0].Capabilities = []string{"vision"}
	resp, _ := e.post(t, `{"model":"r","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if got := resp.Header.Get("X-Cbrt-Attempts"); got != "a:m1:skipped(no_tools),b:m2:ok" {
		t.Errorf("capability skip: %q", got)
	}

	rt.cfg.Routes["r"][0].Capabilities = nil
	rt.cfg.Routes["r"][0].MaxContext = 10
	long := `{"model":"r","messages":[{"role":"user","content":"` + strings.Repeat("x", 400) + `"}]}`
	resp, _ = e.post(t, long)
	if got := resp.Header.Get("X-Cbrt-Attempts"); !strings.HasPrefix(got, "a:m1:skipped(context_too_small)") {
		t.Errorf("context skip: %q", got)
	}
}

func TestLearnsContextFromUpstreamError(t *testing.T) {
	ctxErr := fixture(t, "ollama/error_400_context-exceeded.json")
	a := newFake(t, status(400, ctxErr))
	b := newFake(t, okJSON("b"))
	e := newEnv(t, "", a, b)
	big := `{"model":"r","messages":[{"role":"user","content":"` + strings.Repeat("y", 20000) + `"}]}`
	resp, _ := e.post(t, big)
	if !strings.Contains(resp.Header.Get("X-Cbrt-Attempts"), "a:m1:context_exceeded") {
		t.Fatalf("attempts %q", resp.Header.Get("X-Cbrt-Attempts"))
	}
	resp, _ = e.post(t, big)
	if !strings.Contains(resp.Header.Get("X-Cbrt-Attempts"), "a:m1:skipped(context_too_small)") {
		t.Errorf("learned n_ctx not applied: %q", resp.Header.Get("X-Cbrt-Attempts"))
	}
}

func TestRoutingAndValidation(t *testing.T) {
	a, b := newFake(t, okJSON("a")), newFake(t, okJSON("b"))
	e := newEnv(t, "", a, b)
	if resp, _ := e.post(t, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != 404 {
		t.Errorf("unknown model: %d", resp.StatusCode)
	}
	if resp, _ := e.post(t, `{"model":"r"}`); resp.StatusCode != 400 {
		t.Errorf("invalid request: %d", resp.StatusCode)
	}
	if resp, _ := e.post(t, `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`); resp.Header.Get("X-Cbrt-Route") != "claude-*" {
		t.Errorf("glob route not used: %v", resp.Header)
	}
	resp, _ := e.post(t, `{"model":"a:custom:tag","messages":[{"role":"user","content":"hi"}]}`)
	if resp.Header.Get("X-Cbrt-Route") != "direct" || a.lastBody(t)["model"] != "custom:tag" {
		t.Errorf("direct addressing: %v %v", resp.Header, a.lastBody(t)["model"])
	}
}

func TestClientCancelDoesNotPenalize(t *testing.T) {
	started := make(chan struct{})
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}, okJSON("later"))
	e := newEnv(t, "", a, newFake(t, okJSON("b")))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL, strings.NewReader(simple))
	go func() { <-started; cancel() }()
	http.DefaultClient.Do(req)
	time.Sleep(100 * time.Millisecond)

	resp, _ := e.post(t, simple)
	if resp.Header.Get("X-Cbrt-Provider") != "a" {
		t.Errorf("provider a must stay eligible after a client cancel: %q", resp.Header.Get("X-Cbrt-Attempts"))
	}
}

func TestSSECommentAndBodyMetadataOptIn(t *testing.T) {
	a := newFake(t, status(500, `oops`))
	b := newFake(t, sse("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\ndata: [DONE]\n\n"), okJSON("b"))
	e := newEnv(t, `, "visibility": {"sse_comments": true, "body_metadata": true}`, a, b)
	resp, err := http.Post(e.srv.URL, "application/json", strings.NewReader(streaming))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := bufio.NewReader(resp.Body).ReadString('\n')
	resp.Body.Close()
	if !strings.HasPrefix(first, ": cbrt fallback a:m1:upstream_error -> b:m2") {
		t.Errorf("first line %q", first)
	}
	_, body := e.post(t, simple)
	if !strings.Contains(body, `"cbrt":{`) {
		t.Errorf("body metadata missing: %s", body)
	}
}
