# Cross-tool agents

Claude Code, Antigravity CLI and Codex CLI each read custom agents, in different formats:

| Tool | Agent file | Format |
|---|---|---|
| Claude Code | `~/.claude/agents/<name>.md`, `.claude/agents/<name>.md` | Markdown with YAML frontmatter |
| Antigravity CLI | `~/.gemini/config/agents/<name>/agent.md` or `<name>.md`, `.agents/agents/…` | Markdown with YAML frontmatter |
| Codex CLI | `~/.codex/agents/<name>.toml`, `.codex/agents/<name>.toml` | TOML; the instructions go in `developer_instructions` |

`cbrt agents` keeps one copy of an agent and (in a later release) installs it into all three.

## Import

```
cbrt agents import ~/.claude/agents/reviewer.md
```

Import reads the file, never changes it, and writes the canonical copy to cbrt's agents folder (`$CBRT_AGENTS_HOME`, default `<config dir>/cbrt/agents`) as `cbrt-<name>.md`. Every agent cbrt writes is named `cbrt-…`, like cbrt's skills, so it never collides with your own agents; a name that already starts with `cbrt-` keeps it.

- The source tool is detected: `.toml` is Codex; a `.md` file is Antigravity when it uses Antigravity-only fields (`mainAgent`, `subagent`, `hidden`, `inheritMcp`, `commandExecutionPolicy`) or sits under `.gemini` or `.agents`, else Claude Code. `--from` overrides it.
- An earlier import of the same name stays until you pass `--force`.
- `--dry-run` prints the report and writes nothing.

The canonical file uses Claude Code's frontmatter and tool names, the widest of the three. Antigravity tool names are mapped to Claude's (`view_file` to `Read`, `run_command` to `Bash`, …). From a Codex agent, `model_reasoning_effort` becomes `effort`, `sandbox_mode = "read-only"` becomes `permissionMode: plan`, and `features.shell_tool = false` becomes `disallowedTools: Bash, PowerShell`.

## The report

Import prints what it changed and, for each tool, what installing the agent would do with every field:

| Code | Meaning |
|---|---|
| K | kept as is |
| M | mapped to the tool's own field or names |
| D | dropped: the tool has no such setting (a model name, a color) |
| W | dropped, and a limit is lost: a tool restriction, a permission mode, a hook |

Nothing is dropped silently, and nothing is mapped to a looser setting: a `bypassPermissions` agent is not installed with a permissive mode anywhere, it gets a W.

Things that don't carry over:

- **Tools.** Codex has no per-agent tool list; only the shell can be switched off. Tools from MCP servers (`mcp__…`) exist only in Claude Code.
- **Models.** Model names differ per vendor and are dropped.
- **Sandboxing in Codex.** A Codex agent runs inside the session's sandbox; `sandbox_mode` in an agent file did not restrict it in a test with Codex CLI 0.163.0.
- **Claude Code-only settings:** `color`, `memory`, `hooks`, `isolation`, `background`, `initialPrompt`, `maxTurns`.
