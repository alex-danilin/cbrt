package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/autoconfig"
	"github.com/alex-danilin/cbrt/internal/config"
)

// fakeOllama is fakeUpstream plus Ollama's native API, so autoconfig can detect
// it. The single model reports tools only while tools is true.
type fakeOllama struct {
	*fakeUpstream
	URL   string // base without /v1
	tools atomic.Bool
}

func newFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()
	f := &fakeOllama{fakeUpstream: newFakeUpstream(t)}
	f.tools.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"0.33.3"}`))
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"` + fakeModel + `:latest","model":"` + fakeModel + `:latest","details":{"parameter_size":"5.1B"}}]}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		caps := `"completion"`
		if f.tools.Load() {
			caps += `,"tools"`
		}
		w.Write([]byte(`{"capabilities":[` + caps + `],"parameters":"num_ctx                        32768","details":{"parameter_size":"5.1B"},` +
			`"model_info":{"general.architecture":"gemma4","gemma4.context_length":131072}}`))
	})
	mux.Handle("/", f.fakeUpstream.Config.Handler)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	f.URL = s.URL
	return f
}

// detectEnv makes detection deterministic: no NVIDIA key (and so no internet), no Ollama overrides.
func detectEnv(t *testing.T) {
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "")
	t.Setenv("CBRT_OFFLINE", "")
}

// startLoader runs serve with load until the test ends.
func startLoader(t *testing.T, load loader) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	addrc := make(chan string, 1)
	go func() { done <- serve(ctx, load, func(a string) { addrc <- a }) }()
	var base string
	select {
	case a := <-addrc:
		base = "http://" + a
	case err := <-done:
		cancel()
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("serve did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("serve did not stop")
		}
	})
	return base
}

// Without -config and without cbrt.json, serve detects providers, prints the
// report, and serves; /api/reload re-runs detection.
func TestServeZeroConfig(t *testing.T) {
	detectEnv(t)
	oll := newFakeOllama(t)
	oll.tools.Store(false) // chat only: just the default route
	dir := t.TempDir()
	var out bytes.Buffer
	load := serveLoader(filepath.Join(dir, "cbrt.json"), false, filepath.Join(dir, "no.env"),
		autoconfig.Options{OllamaURL: oll.URL, Listen: "127.0.0.1:0", DataDir: filepath.Join(dir, "data")}, &out)
	base := startLoader(t, load)

	report := out.String()
	for _, want := range []string{"cbrt.json not found", "Ollama 0.33.3", "route default → ollama:" + fakeModel, "warning: NVIDIA_API_KEY is not set", "no Ollama model supports tools"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if resp, body := post(t, base+"/v1/chat/completions", chatBody); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Good day, friend.") {
		t.Fatalf("chat: %d %s", resp.StatusCode, body)
	}
	coder := `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`
	if resp, body := post(t, base+"/v1/chat/completions", coder); resp.StatusCode == http.StatusOK {
		t.Fatalf("coder route exists before the model supports tools: %s", body)
	}

	// The model gains tool support (e.g. a new model was pulled): a reload picks it up.
	oll.tools.Store(true)
	if code, body := authReq(t, "POST", base+"/api/reload", ""); code != http.StatusOK {
		t.Fatalf("reload: %d %s", code, body)
	}
	if resp, body := post(t, base+"/v1/chat/completions", coder); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Good day, friend.") {
		t.Fatalf("coder after reload: %d %s", resp.StatusCode, body)
	}
	if strings.Count(out.String(), "cbrt.json not found") != 1 {
		t.Errorf("the reload's report must go to the log, not the terminal:\n%s", out.String())
	}
}

// An explicit -config must exist; a cbrt.json that exists is used even without -config.
func TestServeLoaderSource(t *testing.T) {
	detectEnv(t)
	dir := t.TempDir()
	env := filepath.Join(dir, "no.env")
	missing := filepath.Join(dir, "missing.json")
	var out bytes.Buffer
	if _, err := serveLoader(missing, true, env, autoconfig.Options{}, &out)(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("explicit missing -config: err = %v, want not-exist", err)
	}
	if out.Len() != 0 {
		t.Errorf("explicit -config must not detect: %q", out.String())
	}

	path := filepath.Join(dir, "cbrt.json")
	cfg := `{"providers":[{"id":"ollama","type":"ollama","location":"local","base_url":"http://127.0.0.1:1/v1"}],` +
		`"routes":{"from-file":[{"provider":"ollama","model":"m"}]}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := serveLoader(path, false, env, autoconfig.Options{}, &out)()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Routes["from-file"]; !ok || out.Len() != 0 {
		t.Errorf("existing cbrt.json not used: routes %v, output %q", c.RouteNames(), out.String())
	}

	// Detection that finds nothing fails with the report printed.
	out.Reset()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	_, err = serveLoader(missing, false, env, autoconfig.Options{OllamaURL: dead.URL}, &out)()
	if err == nil || !strings.Contains(err.Error(), "no usable provider") || !strings.Contains(out.String(), "Ollama is not reachable") {
		t.Errorf("no provider: err %v, report %q", err, out.String())
	}
}

func TestInit(t *testing.T) {
	detectEnv(t)
	oll := newFakeOllama(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cbrt.json")
	env := filepath.Join(dir, "no.env")
	o := autoconfig.Options{OllamaURL: oll.URL}
	ctx := context.Background()

	var out bytes.Buffer
	if err := initConfig(ctx, path, env, false, o, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "route claude-* → ollama:"+fakeModel) || !strings.Contains(out.String(), "then run `cbrt serve`") {
		t.Errorf("init output:\n%s", out.String())
	}
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(doc)
	if err != nil {
		t.Fatalf("written config does not parse: %v\n%s", err, doc)
	}
	if autoconfig.MinContext(c, "coder") != 32768 {
		t.Errorf("coder context = %d", autoconfig.MinContext(c, "coder"))
	}

	// An existing file is kept without -force ...
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := initConfig(ctx, path, env, false, o, &out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("overwrite without -force: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "mine" {
		t.Errorf("file changed without -force: %q", b)
	}
	// ... and replaced with it.
	if err := initConfig(ctx, path, env, true, o, &out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, doc) {
		t.Errorf("-force did not rewrite the file: %q", b)
	}

	// Nothing usable: an error, and no file.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	none := filepath.Join(dir, "none.json")
	if err := initConfig(ctx, none, env, false, autoconfig.Options{OllamaURL: dead.URL}, &out); err == nil {
		t.Error("init with no provider succeeded")
	}
	if _, err := os.Stat(none); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("init with no provider wrote a file: %v", err)
	}
}
