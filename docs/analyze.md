# Session review (`cbrt analyze`)

`cbrt analyze` finds where your coding-agent sessions lose time, turns or tokens, across tools. A local pass reads the transcripts the tools already keep (Claude Code `~/.claude/projects`, Codex CLI `~/.codex/sessions`, Antigravity `~/.gemini/antigravity*`) and computes per-session facts: exploration before the first edit, corrections, retries, permission waits, context growth, tokens with their coverage, and which provider and model actually answered when the session ran through cbrt. It groups sessions into candidate tasks, even across tools. A weekly review in Claude Code then reads a redacted summary and proposes at most three changes, each with evidence and a way to check later whether it helped.

**Set up once (Claude Code):**

```sh
cbrt analyze install claude          # shows what it would change; nothing is written
cbrt analyze install claude --write  # backs up ~/.claude/settings.json, adds the hooks, installs the skill
```

Run `install` from an installed cbrt (a release binary, or `go install`): the hooks store that binary's path. It adds six exec-form hooks (`SessionStart`, `SessionEnd`, `Stop`, `PermissionRequest`, `PreCompact`, `PostModelSwitch`), each running `cbrt hook record claude <event>`, which writes one small file and always exits 0 (about 15 ms, or about 80 ms when it reads the git branch). Your other hooks and settings are kept. `cbrt analyze uninstall claude --write` removes exactly what was added. It keeps the data collected so far; add `--purge` to also delete the analyzer folder (hook records, parse cache, run outputs, recommendations, archive) and the `settings.json` backups install made. Purge refuses a folder holding anything cbrt did not create, and never touches the tools' own transcripts.

**Optional skills:** `--with <skill,...>` (or `--with all`) also installs optional skills. Today there is one: `/cbrt-session-review`, which reviews only the current conversation with the same heuristics as the weekly review. It runs no tools and reports in the chat. Install never overwrites a hand-made skill of the same name. `uninstall` removes every skill cbrt installed; `uninstall claude --keep-skills --write` removes the hooks but leaves the skills in place.

<a id="context-warning"></a>**Context warning (opt-in):** `cbrt analyze install claude --context-warn --write` adds a `UserPromptSubmit` hook, `cbrt hook warn claude`. Before each prompt it reads the end of the session's transcript (only the usage numbers of the last main-thread model response, never conversation text) and shows you one line, not the model, when:

- the context is at or above `context_warn_tokens` (default `300000`). It repeats only after the context grows by another 100k, and again after a compaction;
- or the session was idle for over an hour with at least 100k of context. The prompt cache has expired by then, so the next request re-sends all of it at full price.

The line suggests `/clear` for a new task, or a short handoff note (or `/compact`) when the next task needs the context. The hook never blocks a prompt and always exits 0; its errors go to `registry/errors.log`. To change the threshold, set `context_warn_tokens` in `config.json` in the analyzer folder (for example `{"context_warn_tokens": 250000}`). It applies from the next prompt; no reinstall is needed. A later `install` keeps the hook, `install claude --context-warn=false --write` removes it, and `uninstall` removes it with the other hooks. Per-session state (the size last warned about) is kept in `warn/` in the analyzer folder, and files older than 30 days are deleted.

**Every week:** run `/cbrt-multi-session-review` in Claude Code (optionally `/cbrt-multi-session-review 14d`). It runs `cbrt analyze run`, follows the rubric, records its recommendations, and evaluates earlier ones. You decide what happens to each: `cbrt analyze rec applied|declined|reverted <id>`.

**What stays on your machine:** everything the analyzer reads and writes lives under `%APPDATA%\cbrt\analyze` (Windows), `~/Library/Application Support/cbrt/analyze` (macOS) or `~/.config/cbrt/analyze` (Linux), or `CBRT_ANALYZE_HOME`. The reviewer model only sees the summary, anchors and excerpts it asks for, all redacted (keys, tokens, passwords, e-mail addresses, private IPs, your home folder and user name) and wrapped as evidence, not instructions. Add your own patterns in `redaction_rules.json`, and exclude projects or sessions in `exclusions.json`. Sessions in your home folder are excluded from excerpts by default.

**Commands:**

| Command | Does |
|---|---|
| `cbrt analyze run [--since 14d] [--tools claude,codex] [--project p]` | Collect and write `summary.md`, `anchors.jsonl`, `excerpt_candidates.jsonl`, `projects.jsonl` (per-project context: instruction files, README, build entry points; names and sizes only) under `out/<run-id>/` |
| `cbrt analyze excerpt <tool>/<store>/<session> <from> <to> [--context 2]` | A redacted transcript slice: one header line per event (seq, kind, time, tool), long text cut, images and injected context left out |
| `cbrt analyze rec list\|due\|add\|eval\|applied\|declined\|reverted` | Recommendations and their status |
| `cbrt analyze prune` | Delete old output folders (the per-session metrics archive is kept) |

Hooks for Codex and Antigravity are not installed yet; their transcripts are read all the same (Antigravity conversations from versions that kept only the compact transcript are skipped). Token figures always come with their coverage: Antigravity records usage only in recent IDE versions, and Codex cache writes are reported as unknown.
