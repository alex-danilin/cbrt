package statusline

import (
	"bytes"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var zone = time.FixedZone("TEST", 3*3600) // UTC+3

func fixedNow() time.Time { return time.Date(2026, 9, 29, 12, 30, 0, 0, zone) }

type captured struct {
	mu    sync.Mutex
	auth  string
	query string
	path  string
}

func (c *captured) get() (auth, query, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth, c.query, c.path
}

// gateway serves body with status code and records the last request.
func gateway(t *testing.T, code int, body string) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.auth, c.query, c.path = r.Header.Get("Authorization"), r.URL.RawQuery, r.URL.Path
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func noEnv(string) string { return "" }

func run(t *testing.T, o Options) string {
	t.Helper()
	var out bytes.Buffer
	o.Stdout = &out
	if o.Getenv == nil {
		o.Getenv = noEnv
	}
	if o.Now == nil {
		o.Now = fixedNow
	}
	if o.Stdin == nil {
		o.Stdin = strings.NewReader("")
	}
	Run(o)
	s := out.String()
	if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
		t.Fatalf("want exactly one line, got %q", s)
	}
	line := strings.TrimSuffix(s, "\n")
	if strings.TrimRight(line, " ") != line {
		t.Errorf("trailing space in %q", line)
	}
	return line
}

const stdinJSON = `{"session_id":"abc 123/x","transcript_path":"/tmp/t","model":{"id":"claude-opus","display_name":"Opus"},"workspace":{"current_dir":"C:\\src"}}`

const routes = `"routes":[
 {"route":"coder","last_target":"nvidia:qwen/qwen3-coder","last_at":"2026-09-29T12:00:00Z"},
 {"route":"fast","last_target":"ollama:gemma-4-e2b-4-32k","last_at":"2026-09-29T12:03:00Z","fallback":true,"reason":"quota_exhausted"},
 {"route":"idle","last_target":"","last_at":""}]`

func TestSessionFound(t *testing.T) {
	srv, c := gateway(t, 200, `{`+routes+`,"session":{"route":"coder","last_target":"nvidia:qwen/qwen3-coder","last_at":"2026-09-29T12:00:00Z"}}`)
	got := run(t, Options{URL: srv.URL, Stdin: strings.NewReader(stdinJSON)})
	if want := "cbrt ● coder → nvidia:qwen/qwen3-coder"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	_, q, p := c.get()
	if p != "/api/status" || q != "session=abc+123%2Fx" {
		t.Errorf("path %q query %q", p, q)
	}
}

func TestSessionFallbackSuffix(t *testing.T) {
	srv, _ := gateway(t, 200, `{"session":{"route":"coder","last_target":"ollama:gemma-4-e2b-4-32k","fallback":true,"reason":"quota_exhausted"}}`)
	got := run(t, Options{URL: srv.URL, Stdin: strings.NewReader(stdinJSON)})
	if want := "cbrt ● coder → ollama:gemma-4-e2b-4-32k (fallback: quota_exhausted)"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestSessionNotFoundRouteWide(t *testing.T) {
	srv, c := gateway(t, 200, `{`+routes+`}`)
	got := run(t, Options{URL: srv.URL}) // no stdin data: no session query
	if want := "cbrt ● fast → ollama:gemma-4-e2b-4-32k (route-wide)"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if _, q, _ := c.get(); q != "" {
		t.Errorf("unexpected query %q", q)
	}
}

func TestNoRoutes(t *testing.T) {
	srv, _ := gateway(t, 200, `{"routes":[],"degraded":[]}`)
	if got := run(t, Options{URL: srv.URL}); got != "cbrt ● ready" {
		t.Errorf("got %q", got)
	}
}

func TestDegradedSingle(t *testing.T) {
	srv, _ := gateway(t, 200, `{"degraded":[
	 {"provider":"nvidia","target":"nvidia:x","status":"cooldown","reason":"rate_limited","until":"2026-09-29T09:40:00Z"},
	 {"provider":"nvidia","target":"","status":"cooldown","reason":"quota_exhausted","until":"2026-09-29T10:03:00Z"}]}`)
	// Target-level entry is skipped; 10:03Z shown as 13:03 in UTC+3.
	if got, want := run(t, Options{URL: srv.URL}), "cbrt ● ready ⚠ nvidia quota_exhausted until 13:03"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDegradedMultipleAndDisabled(t *testing.T) {
	srv, _ := gateway(t, 200, `{`+routes+`,"degraded":[
	 {"provider":"groq","status":"disabled","reason":"auth_failed"},
	 {"provider":"nvidia","status":"cooldown","reason":"quota_exhausted","until":"2026-09-29T10:03:00Z"},
	 {"provider":"mistral","status":"half_open"}]}`)
	want := "cbrt ● fast → ollama:gemma-4-e2b-4-32k (route-wide) ⚠ groq auth_failed +2"
	if got := run(t, Options{URL: srv.URL}); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDegradedReasonFallsBackToStatus(t *testing.T) {
	srv, _ := gateway(t, 200, `{"degraded":[{"provider":"mistral","status":"half_open"}]}`)
	if got, want := run(t, Options{URL: srv.URL}), "cbrt ● ready ⚠ mistral half_open"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestTruncateLongTarget(t *testing.T) {
	srv, _ := gateway(t, 200, `{"session":{"route":"coder","last_target":"nvidia:deepseek-ai/deepseek-v4.1-flash"}}`)
	got := run(t, Options{URL: srv.URL, Stdin: strings.NewReader(stdinJSON)})
	if want := "cbrt ● coder → …/deepseek-v4.1-flash"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got := shortTarget(strings.Repeat("a", 50), "…"); got != "…"+strings.Repeat("a", maxTarget-1) {
		t.Errorf("no-slash truncation: %q", got)
	}
}

func TestLineLengthCappedAndSanitized(t *testing.T) {
	long := strings.Repeat("r", 200)
	srv, _ := gateway(t, 200, `{"session":{"route":"`+long+`\n\u001b[31m","last_target":"t","fallback":true,"reason":"`+long+`"},
	 "degraded":[{"provider":"`+long+`","reason":"`+long+`","until":"2026-09-29T10:03:00Z"},{"provider":"b"}]}`)
	got := run(t, Options{URL: srv.URL, Stdin: strings.NewReader(stdinJSON)})
	if n := len([]rune(got)); n > maxLine {
		t.Errorf("line has %d runes: %q", n, got)
	}
	if strings.ContainsAny(got, "\n\r\x1b") {
		t.Errorf("control characters leaked: %q", got)
	}
}

func TestBearerKey(t *testing.T) {
	srv, c := gateway(t, 200, `{}`)
	run(t, Options{URL: srv.URL, Key: "sekrit"})
	if a, _, _ := c.get(); a != "Bearer sekrit" {
		t.Errorf("Authorization %q", a)
	}
	run(t, Options{URL: srv.URL})
	if a, _, _ := c.get(); a != "" {
		t.Errorf("unexpected Authorization %q", a)
	}
}

func TestDiscoveryPrecedence(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	all := map[string]string{
		"CBRT_URL": "http://f:1/v1/", "CBRT_API_KEY": "fk",
		"ANTHROPIC_BASE_URL": "http://a:2/", "ANTHROPIC_AUTH_TOKEN": "ak",
	}
	cases := []struct {
		name     string
		o        Options
		url, key string
	}{
		{"flags win", Options{URL: "http://x:3/v1", Key: "xk", Getenv: env(all)}, "http://x:3", "xk"},
		{"cbrt env", Options{Getenv: env(all)}, "http://f:1", "fk"},
		{"anthropic env", Options{Getenv: env(map[string]string{"ANTHROPIC_BASE_URL": "http://a:2/v1", "ANTHROPIC_AUTH_TOKEN": "ak"})}, "http://a:2", "ak"},
		{"anthropic token not sent to cbrt url", Options{Getenv: env(map[string]string{"CBRT_URL": "http://f:1", "ANTHROPIC_AUTH_TOKEN": "ak"})}, "http://f:1", ""},
		{"anthropic token not sent to default", Options{Getenv: env(map[string]string{"ANTHROPIC_AUTH_TOKEN": "ak"})}, DefaultURL, ""},
		{"flag url, env key", Options{URL: "http://x:3", Getenv: env(all)}, "http://x:3", "fk"},
		{"default", Options{Getenv: noEnv}, DefaultURL, ""},
	}
	for _, tc := range cases {
		u, k := discover(&tc.o)
		if u != tc.url || k != tc.key {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", tc.name, u, k, tc.url, tc.key)
		}
	}
}

func TestEnvUsedByRun(t *testing.T) {
	srv, c := gateway(t, 200, `{}`)
	getenv := func(k string) string {
		switch k {
		case "ANTHROPIC_BASE_URL":
			return srv.URL + "/v1"
		case "ANTHROPIC_AUTH_TOKEN":
			return "tok"
		}
		return ""
	}
	if got := run(t, Options{Getenv: getenv}); got != "cbrt ● ready" {
		t.Errorf("got %q", got)
	}
	if a, _, p := c.get(); a != "Bearer tok" || p != "/api/status" {
		t.Errorf("auth %q path %q", a, p)
	}
}

func TestStdinNeverCloses(t *testing.T) {
	srv, c := gateway(t, 200, `{}`)
	pr, pw := io.Pipe()
	defer pw.Close()
	o := Options{URL: srv.URL, Stdin: pr, StdinTimeout: 200 * time.Millisecond, HTTPTimeout: 300 * time.Millisecond}
	start := time.Now()
	got := run(t, o)
	if el := time.Since(start); el > o.StdinTimeout+o.HTTPTimeout+100*time.Millisecond {
		t.Errorf("took %v", el)
	}
	if got != "cbrt ● ready" {
		t.Errorf("got %q", got)
	}
	if _, q, _ := c.get(); q != "" {
		t.Errorf("query %q", q)
	}
}

func TestStdinPartialThenStalls(t *testing.T) {
	srv, c := gateway(t, 200, `{}`)
	pr, pw := io.Pipe()
	defer pw.Close()
	go io.WriteString(pw, `{"session_id":"s1","model":{"id":"m"`) // never finished
	run(t, Options{URL: srv.URL, Stdin: pr, StdinTimeout: 150 * time.Millisecond})
	if _, q, _ := c.get(); q != "session=s1" {
		t.Errorf("query %q", q)
	}
}

func TestStdinLargerThan64K(t *testing.T) {
	srv, c := gateway(t, 200, `{}`)
	big := `{"session_id":"early","pad":"` + strings.Repeat("x", 200<<10) + `"}`
	start := time.Now()
	run(t, Options{URL: srv.URL, Stdin: strings.NewReader(big)})
	if el := time.Since(start); el > time.Second {
		t.Errorf("took %v", el)
	}
	if _, q, _ := c.get(); q != "session=early" {
		t.Errorf("truncated stdin should still yield the early session id, query %q", q)
	}
	// session_id past the 64 KiB cap is not seen.
	late := `{"pad":"` + strings.Repeat("x", 100<<10) + `","session_id":"late"}`
	run(t, Options{URL: srv.URL, Stdin: strings.NewReader(late)})
	if _, q, _ := c.get(); q != "" {
		t.Errorf("query %q", q)
	}
}

func TestSessionIDParsing(t *testing.T) {
	cases := map[string]string{
		`{"model":{"session_id":"nested"},"session_id":"top"}`:              "top",
		`{"a":[1,{"session_id":"no"}],"b":"session_id","session_id":"yes"}`: "yes",
		`{"session_id":42}`:  "",
		`not json`:           "",
		`["session_id","x"]`: "",
		``:                   "",
	}
	for in, want := range cases {
		if got := sessionID([]byte(in)); got != want {
			t.Errorf("sessionID(%q) = %q want %q", in, got, want)
		}
	}
}

func TestBusyOnSlowServer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	got := run(t, Options{URL: srv.URL, HTTPTimeout: 100 * time.Millisecond})
	if got != "cbrt ⧗ busy" {
		t.Errorf("got %q", got)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("took %v", el)
	}
}

func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestUnreachable(t *testing.T) {
	addr := closedAddr(t)
	start := time.Now()
	got := run(t, Options{URL: "http://" + addr + "/v1"})
	if want := "cbrt ✕ unreachable (" + addr + ")"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if el := time.Since(start); el > DefaultStdinTimeout+DefaultHTTPTimeout+100*time.Millisecond {
		t.Errorf("took %v", el)
	}
}

func TestHostPortDefaults(t *testing.T) {
	for in, want := range map[string]string{
		"https://gw.example": "gw.example:443",
		"http://gw.example":  "gw.example:80",
		"http://[::1]:9":     "[::1]:9",
	} {
		u, _ := url.Parse(in)
		if got := hostPort(u); got != want {
			t.Errorf("hostPort(%q) = %q want %q", in, got, want)
		}
	}
}

// TestNoPlatformSpecificCode keeps the command portable: no shell-outs,
// no os/exec, no syscall, no build-tagged files.
func TestNoPlatformSpecificCode(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			switch p := strings.Trim(imp.Path.Value, `"`); {
			case p == "os/exec", p == "syscall", strings.HasPrefix(p, "golang.org/x/sys"), p == "unsafe":
				t.Errorf("%s imports %s", f, p)
			}
		}
		for _, cg := range af.Comments {
			if strings.Contains(cg.Text(), "go:build") {
				t.Errorf("%s has a build constraint", f)
			}
		}
	}
}

func TestAuthError(t *testing.T) {
	for _, code := range []int{401, 403} {
		srv, _ := gateway(t, code, `{"error":"nope"}`)
		if got := run(t, Options{URL: srv.URL}); got != "cbrt ✕ auth (set CBRT_API_KEY)" {
			t.Errorf("%d: got %q", code, got)
		}
	}
}

func TestServerError(t *testing.T) {
	srv, _ := gateway(t, 500, `oops`)
	if got := run(t, Options{URL: srv.URL}); got != "cbrt ✕ error 500" {
		t.Errorf("got %q", got)
	}
}

func TestMalformedJSON(t *testing.T) {
	srv, _ := gateway(t, 200, `{"routes":[`)
	if got := run(t, Options{URL: srv.URL}); got != "cbrt ✕ error bad response" {
		t.Errorf("got %q", got)
	}
}

func TestBadURL(t *testing.T) {
	if got := run(t, Options{URL: "::nonsense"}); !strings.HasPrefix(got, "cbrt ✕ bad url") {
		t.Errorf("got %q", got)
	}
}

func TestPlain(t *testing.T) {
	srv, _ := gateway(t, 200, `{"session":{"route":"coder","last_target":"nvidia:deepseek-ai/deepseek-v4.1-flash","fallback":true,"reason":"rate_limited"},
	 "degraded":[{"provider":"nvidia","reason":"quota_exhausted","until":"2026-09-29T10:03:00Z"}]}`)
	got := run(t, Options{URL: srv.URL, Plain: true, Stdin: strings.NewReader(stdinJSON)})
	// The ASCII ellipsis makes this line longer than the Unicode one: the
	// fallback reason is dropped before the degraded warning is cut.
	want := "cbrt * coder -> .../deepseek-v4.1-flash (fallback) ! nvidia quota_exhausted until 13:03"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	for _, r := range got {
		if r > 127 {
			t.Errorf("non-ASCII rune %q in plain output", r)
		}
	}
	addr := closedAddr(t)
	if got := run(t, Options{URL: "http://" + addr, Plain: true}); got != "cbrt x unreachable ("+addr+")" {
		t.Errorf("got %q", got)
	}
}

func TestRunNilStdoutAndDefaults(t *testing.T) {
	// Must not panic with a zero Options (nil Stdout, nil Stdin, real env).
	Run(Options{URL: "http://" + closedAddr(t)})
	Run(Options{Stdout: io.Discard, URL: "http://" + closedAddr(t), Getenv: noEnv})
}

func TestMainAlwaysReturnsZero(t *testing.T) {
	// Main uses the process's stdin/stdout; swap them for a pipe and a file.
	oldIn, oldOut := os.Stdin, os.Stdout
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close() // never written: exercises the stdin timeout
	outF, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer outF.Close()
	os.Stdin, os.Stdout = inR, outF

	srv, c := gateway(t, 200, `{}`)
	addr := closedAddr(t)
	// Keep the flag-less runs off any real gateway.
	t.Setenv("CBRT_URL", "http://"+addr)
	t.Setenv("CBRT_API_KEY", "")
	argsList := [][]string{
		{"-url", srv.URL, "-key", "k1"},
		{"-url", "http://" + addr, "-plain"},
		{"-bogus-flag", "x"},
		{"-url"},
		nil,
	}
	for _, args := range argsList {
		if code := Main(args); code != 0 {
			t.Errorf("Main(%q) = %d", args, code)
		}
	}
	os.Stdout = oldOut
	if a, _, _ := c.get(); a != "Bearer k1" {
		t.Errorf("auth %q", a)
	}
	b, _ := os.ReadFile(outF.Name())
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != len(argsList) {
		t.Fatalf("want %d lines, got %q", len(argsList), b)
	}
	if lines[0] != "cbrt ● ready" || lines[1] != "cbrt x unreachable ("+addr+")" {
		t.Errorf("lines %q", lines)
	}
}
