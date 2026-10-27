// Command cbrt is a lightweight gateway for free LLM providers.
//
//	cbrt [serve] [-config cbrt.json] [-env-file .env]
//	cbrt init [-config cbrt.json] [-env-file .env] [-force]
//	cbrt claude [--subscription] [claude args...]
//	cbrt codex [codex args...]
//	cbrt record -upstream <url> [-listen addr] [-out dir]
//	cbrt statusline [-url url] [-key key] [-plain]
//	cbrt hook record <adapter_id> <event>
//	cbrt hook warn claude
//	cbrt analyze <run|excerpt|rec|run-note|prune|install|uninstall> …
//	cbrt projects [serve|connect|disconnect|list] …
//	cbrt version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/alex-danilin/cbrt/internal/autoconfig"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/gateway"
	"github.com/alex-danilin/cbrt/internal/record"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/server"
	"github.com/alex-danilin/cbrt/internal/statusline"
	"github.com/alex-danilin/cbrt/internal/usage"
)

// version is set at build time (-ldflags "-X main.version=v1.2.3"); a plain
// "go build" reports the module version from the build info, or "dev".
var version = ""

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "init":
		err = runInit(args)
	case "claude", "codex":
		os.Exit(runLaunch(cmd, args))
	case "record":
		err = runRecord(args)
	case "analyze":
		os.Exit(runAnalyze(args, os.Stdin, os.Stdout, os.Stderr))
	case "hook":
		os.Exit(runHook(args, os.Stdin, os.Stdout, os.Stderr))
	case "projects":
		os.Exit(runProjects(args, os.Stdout, os.Stderr))
	case "statusline":
		os.Exit(statusline.Main(args))
	case "healthcheck":
		os.Exit(healthcheck(args))
	case "version", "-version", "--version":
		fmt.Println("cbrt", versionString())
		return
	case "help", "-h", "--help":
		usageText()
		return
	default:
		usageText()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cbrt:", err)
		os.Exit(1)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func usageText() { fmt.Fprint(os.Stderr, mainUsage) }

// mainUsage is `cbrt help`; README.md's command reference must cover it
// (TestReadmeCoversHelp).
const mainUsage = `usage:
  cbrt claude [args...]      run Claude Code on free models (starts a private gateway; args go to claude)
  cbrt claude --subscription [args...]
                               run Claude Code on your own Claude login first, free models at its limit
  cbrt codex [args...]       run Codex CLI on free models (starts a private gateway; args go to codex)
  cbrt init [-config cbrt.json] [-env-file .env] [-force]
                               detect NVIDIA_API_KEY and Ollama and write a starting cbrt.json
  cbrt [serve] [-config cbrt.json] [-env-file .env]
                               run the gateway (without cbrt.json: detect providers, as init does)
  cbrt record -upstream <url> [-listen 127.0.0.1:8089] [-out captures]
  cbrt statusline [-url http://127.0.0.1:8080] [-key KEY] [-plain]   (Claude Code status line; reads its JSON on stdin)
  cbrt healthcheck [-url http://127.0.0.1:8080]                      (exit 0 if /healthz answers; for container HEALTHCHECKs)
  cbrt hook record <adapter_id> <event>                              (agent hook: spools stdin for cbrt analyze; always exits 0)
  cbrt hook warn claude                                              (context warning hook, opt-in via analyze install; always exits 0)
  cbrt analyze run|excerpt|rec|install …                              (weekly review of agent sessions; "cbrt analyze help")
  cbrt projects [connect|disconnect|list|serve] …                     (progress of all your projects on one local page; "cbrt projects help")
  cbrt version
`

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "cbrt.json", "configuration file")
	envFile := fs.String("env-file", ".env", "KEY=VALUE file loaded into the environment if present and re-read on /api/reload (process variables win)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	setMemoryLimit()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, serveLoader(*cfgPath, explicit, *envFile, autoconfig.Options{}, os.Stderr), nil)
}

// setMemoryLimit sets a soft memory limit that keeps RSS small under bursts: the live
// heap stays ~10–15 MB, but with the default GOGC the heap goal (and RSS) roughly
// doubles under load (docs/benchmarks.md). It is soft: the GC works harder near it,
// it never OOMs. GOMEMLIMIT in the environment overrides it.
func setMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(32 << 20)
	}
}

// serve runs the gateway until ctx is cancelled, then shuts down gracefully:
// in-flight requests get up to 15s to finish and the usage store is flushed.
// load builds the configuration; it is called once at startup and again on every
// /api/reload. ready, if non-nil, is called with the bound address once the
// listener is up (useful with listen "127.0.0.1:0").
func serve(ctx context.Context, load loader, ready func(addr string)) error {
	log := slog.Default()
	cfg, err := load()
	if err != nil {
		return err
	}

	store, err := usage.Open(usage.Options{Dir: cfg.DataDir, RetentionMonths: cfg.RetentionMonths, Logger: log})
	if err != nil {
		return fmt.Errorf("usage store: %w", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			log.Error("closing usage store", "err", cerr)
		}
	}()
	hub := events.NewHub(500, log, func(e events.Event) { store.AddEvent(e) })
	gw, err := gateway.New(cfg, router.NewHealth(time.Now), hub, store, log)
	if err != nil {
		return err
	}
	srv := server.New(server.Options{Gateway: gw, Store: store, Hub: hub, Logger: log, Reload: load})

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	dctx, stopDiscovery := context.WithCancel(ctx)
	defer stopDiscovery()
	go gw.RunDiscovery(dctx, 10*time.Minute)

	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: streams are long-lived; the gateway sets per-write deadlines instead.
	}
	addr := ln.Addr().String()
	errc := make(chan error, 1)
	go func() {
		log.Info("cbrt listening", "addr", addr, "tls", cfg.TLSCert != "", "offline", cfg.Offline,
			"providers", len(cfg.Providers), "routes", len(cfg.Routes), "auth", cfg.APIKey != "")
		if cfg.TLSCert != "" {
			errc <- hs.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		} else {
			errc <- hs.Serve(ln)
		}
	}()
	if ready != nil {
		ready(addr)
	}

	select {
	case err = <-errc:
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = hs.Shutdown(sctx)
		cancel()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func runRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8089", "address to listen on")
	upstream := fs.String("upstream", "", "upstream base URL, e.g. http://127.0.0.1:11434")
	out := fs.String("out", "captures", "directory for captured exchanges")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *upstream == "" {
		return errors.New("-upstream is required")
	}
	u, err := url.Parse(*upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid -upstream %q", *upstream)
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}

	rec := &record.Recorder{Upstream: u, Dir: *out, OnError: func(err error) {
		slog.Error("capture not saved", "err", err)
	}}
	slog.Warn("recording mode: request and response bodies (prompts, code, tool output) are written to disk; secret headers are redacted",
		"dir", *out, "upstream", u.String(), "listen", *listen)
	return http.ListenAndServe(*listen, rec)
}
