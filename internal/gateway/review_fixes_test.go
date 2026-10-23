package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/usage"
)

func TestContentChars(t *testing.T) {
	cases := map[string]int{
		``:        0,
		`null`:    0,
		`"hello"`: 5,
		`[{"type":"text","text":"hello"},{"type":"image_url","image_url":{}},{"type":"text","text":" you"}]`: 9,
		`["odd", {"text":"ab"}, {"text": 3}]`: 2,
		`{"text":"x"}`:                        0,
		`42`:                                  0,
	}
	for in, want := range cases {
		if got := contentChars([]byte(in)); got != want {
			t.Errorf("contentChars(%s) = %d, want %d", in, got, want)
		}
	}
}

// #13: a non-stream answer whose content is an array of parts is valid, not malformed.
func TestNonStreamArrayContentServed(t *testing.T) {
	body := `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"text","text":" world!!"}]},"finish_reason":"stop"}]}`
	a, b := newFake(t, status(200, body)), newFake(t, okJSON("unused"))
	e := newEnv(t, "", a, b)
	resp, got := e.post(t, simple)
	if resp.StatusCode != 200 || resp.Header.Get("X-Cbrt-Provider") != "a" {
		t.Fatalf("status %d provider %q: %s", resp.StatusCode, resp.Header.Get("X-Cbrt-Provider"), got)
	}
	if !sameJSON(t, got, body) {
		t.Errorf("chat body not passed through:\n%s", got)
	}
	if b.calls.Load() != 0 || !e.gw.health.Check("a", "a:m1").OK {
		t.Error("array content must not fail over or count toward the breaker")
	}
	recs := e.records(t)
	if len(recs) != 1 || recs[0]["usage_source"] != string(usage.SourceEstimated) {
		t.Fatalf("records %v", recs)
	}
	if want := float64(estimateTokens(len("hello world!!"))); recs[0]["completion_tokens"] != want {
		t.Errorf("completion estimate %v, want %v", recs[0]["completion_tokens"], want)
	}
}

func TestStreamArrayContentCounted(t *testing.T) {
	up := sse("data: {\"choices\":[{\"delta\":{\"content\":[{\"type\":\"text\",\"text\":\"abcdefgh\"}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ijkl\"}}]}\n\ndata: [DONE]\n\n")
	e := newEnv(t, "", newFake(t, up), newFake(t, okJSON("b")))
	resp, body := e.post(t, streaming)
	if resp.StatusCode != 200 || !strings.Contains(body, "abcdefgh") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream not relayed:\n%s", body)
	}
	recs := e.records(t)
	if len(recs) != 1 || recs[0]["completion_tokens"] != float64(estimateTokens(12)) {
		t.Fatalf("records %v", recs)
	}
}

// #8: headers arrive at once but the body stalls: the first-byte budget still applies.
func TestNonStreamBodyFirstByteTimeoutFailsOver(t *testing.T) {
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	b := newFake(t, okJSON("fast"))
	e := newEnv(t, "", a, b)
	cfg := e.gw.rt.Load().cfg
	cfg.Timeouts.Idle = config.Duration(5 * time.Second)
	cfg.Routes["r"][0].FirstByteTimeout = config.Duration(100 * time.Millisecond)
	start := time.Now()
	resp, body := e.post(t, simple)
	if !strings.Contains(body, "fast") || time.Since(start) > 2*time.Second {
		t.Fatalf("body first-byte failover took %v: %s", time.Since(start), body)
	}
	if !strings.Contains(resp.Header.Get("X-Cbrt-Attempts"), "a:m1:timeout") {
		t.Errorf("attempts %q", resp.Header.Get("X-Cbrt-Attempts"))
	}
	if e.gw.health.Check("a", "a:m1").OK || !e.gw.health.Check("a", "a:other").OK {
		t.Error("a first-byte timeout cools down the target only")
	}
}

// flushErrWriter accepts writes but fails every flush (a broken connection).
type flushErrWriter struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func (w *flushErrWriter) Header() http.Header         { return w.h }
func (w *flushErrWriter) WriteHeader(code int)        { w.status = code }
func (w *flushErrWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *flushErrWriter) FlushError() error           { return errors.New("broken pipe") }

// #15: a failed stream flush ends the stream as a client cancel.
func TestStreamFlushErrorIsCancelled(t *testing.T) {
	// the pause makes the gateway flush after the first event
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\ndata: [DONE]\n\n")
	}
	a, b := newFake(t, up), newFake(t, okJSON("b"))
	e := newEnv(t, "", a, b)
	w := &flushErrWriter{h: http.Header{}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(streaming))
	e.gw.ServeChat(w, r, []byte(streaming))
	if w.status != 200 || b.calls.Load() != 0 {
		t.Fatalf("status %d, b calls %d", w.status, b.calls.Load())
	}
	if strings.Contains(w.buf.String(), "[DONE]") {
		t.Errorf("stream went on after a failed flush:\n%s", w.buf.String())
	}
	recs := e.records(t)
	if len(recs) != 1 || recs[0]["outcome"] != string(usage.OutcomeCancelled) || recs[0]["class"] != string(router.ClientCancelled) {
		t.Fatalf("records %v", recs)
	}
	if !e.gw.health.Check("a", "a:m1").OK {
		t.Error("a flush failure must not penalize the provider")
	}
}

// #9: downgrade kinds are truncated and the remembered set is capped.
func TestNoteDowngradedCapAndTruncation(t *testing.T) {
	var logBuf bytes.Buffer
	g := &Gateway{log: slog.New(slog.NewTextHandler(&logBuf, nil))}
	long := strings.Repeat("é", 50) // 100 bytes
	g.noteDowngraded([]string{long, long})
	want := strings.Repeat("é", 32) + "…"
	if _, ok := g.seenDowngrade[want]; !ok || len(g.seenDowngrade) != 1 {
		t.Fatalf("truncated kind not stored: %q", g.seenDowngrade)
	}
	for i := 0; i < 2*maxDowngradeKinds; i++ {
		g.noteDowngraded([]string{fmt.Sprintf("kind %d", i)})
	}
	if len(g.seenDowngrade) != maxDowngradeKinds {
		t.Errorf("cache holds %d kinds, want %d", len(g.seenDowngrade), maxDowngradeKinds)
	}
	if n := strings.Count(logBuf.String(), "further downgrade kinds not logged"); n != 1 {
		t.Errorf("cap notice logged %d times", n)
	}
	if n := strings.Count(logBuf.String(), "feature downgraded"); n != maxDowngradeKinds {
		t.Errorf("logged %d kinds, want %d", n, maxDowngradeKinds)
	}
	// past the cap the counter still counts
	g.repairToolNames([]byte(`{"name":"exec<|channel|>x"}`), false)
	if d, _ := g.Counters(); d != 1 {
		t.Errorf("downgraded = %d, want 1", d)
	}
}

// #6: a 401 from a request that started before a reload does not disable the
// provider for the new config.
func TestStaleConfigAuthErrorDoesNotDisable(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		status(401, `{"error":{"message":"invalid api key"}}`)(w, r)
	}, okJSON("new key works"))
	b := newFake(t, okJSON("fallback"))
	e := newEnv(t, "", a, b)

	done := make(chan string)
	go func() {
		resp, body := e.post(t, simple)
		done <- resp.Header.Get("X-Cbrt-Provider") + " " + body
	}()
	<-arrived
	nc := *e.gw.Config()
	nc.Providers = append([]config.Provider(nil), nc.Providers...)
	nc.Providers[0].APIKey = "new-key"
	if err := e.gw.Reload(&nc); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := <-done; !strings.HasPrefix(got, "b ") {
		t.Fatalf("in-flight request should fail over: %s", got)
	}
	if !e.gw.health.Check("a", "a:m1").OK {
		t.Fatal("stale-config auth failure disabled the provider")
	}
	resp, body := e.post(t, simple)
	if resp.Header.Get("X-Cbrt-Provider") != "a" || !strings.Contains(body, "new key works") {
		t.Errorf("next request not served by a: %v %s", resp.Header, body)
	}
	recs := e.records(t)
	if len(recs) < 1 || recs[0]["class"] != string(router.AuthError) {
		t.Errorf("stale auth attempt not recorded: %v", recs)
	}
}
