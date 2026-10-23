package codex

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// toolCall is what normalization needs from a function_call, custom_tool_call
// or local_shell_call.
type toolCall struct {
	raw       string // name as written, with "<namespace>." when there is one
	normName  string
	args      []byte // arguments JSON (function_call) or the free-form input
	files     []string
	shellText string
}

// normalizeTool maps a Codex tool to the core's vocabulary.
//
//   - shell, shell_command, exec_command, local_shell, and the free-form
//     "exec" tool are shell commands, refined by classifyShell; write_stdin
//     and "wait" on an exec cell (arguments carry cell_id) continue one.
//   - apply_patch (custom or function tool, or `apply_patch` run as a shell
//     command) → edit; view_image → read_file.
//   - web_search, and tools in the "web" namespace → web.
//   - spawn_agent, send_input, resume_agent, close_agent, wait_agent, and
//     "wait" without cell_id (waiting on agents) → subagent.
//   - request_user_input → ask_user.
//   - a namespace or name starting with "mcp" / containing "__" → mcp.
//   - anything else (update_plan, get_goal, …) → other.
func normalizeTool(namespace, name string, args []byte, freeform bool) toolCall {
	tc := toolCall{raw: name, args: args}
	if namespace != "" {
		tc.raw = namespace + "." + name
	}
	switch {
	case namespace == "web":
		tc.normName = analyze.ToolWeb
		return tc
	case strings.HasPrefix(namespace, "mcp"), strings.HasPrefix(name, "mcp__"):
		tc.normName = analyze.ToolMCP
		return tc
	case namespace != "":
		tc.normName = analyze.ToolOther
		return tc
	}
	switch name {
	case "apply_patch":
		tc.normName = analyze.ToolEdit
		patch := string(args)
		if !freeform {
			var a struct {
				Input string `json:"input"`
			}
			if json.Unmarshal(args, &a) == nil && a.Input != "" {
				patch = a.Input
			}
		}
		tc.files = patchFiles(patch)
	case "shell", "shell_command", "exec_command", "local_shell", "exec":
		text := string(args)
		if !freeform {
			text = shellCommand(args)
		}
		tc.shellText = text
		tc.normName, tc.files = classifyShell(text)
	case "write_stdin":
		tc.normName = analyze.ToolShell
	case "wait":
		var a map[string]json.RawMessage
		if json.Unmarshal(args, &a) == nil && a["cell_id"] != nil {
			tc.normName = analyze.ToolShell
		} else {
			tc.normName = analyze.ToolSubagent
		}
	case "view_image":
		tc.normName = analyze.ToolReadFile
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(args, &a) == nil && a.Path != "" {
			tc.files = []string{a.Path}
		}
	case "web_search":
		tc.normName = analyze.ToolWeb
	case "spawn_agent", "send_input", "resume_agent", "close_agent", "wait_agent":
		tc.normName = analyze.ToolSubagent
	case "request_user_input":
		tc.normName = analyze.ToolAskUser
	default:
		if strings.Contains(name, "__") {
			tc.normName = analyze.ToolMCP
		} else {
			tc.normName = analyze.ToolOther
		}
	}
	return tc
}

// shellCommand extracts the command text from shell-like arguments: a string
// "command" or "cmd", or an argv array whose script is the last element when
// it runs through a shell (`bash -lc …`, `powershell -Command …`).
func shellCommand(args []byte) string {
	var a struct {
		Command json.RawMessage `json:"command"`
		Cmd     json.RawMessage `json:"cmd"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	v := a.Command
	if len(v) == 0 {
		v = a.Cmd
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var argv []string
	if json.Unmarshal(v, &argv) != nil || len(argv) == 0 {
		return ""
	}
	return argvText(argv)
}

func argvText(argv []string) string {
	if len(argv) >= 3 {
		switch base := strings.ToLower(strings.TrimSuffix(lastPathElem(argv[0]), ".exe")); base {
		case "bash", "sh", "zsh", "powershell", "pwsh", "cmd":
			return argv[len(argv)-1]
		}
	}
	return strings.Join(argv, " ")
}

func lastPathElem(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

var (
	readCmds   = map[string]bool{"cat": true, "head": true, "tail": true, "less": true, "get-content": true, "gc": true, "type": true, "nl": true}
	searchCmds = map[string]bool{"rg": true, "grep": true, "egrep": true, "find": true, "fd": true, "ls": true, "dir": true, "tree": true,
		"get-childitem": true, "gci": true, "select-string": true, "sls": true}
	shellMeta = regexp.MustCompile(`[|;&<>` + "`" + `$]`)
)

// classifyShell decides on the command's first word only: a read command
// (cat, head, tail, Get-Content, `sed -n`, …) is read_file and, when the
// command has no pipes, redirections or chaining, its non-flag arguments are
// the files read; a search command (rg, grep, find, ls, Get-ChildItem,
// Select-String, …) is search; `apply_patch` is edit; anything else is shell.
// A pipeline is classified by its first command.
func classifyShell(cmd string) (string, []string) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return analyze.ToolShell, nil
	}
	first := strings.ToLower(strings.TrimSuffix(lastPathElem(fields[0]), ".exe"))
	switch {
	case first == "apply_patch":
		return analyze.ToolEdit, patchFiles(cmd)
	case readCmds[first], first == "sed" && len(fields) > 1 && fields[1] == "-n":
		var files []string
		if !shellMeta.MatchString(cmd) {
			for i, f := range fields[1:] {
				if strings.HasPrefix(f, "-") || (first == "sed" && i == 1) {
					continue // flags, and sed's script
				}
				files = append(files, strings.Trim(f, `"'`))
			}
		}
		return analyze.ToolReadFile, files
	case searchCmds[first]:
		return analyze.ToolSearch, nil
	}
	return analyze.ToolShell, nil
}

// patchFiles lists the paths an apply_patch envelope adds, updates, deletes
// or moves to, in order of appearance.
func patchFiles(patch string) []string {
	var files []string
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimRight(line, "\r")
		for _, p := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
			if f, ok := strings.CutPrefix(line, p); ok && strings.TrimSpace(f) != "" {
				files = append(files, strings.TrimSpace(f))
			}
		}
	}
	return files
}

var exitCodeRe = regexp.MustCompile(`^(?:Exit code:|Process exited with code) (-?\d+)`)

// exitCode finds the exit code a tool output records: the text form
// ("Exit code: N" / "Process exited with code N" on the first line) or the
// JSON form ({"output": …, "metadata": {"exit_code": N}}).
func exitCode(out string) (int, bool) {
	t := strings.TrimSpace(out)
	if m := exitCodeRe.FindStringSubmatch(t); m != nil {
		n, err := strconv.Atoi(m[1])
		return n, err == nil
	}
	if strings.HasPrefix(t, "{") {
		var j struct {
			Metadata *struct {
				ExitCode *int `json:"exit_code"`
			} `json:"metadata"`
		}
		if json.Unmarshal([]byte(t), &j) == nil && j.Metadata != nil && j.Metadata.ExitCode != nil {
			return *j.Metadata.ExitCode, true
		}
	}
	return 0, false
}

// outputText flattens a tool output: a string, or an array of content parts
// whose "text" fields are joined.
func outputText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for i, p := range parts {
			if i > 0 && p.Text != "" {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}
