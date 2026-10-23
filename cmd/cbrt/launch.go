package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alex-danilin/cbrt/internal/autoconfig"
	"github.com/alex-danilin/cbrt/internal/config"
)

// launcher runs `cbrt claude` / `cbrt codex`: a private gateway on a free
// loopback port, and the client pointed at it, sharing this terminal.
type launcher struct {
	client string   // "claude" or "codex"
	args   []string // passed to the client verbatim, after the arguments cbrt adds
	// subscription (claude --subscription): Claude Code keeps the user's own
	// login and its requests go to Anthropic unchanged first, falling back to
	// the free targets when the plan's limit is hit.
	subscription bool
	environ      []string // the original process environment: the child gets this, not the env file's variables
	goos         string
	cwd          string
	dir          string // <UserConfigDir>/cbrt: config, .env, data, log
	exe          string // this program, for the Claude Code status line
	lookPath     func(string) (string, error)
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer
	detect       autoconfig.Options // base detection options for zero-config (tests point it at a fake Ollama)
}

// clientInfo is what differs between the supported clients.
type clientInfo struct {
	route string // the route the client's requests use
	hint  string // how to install it
}

var clients = map[string]clientInfo{
	"claude": {route: "claude-*", hint: "npm install -g @anthropic-ai/claude-code (or see https://docs.claude.com/en/docs/claude-code)"},
	"codex":  {route: "coder", hint: "npm install -g @openai/codex"},
}

func runLaunch(client string, args []string) int {
	l, err := newLauncher(client, args)
	if err == nil {
		var code int
		if code, err = l.run(context.Background()); err == nil {
			return code
		}
	}
	fmt.Fprintln(os.Stderr, "cbrt:", err)
	return 1
}

func newLauncher(client string, args []string) (*launcher, error) {
	subscription := false
	if len(args) > 0 && args[0] == "--subscription" {
		if client != "claude" {
			return nil, errors.New("--subscription is only for claude: it relays Claude Code's own requests with your Claude login")
		}
		subscription, args = true, args[1:]
	}
	ucd, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("no user configuration directory: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &launcher{
		client: client, args: args, subscription: subscription, environ: os.Environ(), goos: runtime.GOOS,
		cwd: cwd, dir: filepath.Join(ucd, "cbrt"), exe: exe, lookPath: exec.LookPath,
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
	}, nil
}

// getenv reads the original environment (not what the env file added).
func (l *launcher) getenv(k string) string { return envValue(l.environ, k, l.goos) }

// configSource picks the configuration: $CBRT_CONFIG, ./cbrt.json,
// <dir>/cbrt.json, else "" (detect providers). desc says which, for the summary.
func (l *launcher) configSource() (cfgPath, desc string) {
	if p := l.getenv("CBRT_CONFIG"); p != "" {
		return p, p + " (CBRT_CONFIG)"
	}
	for _, p := range []string{filepath.Join(l.cwd, "cbrt.json"), filepath.Join(l.dir, "cbrt.json")} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, p
		}
	}
	return "", "none found, detected providers (write one with `cbrt init`, or put it in " + filepath.Join(l.dir, "cbrt.json") + ")"
}

// envFile is $CBRT_ENV_FILE, else <dir>/.env. Never the current directory's
// .env: the launcher runs inside arbitrary projects.
func (l *launcher) envFile() string {
	if p := l.getenv("CBRT_ENV_FILE"); p != "" {
		return p
	}
	return filepath.Join(l.dir, ".env")
}

func (l *launcher) logPath() string { return filepath.Join(l.dir, "cbrt.log") }

// loopbackOnly makes a configuration serve only this launcher's client: a free
// loopback port, plain HTTP, no key (the Host check still guards against DNS rebinding).
func loopbackOnly(c *config.Config) {
	c.Listen = "127.0.0.1:0"
	c.APIKey = ""
	c.TLSCert, c.TLSKey = "", ""
	c.TrustProxy = false
	c.AllowedHosts = nil
}

func (l *launcher) run(ctx context.Context) (int, error) {
	info, ok := clients[l.client]
	if !ok {
		return 0, fmt.Errorf("unknown client %q", l.client)
	}
	bin, prefix, viaCmd, err := resolveClient(l.lookPath, l.client, l.goos)
	if err != nil {
		return 0, fmt.Errorf("%w: install it with %s", err, info.hint)
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return 0, err
	}

	// Gateway logs go to a file: on the terminal they would corrupt the client's TUI.
	lf, err := os.OpenFile(l.logPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("log file: %w", err)
	}
	defer lf.Close()
	log := slog.New(slog.NewTextHandler(lf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prevLog, prevStd := slog.Default(), stdlog.Writer()
	slog.SetDefault(log) // serve logs through slog.Default
	stdlog.SetOutput(lf) // net/http's error log
	defer func() { slog.SetDefault(prevLog); stdlog.SetOutput(prevStd) }()
	log.Info("launcher starting", "client", l.client, "version", versionString(), "cwd", l.cwd)

	cfgPath, source := l.configSource()
	env := config.NewEnvFile(l.envFile())
	var base loader
	var warnings []string // from the first detection; read after the gateway is ready
	if cfgPath != "" {
		base = fileLoader(env, cfgPath)
	} else {
		o := l.detect
		if o.DataDir == "" {
			o.DataDir = filepath.Join(l.dir, "data")
		}
		first := true
		base = zeroLoader(env, o, func(rep autoconfig.Report) {
			logReport(log, rep)
			if first {
				first = false
				warnings = rep.Warnings
			}
		})
	}
	var mu sync.Mutex
	var initial *config.Config
	var dataDir string // claimed on the first load and kept for reloads
	release := func() {}
	defer func() { release() }()
	load := func() (*config.Config, error) {
		c, err := base()
		if err != nil {
			return nil, err
		}
		loopbackOnly(c)
		if l.subscription {
			if err := addSubscription(c, info.route); err != nil {
				return nil, err
			}
		} else if dropped := dropPassthrough(c); dropped {
			if initial == nil {
				note := "the config's anthropic targets need your own login: they are left out (run cbrt claude --subscription to use them)"
				warnings = append(warnings, note)
				log.Warn(note)
			}
			if err := c.Validate(); err != nil {
				return nil, err
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if dataDir == "" {
			dir := c.DataDir
			if !filepath.IsAbs(dir) && cfgPath != "" { // relative to the config file, not the project dir
				dir = filepath.Join(filepath.Dir(cfgPath), dir)
			}
			var note string
			dataDir, release, note, err = claimDataDir(l.dir, dir)
			if err != nil {
				return nil, err
			}
			if note != "" {
				warnings = append(warnings, note)
				log.Warn(note)
			}
		}
		c.DataDir = dataDir
		if initial == nil {
			initial = c
		}
		return c, nil
	}

	// Signals: before the client runs, Ctrl+C aborts the startup. While it runs,
	// the client owns Ctrl+C (it shares the console); a SIGTERM is passed on.
	gctx, stopGateway := context.WithCancel(ctx)
	defer stopGateway()
	var child atomic.Pointer[os.Process]
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)
	sigDone := make(chan struct{})
	defer close(sigDone)
	go func() {
		for {
			select {
			case <-sigDone:
				return
			case s := <-sigc:
				p := child.Load()
				switch {
				case p == nil:
					stopGateway()
				case s != os.Interrupt && l.goos != "windows":
					p.Signal(s)
				}
			}
		}
	}()

	addrc := make(chan string, 1)
	errc := make(chan error, 1)
	go func() { errc <- serve(gctx, load, func(a string) { addrc <- a }) }()
	var addr string
	select {
	case addr = <-addrc:
	case err := <-errc:
		if err == nil {
			err = errors.New("interrupted")
		}
		fmt.Fprintf(l.stderr, "cbrt: config: %s\n", source)
		for _, w := range warnings {
			fmt.Fprintln(l.stderr, "cbrt: warning: "+w)
		}
		return 0, fmt.Errorf("gateway: %w (log: %s)", err, l.logPath())
	}
	baseURL := "http://" + addr
	stop := func() {
		stopGateway()
		select {
		case err := <-errc:
			if err != nil {
				log.Error("gateway shutdown", "err", err)
			}
		case <-time.After(20 * time.Second):
			log.Error("gateway did not stop within 20s")
		}
	}
	defer func() {
		if stop != nil {
			stop()
		}
	}()

	mu.Lock()
	cfg := initial
	mu.Unlock()
	maxCtx := autoconfig.MinContext(cfg, info.route)
	l.summary(source, cfg, info.route, maxCtx, baseURL, warnings)

	var args, childEnv []string
	switch l.client {
	case "claude":
		settings := ""
		if viaCmd {
			// A JSON string cannot pass through cmd.exe intact: use a file.
			f, err := os.CreateTemp(l.dir, "claude-settings-*.json")
			if err != nil {
				return 0, err
			}
			settings = f.Name()
			defer os.Remove(settings)
			_, err = f.Write(claudeSettings(l.exe, baseURL, l.goos))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return 0, err
			}
		}
		args = claudeArgs(l.exe, baseURL, settings, l.goos, l.args)
		childEnv = claudeEnv(l.environ, baseURL, maxCtx, l.goos, l.subscription)
	case "codex":
		args = codexArgs(baseURL, maxCtx, l.args)
		childEnv = append([]string(nil), l.environ...)
	}
	args = append(append([]string(nil), prefix...), args...)
	if viaCmd {
		if err := cmdSafe(args); err != nil {
			return 0, fmt.Errorf("%s is a batch file (%s): %w", l.client, bin, err)
		}
	}

	cmd := exec.Command(bin, args...)
	cmd.Env = childEnv
	cmd.Stdin, cmd.Stdout, cmd.Stderr = l.stdin, l.stdout, l.stderr
	log.Info("starting client", "path", bin, "gateway", baseURL)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s: %w", l.client, err)
	}
	child.Store(cmd.Process)
	werr := cmd.Wait()
	code := exitCode(cmd, werr)
	log.Info("client exited", "code", code)
	stop()
	stop = nil
	if code < 0 {
		return 0, fmt.Errorf("%s: %w", l.client, werr)
	}
	return code, nil
}

// summary prints, before the client takes over the terminal, where the
// configuration came from, what serves the client, and where to look.
func (l *launcher) summary(source string, cfg *config.Config, route string, maxCtx int, baseURL string, warnings []string) {
	w := l.stderr
	fmt.Fprintf(w, "cbrt: config: %s\n", source)
	name, ts := routeTargets(cfg, route)
	if len(ts) == 0 {
		fmt.Fprintf(w, "cbrt: warning: the config has no %q route, so %s's requests will fail: add one (see README)\n", route, l.client)
	} else {
		keys := make([]string, len(ts))
		for i, t := range ts {
			keys[i] = t.Key()
		}
		ctx := ""
		if maxCtx > 0 {
			ctx = fmt.Sprintf(" (context %d)", maxCtx)
		}
		fmt.Fprintf(w, "cbrt: route %s → %s%s\n", name, strings.Join(keys, ", "), ctx)
	}
	if l.subscription {
		fmt.Fprintln(w, "cbrt: subscription: Claude Code uses your own Claude login; the free targets take over at your plan's limit")
	}
	fmt.Fprintf(w, "cbrt: dashboard %s/  log %s\n", baseURL, l.logPath())
	for _, x := range warnings {
		fmt.Fprintln(w, "cbrt: warning: "+x)
	}
}

// routeTargets finds the route a model name uses: an exact route, else the
// first glob route (sorted) that matches, as the gateway resolves it.
func routeTargets(c *config.Config, model string) (string, []config.Target) {
	if c == nil {
		return "", nil
	}
	if ts, ok := c.Routes[model]; ok {
		return model, ts
	}
	var globs []string
	for g := range c.Routes {
		if strings.ContainsAny(g, "*?[") {
			globs = append(globs, g)
		}
	}
	sort.Strings(globs)
	for _, g := range globs {
		if ok, _ := path.Match(g, model); ok {
			return g, c.Routes[g]
		}
	}
	return "", nil
}

// exitCode is the child's exit status: its code, 128+signal when a signal
// killed it (Unix), or -1 when it could not be determined (err says why).
func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState == nil {
		return -1
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if c := cmd.ProcessState.ExitCode(); c >= 0 {
		return c
	}
	return -1
}

// claudeSettings is the --settings JSON that adds the cbrt status line.
func claudeSettings(exe, baseURL, goos string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]any{"statusLine": map[string]any{
		"type": "command", "command": statusLineCommand(exe, baseURL, goos),
	}})
	return bytes.TrimSpace(b.Bytes())
}

// statusLineCommand is the shell command Claude Code runs for the status line.
// Claude Code runs it through a shell (Git Bash or cmd.exe on Windows), so the
// executable path is quoted: on Windows with forward slashes in double quotes,
// which both shells accept and which needs no backslash escaping; elsewhere in
// POSIX single quotes.
func statusLineCommand(exe, baseURL, goos string) string {
	q := shellQuote(exe)
	if goos == "windows" {
		q = `"` + strings.ReplaceAll(exe, `\`, "/") + `"`
	}
	return q + " statusline -url " + baseURL
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// claudeSubcommands are Claude Code's subcommands (2.1.x). They don't start an
// interactive session, and some reject options they don't know, so they get
// no --settings (the environment still points them at the gateway).
var claudeSubcommands = map[string]bool{
	"agents": true, "attach": true, "auth": true, "auto-mode": true, "doctor": true, "gateway": true,
	"import": true, "install": true, "logs": true, "mcp": true, "plugin": true, "plugins": true,
	"project": true, "respawn": true, "rm": true, "setup-token": true, "stop": true, "kill": true,
	"ultrareview": true, "update": true, "upgrade": true,
}

// claudeArgs prepends the status-line settings (a JSON string, or a file when
// settingsFile is set) to the user's arguments, unless they run a subcommand.
func claudeArgs(exe, baseURL, settingsFile, goos string, user []string) []string {
	if len(user) > 0 && claudeSubcommands[user[0]] {
		return user
	}
	s := settingsFile
	if s == "" {
		s = string(claudeSettings(exe, baseURL, goos))
	}
	return append([]string{"--settings", s}, user...)
}

// claudeEnv is the original environment pointed at the gateway. ANTHROPIC_API_KEY
// is removed (it would be sent instead of the token); maxCtx 0 leaves
// CLAUDE_CODE_MAX_CONTEXT_TOKENS as the user has it. With subscription, only
// the base URL changes: Claude Code keeps sending the user's own login (its
// claude.ai sign-in, or their API key), which the gateway relays to Anthropic.
func claudeEnv(orig []string, baseURL string, maxCtx int, goos string, subscription bool) []string {
	drop := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"}
	set := []string{"ANTHROPIC_BASE_URL=" + baseURL, "ANTHROPIC_AUTH_TOKEN=cbrt"}
	if subscription {
		drop, set = []string{"ANTHROPIC_BASE_URL"}, []string{"ANTHROPIC_BASE_URL=" + baseURL}
	}
	if maxCtx > 0 {
		drop = append(drop, "CLAUDE_CODE_MAX_CONTEXT_TOKENS")
		set = append(set, "CLAUDE_CODE_MAX_CONTEXT_TOKENS="+strconv.Itoa(maxCtx))
	}
	return append(withoutEnv(orig, goos, drop...), set...)
}

// subscriptionProvider is the passthrough provider --subscription adds.
const subscriptionProvider = "anthropic"

// addSubscription puts Anthropic passthrough first in the route Claude Code
// uses (adding the provider unless the config has one), so the user's plan
// serves first and the configured targets take over at its limit.
func addSubscription(c *config.Config, route string) error {
	name, ts := routeTargets(c, route)
	if name == "" {
		return fmt.Errorf("--subscription: the config has no %q route to put Anthropic in front of", route)
	}
	id := ""
	for _, p := range c.Providers {
		if p.Passthrough() {
			id = p.ID
			break
		}
	}
	if id == "" {
		id = subscriptionProvider
		for _, p := range c.Providers {
			if p.ID == id {
				return fmt.Errorf("--subscription: provider id %q is taken by a provider of type %s", id, p.Type)
			}
		}
		c.Providers = append(c.Providers, config.Provider{ID: id, Type: "anthropic", Location: "cloud", BaseURL: "https://api.anthropic.com"})
	}
	present := false
	for _, t := range ts {
		present = present || t.Provider == id
	}
	if !present {
		c.Routes[name] = append([]config.Target{{Provider: id, Model: "*"}}, ts...)
	}
	return c.Validate()
}

// dropPassthrough removes passthrough targets from every route. Without
// --subscription the client sends cbrt's placeholder token, not the user's
// login, so Anthropic would answer 401, which is relayed rather than failed over.
func dropPassthrough(c *config.Config) bool {
	dropped := false
	for name, ts := range c.Routes {
		kept := make([]config.Target, 0, len(ts))
		for _, t := range ts {
			if p, ok := c.Provider(t.Provider); ok && p.Passthrough() {
				dropped = true
				continue
			}
			kept = append(kept, t)
		}
		c.Routes[name] = kept
	}
	return dropped
}

// codexArgs configures a "cbrt" model provider with -c overrides ahead of the
// user's arguments. Codex parses each value as TOML and falls back to the literal
// string, so no quoting is needed (and none survives an npm .cmd shim reliably).
func codexArgs(baseURL string, maxCtx int, user []string) []string {
	a := []string{
		"-c", "model_provider=cbrt",
		"-c", "model_providers.cbrt.name=cbrt",
		"-c", "model_providers.cbrt.base_url=" + baseURL + "/v1",
		"-c", "model_providers.cbrt.wire_api=responses",
		"-c", "model=coder",
	}
	if maxCtx > 0 {
		a = append(a, "-c", "model_context_window="+strconv.Itoa(maxCtx))
	}
	return append(a, user...)
}

func envName(kv string) string {
	if k, _, ok := strings.Cut(kv, "="); ok {
		return k
	}
	return kv
}

func sameEnvName(a, b, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// withoutEnv returns env without the named variables (case-insensitively on Windows).
func withoutEnv(env []string, goos string, names ...string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		k := envName(kv)
		for _, n := range names {
			if k != "" && sameEnvName(k, n, goos) {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// envValue returns the last value of name in env.
func envValue(env []string, name, goos string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k != "" && sameEnvName(k, name, goos) {
			v = val
		}
	}
	return v
}

// resolveClient finds the client executable. On Windows an npm-installed client
// is a .cmd shim, and a batch file runs under cmd.exe, which re-parses its
// command line (quotes, %, &, ^ ...): Go quotes arguments for the C runtime
// rules only. So an npm shim is resolved to the `node <script>.js` it runs.
// viaCmd reports a batch file that could not be resolved; its arguments must
// then pass cmdSafe.
func resolveClient(lookPath func(string) (string, error), name, goos string) (bin string, prefix []string, viaCmd bool, err error) {
	p, err := lookPath(name)
	if err != nil {
		return "", nil, false, fmt.Errorf("%s was not found in PATH", name)
	}
	ext := strings.ToLower(filepath.Ext(p))
	if goos != "windows" || (ext != ".cmd" && ext != ".bat") {
		return p, nil, false, nil
	}
	if js := npmShimScript(p); js != "" {
		node := filepath.Join(filepath.Dir(p), "node.exe")
		if st, err := os.Stat(node); err != nil || st.IsDir() {
			node, err = lookPath("node")
			if err != nil {
				return p, nil, true, nil
			}
		}
		return node, []string{js}, false, nil
	}
	return p, nil, true, nil
}

// npmShimLine matches the script an npm cmd-shim runs: "%dp0%\node_modules\...\cli.js".
var npmShimLine = regexp.MustCompile(`"%dp0%\\([^"%]+\.[cm]?js)"`)

// npmShimScript returns the absolute path of the script an npm .cmd shim runs, or "".
func npmShimScript(shim string) string {
	f, err := os.Open(shim)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return ""
	}
	m := npmShimLine.FindAllSubmatch(b, -1)
	if len(m) == 0 {
		return ""
	}
	rel := strings.ReplaceAll(string(m[len(m)-1][1]), `\`, string(filepath.Separator))
	js := filepath.Join(filepath.Dir(shim), rel)
	if st, err := os.Stat(js); err != nil || st.IsDir() {
		return ""
	}
	return js
}

// cmdSafe rejects arguments that cmd.exe would interpret when it runs a batch file.
func cmdSafe(args []string) error {
	for _, a := range args {
		if i := strings.IndexAny(a, "\"%^&|<>!\r\n"); i >= 0 {
			return fmt.Errorf("argument %q contains %q, which cmd.exe would interpret: install the client as an .exe, or avoid the character", a, a[i])
		}
	}
	return nil
}
