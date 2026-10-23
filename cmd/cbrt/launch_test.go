package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/autoconfig"
	"github.com/alex-danilin/cbrt/internal/config"
)

// Environment switch that turns the test binary into a fake client (see TestMain).
const fakeClientEnv = "CBRT_TEST_FAKE_CLIENT"

// fakeReport is what the fake client writes to $CBRT_TEST_FAKE_OUT.
type fakeReport struct {
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Base     string   `json:"base"`
	Healthz  int      `json:"healthz"`
	Messages int      `json:"messages"`
	Body     string   `json:"body"`
}

// fakeClientMain is the fake claude/codex: it finds the gateway from the
// environment (Claude) or the -c base_url override (Codex), calls it, records
// what it saw, and exits with $CBRT_TEST_FAKE_EXIT.
func fakeClientMain() int {
	rep := fakeReport{Args: os.Args[1:], Env: os.Environ(), Base: os.Getenv("ANTHROPIC_BASE_URL")}
	for _, a := range rep.Args {
		if v, ok := strings.CutPrefix(a, "model_providers.cbrt.base_url="); ok {
			rep.Base = strings.TrimSuffix(v, "/v1")
		}
	}
	c := &http.Client{Timeout: 10 * time.Second}
	if resp, err := c.Get(rep.Base + "/healthz"); err == nil {
		rep.Healthz = resp.StatusCode
		resp.Body.Close()
	}
	if os.Getenv("ANTHROPIC_BASE_URL") != "" {
		req, _ := http.NewRequest("POST", rep.Base+"/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+os.Getenv("ANTHROPIC_AUTH_TOKEN"))
		if resp, err := c.Do(req); err == nil {
			var b bytes.Buffer
			b.ReadFrom(resp.Body)
			resp.Body.Close()
			rep.Messages, rep.Body = resp.StatusCode, b.String()
		}
	}
	b, _ := json.Marshal(rep)
	os.WriteFile(os.Getenv("CBRT_TEST_FAKE_OUT"), b, 0o600)
	code, _ := strconv.Atoi(os.Getenv("CBRT_TEST_FAKE_EXIT"))
	return code
}

func TestLauncherEndToEnd(t *testing.T) {
	for _, client := range []string{"claude", "codex"} {
		t.Run(client, func(t *testing.T) { testLauncher(t, client) })
	}
}

func testLauncher(t *testing.T, client string) {
	detectEnv(t)
	const leak = "TEST_CBRT_ENVFILE_ONLY"
	os.Unsetenv(leak)
	t.Cleanup(func() { os.Unsetenv(leak) })
	oll := newFakeOllama(t)
	tmp := t.TempDir()
	envPath := filepath.Join(tmp, "launcher.env")
	if err := os.WriteFile(envPath, []byte(leak+"=from-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "fake.json")
	cwd := filepath.Join(tmp, "project")
	os.Mkdir(cwd, 0o700)
	var stderr bytes.Buffer
	l := &launcher{
		client: client, args: []string{"-p", "hi there"},
		environ: append(os.Environ(), fakeClientEnv+"=1", "CBRT_TEST_FAKE_OUT="+outPath, "CBRT_TEST_FAKE_EXIT=7",
			"ANTHROPIC_API_KEY=sk-must-not-reach-claude", "CBRT_ENV_FILE="+envPath),
		goos: runtime.GOOS, cwd: cwd, dir: filepath.Join(tmp, "ucd", "cbrt"), exe: `C:\Program Files\cbrt\cbrt.exe`,
		lookPath: func(name string) (string, error) { return os.Args[0], nil },
		stdin:    strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &stderr,
		detect: autoconfig.Options{OllamaURL: oll.URL},
	}
	code, err := l.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, stderr.String())
	}
	if code != 7 {
		t.Errorf("exit code %d, want the client's 7", code)
	}
	if os.Getenv(leak) != "from-env-file" {
		t.Errorf("env file was not loaded into the gateway process")
	}

	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("fake client did not run: %v\nstderr:\n%s", err, stderr.String())
	}
	var rep fakeReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Healthz != http.StatusOK {
		t.Errorf("client could not reach the gateway at %q: %d", rep.Base, rep.Healthz)
	}
	env := strings.Join(rep.Env, "\n")
	if strings.Contains(env, leak) {
		t.Error("a variable from the env file leaked into the client's environment")
	}
	route := "coder"
	if client == "claude" {
		route = "claude-*"
		if rep.Messages != http.StatusOK || !strings.Contains(rep.Body, "Good day, friend.") {
			t.Errorf("/v1/messages through the gateway: %d %s", rep.Messages, rep.Body)
		}
		if strings.Contains(env, "sk-must-not-reach-claude") {
			t.Error("ANTHROPIC_API_KEY reached Claude Code")
		}
		for _, want := range []string{"ANTHROPIC_BASE_URL=" + rep.Base, "ANTHROPIC_AUTH_TOKEN=cbrt", "CLAUDE_CODE_MAX_CONTEXT_TOKENS=32768"} {
			if !containsLine(rep.Env, want) {
				t.Errorf("client env lacks %s", want)
			}
		}
		if len(rep.Args) < 4 || rep.Args[0] != "--settings" || !json.Valid([]byte(rep.Args[1])) {
			t.Errorf("claude args: %q", rep.Args)
		}
	} else {
		if !containsLine(rep.Env, "ANTHROPIC_API_KEY=sk-must-not-reach-claude") {
			t.Error("codex should get the original environment unchanged")
		}
		if !containsLine(rep.Args, "model_context_window=32768") {
			t.Errorf("codex args: %q", rep.Args)
		}
	}
	if got := rep.Args[len(rep.Args)-2:]; !reflect.DeepEqual(got, []string{"-p", "hi there"}) {
		t.Errorf("user args must come last, verbatim: %q", rep.Args)
	}

	for _, want := range []string{"config: none found", "route " + route + " → ollama:" + fakeModel + " (context 32768)", "dashboard " + rep.Base + "/", "warning: NVIDIA_API_KEY is not set"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, stderr.String())
		}
	}
	// The gateway is gone once run returns.
	if c, err := net.DialTimeout("tcp", strings.TrimPrefix(rep.Base, "http://"), time.Second); err == nil {
		c.Close()
		t.Error("gateway still listening after the client exited")
	}
	logb, err := os.ReadFile(filepath.Join(l.dir, "cbrt.log"))
	if err != nil || !strings.Contains(string(logb), "cbrt listening") || !strings.Contains(string(logb), "client exited") {
		t.Errorf("log file: %v\n%s", err, logb)
	}
	if _, err := os.Stat(filepath.Join(l.dir, "data")); err != nil {
		t.Errorf("zero-config data dir not under the user config dir: %v", err)
	}
}

func containsLine(lines []string, s string) bool {
	for _, l := range lines {
		if l == s {
			return true
		}
	}
	return false
}

func TestLauncherMissingClient(t *testing.T) {
	l := &launcher{client: "codex", goos: runtime.GOOS, dir: t.TempDir(),
		lookPath: func(string) (string, error) { return "", errors.New("not found") }}
	_, err := l.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "codex was not found") || !strings.Contains(err.Error(), "npm install -g @openai/codex") {
		t.Errorf("err = %v", err)
	}
}

func TestLauncherConfigSource(t *testing.T) {
	tmp := t.TempDir()
	cwd, dir := filepath.Join(tmp, "cwd"), filepath.Join(tmp, "cbrt")
	os.MkdirAll(cwd, 0o700)
	os.MkdirAll(dir, 0o700)
	l := &launcher{goos: runtime.GOOS, cwd: cwd, dir: dir}
	check := func(want string) {
		t.Helper()
		if got, _ := l.configSource(); got != want {
			t.Errorf("config source %q, want %q", got, want)
		}
	}
	check("") // nothing: detect
	os.WriteFile(filepath.Join(dir, "cbrt.json"), []byte("{}"), 0o600)
	check(filepath.Join(dir, "cbrt.json"))
	os.WriteFile(filepath.Join(cwd, "cbrt.json"), []byte("{}"), 0o600)
	check(filepath.Join(cwd, "cbrt.json"))
	l.environ = []string{"CBRT_CONFIG=/elsewhere/x.json"}
	check("/elsewhere/x.json") // even if it does not exist: loading then reports the error

	if got := l.envFile(); got != filepath.Join(dir, ".env") {
		t.Errorf("env file %q", got)
	}
	l.environ = append(l.environ, "CBRT_ENV_FILE=/secrets/.env")
	if got := l.envFile(); got != "/secrets/.env" {
		t.Errorf("env file %q", got)
	}
}

func TestClaudeEnv(t *testing.T) {
	orig := []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk", "ANTHROPIC_BASE_URL=https://api.anthropic.com", "CLAUDE_CODE_MAX_CONTEXT_TOKENS=1", "=C:=C:\\x"}
	got := claudeEnv(orig, "http://127.0.0.1:5", 32768, "linux", false)
	want := []string{"PATH=/bin", "=C:=C:\\x", "ANTHROPIC_BASE_URL=http://127.0.0.1:5", "ANTHROPIC_AUTH_TOKEN=cbrt", "CLAUDE_CODE_MAX_CONTEXT_TOKENS=32768"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claudeEnv:\n got %q\nwant %q", got, want)
	}
	// Windows names are case-insensitive; a zero context leaves the user's value.
	got = claudeEnv([]string{"Anthropic_Api_Key=sk", "CLAUDE_CODE_MAX_CONTEXT_TOKENS=1"}, "http://127.0.0.1:5", 0, "windows", false)
	want = []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS=1", "ANTHROPIC_BASE_URL=http://127.0.0.1:5", "ANTHROPIC_AUTH_TOKEN=cbrt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claudeEnv windows:\n got %q\nwant %q", got, want)
	}
}

func TestClaudeArgs(t *testing.T) {
	for _, tc := range []struct{ exe, goos, cmd string }{
		{`C:\Program Files\My Tools\cbrt.exe`, "windows", `"C:/Program Files/My Tools/cbrt.exe" statusline -url http://127.0.0.1:5`},
		{`/opt/it's here/cbrt`, "linux", `'/opt/it'\''s here/cbrt' statusline -url http://127.0.0.1:5`},
	} {
		args := claudeArgs(tc.exe, "http://127.0.0.1:5", "", tc.goos, []string{"-p", "hi"})
		if len(args) != 4 || args[0] != "--settings" || args[2] != "-p" || args[3] != "hi" {
			t.Fatalf("args %q", args)
		}
		var s struct {
			StatusLine struct{ Type, Command string } `json:"statusLine"`
		}
		if err := json.Unmarshal([]byte(args[1]), &s); err != nil {
			t.Fatalf("settings is not valid JSON: %v: %s", err, args[1])
		}
		if s.StatusLine.Type != "command" || s.StatusLine.Command != tc.cmd {
			t.Errorf("status line %+v, want command %s", s.StatusLine, tc.cmd)
		}
	}
	if args := claudeArgs("x", "http://h", `C:\d\s.json`, "windows", nil); !reflect.DeepEqual(args, []string{"--settings", `C:\d\s.json`}) {
		t.Errorf("settings file args %q", args)
	}
}

func TestCodexArgs(t *testing.T) {
	got := codexArgs("http://127.0.0.1:5", 32768, []string{"exec", "hi"})
	want := []string{"-c", "model_provider=cbrt", "-c", "model_providers.cbrt.name=cbrt",
		"-c", "model_providers.cbrt.base_url=http://127.0.0.1:5/v1", "-c", "model_providers.cbrt.wire_api=responses",
		"-c", "model=coder", "-c", "model_context_window=32768", "exec", "hi"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("codexArgs:\n got %q\nwant %q", got, want)
	}
	if err := cmdSafe(got); err != nil {
		t.Errorf("codex args must pass through a .cmd shim: %v", err)
	}
	if got := codexArgs("http://h", 0, nil); containsLine(got, "model_context_window=0") || len(got) != 10 {
		t.Errorf("no context: %q", got)
	}
}

func TestResolveClient(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "codex.cmd")
	js := filepath.Join(dir, "node_modules", "@openai", "codex", "bin", "codex.js")
	os.MkdirAll(filepath.Dir(js), 0o700)
	os.WriteFile(js, []byte("//"), 0o600)
	// The body of an npm cmd-shim (npm 10).
	os.WriteFile(shim, []byte("@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nSETLOCAL\r\nCALL :find_dp0\r\n\r\n"+
		"IF EXIST \"%dp0%\\node.exe\" (\r\n  SET \"_prog=%dp0%\\node.exe\"\r\n) ELSE (\r\n  SET \"_prog=node\"\r\n  SET PATHEXT=%PATHEXT:;.JS;=;%\r\n)\r\n\r\n"+
		"endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & \"%_prog%\"  \"%dp0%\\node_modules\\@openai\\codex\\bin\\codex.js\" %*\r\n"), 0o600)
	look := func(name string) (string, error) {
		switch name {
		case "codex":
			return shim, nil
		case "node":
			return `C:\nodejs\node.exe`, nil
		}
		return filepath.Join(dir, name+".cmd"), nil
	}
	bin, prefix, viaCmd, err := resolveClient(look, "codex", "windows")
	if err != nil || bin != `C:\nodejs\node.exe` || !reflect.DeepEqual(prefix, []string{js}) || viaCmd {
		t.Errorf("npm shim: %q %q %v %v", bin, prefix, viaCmd, err)
	}
	// A node.exe next to the shim wins over PATH.
	os.WriteFile(filepath.Join(dir, "node.exe"), nil, 0o600)
	if bin, _, _, _ := resolveClient(look, "codex", "windows"); bin != filepath.Join(dir, "node.exe") {
		t.Errorf("local node.exe not used: %q", bin)
	}
	// Some other batch file runs as is, with argument checks.
	os.WriteFile(filepath.Join(dir, "other.cmd"), []byte("@echo off\r\nsomething.exe %*\r\n"), 0o600)
	if bin, prefix, viaCmd, _ := resolveClient(look, "other", "windows"); bin != filepath.Join(dir, "other.cmd") || prefix != nil || !viaCmd {
		t.Errorf("plain batch file: %q %q %v", bin, prefix, viaCmd)
	}
	// Not Windows: never rewritten.
	if bin, _, viaCmd, _ := resolveClient(look, "codex", "linux"); bin != shim || viaCmd {
		t.Errorf("linux: %q %v", bin, viaCmd)
	}

	if err := cmdSafe([]string{"-p", "fix the bug & push"}); err == nil {
		t.Error("cmdSafe accepted &")
	}
	if err := cmdSafe([]string{"exec", "--sandbox", "read-only", "Reply with the single word: ok"}); err != nil {
		t.Error(err)
	}
}

// With --subscription Claude Code keeps the user's own login: only the base URL changes.
func TestClaudeEnvSubscription(t *testing.T) {
	orig := []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk", "ANTHROPIC_BASE_URL=https://api.anthropic.com"}
	got := claudeEnv(orig, "http://127.0.0.1:5", 32768, "linux", true)
	want := []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk", "ANTHROPIC_BASE_URL=http://127.0.0.1:5", "CLAUDE_CODE_MAX_CONTEXT_TOKENS=32768"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claudeEnv:\n got %q\nwant %q", got, want)
	}
}

func TestAddSubscription(t *testing.T) {
	parse := func(doc string) *config.Config {
		t.Helper()
		c, err := config.Parse([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		loopbackOnly(c)
		return c
	}
	c := parse(`{"providers":[{"id":"o","type":"ollama","location":"local"}],"routes":{"claude-*":["o:m"],"coder":["o:m"]}}`)
	if err := addSubscription(c, "claude-*"); err != nil {
		t.Fatal(err)
	}
	if got := c.Routes["claude-*"]; len(got) != 2 || got[0].Key() != "anthropic:*" || got[1].Key() != "o:m" {
		t.Fatalf("route %v", got)
	}
	if p, ok := c.Provider("anthropic"); !ok || !p.Passthrough() {
		t.Fatalf("provider %+v", p)
	}
	if len(c.Routes["coder"]) != 1 {
		t.Error("other routes changed")
	}
	// idempotent: a reload of an already-extended config adds nothing
	if err := addSubscription(c, "claude-*"); err != nil || len(c.Routes["claude-*"]) != 2 || len(c.Providers) != 2 {
		t.Fatalf("second add: %v %v", err, c.Routes["claude-*"])
	}

	if err := addSubscription(parse(`{"providers":[{"id":"o","type":"ollama","location":"local"}],"routes":{"coder":["o:m"]}}`), "claude-*"); err == nil {
		t.Error("no claude route: want an error")
	}
	taken := parse(`{"providers":[{"id":"anthropic","type":"ollama","location":"local"}],"routes":{"claude-*":["anthropic:m"]}}`)
	if err := addSubscription(taken, "claude-*"); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Errorf("taken id: %v", err)
	}
}

func TestSubscriptionFlag(t *testing.T) {
	l, err := newLauncher("claude", []string{"--subscription", "-p", "hi"})
	if err != nil || !l.subscription || !reflect.DeepEqual(l.args, []string{"-p", "hi"}) {
		t.Fatalf("claude: %v %+v", err, l)
	}
	if _, err := newLauncher("codex", []string{"--subscription"}); err == nil {
		t.Error("codex --subscription: want an error")
	}
	// later in the arguments it is the client's
	if l, _ := newLauncher("claude", []string{"-p", "--subscription"}); l.subscription {
		t.Error("flag taken from the client's arguments")
	}
}

// Without --subscription Claude Code sends cbrt's placeholder token, so the
// config's passthrough targets are left out instead of answering 401.
func TestDropPassthrough(t *testing.T) {
	c, err := config.Parse([]byte(`{"providers":[{"id":"an","type":"anthropic"},{"id":"o","type":"ollama","location":"local"}],"routes":{"claude-*":["an:*","o:m"],"coder":["o:m"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !dropPassthrough(c) {
		t.Fatal("nothing dropped")
	}
	if got := c.Routes["claude-*"]; len(got) != 1 || got[0].Key() != "o:m" || len(c.Routes["coder"]) != 1 {
		t.Fatalf("routes %v", c.Routes)
	}
	if dropPassthrough(c) {
		t.Error("dropped twice")
	}
}
