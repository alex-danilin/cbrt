package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
)

// reloadWith makes /api/reload return a copy of the current config changed by edit.
func reloadWith(s *Server, edit func(c *config.Config)) {
	s.reload = func() (*config.Config, error) {
		c := *s.cfg()
		edit(&c)
		return &c, nil
	}
}

func TestReloadRejectsRestartOnlyFields(t *testing.T) {
	s, ts := newServer(t, "")
	reloadWith(s, func(c *config.Config) { c.RetentionMonths = 3; c.Limits.MaxConcurrentRequests = 2 })
	r := do(t, "POST", ts.URL+"/api/reload", "", nil)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "retention_months, limits.max_concurrent_requests") || !strings.Contains(string(b), "restart") {
		t.Fatalf("restart-only change: %d %s", r.StatusCode, b)
	}
	if s.cfg().RetentionMonths != 6 {
		t.Errorf("rejected config was applied")
	}

	// Limits read per request stay live-reloadable.
	reloadWith(s, func(c *config.Config) { c.Limits.MaxRequestBytes = 50; c.Limits.MaxInflightBytes = 50 })
	if r := do(t, "POST", ts.URL+"/api/reload", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("live reload: %d", r.StatusCode)
	}
	if r := do(t, "POST", ts.URL+"/v1/chat/completions", chatBody+strings.Repeat(" ", 60), nil); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("reloaded max_request_bytes not applied: %d", r.StatusCode)
	}
}

func TestReloadWithNewKeyDropsSessions(t *testing.T) {
	s, ts := newServer(t, `, "api_key": "secret"`)
	r := do(t, "POST", ts.URL+"/login", url.Values{"key": {"secret"}}.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	var cookie string
	for _, c := range r.Cookies() {
		if c.Name == sessionCookie {
			cookie = sessionCookie + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	withCookie := map[string]string{"Cookie": cookie}
	if r := do(t, "GET", ts.URL+"/api/stats", "", withCookie); r.StatusCode != 200 {
		t.Fatalf("session before reload: %d", r.StatusCode)
	}

	// A reload that keeps the key keeps the session.
	reloadWith(s, func(c *config.Config) {})
	if r := do(t, "POST", ts.URL+"/api/reload", "", withCookie); r.StatusCode != 200 {
		t.Fatalf("reload: %d", r.StatusCode)
	}
	if r := do(t, "GET", ts.URL+"/api/stats", "", withCookie); r.StatusCode != 200 {
		t.Fatalf("session after same-key reload: %d", r.StatusCode)
	}

	reloadWith(s, func(c *config.Config) { c.APIKey = "rotated" })
	if r := do(t, "POST", ts.URL+"/api/reload", "", withCookie); r.StatusCode != 200 {
		t.Fatalf("reload: %d", r.StatusCode)
	}
	if r := do(t, "GET", ts.URL+"/api/stats", "", withCookie); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("session after key rotation: %d, want 401", r.StatusCode)
	}
}

func TestSlowBodyTimesOut(t *testing.T) {
	s, ts := newServer(t, `, "timeouts": {"read": "200ms"}`)
	c, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"model\":")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no response to a stalled body: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestTimeout || !strings.Contains(string(b), "request_timeout") {
		t.Errorf("stalled body: %d %s", resp.StatusCode, b)
	}
	if n := s.inflight.Load(); n != 0 {
		t.Errorf("in-flight reservation leaked: %d", n)
	}
}

// A handler that sets a short write deadline (as streaming does) must not poison the
// next request on the same keep-alive connection (regression guard; net/http also clears it).
func TestWriteDeadlineClearedForKeepAlive(t *testing.T) {
	s, _ := newServer(t, "")
	h := s.llm(func(w http.ResponseWriter, r *http.Request, body []byte) {
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
		io.WriteString(w, "ok")
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	c, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	for i := 0; i < 2; i++ {
		fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}")
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("request %d on the same connection: %v", i, err)
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(b) != "ok" {
			t.Fatalf("request %d: %d %q", i, resp.StatusCode, b)
		}
		time.Sleep(150 * time.Millisecond) // let the handler's deadline expire
	}
}
