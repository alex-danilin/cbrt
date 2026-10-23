package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeClientEnv) != "" {
		os.Exit(fakeClientMain())
	}
	flag.Parse()
	if !testing.Verbose() {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	os.Exit(m.Run())
}

const fakeModel = "gemma-4-e2b-4"

// fakeUpstream is an Ollama-shaped OpenAI-compatible upstream on loopback,
// replaying the recorded fixtures in testdata/upstream/ollama.
type fakeUpstream struct {
	*httptest.Server
	chats int // chat requests seen (guarded by mu)
	mu    sync.Mutex

	// If hold is non-nil, streaming responses send their first event, signal
	// started, then wait for hold (bounded) before sending the rest.
	hold    chan struct{}
	started chan struct{}
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	basic := readFixture(t, "gemma-4-e2b-4_basic.json")
	sse := readFixture(t, "gemma-4-e2b-4_stream.sse")
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[{"id":"`+fakeModel+`","object":"model"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			f.mu.Lock()
			f.chats++
			f.mu.Unlock()
			var req struct {
				Stream bool `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if !req.Stream {
				w.Header().Set("Content-Type", "application/json")
				w.Write(basic)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			rest := sse
			if f.hold != nil {
				i := bytes.Index(sse, []byte("\n\n")) + 2
				w.Write(sse[:i])
				w.(http.Flusher).Flush()
				rest = sse[i:]
				select {
				case f.started <- struct{}{}:
				default:
				}
				select {
				case <-f.hold:
				case <-time.After(10 * time.Second):
				case <-r.Context().Done():
					return
				}
			}
			w.Write(rest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) chatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chats
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "upstream", "ollama", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runningServe is a serve() instance started by startServe.
type runningServe struct {
	base    string // http://127.0.0.1:port
	dataDir string
	cfgPath string
	cancel  context.CancelFunc
	done    chan error
}

// startServe writes cfg (with listen and data_dir filled in) to a temp file
// and runs serve until the test ends or stop is called.
func startServe(t *testing.T, cfg map[string]any) *runningServe {
	t.Helper()
	return startServeEnv(t, cfg, "")
}

// startServeEnv is startServe with an env file (a missing one if envPath is "").
func startServeEnv(t *testing.T, cfg map[string]any, envPath string) *runningServe {
	t.Helper()
	dir := t.TempDir()
	rs := &runningServe{dataDir: filepath.Join(dir, "data"), done: make(chan error, 1)}
	cfg["listen"] = "127.0.0.1:0"
	cfg["data_dir"] = rs.dataDir
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "cbrt.json")
	rs.cfgPath = cfgPath
	if envPath == "" {
		envPath = filepath.Join(dir, "no.env")
	}
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rs.cancel = cancel
	addrc := make(chan string, 1)
	go func() {
		rs.done <- serve(ctx, fileLoader(config.NewEnvFile(envPath), cfgPath), func(addr string) { addrc <- addr })
	}()
	select {
	case addr := <-addrc:
		rs.base = "http://" + addr
	case err := <-rs.done:
		cancel()
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("serve did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-rs.done:
		case <-time.After(20 * time.Second):
			t.Error("serve did not stop")
		}
	})
	return rs
}

// stop cancels serve and waits (bounded) for it to return.
func (rs *runningServe) stop(t *testing.T) error {
	t.Helper()
	rs.cancel()
	select {
	case err := <-rs.done:
		rs.done <- err // let the cleanup see it too
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within 10s of cancel")
		return nil
	}
}

var testClient = &http.Client{Timeout: 20 * time.Second}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	resp, err := testClient.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("POST %s: reading body: %v", url, err)
	}
	return resp, string(b)
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := testClient.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: reading body: %v", url, err)
	}
	return resp, string(b)
}

func localConfig(upstreamURL string) map[string]any {
	return map[string]any{
		"providers": []any{
			map[string]any{"id": "ollama", "type": "ollama", "location": "local", "base_url": upstreamURL + "/v1"},
		},
		"routes": map[string]any{
			"default": []any{map[string]any{"provider": "ollama", "model": fakeModel, "max_context": 32768}},
		},
	}
}

const chatBody = `{"model":"default","messages":[{"role":"user","content":"hi"}]}`
const chatStreamBody = `{"model":"default","stream":true,"messages":[{"role":"user","content":"count"}]}`

func TestServeGracefulShutdown(t *testing.T) {
	up := newFakeUpstream(t)
	up.hold = make(chan struct{})
	up.started = make(chan struct{}, 1)
	rs := startServe(t, localConfig(up.URL))

	const n = 3
	for i := 0; i < n; i++ {
		resp, body := post(t, rs.base+"/v1/chat/completions", chatBody)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Good day, friend.") {
			t.Fatalf("chat %d: %d %s", i, resp.StatusCode, body)
		}
	}

	// A stream in flight when shutdown begins must still complete.
	type streamResult struct {
		status int
		body   string
		err    error
	}
	streamc := make(chan streamResult, 1)
	go func() {
		resp, err := testClient.Post(rs.base+"/v1/chat/completions", "application/json", strings.NewReader(chatStreamBody))
		if err != nil {
			streamc <- streamResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		streamc <- streamResult{status: resp.StatusCode, body: string(b), err: err}
	}()
	select {
	case <-up.started:
	case <-time.After(10 * time.Second):
		t.Fatal("stream never reached the upstream")
	}

	rs.cancel()
	// Wait until the listener is closed (Shutdown has begun), then release the stream.
	addr := strings.TrimPrefix(rs.base, "http://")
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepting 5s after cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-rs.done:
		t.Fatalf("serve returned (%v) while a stream was still in flight", err)
	default:
	}
	close(up.hold)

	select {
	case r := <-streamc:
		if r.err != nil || r.status != http.StatusOK || !strings.Contains(r.body, "data: [DONE]") {
			t.Fatalf("in-flight stream: status=%d err=%v body=%q", r.status, r.err, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight stream did not complete")
	}
	if err := rs.stop(t); err != nil {
		t.Fatalf("serve returned %v, want nil", err)
	}

	// The usage store was flushed on shutdown.
	if _, err := os.Stat(filepath.Join(rs.dataDir, "aggregates.json")); err != nil {
		t.Errorf("aggregates.json: %v", err)
	}
	logs, _ := filepath.Glob(filepath.Join(rs.dataDir, "attempts-*.jsonl"))
	if len(logs) != 1 || !regexp.MustCompile(`attempts-\d{4}-\d{2}\.jsonl$`).MatchString(logs[0]) {
		t.Fatalf("attempt logs = %v", logs)
	}
	b, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != n+1 {
		t.Fatalf("got %d attempt records, want %d:\n%s", len(lines), n+1, b)
	}
	for _, l := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("bad record %q: %v", l, err)
		}
		if !strings.Contains(l, `"ollama"`) || !strings.Contains(l, fakeModel) {
			t.Errorf("record does not name the provider/model: %s", l)
		}
	}
}

func TestServeReportsListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dir := t.TempDir()
	cfg := localConfig("http://127.0.0.1:1")
	cfg["listen"] = ln.Addr().String()
	cfg["data_dir"] = filepath.Join(dir, "data")
	b, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "cbrt.json")
	os.WriteFile(path, b, 0o600)

	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), fileLoader(config.NewEnvFile(filepath.Join(dir, "no.env")), path), nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve on a busy port returned nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve on a busy port did not fail")
	}
}
