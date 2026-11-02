package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/alex-danilin/cbrt/internal/agents"
)

const agentsUsage = `usage: cbrt agents <command>

  import <file> [--from claude|antigravity|codex] [--force] [--dry-run]
        read a Claude Code or Antigravity agent (.md) or a Codex agent (.toml)
        into cbrt's agents folder as cbrt-<name>.md, and show what each tool
        keeps, maps or drops when the agent is installed. Never changes <file>.
        --from overrides the detected tool; --force replaces an earlier
        import; --dry-run shows the report and writes nothing

  install <name> [--tools claude,antigravity,codex] [--write]
        write an imported agent into each tool's agent folder (~/.claude/agents,
        ~/.gemini/config/agents, ~/.codex/agents) and list what each tool drops.
        Shows the changes only until you add --write. --tools picks the tools
        (agy is antigravity; default all). cbrt replaces its own files; a file
        of yours is backed up to <file>.bak first, and never twice

Agents folder: $CBRT_AGENTS_HOME, default <config dir>/cbrt/agents.
`

func runAgents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, agentsUsage)
		return 2
	}
	cmd, args := args[0], args[1:]
	var err error
	switch cmd {
	case "import":
		var src string
		fs := flag.NewFlagSet("agents import", flag.ContinueOnError)
		fs.SetOutput(stderr)
		from := fs.String("from", "", "claude, antigravity or codex (default: detected)")
		force := fs.Bool("force", false, "replace an earlier import of the same name")
		dry := fs.Bool("dry-run", false, "show the report and write nothing")
		if src, err = oneArg(fs, args, "import <file> [--from claude|antigravity|codex] [--force] [--dry-run]"); err != nil {
			break
		}
		o := agents.ImportOptions{Force: *force, DryRun: *dry}
		if *from != "" {
			if o.From, err = agents.ParseTool(*from); err != nil {
				break
			}
		}
		var r agents.Result
		if r, err = agents.Import(src, o); err == nil {
			agents.WriteReport(stdout, r)
		}
	case "install":
		var name string
		fs := flag.NewFlagSet("agents install", flag.ContinueOnError)
		fs.SetOutput(stderr)
		tools := fs.String("tools", "", "comma-separated: claude, antigravity (agy), codex (default: all)")
		write := fs.Bool("write", false, "write the files (default: show the changes only)")
		if name, err = oneArg(fs, args, "install <name> [--tools claude,antigravity,codex] [--write]"); err != nil {
			break
		}
		o := agents.InstallOptions{Write: *write, Out: stdout}
		for _, s := range strings.Split(*tools, ",") {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			var t agents.Tool
			if t, err = agents.ParseTool(s); err != nil {
				break
			}
			o.Tools = append(o.Tools, t)
		}
		if err == nil {
			err = agents.Install(name, o)
		}
	case "help", "-h", "--help":
		fmt.Fprint(stdout, agentsUsage)
	default:
		fmt.Fprintf(stderr, "cbrt agents: unknown command %q\n\n%s", cmd, agentsUsage)
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "cbrt agents:", err)
		return 1
	}
	return 0
}
