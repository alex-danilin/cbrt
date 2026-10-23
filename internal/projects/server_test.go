package projects

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const host = "127.0.0.1:8091"

// post sends a form; origin and referer "" are left out.
func post(t *testing.T, h http.Handler, path, hostHdr, origin, referer string, form url.Values) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://"+hostHdr+path, strings.NewReader(form.Encode()))
	r.Host = hostHdr
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if referer != "" {
		r.Header.Set("Referer", referer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b), w.Result().Header
}

func serverEnv(t *testing.T, plan string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PLAN.md"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	es := []Entry{{Name: "inkwell", Path: dir}}
	s := NewServer(ServerOptions{Load: func() ([]Entry, error) { return es, nil }, Token: "tok"})
	return s, dir
}

func TestServerPark(t *testing.T) {
	plan := "# Plan: inkwell\r\n\r\n## Core\r\n- [ ] export\r\n- [~] archive\r\n"
	s, dir := serverEnv(t, plan)
	p := filepath.Join(dir, "PLAN.md")
	form := func(line, text string) url.Values {
		return url.Values{"token": {"tok"}, "project": {"inkwell"}, "line": {line}, "text": {text}, "hash": {PlanHash([]byte(read(t, p)))}}
	}
	self := "http://" + host

	// the page carries the forms with the token and the hash
	_, body, hdr := get(t, s, "GET", host, "/")
	for _, want := range []string{`action="/api/park"`, `name="token" value="tok"`, `name="hash" value="` + PlanHash([]byte(plan)) + `"`, `name="line" value="4"`, `name="text" value="export"`, ">park</button>", ">unpark</button>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self'") || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script") {
		t.Errorf("CSP %q", csp)
	}
	if rp := hdr.Get("Referrer-Policy"); rp != "same-origin" {
		t.Errorf("Referrer-Policy %q", rp)
	}

	// refusals change nothing
	for _, c := range []struct {
		name, path, host, origin, referer string
		form                              url.Values
		code                              int
	}{
		{"bad token", "/api/park", host, self, "", func() url.Values { f := form("4", "export"); f.Set("token", "nope"); return f }(), 403},
		{"no token", "/api/park", host, self, "", func() url.Values { f := form("4", "export"); f.Del("token"); return f }(), 403},
		{"foreign origin", "/api/park", host, "http://evil.example", "", form("4", "export"), 403},
		{"null origin", "/api/park", host, "null", "", form("4", "export"), 403},
		{"other port", "/api/park", host, "http://127.0.0.1:9999", "", form("4", "export"), 403},
		{"https origin", "/api/park", host, "https://" + host, "", form("4", "export"), 403},
		{"no origin or referer", "/api/park", host, "", "", form("4", "export"), 403},
		{"foreign referer", "/api/park", host, "", "http://evil.example/", form("4", "export"), 403},
		{"foreign host", "/api/park", "evil.example:8091", "http://evil.example:8091", "", form("4", "export"), 421},
		{"unknown project", "/api/park", host, self, "", func() url.Values { f := form("4", "export"); f.Set("project", "other"); return f }(), 404},
		{"bad line", "/api/park", host, self, "", form("x", "export"), 400},
		{"wrong text", "/api/park", host, self, "", form("4", "exports"), 409},
		{"stale hash", "/api/park", host, self, "", func() url.Values { f := form("4", "export"); f.Set("hash", PlanHash([]byte("old"))); return f }(), 409},
	} {
		code, body, _ := post(t, s, c.path, c.host, c.origin, c.referer, c.form)
		if code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
		if read(t, p) != plan {
			t.Fatalf("%s: PLAN.md changed", c.name)
		}
	}
	if code, body, _ := post(t, s, "/api/park", host, self, "", func() url.Values { f := form("4", "export"); f.Set("hash", "x"); return f }()); !strings.Contains(body, "changed since the page was loaded") || code != 409 {
		t.Errorf("409 message %q", body)
	}
	if code, _, _ := get(t, s, "GET", host, "/api/park"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/park: %d", code)
	}

	// park by Origin, unpark by Referer
	code, _, h := post(t, s, "/api/park", host, self, "", form("4", "export"))
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("park: %d %v", code, h)
	}
	if got := read(t, p); got != strings.Replace(plan, "- [ ] export", "- [~] export", 1) {
		t.Errorf("parked: %q", got)
	}
	if code, body, _ := post(t, s, "/api/park", "localhost:8091", "", "http://localhost:8091/", form("5", "archive")); code != http.StatusSeeOther {
		t.Fatalf("unpark: %d %s", code, body)
	}
	if got := read(t, p); got != "# Plan: inkwell\r\n\r\n## Core\r\n- [~] export\r\n- [ ] archive\r\n" {
		t.Errorf("unparked: %q", got)
	}
}

func TestServerCheck(t *testing.T) {
	s, dir := serverEnv(t, "# Plan: inkwell\n\n## Core\n- [ ] export\n")
	release := make(chan struct{})
	started := make(chan Command, 1)
	answer, fail := "Verdict: on track\n\nGood.", error(nil)
	s.o.Run = func(ctx context.Context, c Command) (string, error) {
		started <- c
		<-release
		return answer, fail
	}
	self := "http://" + host
	form := url.Values{"token": {"tok"}, "project": {"inkwell"}, "agent": {"codex"}}

	if code, _, _ := post(t, s, "/api/check", host, "http://evil.example", "", form); code != 403 {
		t.Errorf("foreign origin: %d", code)
	}
	bad := url.Values{"token": {"tok"}, "project": {"inkwell"}, "agent": {"bash"}}
	if code, _, _ := post(t, s, "/api/check", host, self, "", bad); code != 400 {
		t.Errorf("bad agent: %d", code)
	}
	if code, _, _ := post(t, s, "/api/check", host, self, "", url.Values{"token": {"x"}, "project": {"inkwell"}, "agent": {"codex"}}); code != 403 {
		t.Errorf("bad token: %d", code)
	}

	if code, _, _ := post(t, s, "/api/check", host, self, "", form); code != http.StatusSeeOther {
		t.Fatalf("check: %d", code)
	}
	c := <-started
	if c.Agent != "codex" || c.Dir != dir {
		t.Errorf("command %+v", c)
	}
	_, body, _ := get(t, s, "GET", host, "/")
	if !strings.Contains(body, "checking… (codex, started just now)") || strings.Contains(body, "Run check") {
		t.Errorf("page does not show the running check")
	}
	if code, body, _ := post(t, s, "/api/check", host, self, "", form); code != http.StatusConflict || !strings.Contains(body, "already running") {
		t.Errorf("second check: %d %s", code, body)
	}
	close(release)
	s.wg.Wait()
	_, body, _ = get(t, s, "GET", host, "/")
	if strings.Contains(body, "checking…") || !strings.Contains(body, `<span class="verdict on-track">on track</span>`) || !strings.Contains(body, "Run check") {
		t.Errorf("page after the check:\n%s", body)
	}

	// a failing check shows its error; the next one may start
	release = make(chan struct{})
	answer, fail = "", errors.New("codex did not answer within 10m0s")
	post(t, s, "/api/check", host, self, "", form)
	<-started
	close(release)
	s.wg.Wait()
	if _, body, _ := get(t, s, "GET", host, "/"); !strings.Contains(body, "Last check failed: codex did not answer within 10m0s") {
		t.Errorf("no failure shown")
	}
}

func TestServerRandomToken(t *testing.T) {
	a := NewServer(ServerOptions{Load: func() ([]Entry, error) { return nil, nil }})
	b := NewServer(ServerOptions{Load: func() ([]Entry, error) { return nil, nil }})
	if len(a.o.Token) != 64 || a.o.Token == b.o.Token {
		t.Errorf("tokens %q %q", a.o.Token, b.o.Token)
	}
}
