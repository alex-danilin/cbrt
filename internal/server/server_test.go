package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/gateway"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/usage"
)

func newServer(t *testing.T, extra string) (*Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"m1"}]}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "providers": [{"id": "a", "location": "local", "base_url": %q}],
	  "routes": {"r": ["a:m1"]}%s}`, up.URL+"/v1", extra)))
	if err != nil {
		t.Fatal(err)
	}
	store, err := usage.Open(usage.Options{Dir: t.TempDir(), RetentionMonths: 6})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	hub := events.NewHub(10, nil, nil)
	gw, err := gateway.New(cfg, router.NewHealth(time.Now), hub, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Gateway: gw, Store: store, Hub: hub, Reload: func() (*config.Config, error) { return cfg, nil }})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

const chatBody = `{"model":"r","messages":[{"role":"user","content":"hi"}]}`

func do(t *testing.T, method, u, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestNoKeyMeansOpenLocally(t *testing.T) {
	_, ts := newServer(t, "")
	if r := do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, nil); r.StatusCode != 200 {
		t.Errorf("chat: %d", r.StatusCode)
	}
	if r := do(t, "GET", ts.URL+"/", "", nil); r.StatusCode != 200 {
		t.Errorf("dashboard: %d", r.StatusCode)
	}
}

func TestAPIKeyRequired(t *testing.T) {
	_, ts := newServer(t, `, "api_key": "secret"`)
	if r := do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, nil); r.StatusCode != 401 {
		t.Errorf("missing key: %d", r.StatusCode)
	}
	if r := do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, map[string]string{"Authorization": "Bearer wrong"}); r.StatusCode != 401 {
		t.Errorf("wrong key: %d", r.StatusCode)
	}
	for _, h := range []map[string]string{{"Authorization": "Bearer secret"}, {"X-Api-Key": "secret"}} {
		if r := do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, h); r.StatusCode != 200 {
			t.Errorf("header %v: %d", h, r.StatusCode)
		}
	}
	if r := do(t, "GET", ts.URL+"/healthz", "", nil); r.StatusCode != 200 {
		t.Errorf("healthz must not need auth: %d", r.StatusCode)
	}
	if r := do(t, "GET", ts.URL+"/", "", nil); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
		t.Errorf("dashboard should redirect to login: %d %v", r.StatusCode, r.Header)
	}
}

func TestLoginFlowAndThrottle(t *testing.T) {
	_, ts := newServer(t, `, "api_key": "secret"`)
	form := func(k string) string { return url.Values{"key": {k}}.Encode() }
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

	r := do(t, "POST", ts.URL+"/login", form("secret"), ct)
	var cookie *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if r.StatusCode != http.StatusSeeOther || cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("login: %d %+v", r.StatusCode, cookie)
	}
	if r := do(t, "GET", ts.URL+"/api/stats", "", map[string]string{"Cookie": sessionCookie + "=" + cookie.Value}); r.StatusCode != 200 {
		t.Errorf("session cookie not accepted: %d", r.StatusCode)
	}
	for i := 0; i < 4; i++ {
		do(t, "POST", ts.URL+"/login", form("bad"), ct)
	}
	if r := do(t, "POST", ts.URL+"/login", form("secret"), ct); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("6th attempt within a minute must be throttled: %d", r.StatusCode)
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	_, ts := newServer(t, "")
	if r := do(t, "POST", ts.URL+"/api/reload", "", map[string]string{"Origin": "https://evil.example"}); r.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d", r.StatusCode)
	}
	if r := do(t, "POST", ts.URL+"/api/reload", "", nil); r.StatusCode != 200 {
		t.Errorf("same-origin reload: %d", r.StatusCode)
	}
}

func TestAllowedHosts(t *testing.T) {
	_, ts := newServer(t, `, "allowed_hosts": ["gateway.example"]`)
	if r := do(t, "GET", ts.URL+"/healthz", "", nil); r.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("unlisted host: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
	req.Host = "gateway.example"
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("listed host: %d", resp.StatusCode)
	}
}

func TestLimits(t *testing.T) {
	s, ts := newServer(t, `, "limits": {"max_request_bytes": 100, "requests_per_minute": 3}`)
	if r := do(t, "POST", ts.URL+"/v1/chat/completions", `{"model":"r","messages":[{"role":"user","content":"`+strings.Repeat("x", 200)+`"}]}`, nil); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d", r.StatusCode)
	}
	do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, nil)
	do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, nil)
	if r := do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, nil); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("rate limit: %d", r.StatusCode)
	}
	// The handler releases its reservation in a defer that runs after the
	// response reached the client, so wait for it rather than read at once.
	deadline := time.Now().Add(5 * time.Second)
	for s.inflight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := s.inflight.Load(); n != 0 {
		t.Errorf("in-flight bytes leaked: %d", n)
	}
}

func TestInflightBudget(t *testing.T) {
	s, ts := newServer(t, `, "limits": {"max_request_bytes": 100, "max_inflight_bytes": 100}`)
	s.inflight.Add(50) // another request holds half the budget
	if r := do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, nil); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("over budget: %d", r.StatusCode)
	}
	if s.inflight.Load() != 50 {
		t.Errorf("budget not released: %d", s.inflight.Load())
	}
}

func TestStatsStatusAndModels(t *testing.T) {
	s, ts := newServer(t, "")
	do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, map[string]string{"X-Claude-Code-Session-Id": "sess-1"})
	s.gw.Discover(t.Context())

	var stats map[string]any
	json.NewDecoder(do(t, "GET", ts.URL+"/api/stats", "", nil).Body).Decode(&stats)
	for _, k := range []string{"generated_at", "offline", "providers", "models", "daily", "routes", "events", "health"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("/api/stats missing %q", k)
		}
	}
	p := stats["providers"].([]any)[0].(map[string]any)
	if p["provider"] != "a" || p["status"] != "healthy" || p["today"].(map[string]any)["completed"] != 1.0 {
		t.Errorf("provider entry: %v", p)
	}

	var st map[string]any
	json.NewDecoder(do(t, "GET", ts.URL+"/api/status?session=sess-1", "", nil).Body).Decode(&st)
	if sess, ok := st["session"].(map[string]any); !ok || sess["last_target"] != "a:m1" {
		t.Errorf("session status: %v", st)
	}

	var models struct{ Data []struct{ ID string } }
	json.NewDecoder(do(t, "GET", ts.URL+"/v1/models", "", nil).Body).Decode(&models)
	var ids []string
	for _, m := range models.Data {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "r,a:m1" {
		t.Errorf("models: %v", ids)
	}
}

func TestDashboardIsSelfContained(t *testing.T) {
	if len(dashboardHTML) == 0 {
		t.Fatal("dashboard not embedded")
	}
	if m := regexp.MustCompile(`(?i)https?://`).Find(dashboardHTML); m != nil {
		t.Errorf("dashboard references an external URL (%s); it must work offline", m)
	}
	if !strings.Contains(string(dashboardHTML), "/api/stats") {
		t.Error("dashboard does not poll /api/stats")
	}
}

const messagesBody = `{"model":"r","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

func anthropicError(t *testing.T, r *http.Response) (string, string) {
	t.Helper()
	var eb struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&eb); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	return eb.Type, eb.Error.Type
}

func TestMessagesEndpointAuth(t *testing.T) {
	_, ts := newServer(t, `, "api_key": "secret"`)
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		r := do(t, "POST", ts.URL+path, messagesBody, nil)
		if r.StatusCode != 401 {
			t.Errorf("%s without key: %d", path, r.StatusCode)
			continue
		}
		if typ, et := anthropicError(t, r); typ != "error" || et != "authentication_error" {
			t.Errorf("%s: 401 not Anthropic-shaped: %q %q", path, typ, et)
		}
	}
	r := do(t, "POST", ts.URL+"/v1/messages", messagesBody, map[string]string{"X-Api-Key": "secret", "Anthropic-Version": "2023-06-01"})
	var msg struct {
		Type    string `json:"type"`
		Model   string `json:"model"`
		Content []struct{ Type, Text string }
	}
	json.NewDecoder(r.Body).Decode(&msg)
	if r.StatusCode != 200 || msg.Type != "message" || msg.Model != "r" || len(msg.Content) != 1 || msg.Content[0].Text != "hi" {
		t.Errorf("x-api-key on /v1/messages: %d %+v", r.StatusCode, msg)
	}
	// The chat endpoint keeps OpenAI-shaped errors.
	r = do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, nil)
	var oe struct {
		Type  string                `json:"type"`
		Error struct{ Type string } `json:"error"`
	}
	json.NewDecoder(r.Body).Decode(&oe)
	if oe.Type != "" || oe.Error.Type != "authentication_error" {
		t.Errorf("chat 401 shape: %+v", oe)
	}
}

func TestCountTokensIsLocal(t *testing.T) {
	s, ts := newServer(t, "")
	body := `{"model":"claude-sonnet-4-5","system":"You are helpful.","messages":[{"role":"user","content":"` + strings.Repeat("word ", 200) + `"}],
		"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}]}`
	r := do(t, "POST", ts.URL+"/v1/messages/count_tokens", body, nil)
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	n, ok := out["input_tokens"].(float64)
	if r.StatusCode != 200 || !ok || n <= 0 || len(out) != 1 {
		t.Fatalf("count_tokens: %d %v", r.StatusCode, out)
	}
	if st := s.store.Stats(); len(st.Models) != 0 {
		t.Errorf("count_tokens must not reach an upstream: %+v", st.Models)
	}
	r = do(t, "POST", ts.URL+"/v1/messages/count_tokens", `not json`, nil)
	if r.StatusCode != 400 {
		t.Errorf("invalid count_tokens body: %d", r.StatusCode)
	} else if typ, et := anthropicError(t, r); typ != "error" || et != "invalid_request_error" {
		t.Errorf("count_tokens 400 shape: %q %q", typ, et)
	}
}

const responsesBody = `{"model":"r","input":"hi"}`

func TestResponsesEndpointAuth(t *testing.T) {
	_, ts := newServer(t, `, "api_key": "secret"`)
	r := do(t, "POST", ts.URL+"/v1/responses", responsesBody, nil)
	var eb struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.NewDecoder(r.Body).Decode(&eb)
	if r.StatusCode != 401 || eb.Type != "" || eb.Error.Type != "authentication_error" || r.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("/v1/responses without key: %d %+v", r.StatusCode, eb)
	}
	r = do(t, "POST", ts.URL+"/v1/responses", responsesBody, map[string]string{"Authorization": "Bearer secret", "Originator": "codex_exec"})
	var resp struct {
		Object, Status, Model string
		Output                []struct {
			Type    string
			Content []struct{ Type, Text string }
		}
	}
	json.NewDecoder(r.Body).Decode(&resp)
	if r.StatusCode != 200 || resp.Object != "response" || resp.Status != "completed" || resp.Model != "r" ||
		len(resp.Output) != 1 || resp.Output[0].Type != "message" || resp.Output[0].Content[0].Text != "hi" {
		t.Errorf("/v1/responses with key: %d %+v", r.StatusCode, resp)
	}
	r = do(t, "POST", ts.URL+"/v1/responses", `{"model":"r","previous_response_id":"resp_1","input":"hi"}`, map[string]string{"X-Api-Key": "secret"})
	json.NewDecoder(r.Body).Decode(&eb)
	if r.StatusCode != 400 || eb.Error.Type != "invalid_request_error" {
		t.Errorf("previous_response_id through the server: %d %+v", r.StatusCode, eb)
	}
}

func TestModelsCodexCatalog(t *testing.T) {
	routes := `, "routes": {"r": [{"provider":"a","model":"m1","max_context":32768},{"provider":"a","model":"m2","max_context":8192}]}`
	codexHdr := map[string]string{"Originator": "codex_exec"}

	openAIList := func(t *testing.T, r *http.Response) []string {
		t.Helper()
		var m struct {
			Object string `json:"object"`
			Data   []struct{ ID, Object string }
			Models []any `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil || r.StatusCode != 200 || m.Object != "list" || m.Models != nil {
			t.Fatalf("want the OpenAI list: %d %v %+v", r.StatusCode, err, m)
		}
		var ids []string
		for _, d := range m.Data {
			ids = append(ids, d.ID)
		}
		return ids
	}

	t.Run("catalog off", func(t *testing.T) {
		_, ts := newServer(t, routes)
		for _, c := range []struct {
			q   string
			hdr map[string]string
		}{{"?client_version=0.154.0", nil}, {"", codexHdr}, {"", nil}} {
			if ids := openAIList(t, do(t, "GET", ts.URL+"/v1/models"+c.q, "", c.hdr)); len(ids) == 0 || ids[0] != "r" {
				t.Errorf("%q %v: ids %v", c.q, c.hdr, ids)
			}
		}
	})

	t.Run("catalog on", func(t *testing.T) {
		_, ts := newServer(t, routes+`, "codex_catalog": true`)
		for _, c := range []struct {
			q   string
			hdr map[string]string
		}{{"?client_version=0.154.0", nil}, {"", codexHdr}} {
			r := do(t, "GET", ts.URL+"/v1/models"+c.q, "", c.hdr)
			var m struct {
				Models []map[string]any `json:"models"`
				Data   []any            `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil || r.StatusCode != 200 || m.Data != nil {
				t.Fatalf("%q: %d %v", c.q, r.StatusCode, err)
			}
			var route map[string]any
			for _, mi := range m.Models {
				if mi["slug"] == "r" {
					route = mi
				}
			}
			if route == nil {
				t.Fatalf("%q: route r missing from the catalog: %v", c.q, m.Models)
			}
			if route["context_window"] != 8192.0 {
				t.Errorf("context_window %v, want the smallest max_context 8192", route["context_window"])
			}
			if s, _ := route["shell_type"].(string); s == "" {
				t.Errorf("shell_type missing: %v", route)
			}
			if s, _ := route["base_instructions"].(string); s == "" {
				t.Errorf("base_instructions missing: %v", route)
			}
		}
		// A normal OpenAI client still gets the OpenAI list.
		if ids := openAIList(t, do(t, "GET", ts.URL+"/v1/models", "", nil)); len(ids) == 0 || ids[0] != "r" {
			t.Errorf("plain client: %v", ids)
		}
	})
}
