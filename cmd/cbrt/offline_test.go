package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/provider"
)

// dialGuard records every upstream dial and refuses (and reports) any that
// is not to a loopback address.
type dialGuard struct {
	t        *testing.T
	mu       sync.Mutex
	loopback int
	blocked  []string
}

func (g *dialGuard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		g.mu.Lock()
		g.blocked = append(g.blocked, network+" "+addr)
		g.mu.Unlock()
		g.t.Errorf("offline gateway dialed non-loopback address %s %s", network, addr)
		return nil, fmt.Errorf("dial guard: non-loopback %s refused", addr)
	}
	g.mu.Lock()
	g.loopback++
	g.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (g *dialGuard) counts() (int, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.loopback, append([]string(nil), g.blocked...)
}

const cloudTarget = "nvidia:openai/gpt-oss-20b"

func TestOfflineEndToEnd(t *testing.T) {
	// A proxy variable must not route traffic elsewhere (proxy dials would go
	// through the guard anyway, but keep the setup deterministic).
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	guard := &dialGuard{t: t}
	restore := provider.SetDialContextForTest(guard.dial)
	defer restore()

	up := newFakeUpstream(t)
	cloud := map[string]any{"provider": "nvidia", "model": "openai/gpt-oss-20b", "max_context": 131072}
	local := map[string]any{"provider": "ollama", "model": fakeModel, "max_context": 32768}
	cfg := map[string]any{
		"offline": true,
		"providers": []any{
			map[string]any{"id": "nvidia", "type": "openai", "location": "cloud",
				"base_url": "https://integrate.api.nvidia.com/v1", "api_key": "nvapi-unused"},
			map[string]any{"id": "ollama", "type": "ollama", "location": "local", "base_url": up.URL + "/v1"},
		},
		"routes": map[string]any{
			"default": []any{cloud, local},
			"deep":    []any{cloud, local},
		},
	}
	rs := startServe(t, cfg)
	// serve's own startup discovery ran against every provider it would contact;
	// wait for the local model to show up.
	waitForModel(t, rs.base, "ollama:"+fakeModel)

	calls := []struct {
		name, path, body, want string
	}{
		{"chat", "/v1/chat/completions", chatBody, `"Good day, friend."`},
		{"chat stream", "/v1/chat/completions", chatStreamBody, "data: [DONE]"},
		{"messages", "/v1/messages",
			`{"model":"default","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`, `"Good day, friend."`},
		{"messages stream", "/v1/messages",
			`{"model":"deep","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "message_stop"},
		{"responses", "/v1/responses",
			`{"model":"default","stream":false,"input":"hi"}`, `"Good day, friend."`},
		{"responses stream", "/v1/responses",
			`{"model":"deep","stream":true,"input":[{"role":"user","content":"hi"}]}`, "response.completed"},
	}
	for _, c := range calls {
		resp, body := post(t, rs.base+c.path, c.body)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, c.want) {
			t.Errorf("%s: status %d, want 200 with %q; body: %.400s", c.name, resp.StatusCode, c.want, body)
			continue
		}
		if p := resp.Header.Get("X-Cbrt-Provider"); p != "ollama" {
			t.Errorf("%s: served by %q, want ollama", c.name, p)
		}
		att := resp.Header.Get("X-Cbrt-Attempts")
		if !strings.Contains(att, cloudTarget+":skipped(offline)") || !strings.HasSuffix(att, "ollama:"+fakeModel+":ok") {
			t.Errorf("%s: attempts %q, want the cloud target skipped(offline) then ollama ok", c.name, att)
		}
	}
	if n := up.chatCount(); n != len(calls) {
		t.Errorf("local upstream saw %d chat requests, want %d", n, len(calls))
	}

	resp, body := post(t, rs.base+"/v1/messages/count_tokens", `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "input_tokens") {
		t.Errorf("count_tokens: %d %s", resp.StatusCode, body)
	}
	checkDashboard(t, rs.base)

	// The dashboard has no external dependencies: it still works with the
	// only reachable upstream gone.
	up.Close()
	checkDashboard(t, rs.base)

	if err := rs.stop(t); err != nil {
		t.Errorf("serve returned %v", err)
	}
	loop, blocked := guard.counts()
	if len(blocked) > 0 {
		t.Errorf("non-loopback dials: %v", blocked)
	}
	if loop == 0 {
		t.Error("the dial guard saw no dials at all; is it installed?")
	}
}

func waitForModel(t *testing.T, base, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, body := get(t, base+"/v1/models")
		if resp.StatusCode == http.StatusOK && strings.Contains(body, `"`+id+`"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/v1/models never listed %s: %d %s", id, resp.StatusCode, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var externalURL = regexp.MustCompile(`(?i)https?://`)

func checkDashboard(t *testing.T, base string) {
	t.Helper()
	resp, html := get(t, base+"/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || len(html) < 100 {
		t.Errorf("dashboard: %d %s (%d bytes)", resp.StatusCode, resp.Header.Get("Content-Type"), len(html))
	}
	for _, loc := range externalURL.FindAllStringIndex(html, 5) {
		t.Errorf("dashboard HTML references an absolute URL: %.80q", html[loc[0]:])
	}

	resp, body := get(t, base+"/api/stats")
	var stats struct {
		Offline *bool `json:"offline"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &stats) != nil || stats.Offline == nil || !*stats.Offline {
		t.Errorf("/api/stats: %d, want offline=true; body: %.300s", resp.StatusCode, body)
	}
	for _, p := range []string{"/api/status", "/v1/models", "/healthz"} {
		if resp, body := get(t, base+p); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
}
