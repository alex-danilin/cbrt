package agents

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// Tool names, Claude Code -> Antigravity. The Antigravity names come from its
// docs and IDE and are not yet probed in the CLI (docs/internal).
var toAntigravity = map[string][]string{
	"Read": {"view_file"}, "Edit": {"replace_file_content"}, "Write": {"write_to_file"},
	"Bash": {"run_command"}, "PowerShell": {"run_command"}, "Grep": {"grep_search"}, "Glob": {"find_by_name"},
	"WebFetch": {"read_url_content"}, "WebSearch": {"search_web"},
	"TaskCreate": {"manage_task"}, "TaskUpdate": {"manage_task"}, "TaskList": {"manage_task"}, "TaskGet": {"manage_task"},
}

var fromAntigravity = map[string][]string{
	"view_file": {"Read"}, "replace_file_content": {"Edit"}, "write_to_file": {"Write"},
	"run_command": {"Bash"}, "grep_search": {"Grep"}, "find_by_name": {"Glob"},
	"read_url_content": {"WebFetch"}, "search_web": {"WebSearch"},
	"manage_task": {"TaskCreate", "TaskUpdate", "TaskList"},
}

// mapTools maps tool names through table; "Bash(git diff:*)" maps by its
// base name. It returns the mapped names (deduplicated) and the lost ones.
func mapTools(tools []string, table map[string][]string) (mapped, lost []string) {
	for _, t := range tools {
		base, _, _ := strings.Cut(t, "(")
		to, ok := table[strings.TrimSpace(base)]
		if !ok {
			lost = append(lost, t)
			continue
		}
		for _, m := range to {
			if !slices.Contains(mapped, m) {
				mapped = append(mapped, m)
			}
		}
	}
	return mapped, lost
}

func isShell(tool string) bool {
	base, _, _ := strings.Cut(tool, "(")
	base = strings.TrimSpace(base)
	return base == "Bash" || base == "PowerShell"
}

var safeAntigravityModes = set("default", "acceptEdits", "plan")

// Plan says what installing a into tool t does with each field, in the
// order name, description, body, then a.Fields.
func Plan(a Agent, t Tool) []Note {
	notes := []Note{
		nameNote(a, t),
		{"description", Kept, ""},
		{"body", Kept, ""},
	}
	if t == Codex {
		notes[2] = Note{"body", Mapped, "as developer_instructions"}
	}
	for _, f := range a.Fields {
		n := fieldNote(f, t)
		n.Field = f.Key
		notes = append(notes, n)
	}
	return notes
}

func nameNote(a Agent, t Tool) Note {
	if t == Codex && strings.Contains(a.Name, "-") {
		return Note{"name", Kept, "hyphens in Codex agent names are not yet probed"}
	}
	return Note{"name", Kept, ""}
}

func fieldNote(f Field, t Tool) Note {
	k, v := f.Key, f.Value
	if !claudeFields[k] && !antigravityOnly[k] {
		return Note{Code: Dropped, Detail: "not a field cbrt knows"}
	}
	if antigravityOnly[k] {
		switch {
		case t == Antigravity:
			return Note{Code: Kept}
		case k == "commandExecutionPolicy":
			return Note{Code: Warned, Detail: "no per-agent command policy"}
		case t == Claude && (k == "mainAgent" || k == "subagent"):
			return Note{Code: Dropped, Detail: "Claude Code agents are both main agents and subagents"}
		case t == Codex && (k == "mainAgent" || k == "subagent"):
			return Note{Code: Dropped, Detail: "Codex agents are subagents only"}
		}
		return Note{Code: Dropped, Detail: "Antigravity only"}
	}
	if t == Claude {
		return Note{Code: Kept}
	}
	switch k {
	case "tools":
		return toolsNote(v.List, t)
	case "disallowedTools":
		if t == Antigravity {
			return Note{Code: Warned, Detail: "no deny list: " + strings.Join(v.List, ", ") + " stay allowed"}
		}
		var other []string
		shell := false
		for _, tool := range v.List {
			if isShell(tool) {
				shell = true
			} else {
				other = append(other, tool)
			}
		}
		switch {
		case shell && len(other) == 0:
			return Note{Code: Mapped, Detail: "features.shell_tool = false"}
		case shell:
			return Note{Code: Warned, Detail: "shell off (features.shell_tool = false); " + strings.Join(other, ", ") + " stay allowed"}
		}
		return Note{Code: Warned, Detail: strings.Join(other, ", ") + " stay allowed"}
	case "model":
		return Note{Code: Dropped, Detail: "model names don't port"}
	case "effort":
		if t == Antigravity {
			return Note{Code: Dropped, Detail: "no effort setting"}
		}
		switch v.Str {
		case "low", "medium", "high":
			return Note{Code: Mapped, Detail: "model_reasoning_effort = " + v.Str}
		case "xhigh", "max":
			return Note{Code: Mapped, Detail: "model_reasoning_effort = high (Codex has no " + v.Str + ")"}
		}
		return Note{Code: Dropped, Detail: fmt.Sprintf("effort %q has no Codex value", v.Str)}
	case "permissionMode":
		if t == Codex {
			return Note{Code: Warned, Detail: "the Codex session's own sandbox decides"}
		}
		if safeAntigravityModes[v.Str] {
			return Note{Code: Kept}
		}
		return Note{Code: Warned, Detail: fmt.Sprintf("%q is not copied: cbrt never installs a looser mode", v.Str)}
	case "skills":
		if t == Antigravity {
			return Note{Code: Kept}
		}
		return Note{Code: Dropped, Detail: "Codex agents use the session's skills"}
	case "mcpServers":
		if t == Antigravity {
			return Note{Code: Warned, Detail: "inheritMcp: true; the server list itself is lost"}
		}
		return Note{Code: Dropped, Detail: "set the MCP servers up in Codex"}
	case "hooks":
		return Note{Code: Warned, Detail: "Claude Code only: a rule a hook enforces is lost"}
	}
	return Note{Code: Dropped, Detail: "Claude Code only"} // maxTurns, memory, color, background, isolation, initialPrompt
}

func toolsNote(tools []string, t Tool) Note {
	if t == Antigravity {
		mapped, lost := mapTools(tools, toAntigravity)
		if len(lost) > 0 {
			return Note{Code: Warned, Detail: "as " + list(mapped) + "; no equivalent for " + strings.Join(lost, ", ")}
		}
		return Note{Code: Mapped, Detail: "as " + list(mapped)}
	}
	if slices.ContainsFunc(tools, isShell) {
		return Note{Code: Warned, Detail: "Codex has no tool list: the agent gets every tool"}
	}
	return Note{Code: Warned, Detail: "shell off (features.shell_tool = false); Codex has no other tool limits"}
}

func list(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// WriteReport prints what an import did and what each tool would do with
// the agent at install.
func WriteReport(w io.Writer, r Result) {
	a := r.Agent
	verb := "imported"
	if !r.Wrote {
		verb = "would import"
	}
	fmt.Fprintf(w, "%s %s (%s) as %s\n  -> %s\n", verb, r.From, a.Source, a.Name, r.Path)
	if len(r.Notes) > 0 {
		fmt.Fprintln(w, "\nimport:")
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  %c %-22s %s\n", n.Code, n.Field, n.Detail)
		}
	}
	plans := map[Tool][]Note{}
	for _, t := range Tools {
		plans[t] = Plan(a, t)
	}
	fmt.Fprintln(w, "\ninstall preview (K kept, M mapped, D dropped, W dropped and a limit is lost):")
	fmt.Fprintf(w, "  %-22s %-7s %-12s %s\n", "field", "claude", "antigravity", "codex")
	for i, n := range plans[Claude] {
		fmt.Fprintf(w, "  %-22s %-7c %-12c %c\n", n.Field, n.Code, plans[Antigravity][i].Code, plans[Codex][i].Code)
	}
	var details []string
	for _, t := range Tools {
		for _, n := range plans[t] {
			if n.Detail != "" {
				details = append(details, fmt.Sprintf("  %-12s %-22s %c %s", t, n.Field, n.Code, n.Detail))
			}
		}
	}
	if len(details) > 0 {
		fmt.Fprintln(w, "\n"+strings.Join(details, "\n"))
	}
}
