// Command synth-agents writes the custom-agent fixtures in testdata/agents/,
// the input for `cbrt agents import`.
//
// Every fixture is synthetic: the agents, their instructions, tool lists,
// MCP servers and paths are invented for a small fictional project (tasky, a
// to-do CLI written in Go). Only the file formats are modelled on what Claude
// Code (Markdown with YAML frontmatter), Antigravity CLI (the same, flat or
// as <name>/agent.md) and the Codex CLI (TOML) read. User paths use the
// placeholder user name "user".
//
// What the fixtures exercise, by file:
//
//	claude/reviewer.md          comma tool list, block-list skills; body with no TOML quote runs
//	claude/db-analyst.md        quoted description with \n and \" escapes and <example> tags,
//	                            MCP tool names, disallowedTools, effort xhigh, memory, color, mcpServers
//	claude/release-helper.md    block tool list incl. tools with no equivalent, bypassPermissions,
//	                            hooks with a Windows path, Claude-only fields; body holds ''' and """,
//	                            backslashes, a regex and Windows paths
//	claude/minimal.md           UTF-8 BOM and CRLF line endings, only name and description
//	claude/cbrt-changelog.md    name already carries the cbrt- prefix (import must not add it twice)
//	antigravity/triage/agent.md folder layout, Antigravity tool names, model, all Antigravity flags
//	antigravity/notes-writer.md flat layout, flow-sequence tool list, hidden, mainAgent false
//	codex/test_runner.toml      basic multi-line string with escapes, [features], [[skills.config]]
//	                            with a Windows path, [mcp_servers.*], nickname_candidates
//	codex/doc-writer.toml       CRLF, hyphenated name, dotted features key, literal ''' string
//	invalid/*                   files an import must reject with a clear error
//
// Run from the repository root; the output is deterministic:
//
//	go run ./scripts/synth-agents
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const outDir = "testdata/agents"

// file is one fixture; crlf and bom reproduce files saved by Windows editors.
type file struct {
	text      string
	crlf, bom bool
}

// bt stands for a backtick, which Go raw strings cannot hold.
func bt(s string) string { return strings.ReplaceAll(s, "{bt}", "`") }

var files = map[string]file{
	"claude/reviewer.md": {text: bt(`---
name: reviewer
description: Reviews tasky diffs for correctness, missing tests and unclear names. Use after a change is ready.
tools: Read, Grep, Glob, Bash
model: sonnet
skills:
  - go-style
  - test-gaps
---
You review code like its owner would. Lead with concrete findings, each with a file and line.

1. Run {bt}git diff main{bt} and read every changed file in full.
2. Check that each new function in internal/store has a test.
3. Never edit files; report only.
`)},

	"claude/db-analyst.md": {text: `---
name: "db-analyst"
description: "Use this agent when the user asks about tasky's task database: counts, overdue tasks or schema questions.\n\n<example>\nuser: \"How many tasks are overdue?\"\nassistant: \"I'll use the db-analyst agent to query the task database.\"\n</example>"
tools: Read, Grep, mcp__tasky_db__query, mcp__tasky_db__schema, WebSearch
disallowedTools: Bash, Write
model: opus
effort: xhigh
permissionMode: acceptEdits
memory: user
color: green
mcpServers:
  - tasky_db
---
You answer questions about the tasky task database. Use mcp__tasky_db__schema before writing a query, and mcp__tasky_db__query to run it.

Report numbers with the query that produced them. Never change data.
`},

	"claude/release-helper.md": {text: bt(`---
name: release-helper
description: Prepares a tasky release (changelog, version bump, tag). Use when the user says "cut a release".
tools:
  - Bash
  - PowerShell
  - Read
  - Edit
  - Write
  - TaskCreate
  - CronCreate
  - EnterWorktree
model: claude-opus-5-5
permissionMode: bypassPermissions
maxTurns: 40
background: true
isolation: worktree
initialPrompt: Check the working tree, then draft the changelog.
hooks:
  PreToolUse:
    - matcher: Bash
      hooks:
        - type: command
          command: powershell -File C:\Users\user\.claude\hooks\guard-release.ps1
---
Prepare the release in C:\Users\user\tasky.

1. Read C:\Users\user\tasky\CHANGELOG.md and collect entries since the last tag matching {bt}v\d+\.\d+\.\d+{bt}.
2. Bump the version in internal\version\version.go.
3. Update the release config, which looks like this:

{bt}{bt}{bt}toml
notes = '''
Fixed: "due" dates in C:\ paths.
'''
summary = """
Tab-separated: a\tb
"""
{bt}{bt}{bt}

4. Tag with {bt}git tag -a v1.4.0 -m "tasky 1.4.0"{bt} and stop; never push.
`)},

	"claude/minimal.md": {crlf: true, bom: true, text: `---
name: minimal
description: Answers questions about tasky's command-line flags.
---
Answer from the output of tasky --help only.
`},

	"claude/cbrt-changelog.md": {text: `---
name: cbrt-changelog
description: Summarises tasky's changes since the last tag as changelog entries.
tools: Read, Bash
---
List the commits since the last tag and group them as Added, Fixed and Changed.
`},

	"antigravity/triage/agent.md": {text: `---
name: triage
description: Sorts new tasky issues by area and urgency.
model: flash
mainAgent: true
subagent: true
hidden: false
inheritMcp: true
permissionMode: acceptEdits
commandExecutionPolicy: auto
tools:
  - view_file
  - grep_search
  - run_command
  - manage_task
skills:
  - issue-labels
---
Label each new issue with one area (cli, store, sync) and one urgency (now, soon, later).
Read the code the issue mentions before choosing the area.
`},

	"antigravity/notes-writer.md": {text: `---
name: notes-writer
description: Drafts release notes from merged pull requests.
mainAgent: false
subagent: true
hidden: true
tools: [view_file, write_to_file, read_url_content]
---
Write release notes for users, not developers: what changed for them, in one line each.
`},

	"codex/test_runner.toml": {text: `name = "test_runner"
description = "Runs the tasky test suite and explains each failure."
model = "gpt-5.4-mini"
model_reasoning_effort = "high"
model_reasoning_summary = "concise"
sandbox_mode = "read-only"
nickname_candidates = ["Tess", "Runner"]
developer_instructions = """
Run ` + "`go test ./...`" + ` from C:\\Users\\user\\tasky and summarise each failure.
Quote failing assertions as \"got X, want Y\".
Never edit files under internal\\store.
"""

[features]
memories = false

[[skills.config]]
path = "C:\\Users\\user\\.codex\\skills\\go-test\\SKILL.md"
enabled = false

[mcp_servers.tasky_db]
command = "tasky-mcp"
args = ["--db", "C:\\Users\\user\\tasky\\data\\tasks.db"]
`},

	"codex/doc-writer.toml": {crlf: true, text: `# Writes tasky's user docs.
name = "doc-writer"
description = "Writes and updates the docs in docs\\ for tasky."
sandbox_mode = "workspace-write"
features.shell_tool = false
developer_instructions = '''
Edit only files under docs\ and README.md.
Headings match ^#{1,3}\s+\S and keep "quotes" and \n as written.
'''
`},

	"invalid/no-frontmatter.md": {text: `# Notes

Just a Markdown file, not an agent.
`},

	"invalid/no-name.md": {text: `---
description: An agent without a name.
---
Do nothing.
`},

	"invalid/unclosed.md": {text: `---
name: unclosed
description: The frontmatter never ends.
Do nothing.
`},

	"invalid/no-instructions.toml": {text: `name = "no_instructions"
description = "A Codex agent without developer_instructions."
`},

	"invalid/bad-string.toml": {text: `name = "bad_string"
description = "The instructions string is never closed."
developer_instructions = """
Do nothing.
`},
}

func main() {
	if _, err := os.Stat("go.mod"); err != nil {
		fail(fmt.Errorf("run from the repository root: %v", err))
	}
	// The generator owns testdata/agents: start from an empty folder so no
	// stale fixture survives.
	if err := os.RemoveAll(outDir); err != nil {
		fail(err)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := files[name]
		text := f.text
		if f.crlf {
			text = strings.ReplaceAll(text, "\n", "\r\n")
		}
		if f.bom {
			text = "\ufeff" + text
		}
		path := filepath.Join(outDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			fail(err)
		}
	}
	fmt.Printf("synth-agents: wrote %d files to %s\n", len(names), outDir)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "synth-agents:", err)
	os.Exit(1)
}
