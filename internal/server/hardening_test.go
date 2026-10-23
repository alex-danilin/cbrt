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

func getWithHost(t *testing.T, ts *httptest.Server, host string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestLoopbackHostHelpers(t *testing.T) {
	for h, want := range map[string]bool{
		"localhost": true, "LOCALHOST:8080": true, "127.0.0.1": true, "127.9.9.9:80": true,
		"[::1]:8080": true, "[::1]": true, "::1": true,
		"evil.example": false, "evil.example:80": false, "10.0.0.1": false, "[::2]:80": false,
		"localhost.evil.example": false, "": false,
	} {
		if got := loopbackHost(hostName(h)); got != want {
			t.Errorf("%q: loopback=%v, want %v", h, got, want)
		}
	}
}

func TestKeylessServerRefusesNonLoopbackHost(t *testing.T) {
	s, ts := newServer(t, "")
	for _, h := range []string{"evil.example", "evil.example:1234", "192.168.1.5"} {
		if c := getWithHost(t, ts, h); c != http.StatusMisdirectedRequest {
			t.Errorf("keyless, Host %q: %d, want 421", h, c)
		}
	}
	for _, h := range []string{"localhost:1234", "127.0.0.1", "[::1]:1234"} {
		if c := getWithHost(t, ts, h); c != 200 {
			t.Errorf("keyless, Host %q: %d, want 200", h, c)
		}
	}
	// Read per request: once a key is set (reload), the key protects and no implicit restriction applies.
	reloadWith(s, func(c *config.Config) { c.APIKey = "secret" })
	if r := do(t, "POST", ts.URL+"/api/reload", "", nil); r.StatusCode != 200 {
		t.Fatalf("reload: %d", r.StatusCode)
	}
	if c := getWithHost(t, ts, "evil.example"); c != 200 {
		t.Errorf("with api_key, Host evil.example: %d, want 200", c)
	}
}

func TestAllowedHostsReplacesLoopbackDefault(t *testing.T) {
	_, ts := newServer(t, `, "allowed_hosts": ["box.lan"]`)
	if c := getWithHost(t, ts, "box.lan:8080"); c != 200 {
		t.Errorf("listed host: %d", c)
	}
	if c := getWithHost(t, ts, "localhost"); c != http.StatusMisdirectedRequest {
		t.Errorf("unlisted localhost with allowed_hosts: %d", c)
	}
}

func TestLoginSlowFormTimesOut(t *testing.T) {
	_, ts := newServer(t, `, "api_key": "secret", "timeouts": {"read": "200ms"}`)
	c, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /login HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nkey=")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no response to a stalled login form: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestTimeout || !strings.Contains(string(b), "Timed out") {
		t.Errorf("stalled login: %d %s", resp.StatusCode, b)
	}
}

func TestLoginConcurrencyBound(t *testing.T) {
	s, ts := newServer(t, `, "api_key": "secret"`)
	form := url.Values{"key": {"secret"}}.Encode()
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for range cap(s.loginSlots) {
		s.loginSlots <- struct{}{}
	}
	if r := do(t, "POST", ts.URL+"/login", form, ct); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("login with all slots busy: %d, want 429", r.StatusCode)
	}
	for range cap(s.loginSlots) {
		<-s.loginSlots
	}
	if r := do(t, "POST", ts.URL+"/login", form, ct); r.StatusCode != http.StatusSeeOther {
		t.Errorf("login after slots freed: %d", r.StatusCode)
	}
}

func TestSessionStoreEpoch(t *testing.T) {
	st := newSessionStore(time.Hour)
	ep := st.epoch()
	tok, ok := st.create(ep)
	if !ok || !st.valid(tok) {
		t.Fatal("create under the current epoch must succeed")
	}
	st.reset()
	if st.valid(tok) {
		t.Error("reset must drop sessions")
	}
	if st.epoch() == ep {
		t.Error("reset must bump the epoch")
	}
	if _, ok := st.create(ep); ok {
		t.Error("create under a stale epoch must be refused")
	}
	if _, ok := st.create(st.epoch()); !ok {
		t.Error("create under the new epoch must succeed")
	}
}

// A key rotation landing between the key check and session creation must not leave a
// session granted under the old key.
func TestLoginRefusedWhenKeyRotatesMidLogin(t *testing.T) {
	s, ts := newServer(t, `, "api_key": "secret"`)
	reloadWith(s, func(c *config.Config) { c.APIKey = "rotated" })
	s.afterKeyCheck = func() {
		rec := httptest.NewRecorder()
		s.reloadConfig(rec, httptest.NewRequest("POST", "/api/reload", nil))
		if rec.Code != 200 {
			t.Errorf("reload in hook: %d %s", rec.Code, rec.Body)
		}
	}
	r := do(t, "POST", ts.URL+"/login", url.Values{"key": {"secret"}}.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b), "Key changed; sign in again.") {
		t.Errorf("login across rotation: %d %s", r.StatusCode, b)
	}
	for _, c := range r.Cookies() {
		if c.Name == sessionCookie {
			t.Errorf("session cookie issued across a key rotation")
		}
	}
	s.sessions.mu.Lock()
	n := len(s.sessions.m)
	s.sessions.mu.Unlock()
	if n != 0 {
		t.Errorf("%d sessions left after rotation", n)
	}
}

func TestCountTokensTakesSlotButNotRate(t *testing.T) {
	s, ts := newServer(t, `, "limits": {"max_concurrent_requests": 1, "requests_per_minute": 1}`)
	body := `{"model":"r","messages":[{"role":"user","content":"hi"}]}`
	s.bucket.take() // exhaust the rate bucket
	if r := do(t, "POST", ts.URL+"/v1/chat/completions", chatBody, nil); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("chat with an empty bucket: %d, want 429", r.StatusCode)
	}
	if r := do(t, "POST", ts.URL+"/v1/messages/count_tokens", body, nil); r.StatusCode != 200 {
		t.Errorf("count_tokens must not use the rate bucket: %d", r.StatusCode)
	}
	s.reqSlots <- struct{}{} // occupy the only slot
	r := do(t, "POST", ts.URL+"/v1/messages/count_tokens", body, nil)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(b), "too many concurrent requests") {
		t.Errorf("count_tokens with no free slot: %d %s", r.StatusCode, b)
	}
	<-s.reqSlots
}
