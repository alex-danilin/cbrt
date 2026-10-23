package projects

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Agents are the agent CLIs cbrt runs headless, read-only.
var Agents = []string{"claude", "codex", "agy"}

// DefaultAgentTimeout bounds one headless agent run.
const DefaultAgentTimeout = 10 * time.Minute

// agyArgMax is the longest prompt passed to agy on its command line (agy
// takes the prompt only as --print=<prompt>, and a Windows command line
// holds 32767 characters). A longer prompt goes into a file agy is told to
// read.
const agyArgMax = 20000

// maxAnswer bounds what is kept of an agent's output.
const maxAnswer = 1 << 20

// Command is one headless, read-only agent run.
type Command struct {
	Agent      string
	Name       string // the executable, looked up on PATH
	Args       []string
	Dir        string // the working directory: the project
	Stdin      string // the prompt, for CLIs that read it from stdin
	OutputFile string // codex writes its final answer here

	promptFile, prompt string // agy with a long prompt: written before the run
}

// String shows the command for a dry run.
func (c Command) String() string {
	parts := []string{c.Name}
	for _, a := range c.Args {
		if len(a) > 200 {
			a = a[:200] + "…"
		}
		if a == "" || strings.ContainsAny(a, " \t\n\"'<>|&") {
			a = strconv.Quote(a)
		}
		parts = append(parts, a)
	}
	s := strings.Join(parts, " ")
	if c.Stdin != "" {
		s += "  (the prompt on stdin)"
	}
	s += "  (in " + c.Dir + ")"
	return s
}

// RunFunc runs an agent command and returns its answer: stdout, or the
// content of OutputFile when the command has one. Tests inject a fake.
type RunFunc func(ctx context.Context, c Command) (string, error)

func validAgent(agent string) error {
	if slices.Contains(Agents, agent) {
		return nil
	}
	return fmt.Errorf("agent %q: want one of %s", agent, strings.Join(Agents, ", "))
}

// newCommand builds the headless, read-only command line for agent in dir.
// tmp is a folder for the run's own files (codex's answer, agy's long
// prompt), outside the project. No permission-skipping flag is ever used:
//
//   - claude: -p with the prompt on stdin, plan permission mode, only the
//     read-only tools Read, Grep and Glob, and no MCP servers
//     (--strict-mcp-config without --mcp-config).
//   - codex: exec in the read-only sandbox in the project folder (its working
//     folder, not -C: npm installs codex as a .cmd, whose arguments cmd.exe
//     re-parses), the prompt on stdin ("-"), configured MCP servers replaced
//     by none (-c mcp_servers={}), the last message written to a file
//     (stdout carries the whole transcript).
//   - agy: plan mode and its terminal sandbox; the prompt attached to
//     --print= (a bare --print swallows the next flag); agy has no stdin
//     prompt in text mode, so a long prompt goes into a file in tmp that agy
//     may read (--add-dir). --disable-slash-commands is never passed: it
//     makes agy ignore --mode plan. agy has no flag to skip its MCP servers.
func newCommand(agent, dir, prompt string, timeout time.Duration, tmp string) (Command, error) {
	if err := validAgent(agent); err != nil {
		return Command{}, err
	}
	c := Command{Agent: agent, Name: agent, Dir: dir}
	switch agent {
	case "claude":
		c.Args = []string{"-p", "--output-format", "text", "--permission-mode", "plan", "--tools", "Read,Grep,Glob", "--strict-mcp-config"}
		c.Stdin = prompt
	case "codex":
		c.OutputFile = filepath.Join(tmp, "answer.md")
		c.Args = []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "-c", "mcp_servers={}", "--output-last-message", c.OutputFile, "-"}
		c.Stdin = prompt
	case "agy":
		arg := prompt
		var extra []string
		if len(prompt) > agyArgMax {
			c.promptFile, c.prompt = filepath.Join(tmp, "prompt.md"), prompt
			arg = "Read the file " + c.promptFile + " and follow the instructions in it exactly. Do not edit, create or delete any file."
			extra = []string{"--add-dir", tmp}
		}
		c.Args = append([]string{"--print=" + arg, "--mode", "plan", "--sandbox", "--add-dir", dir}, extra...)
		c.Args = append(c.Args, "--output-format", "text", "--print-timeout", agyTimeout(timeout))
	}
	return c, nil
}

// agyTimeout is agy's own limit: a minute under cbrt's, so agy can still
// answer before it is stopped.
func agyTimeout(d time.Duration) string {
	if d > 2*time.Minute {
		d -= time.Minute
	} else {
		d = d * 9 / 10
	}
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return d.Round(time.Second).String()
}

// batchMeta are the characters cmd.exe interprets in a batch file's
// arguments.
const batchMeta = "&|<>^%!\"\r\n"

// lookPath finds an agent executable; tests point it at a fake.
var lookPath = exec.LookPath

// ExecRun runs c as a process and returns its answer.
func ExecRun(ctx context.Context, c Command) (string, error) {
	path, err := lookPath(c.Name)
	if err != nil {
		return "", fmt.Errorf("%s is not on PATH", c.Name)
	}
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".cmd" || ext == ".bat" {
		// cmd.exe re-parses a batch file's arguments: refuse any it could
		// read as an operator or a variable.
		for _, a := range c.Args {
			if strings.ContainsAny(a, batchMeta) {
				return "", fmt.Errorf("%s is a batch file (%s) and the argument %q holds a character cmd.exe would interpret (%s): not run", c.Name, filepath.Base(path), Clean(clipBytes(a, 80)), batchMeta)
			}
		}
	}
	cmd := exec.CommandContext(ctx, path, c.Args...)
	cmd.Dir = c.Dir
	cmd.WaitDelay = 2 * time.Second
	killTree(cmd) // a timeout stops the agent and everything it started
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	var stdout, stderr capped
	stdout.max, stderr.max = maxAnswer, 64<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		msg := lastLine(stderr.String())
		if msg == "" {
			msg = lastLine(stdout.String())
		}
		if msg != "" {
			return "", fmt.Errorf("%s: %v: %s", c.Name, err, Clean(msg))
		}
		return "", fmt.Errorf("%s: %v", c.Name, err)
	}
	if c.OutputFile != "" {
		b, err := os.ReadFile(c.OutputFile)
		if err != nil {
			return "", fmt.Errorf("%s wrote no answer: %v", c.Name, reason(err))
		}
		if len(b) > maxAnswer {
			b = b[:maxAnswer]
		}
		return string(b), nil
	}
	return stdout.String(), nil
}

// capped is a buffer that keeps the first max bytes.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// runAgent runs agent in dir with prompt under a timeout and returns its
// trimmed answer. The run's temporary files live outside the project and
// are removed afterwards.
func runAgent(ctx context.Context, run RunFunc, agent, dir, prompt string, timeout time.Duration) (string, error) {
	if run == nil {
		run = ExecRun
	}
	if timeout <= 0 {
		timeout = DefaultAgentTimeout
	}
	tmp, err := os.MkdirTemp("", "cbrt-agent-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	c, err := newCommand(agent, dir, prompt, timeout, tmp)
	if err != nil {
		return "", err
	}
	if c.promptFile != "" {
		if err := os.WriteFile(c.promptFile, []byte(c.prompt), 0o600); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := run(ctx, c)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%s did not answer within %v", agent, timeout)
	}
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(strings.TrimPrefix(out, bom))
	if out == "" {
		return "", fmt.Errorf("%s gave an empty answer", agent)
	}
	return out, nil
}
