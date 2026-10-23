package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alex-danilin/cbrt/internal/hostcheck"
	"github.com/alex-danilin/cbrt/internal/projects"
)

const projectsUsage = `usage: cbrt projects [command]

  (none) | serve [--listen 127.0.0.1:8091] [--no-open]
        the overview page of all connected projects (loopback only); opens it
        in the browser. Its forms park/unpark tasks in PLAN.md and start checks
  connect <dir> [--dry-run] [--public|--private] [--agent claude|codex|agy]
        track a project: git init if needed, PLAN.md, a README.md skeleton
        (with --agent, that agent writes its overview, headless and read-only),
        the plan rule block in AGENTS.md (@AGENTS.md in a CLAUDE.md); never
        overwrites a file; after a git init, commits only the files it created.
        A public repository (asked anonymously of GitHub/GitLab; unknown hosts
        count as public) keeps PLAN.md and .cbrt/ out of git in
        .git/info/exclude; --public/--private skip the detection
  disconnect <name|dir> [--remove-files]
        stop tracking it; --remove-files also removes the AGENTS.md rule block
        and the .git/info/exclude block (PLAN.md and the README stay)
  list  one line per project: name, Now, progress, last commit
  check <name|dir> --agent claude|codex|agy [--dry-run] [--timeout 10m]
        ask the agent (headless, read-only, in the project folder) whether the
        work is heading where PLAN.md says: on track, drifting or off track.
        The answer goes to .cbrt/checks/<UTC time>-<agent>.md (not
        committed); --dry-run prints the prompt and the command

Project list: $CBRT_PROJECTS_HOME/projects.json, default <config dir>/cbrt/projects.json.
`

func runProjects(args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}
	o := projects.Options{Out: stdout}
	var err error
	switch cmd {
	case "serve":
		err = projectsServe(args, stdout, stderr)
	case "connect":
		var dir string
		fs := projectsFlags("connect", stderr)
		dry := fs.Bool("dry-run", false, "print what would change and write nothing")
		pub := fs.Bool("public", false, "treat the repository as public (skip detection)")
		priv := fs.Bool("private", false, "treat the repository as private (skip detection)")
		agent := fs.String("agent", "", "claude, codex or agy: writes the overview of a README connect creates (headless, read-only)")
		timeout := fs.Duration("timeout", projects.DefaultAgentTimeout, "time limit of the --agent run")
		if dir, err = oneArg(fs, args, "connect <dir> [--dry-run] [--public|--private] [--agent claude|codex|agy]"); err == nil {
			o.DryRun = *dry
			o.Agent, o.AgentTimeout = *agent, *timeout
			switch {
			case *pub && *priv:
				err = errors.New("--public and --private exclude each other")
			case *pub:
				o.Visibility = projects.Public
			case *priv:
				o.Visibility = projects.Private
			}
			if err == nil {
				err = projects.Connect(dir, o)
			}
		}
	case "disconnect":
		var key string
		fs := projectsFlags("disconnect", stderr)
		rm := fs.Bool("remove-files", false, "also remove the AGENTS.md rule block")
		if key, err = oneArg(fs, args, "disconnect <name|dir> [--remove-files]"); err == nil {
			err = projects.Disconnect(key, *rm, o)
		}
	case "check":
		var key string
		fs := projectsFlags("check", stderr)
		agent := fs.String("agent", "", "claude, codex or agy (required)")
		dry := fs.Bool("dry-run", false, "print the prompt and the command; run nothing")
		timeout := fs.Duration("timeout", projects.DefaultAgentTimeout, "time limit of the agent run")
		if key, err = oneArg(fs, args, "check <name|dir> --agent claude|codex|agy [--dry-run] [--timeout 10m]"); err == nil {
			if *agent == "" {
				err = errors.New("--agent is required: claude, codex or agy")
				break
			}
			_, err = projects.Check(key, projects.CheckOptions{Agent: *agent, DryRun: *dry, Timeout: *timeout, Out: stdout})
		}
	case "list":
		if len(args) > 0 {
			err = fmt.Errorf("unexpected arguments %q", args)
			break
		}
		var es []projects.Entry
		if es, err = projects.Load(projects.Home()); err == nil {
			err = projects.WriteList(stdout, projects.Collect(context.Background(), es), time.Now())
		}
	case "help", "-h", "--help":
		fmt.Fprint(stdout, projectsUsage)
	default:
		fmt.Fprintf(stderr, "cbrt projects: unknown command %q\n\n%s", cmd, projectsUsage)
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "cbrt projects:", err)
		return 1
	}
	return 0
}

func projectsFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("projects "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// oneArg parses flags placed before or after the single positional argument.
func oneArg(fs *flag.FlagSet, args []string, usage string) (string, error) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !isFlag(a) || len(a) < 2 {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		// a flag that takes a value ("--agent claude") takes the next argument
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if f := fs.Lookup(name); f != nil && i+1 < len(args) {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	if err := fs.Parse(flags); err != nil {
		return "", err
	}
	if len(pos) != 1 || fs.NArg() != 0 {
		return "", errors.New("usage: cbrt projects " + usage)
	}
	return pos[0], nil
}

// projectsListen checks that addr is a loopback host and port: the page has
// no authentication, so it never listens beyond this machine.
func projectsListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", addr, err)
	}
	if !hostcheck.Loopback(host) {
		return fmt.Errorf("--listen %q: the projects page listens on loopback only (127.0.0.1, ::1 or localhost)", addr)
	}
	return nil
}

func projectsServe(args []string, stdout, stderr io.Writer) error {
	fs := projectsFlags("serve", stderr)
	listen := fs.String("listen", "127.0.0.1:8091", "loopback address to listen on")
	noOpen := fs.Bool("no-open", false, "do not open the browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if err := projectsListen(*listen); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	home := projects.Home()
	hs := &http.Server{
		Handler:           projects.Handler(func() ([]projects.Entry, error) { return projects.Load(home) }, nil, nil),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	url := "http://" + ln.Addr().String() + "/"
	fmt.Fprintf(stdout, "cbrt projects: %s (Ctrl+C to stop)\n", url)
	if !*noOpen {
		if err := projects.OpenBrowser(url); err != nil {
			fmt.Fprintf(stdout, "could not open the browser (%v); open the address above\n", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err = <-errc:
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = hs.Shutdown(sctx)
		cancel()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
