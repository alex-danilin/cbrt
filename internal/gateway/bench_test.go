package gateway

// Gateway overhead benchmarks (results in docs/benchmarks.md).
//
// Every benchmark posts over loopback HTTP to a fake upstream that answers
// instantly from memory. The Baseline* benchmarks post the same body straight
// to that upstream; the Gateway* benchmarks post it to the gateway, which
// parses, translates, forwards, relays and records the request. The overhead
// is Gateway* minus the matching Baseline*.
//
//	go test -run '^$' -bench . -benchmem ./internal/gateway
//
// Besides ns/op, each benchmark reports p50-ms and p99-ms per request.

import (
	"bytes"
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
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/usage"
)

// benchUpstream serves h for every request; unlike newFake it takes a testing.TB.
func benchUpstream(tb testing.TB, h http.HandlerFunc) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		h(w, r)
	}))
	tb.Cleanup(srv.Close)
	return srv
}

// benchGateway starts a gateway (one provider, routes "r" and "claude-*") in
// front of up and returns the URLs of its three client APIs.
func benchGateway(tb testing.TB, up *httptest.Server) (chat, messages, resp string) {
	doc := fmt.Sprintf(`{
	  "timeouts": {"idle": "5s", "first_byte_local": "5s", "first_byte_cloud": "5s"},
	  "providers": [{"id": "a", "location": "local", "base_url": %q}],
	  "routes": {"r": ["a:m1"], "claude-*": ["a:m1"]}
	}`, up.URL+"/v1")
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		tb.Fatal(err)
	}
	store, err := usage.Open(usage.Options{Dir: tb.TempDir(), RetentionMonths: 6})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { store.Close() })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := New(cfg, router.NewHealth(time.Now), events.NewHub(100, nil, nil), store, quiet)
	if err != nil {
		tb.Fatal(err)
	}
	mux := http.NewServeMux()
	for path, serve := range map[string]func(http.ResponseWriter, *http.Request, []byte){
		"/chat": gw.ServeChat, "/messages": gw.ServeMessages, "/responses": gw.ServeResponses,
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			serve(w, r, b)
		})
	}
	srv := httptest.NewServer(mux)
	tb.Cleanup(srv.Close)
	return srv.URL + "/chat", srv.URL + "/messages", srv.URL + "/responses"
}

func benchFixture(tb testing.TB, path string) []byte {
	b, err := os.ReadFile("../../testdata/" + path)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// benchCapture returns a captured client request body with fields overridden.
func benchCapture(tb testing.TB, path string, set map[string]any) []byte {
	var capt struct {
		RequestBody map[string]json.RawMessage `json:"request_body"`
	}
	if err := json.Unmarshal(benchFixture(tb, path), &capt); err != nil {
		tb.Fatal(err)
	}
	for k, v := range set {
		capt.RequestBody[k], _ = json.Marshal(v)
	}
	b, _ := json.Marshal(capt.RequestBody)
	return b
}

// runPost posts body to url b.N times, reading each response fully, and
// reports per-request latency percentiles next to ns/op.
func runPost(b *testing.B, url string, body []byte) {
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
	b.Cleanup(client.CloseIdleConnections)
	post := func() {
		resp, err := client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || n == 0 {
			b.Fatalf("status %d, %d bytes", resp.StatusCode, n)
		}
	}
	post() // warm the connection and the gateway
	lat := make([]time.Duration, 0, b.N)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		post()
		lat = append(lat, time.Since(t0))
	}
	b.StopTimer()
	slices.Sort(lat)
	pct := func(p float64) float64 { return float64(lat[int(p*float64(len(lat)-1))]) / 1e6 }
	b.ReportMetric(pct(0.50), "p50-ms")
	b.ReportMetric(pct(0.99), "p99-ms")
}

type benchCase struct {
	name     string
	api      string // chat | messages | responses
	body     func(testing.TB) []byte
	upstream func(testing.TB) http.HandlerFunc
}

var chatNonStream = func(testing.TB) []byte {
	return []byte(`{"model":"r","messages":[{"role":"user","content":"hi"}]}`)
}

var toolsStream = func(tb testing.TB) http.HandlerFunc {
	return sse(string(benchFixture(tb, "upstream/ollama/gemma-4-e2b-4_toolsstream.sse")))
}

var toolCalls = func(testing.TB) http.HandlerFunc {
	return toolCallsJSON(
		[3]string{"call_1", "Glob", `{"pattern":"**/*"}`},
		[3]string{"call_2", "Read", `{"file_path":"notes.md"}`},
	)
}

var benchCases = []benchCase{
	{"ChatNonStream", "chat", chatNonStream,
		func(testing.TB) http.HandlerFunc { return okJSON("hello") }},
	{"ChatNonStream50KB", "chat", // the load-test body size
		func(testing.TB) []byte {
			b, _ := json.Marshal(map[string]any{"model": "r", "messages": []any{
				map[string]any{"role": "system", "content": "You are a helpful assistant."},
				map[string]any{"role": "user", "content": strings.Repeat("The quick brown fox jumps over the lazy dog. ", 50<<10/45)},
			}})
			return b
		},
		func(testing.TB) http.HandlerFunc { return okJSON("hello") }},
	{"ChatStream", "chat",
		func(testing.TB) []byte {
			return []byte(`{"model":"r","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		}, toolsStream},
	{"MessagesNonStream", "messages",
		func(tb testing.TB) []byte {
			return benchCapture(tb, "clients/claude-code-2.1.284/0016-POST-v1_messages.json",
				map[string]any{"model": "claude-sonnet-4-5", "stream": false})
		}, toolCalls},
	{"MessagesStream", "messages",
		func(tb testing.TB) []byte {
			return benchCapture(tb, "clients/claude-code-2.1.284/0016-POST-v1_messages.json",
				map[string]any{"model": "claude-sonnet-4-5", "stream": true})
		}, toolsStream},
	{"ResponsesNonStream", "responses",
		func(tb testing.TB) []byte {
			return benchCapture(tb, "clients/codex-0.154.0/0003-POST-v1_responses.json",
				map[string]any{"model": "r", "stream": false})
		}, toolCalls},
	{"ResponsesStream", "responses",
		func(tb testing.TB) []byte {
			return benchCapture(tb, "clients/codex-0.154.0/0003-POST-v1_responses.json",
				map[string]any{"model": "r", "stream": true})
		}, toolsStream},
}

// BenchmarkBaseline posts each case's body straight to the fake upstream (no gateway).
func BenchmarkBaseline(b *testing.B) {
	for _, c := range benchCases {
		b.Run(c.name, func(b *testing.B) {
			up := benchUpstream(b, c.upstream(b))
			runPost(b, up.URL+"/v1/chat/completions", c.body(b))
		})
	}
}

// BenchmarkGateway posts each case's body through the gateway to the same fake upstream.
func BenchmarkGateway(b *testing.B) {
	for _, c := range benchCases {
		b.Run(c.name, func(b *testing.B) {
			up := benchUpstream(b, c.upstream(b))
			chat, messages, resp := benchGateway(b, up)
			url := map[string]string{"chat": chat, "messages": messages, "responses": resp}[c.api]
			runPost(b, url, c.body(b))
		})
	}
}
