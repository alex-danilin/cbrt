package autoconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/config"
)

type fakeModel struct {
	name    string
	caps    []string
	params  string // Modelfile parameters text
	arch    string
	trained int
	size    string
}

func fakeOllama(t *testing.T, models ...fakeModel) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"0.33.3"}`))
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		var ms []map[string]any
		for _, m := range models {
			ms = append(ms, map[string]any{"name": m.name, "model": m.name, "details": map[string]any{"parameter_size": m.size}})
		}
		json.NewEncoder(w).Encode(map[string]any{"models": ms})
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		json.NewDecoder(r.Body).Decode(&req)
		for _, m := range models {
			if m.name == req.Model {
				arch := m.arch
				if arch == "" {
					arch = "gemma4"
				}
				json.NewEncoder(w).Encode(map[string]any{
					"capabilities": m.caps,
					"parameters":   m.params,
					"details":      map[string]any{"parameter_size": m.size},
					"model_info":   map[string]any{"general.architecture": arch, arch + ".context_length": m.trained},
				})
				return
			}
		}
		http.Error(w, `{"error":"not found"}`, 404)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s.URL
}

func deadURL(t *testing.T) string {
	s := httptest.NewServer(http.NotFoundHandler())
	u := s.URL
	s.Close()
	return u
}

func cleanEnv(t *testing.T) {
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "")
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("NVIDIA_API_KEY", "test-key")
	t.Setenv("CBRT_OFFLINE", "")
}

// gen runs Generate and checks the document round-trips through config.Parse.
func gen(t *testing.T, o Options) (*config.Config, []byte, Report) {
	t.Helper()
	doc, rep, err := Generate(context.Background(), o)
	if err != nil {
		t.Fatalf("Generate: %v (report %+v)", err, rep)
	}
	c, err := config.Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, doc)
	}
	again, _, _ := Generate(context.Background(), o)
	if !bytes.Equal(doc, again) {
		t.Fatalf("output not deterministic:\n%s\n---\n%s", doc, again)
	}
	return c, doc, rep
}

func keys(ts []config.Target) string {
	var s []string
	for _, t := range ts {
		s = append(s, t.Key())
	}
	return strings.Join(s, ",")
}

func hasWarning(rep Report, sub string) bool {
	for _, w := range rep.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

var (
	gemma32k = fakeModel{name: "gemma-32k:latest", caps: []string{"completion", "tools", "thinking"}, params: "num_ctx                        32768\nstop \"<turn|>\"", trained: 131072, size: "4.6B"}
	gemma    = fakeModel{name: "gemma:latest", caps: []string{"completion", "tools"}, trained: 131072, size: "4.6B"}
	vis32k   = fakeModel{name: "minicpm-v-32k:latest", caps: []string{"completion", "vision"}, params: "num_ctx 32768", arch: "qwen35", trained: 262144, size: "752.16M"}
	embed    = fakeModel{name: "nomic-embed-text:latest", caps: []string{"embedding"}, params: "num_ctx 8192", arch: "nomic-bert", trained: 8192, size: "137M"}
	chatOnly = fakeModel{name: "phi3:latest", caps: []string{"completion"}, arch: "phi3", trained: 4096, size: "3.8B"}
)

func TestNVIDIAOnly(t *testing.T) {
	cleanEnv(t)
	c, _, rep := gen(t, Options{NVIDIA: true, OllamaURL: deadURL(t)})
	for _, r := range []string{"default", "claude-*", "coder"} {
		if got := keys(c.Routes[r]); got != "nvidia:openai/gpt-oss-20b" {
			t.Errorf("route %s = %s", r, got)
		}
	}
	if _, ok := c.Routes["vision"]; ok {
		t.Error("vision route without a vision model")
	}
	if len(c.Providers) != 1 || c.Providers[0].APIKey != "test-key" || len(c.Providers[0].AuthPatterns) != 1 {
		t.Errorf("providers = %+v", c.Providers)
	}
	cl := c.Routes["claude-*"][0]
	if cl.FirstByteTimeout.D().String() != "30s" || cl.Reasoning != "reasoning_content" || cl.MaxContext != 131072 {
		t.Errorf("claude target = %+v", cl)
	}
	if c.Listen != "127.0.0.1:8080" || c.DataDir != "data" || c.APIKey != "" {
		t.Errorf("listen/data_dir/api_key = %q %q %q", c.Listen, c.DataDir, c.APIKey)
	}
	if !hasWarning(rep, "Ollama is not reachable") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
}

func TestOllamaOnly(t *testing.T) {
	cleanEnv(t)
	t.Setenv("NVIDIA_API_KEY", "")
	u := fakeOllama(t, gemma32k)
	c, doc, rep := gen(t, Options{OllamaURL: u + "/", Listen: "127.0.0.1:9090", DataDir: "d"})
	if got := keys(c.Routes["default"]); got != "ollama:gemma-32k" {
		t.Errorf("default = %s", got)
	}
	tg := c.Routes["coder"][0]
	if tg.MaxContext != 32768 || tg.TokenParam != "max_tokens" || tg.SupportsToolChoiceRequired() || !tg.HasCapability("tools") || tg.HasCapability("vision") {
		t.Errorf("ollama target = %+v", tg)
	}
	if p, _ := c.Provider("ollama"); p.BaseURL != u+"/v1" || p.Location != "local" {
		t.Errorf("ollama provider = %+v", p)
	}
	if c.Listen != "127.0.0.1:9090" || c.DataDir != "d" {
		t.Errorf("listen/data_dir = %q %q", c.Listen, c.DataDir)
	}
	if strings.Contains(string(doc), "nvidia") {
		t.Errorf("nvidia in doc without key:\n%s", doc)
	}
	if hasWarning(rep, "context") {
		t.Errorf("unexpected context warning: %v", rep.Warnings)
	}
	if !strings.Contains(strings.Join(rep.Found, "\n"), "Ollama 0.33.3 at "+u) {
		t.Errorf("found = %v", rep.Found)
	}
}

func TestBoth(t *testing.T) {
	cleanEnv(t)
	c, _, rep := gen(t, Options{NVIDIA: true, OllamaURL: fakeOllama(t, gemma32k, vis32k)})
	for _, r := range []string{"default", "claude-*", "coder"} {
		if got := keys(c.Routes[r]); got != "nvidia:openai/gpt-oss-20b,ollama:gemma-32k" {
			t.Errorf("route %s = %s", r, got)
		}
	}
	if MinContext(c, "claude-*") != 32768 || MinContext(c, "claude-sonnet-4") != 32768 {
		t.Errorf("MinContext = %d", MinContext(c, "claude-*"))
	}
	if !strings.Contains(strings.Join(rep.Found, "\n"), "route claude-* → nvidia:openai/gpt-oss-20b, ollama:gemma-32k") {
		t.Errorf("found = %v", rep.Found)
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("warnings = %v", rep.Warnings)
	}
}

func TestNoNumCtxDefaultsTo4096(t *testing.T) {
	cleanEnv(t)
	c, _, rep := gen(t, Options{OllamaURL: fakeOllama(t, gemma)})
	if tg := c.Routes["default"][0]; tg.MaxContext != 4096 {
		t.Errorf("max_context = %d", tg.MaxContext)
	}
	if !hasWarning(rep, "PARAMETER num_ctx 32768") || !hasWarning(rep, "ollama create gemma-32k -f Modelfile") || !hasWarning(rep, "OLLAMA_CONTEXT_LENGTH") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
}

func TestLargestContextWins(t *testing.T) {
	cleanEnv(t)
	c, _, _ := gen(t, Options{OllamaURL: fakeOllama(t, gemma, gemma32k)})
	if got := keys(c.Routes["coder"]); got != "ollama:gemma-32k" {
		t.Errorf("coder = %s", got)
	}
	// Tie on context: the larger model wins (better at tool use).
	small := fakeModel{name: "small:latest", caps: []string{"completion", "tools"}, params: "num_ctx 32768", trained: 65536, size: "1.5B"}
	c, _, _ = gen(t, Options{OllamaURL: fakeOllama(t, gemma32k, small)})
	if got := keys(c.Routes["coder"]); got != "ollama:gemma-32k" {
		t.Errorf("tie: coder = %s", got)
	}
	// Tie on context: a text-only model beats a vision model, even a larger one;
	// the vision model still serves the vision route.
	vis := fakeModel{name: "vis:latest", caps: []string{"completion", "tools", "vision"}, params: "num_ctx 32768", trained: 65536, size: "9B"}
	c, _, _ = gen(t, Options{OllamaURL: fakeOllama(t, small, vis)})
	if got := keys(c.Routes["coder"]); got != "ollama:small" {
		t.Errorf("text vs vision tie: coder = %s", got)
	}
	if got := keys(c.Routes["vision"]); got != "ollama:vis" {
		t.Errorf("vision = %s", got)
	}
}

func TestContextLengthEnvAndCap(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "65536")
	short := fakeModel{name: "short", caps: []string{"completion", "tools"}, trained: 8192, size: "1B"}
	c, _, rep := gen(t, Options{OllamaURL: fakeOllama(t, gemma, gemma32k, short)})
	// gemma: env 65536 (< trained 131072) beats gemma-32k's num_ctx 32768.
	if tg := c.Routes["default"][0]; tg.Model != "gemma" || tg.MaxContext != 65536 {
		t.Errorf("target = %+v", tg)
	}
	found := strings.Join(rep.Found, "\n")
	if !strings.Contains(found, "short: context 8192 (OLLAMA_CONTEXT_LENGTH, capped at trained max)") ||
		!strings.Contains(found, "gemma-32k: context 32768 (num_ctx)") {
		t.Errorf("found = %v", rep.Found)
	}
}

func TestEmbeddingSkipped(t *testing.T) {
	cleanEnv(t)
	c, doc, rep := gen(t, Options{OllamaURL: fakeOllama(t, embed, gemma32k)})
	if strings.Contains(string(doc), "nomic") {
		t.Errorf("embedding model in doc:\n%s", doc)
	}
	if keys(c.Routes["default"]) != "ollama:gemma-32k" || !strings.Contains(strings.Join(rep.Found, "\n"), "ignored: nomic-embed-text") {
		t.Errorf("found = %v", rep.Found)
	}
}

func TestVisionRoute(t *testing.T) {
	cleanEnv(t)
	vis := fakeModel{name: "llava", caps: []string{"completion", "vision", "tools"}, arch: "llama", trained: 4096, size: "7B"}
	c, _, _ := gen(t, Options{NVIDIA: true, OllamaURL: fakeOllama(t, gemma32k, vis, vis32k)})
	v := c.Routes["vision"]
	if len(v) != 1 || v[0].Model != "minicpm-v-32k" || v[0].MaxContext != 32768 || !v[0].HasCapability("vision") || v[0].HasCapability("tools") {
		t.Errorf("vision = %+v", v)
	}
	if MinContext(c, "vision") != 32768 {
		t.Errorf("MinContext(vision) = %d", MinContext(c, "vision"))
	}
}

func TestNoToolsModel(t *testing.T) {
	cleanEnv(t)
	u := fakeOllama(t, chatOnly, vis32k)
	c, _, rep := gen(t, Options{OllamaURL: u})
	if _, ok := c.Routes["claude-*"]; ok {
		t.Error("agent route without a tools model")
	}
	if _, ok := c.Routes["coder"]; ok {
		t.Error("coder route without a tools model")
	}
	if got := keys(c.Routes["default"]); got != "ollama:minicpm-v-32k" {
		t.Errorf("default = %s", got)
	}
	if !hasWarning(rep, "no Ollama model supports tools") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	// With NVIDIA the agent routes use it alone.
	c, _, _ = gen(t, Options{NVIDIA: true, OllamaURL: u})
	if got := keys(c.Routes["coder"]); got != "nvidia:openai/gpt-oss-20b" {
		t.Errorf("coder = %s", got)
	}
	if got := keys(c.Routes["default"]); got != "nvidia:openai/gpt-oss-20b,ollama:minicpm-v-32k" {
		t.Errorf("default = %s", got)
	}
}

func TestNothingAvailable(t *testing.T) {
	cleanEnv(t)
	for name, u := range map[string]string{"dead": deadURL(t), "embeddings only": fakeOllama(t, embed)} {
		_, _, err := Generate(context.Background(), Options{OllamaURL: u})
		if err == nil || !strings.Contains(err.Error(), "NVIDIA_API_KEY") || !strings.Contains(err.Error(), "Ollama") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestOllamaHost(t *testing.T) {
	for _, tc := range []struct{ opt, env, want string }{
		{"", "", "http://127.0.0.1:11434"},
		{"", "0.0.0.0", "http://127.0.0.1:11434"},
		{"", "0.0.0.0:11500", "http://127.0.0.1:11500"},
		{"", ":11500", "http://127.0.0.1:11500"},
		{"", "ollama:11434", "http://ollama:11434"},
		{"", "http://gpu-box:8000", "http://gpu-box:8000"},
		{"", "http://gpu-box", "http://gpu-box:80"},
		{"", "https://ollama.example.com", "https://ollama.example.com:443"},
		{"", "  [::1]:11434 ", "http://[::1]:11434"},
		{"http://10.0.0.5:11434/v1/", "ignored:1", "http://10.0.0.5:11434"},
		{"", "ftp://x", "http://127.0.0.1:11434"},
	} {
		if got := ollamaBase(tc.opt, tc.env); got != tc.want {
			t.Errorf("ollamaBase(%q, %q) = %q, want %q", tc.opt, tc.env, got, tc.want)
		}
	}
}

func TestOllamaHostEnvUsed(t *testing.T) {
	cleanEnv(t)
	u := fakeOllama(t, gemma32k)
	t.Setenv("OLLAMA_HOST", strings.TrimPrefix(u, "http://"))
	c, _, _ := gen(t, Options{})
	if p, _ := c.Provider("ollama"); p.BaseURL != u+"/v1" {
		t.Errorf("base_url = %q, want %q", p.BaseURL, u+"/v1")
	}
}

func TestMinContext(t *testing.T) {
	c := &config.Config{Routes: map[string][]config.Target{
		"a":     {{MaxContext: 131072}, {MaxContext: 0}, {MaxContext: 32768}},
		"none":  {{}},
		"cl-*":  {{MaxContext: 8000}},
		"empty": nil,
	}}
	for route, want := range map[string]int{"a": 32768, "none": 0, "cl-x": 8000, "cl-*": 8000, "missing": 0, "empty": 0} {
		if got := MinContext(c, route); got != want {
			t.Errorf("MinContext(%q) = %d, want %d", route, got, want)
		}
	}
	if MinContext(nil, "a") != 0 {
		t.Error("nil config")
	}
}

func TestParseParams(t *testing.T) {
	for s, want := range map[string]float64{"4.6B": 4.6e9, "752.16M": 752.16e6, "": 0, "x": 0, "7b": 7e9} {
		if got := parseParams(s); got != want {
			t.Errorf("parseParams(%q) = %v", s, got)
		}
	}
}
