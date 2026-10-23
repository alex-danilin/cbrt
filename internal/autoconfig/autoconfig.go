// Package autoconfig generates a working cbrt.json from what is available on
// the machine: an NVIDIA API key in the environment and/or a reachable Ollama.
package autoconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
)

// Options says what is available. Zero values select the defaults.
type Options struct {
	NVIDIA    bool         // NVIDIA_API_KEY is available; the doc references it as "${NVIDIA_API_KEY}"
	OllamaURL string       // Ollama base without /v1; "" = $OLLAMA_HOST or "http://127.0.0.1:11434"
	Listen    string       // default "127.0.0.1:8080"
	DataDir   string       // default "data"
	Client    *http.Client // nil = a client with a 3s timeout
}

// Report describes what Generate found, for printing to the user.
type Report struct {
	Found    []string // what was detected and how the routes were built
	Warnings []string // actionable problems, each with its fix
}

const (
	defaultOllama = "http://127.0.0.1:11434"
	ollamaDefCtx  = 4096  // Ollama's context when neither num_ctx nor OLLAMA_CONTEXT_LENGTH is set
	agentMinCtx   = 16384 // below this an agent's first request cannot fit
	nvidiaModel   = "openai/gpt-oss-20b"
)

// Document types: only the fields Generate sets, so the output stays short and
// everything else takes config's defaults.
type docProvider struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	Location     string   `json:"location"`
	BaseURL      string   `json:"base_url"`
	APIKey       string   `json:"api_key,omitempty"`
	AuthPatterns []string `json:"auth_patterns,omitempty"`
}

type docTarget struct {
	Provider           string   `json:"provider"`
	Model              string   `json:"model"`
	FirstByteTimeout   string   `json:"first_byte_timeout,omitempty"`
	Capabilities       []string `json:"capabilities,omitempty"`
	MaxContext         int      `json:"max_context,omitempty"`
	Reasoning          string   `json:"reasoning,omitempty"`
	TokenParam         string   `json:"token_param,omitempty"`
	ToolChoiceRequired *bool    `json:"tool_choice_required,omitempty"`
}

type doc struct {
	Listen    string                 `json:"listen"`
	DataDir   string                 `json:"data_dir"`
	Providers []docProvider          `json:"providers"`
	Routes    map[string][]docTarget `json:"routes"` // encoding/json sorts map keys: stable output
}

// model is one Ollama chat model as probed through the native API.
type model struct {
	Name      string // without a ":latest" suffix
	Tools     bool
	Vision    bool
	Params    float64 // parameter count, 0 = unknown
	Trained   int     // <arch>.context_length, 0 = unknown
	NumCtx    int     // Modelfile PARAMETER num_ctx, 0 = unset
	Effective int     // the context Ollama actually runs the model with
	Source    string  // where Effective came from
}

// Generate probes Ollama (if reachable) and returns a cbrt.json document that
// config.Parse accepts, plus a report. It fails only when there is no usable
// provider at all.
func Generate(ctx context.Context, o Options) ([]byte, Report, error) {
	var rep Report
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	base := ollamaBase(o.OllamaURL, os.Getenv("OLLAMA_HOST"))
	d := doc{Listen: o.Listen, DataDir: o.DataDir, Routes: map[string][]docTarget{}}
	if d.Listen == "" {
		d.Listen = "127.0.0.1:8080"
	}
	if d.DataDir == "" {
		d.DataDir = "data"
	}

	if o.NVIDIA {
		d.Providers = append(d.Providers, docProvider{
			ID: "nvidia", Type: "openai", Location: "cloud",
			BaseURL: "https://integrate.api.nvidia.com/v1", APIKey: "${NVIDIA_API_KEY}",
			AuthPatterns: []string{"authorization failed"},
		})
		rep.Found = append(rep.Found, "NVIDIA build API (key found)")
	} else {
		rep.Warnings = append(rep.Warnings, "NVIDIA_API_KEY is not set: no cloud models. Get a free key at https://build.nvidia.com and set NVIDIA_API_KEY (or put it in .env)")
	}

	version, models, err := probeOllama(ctx, client, base, os.Getenv("OLLAMA_CONTEXT_LENGTH"), &rep)
	if err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("Ollama is not reachable at %s (%v): start it (the Ollama app or `ollama serve`) for a local fallback", base, err))
	} else {
		d.Providers = append(d.Providers, docProvider{ID: "ollama", Type: "ollama", Location: "local", BaseURL: base + "/v1"})
		rep.Found = append(rep.Found, fmt.Sprintf("Ollama %s at %s: %d chat model(s)", version, base, len(models)))
		for _, m := range models {
			rep.Found = append(rep.Found, "  "+describe(m))
		}
	}

	tools := best(models, func(m model) bool { return m.Tools })
	vision := best(models, func(m model) bool { return m.Vision })
	anyChat := best(models, func(model) bool { return true })

	if tools != nil && tools.Effective < agentMinCtx {
		rep.Warnings = append(rep.Warnings, contextWarning(*tools))
	}
	if len(models) > 0 && tools == nil {
		rep.Warnings = append(rep.Warnings, "no Ollama model supports tools, so none can serve Claude Code or Codex: pull one that does (e.g. `ollama pull qwen3`) and run `cbrt init` again")
	}

	for _, r := range []struct {
		name, timeout, reasoning string
		caps                     []string
	}{
		{"default", "20s", "", []string{"tools", "json"}},
		{"claude-*", "30s", "reasoning_content", []string{"tools"}},
		{"coder", "60s", "reasoning_content", []string{"tools"}},
	} {
		var ts []docTarget
		if o.NVIDIA {
			ts = append(ts, docTarget{Provider: "nvidia", Model: nvidiaModel, FirstByteTimeout: r.timeout,
				Capabilities: r.caps, MaxContext: 131072, Reasoning: r.reasoning})
		}
		switch {
		case tools != nil:
			ts = append(ts, ollamaTarget(*tools))
		case r.name == "default" && anyChat != nil:
			// Plain chat still works without tools; agent routes stay empty.
			ts = append(ts, ollamaTarget(*anyChat))
		}
		if len(ts) > 0 {
			d.Routes[r.name] = ts
		}
	}
	if vision != nil {
		d.Routes["vision"] = []docTarget{ollamaTarget(*vision)}
	}

	if len(d.Routes["default"]) == 0 {
		return nil, rep, errors.New("no usable provider: set NVIDIA_API_KEY (free key at https://build.nvidia.com), or start Ollama (the Ollama app or `ollama serve`) with a chat model pulled (e.g. `ollama pull qwen3`)")
	}
	for _, name := range []string{"default", "claude-*", "coder", "vision"} {
		if ts, ok := d.Routes[name]; ok {
			keys := make([]string, len(ts))
			for i, t := range ts {
				keys[i] = t.Provider + ":" + t.Model
			}
			rep.Found = append(rep.Found, fmt.Sprintf("route %s → %s", name, strings.Join(keys, ", ")))
		}
	}

	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, rep, err
	}
	return append(out, '\n'), rep, nil
}

func ollamaTarget(m model) docTarget {
	var caps []string
	if m.Tools {
		caps = append(caps, "tools")
	}
	if m.Vision {
		caps = append(caps, "vision")
	}
	f := false
	return docTarget{Provider: "ollama", Model: m.Name, Capabilities: caps, MaxContext: m.Effective,
		TokenParam: "max_tokens", ToolChoiceRequired: &f}
}

func describe(m model) string {
	var caps []string
	if m.Tools {
		caps = append(caps, "tools")
	}
	if m.Vision {
		caps = append(caps, "vision")
	}
	if len(caps) == 0 {
		caps = append(caps, "chat only")
	}
	return fmt.Sprintf("%s: context %d (%s), %s", m.Name, m.Effective, m.Source, strings.Join(caps, ", "))
}

func contextWarning(m model) string {
	return fmt.Sprintf("Ollama model %s runs with a %d-token context; agents need ~32K (Claude Code's first request is ~30K tokens, and some models silently drop the start of a longer prompt). "+
		"Fix: write a Modelfile with `FROM %s` and `PARAMETER num_ctx 32768`, run `ollama create %s-32k -f Modelfile`, then `cbrt init` again; "+
		"or set OLLAMA_CONTEXT_LENGTH=32768 for the Ollama server and restart it", m.Name, m.Effective, m.Name, m.Name)
}

// best returns the model accepted by ok with the largest effective context.
// Ties go to a text-only model (vision models are usually small and weaker at
// tool use), then the larger parameter count (unknown last), then name.
func best(ms []model, ok func(model) bool) *model {
	var b *model
	for i := range ms {
		m := &ms[i]
		if !ok(*m) {
			continue
		}
		if b == nil || better(*m, *b) {
			b = m
		}
	}
	return b
}

func better(a, b model) bool {
	if a.Effective != b.Effective {
		return a.Effective > b.Effective
	}
	if a.Vision != b.Vision {
		return !a.Vision
	}
	if a.Params != b.Params {
		return a.Params > b.Params // unknown (0) sorts last
	}
	return a.Name < b.Name
}

// ollamaBase picks the Ollama base URL (no /v1): explicit option, then
// OLLAMA_HOST ("host:port", "http://host:port", ":port", "0.0.0.0"), then the default.
func ollamaBase(opt, env string) string {
	s := strings.TrimSpace(opt)
	if s == "" {
		s = strings.TrimSpace(env)
	}
	if s == "" {
		return defaultOllama
	}
	// Same default ports as Ollama's own OLLAMA_HOST parsing.
	defPort := "11434"
	switch {
	case !strings.Contains(s, "://"):
		s = "http://" + s
	case strings.HasPrefix(s, "http://"):
		defPort = "80"
	case strings.HasPrefix(s, "https://"):
		defPort = "443"
	default:
		return defaultOllama
	}
	u, err := url.Parse(s)
	if err != nil {
		return defaultOllama
	}
	host, port := u.Hostname(), u.Port()
	switch host {
	case "", "0.0.0.0", "::": // a bind-all server address; connect over loopback
		host = "127.0.0.1"
	}
	if port == "" {
		port = defPort
	}
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, "/v1")
	return u.Scheme + "://" + net.JoinHostPort(host, port) + p
}

// probeOllama returns the server version and its chat models, sorted by name.
// An error means Ollama is not usable at base.
func probeOllama(ctx context.Context, c *http.Client, base, envCtx string, rep *Report) (string, []model, error) {
	var ver struct {
		Version string `json:"version"`
	}
	if err := call(ctx, c, http.MethodGet, base+"/api/version", nil, &ver); err != nil {
		return "", nil, err
	}
	var tags struct {
		Models []struct {
			Name    string `json:"name"`
			Model   string `json:"model"`
			Details struct {
				ParameterSize string `json:"parameter_size"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := call(ctx, c, http.MethodGet, base+"/api/tags", nil, &tags); err != nil {
		return "", nil, fmt.Errorf("listing models: %w", err)
	}
	envN, _ := strconv.Atoi(strings.TrimSpace(envCtx))

	var out []model
	var skipped []string
	for _, t := range tags.Models {
		name := t.Name
		if name == "" {
			name = t.Model
		}
		var show struct {
			Capabilities []string       `json:"capabilities"`
			Parameters   string         `json:"parameters"`
			ModelInfo    map[string]any `json:"model_info"`
			Details      struct {
				ParameterSize string `json:"parameter_size"`
			} `json:"details"`
		}
		body, _ := json.Marshal(map[string]string{"model": name})
		if err := call(ctx, c, http.MethodPost, base+"/api/show", body, &show); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("Ollama model %s skipped: /api/show failed: %v", name, err))
			continue
		}
		m := model{Name: strings.TrimSuffix(name, ":latest")}
		completion := show.Capabilities == nil // older Ollama without capabilities: assume a chat model
		for _, cp := range show.Capabilities {
			switch cp {
			case "completion":
				completion = true
			case "tools":
				m.Tools = true
			case "vision":
				m.Vision = true
			}
		}
		if !completion {
			skipped = append(skipped, m.Name)
			continue
		}
		ps := show.Details.ParameterSize
		if ps == "" {
			ps = t.Details.ParameterSize
		}
		m.Params = parseParams(ps)
		m.Trained = trainedContext(show.ModelInfo)
		m.NumCtx = numCtx(show.Parameters)
		switch {
		case m.NumCtx > 0:
			m.Effective, m.Source = m.NumCtx, "num_ctx"
		case envN > 0:
			m.Effective, m.Source = envN, "OLLAMA_CONTEXT_LENGTH"
		default:
			m.Effective, m.Source = ollamaDefCtx, "Ollama default"
		}
		if m.Trained > 0 && m.Effective > m.Trained {
			m.Effective, m.Source = m.Trained, m.Source+", capped at trained max"
		}
		out = append(out, m)
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		rep.Found = append(rep.Found, "Ollama non-chat models ignored: "+strings.Join(skipped, ", "))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return ver.Version, out, nil
}

func call(ctx context.Context, c *http.Client, method, u string, body []byte, v any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(data, v)
}

// trainedContext reads "<general.architecture>.context_length", or any
// "*.context_length" key when the architecture is missing.
func trainedContext(info map[string]any) int {
	num := func(v any) int {
		if f, ok := v.(float64); ok {
			return int(f)
		}
		return 0
	}
	if arch, ok := info["general.architecture"].(string); ok {
		if n := num(info[arch+".context_length"]); n > 0 {
			return n
		}
	}
	keys := make([]string, 0, len(info))
	for k := range info {
		if strings.HasSuffix(k, ".context_length") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if n := num(info[k]); n > 0 {
			return n
		}
	}
	return 0
}

// numCtx finds "num_ctx N" in the /api/show parameters text.
func numCtx(params string) int {
	for _, line := range strings.Split(params, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "num_ctx" {
			if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// parseParams reads Ollama's "4.6B" / "752.16M" parameter sizes; 0 = unknown.
func parseParams(s string) float64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 'K':
		mult = 1e3
	case 'M':
		mult = 1e6
	case 'B':
		mult = 1e9
	case 'T':
		mult = 1e12
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return f * mult
}

// MinContext returns the smallest known max_context among the targets of route
// (an exact route name, or a model name matched against the glob routes), or 0.
func MinContext(c *config.Config, route string) int {
	if c == nil {
		return 0
	}
	ts, ok := c.Routes[route]
	if !ok {
		var globs []string
		for g := range c.Routes {
			if strings.ContainsAny(g, "*?[") {
				globs = append(globs, g)
			}
		}
		sort.Strings(globs)
		for _, g := range globs {
			if m, _ := path.Match(g, route); m {
				ts = c.Routes[g]
				break
			}
		}
	}
	min := 0
	for _, t := range ts {
		if t.MaxContext > 0 && (min == 0 || t.MaxContext < min) {
			min = t.MaxContext
		}
	}
	return min
}
