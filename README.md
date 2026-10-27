# cbrt

**The optimization engine for AI coding agents.**

`cbrt` makes Claude Code and Codex CLI cheaper to run, faster to work with and easier to keep on track. It runs them on free models with automatic failover, measures where your sessions waste time and tokens and proposes fixes, and keeps every project's progress on one page. One 8 MB binary; everything stays on your machine.

## What it optimizes

- **Cost: free models, no dead ends.** `cbrt claude` and `cbrt codex` run your agent on free cloud models (NVIDIA build API) and local ones (Ollama). When a free tier runs dry, rate-limits or stalls, cbrt moves to the next model before any output is sent, so the session keeps going. With `--subscription`, Claude Code uses your own plan first and continues on free models when it hits the limit.
- **Time and tokens: evidence, not guesses.** `cbrt analyze` reads the transcripts Claude Code, Codex CLI and Antigravity already keep and measures where sessions lose time: exploration before the first edit, corrections, retries, permission waits, context growth. A weekly `/cbrt-multi-session-review` proposes at most three changes with evidence and later checks whether they helped; `/cbrt-session-review` reviews the session you are in.
- **Focus: every project on one page.** `cbrt projects` shows each project's goal, current task, progress and last commit, and can ask an agent (read-only) whether the work still follows the plan.
- **Visibility: always know which model answered.** Response headers, a dashboard and the Claude Code status line show every switch. Nothing is injected into model output.

**Who it is for:** developers who run coding agents every day and want them cheaper, faster and better aimed; people who need an agent setup that keeps working offline; anyone comparing open models under a real agent workload.

**What it is not:** it doesn't make free models as capable as the vendors' own, and small local models make slow agents. It is built for one user or a small team, not as a production load balancer, and it can't fail over once a response has started streaming.

## Quick start

1. **Get cbrt:** download the binary for your system from [Releases](https://github.com/alex-danilin/cbrt/releases), or run `go install github.com/alex-danilin/cbrt/cmd/cbrt@latest`.
2. **Give it at least one provider:**
   - **NVIDIA (free, cloud):** get a key at https://build.nvidia.com and put `NVIDIA_API_KEY=nvapi-…` in the `.env` file in cbrt's config folder (`%APPDATA%\cbrt\.env` on Windows, `~/Library/Application Support/cbrt/.env` on macOS, `~/.config/cbrt/.env` on Linux), or set it in the environment.
   - **Ollama (local):** run Ollama with a chat model that supports tools. cbrt finds it, and warns you with the fix if its context is too small for an agent.
3. **Run your agent through cbrt:** `cbrt claude` or `cbrt codex`. Any extra arguments go to the agent.
4. **Find the waste (Claude Code):** `cbrt analyze install claude --with cbrt-session-review --write` adds the session hooks and the review skills; then run `/cbrt-multi-session-review` once a week.

No config file is needed, and your Claude Code and Codex settings are not changed: each launcher starts a private gateway on a free local port, points only that client at it, and stops it when the client exits.

## Commands

Every command prints its own help (`cbrt help`, `cbrt analyze help`, `cbrt projects help`). `analyze install` and `uninstall` only show what they would change until you add `--write`; `projects connect` and `check` show it with `--dry-run`.

### Agents

| Command | What it does |
|---|---|
| `cbrt claude [args…]` | Run Claude Code on free models through a private gateway |
| `cbrt claude --subscription [args…]` | Claude Code on your own Claude login first, free models at its limit |
| `cbrt codex [args…]` | Run Codex CLI on free models through a private gateway |
| `cbrt statusline [-url URL] [-key KEY] [-plain]` | Claude Code status line: which model served this session, degraded providers |

### Gateway

| Command | What it does |
|---|---|
| `cbrt [serve] [-config cbrt.json] [-env-file .env]` | Run a long-running gateway (dashboard at `http://127.0.0.1:8080/`) |
| `cbrt init [-config cbrt.json] [-env-file .env] [-force]` | Detect providers and write a starting `cbrt.json` to edit |
| `cbrt healthcheck [-url URL] [-timeout 3s]` | Exit 0 if the gateway answers (for container health checks) |
| `cbrt record -upstream <url> [-listen 127.0.0.1:8089] [-out captures]` | Record a client's real traffic locally, to check a new client version |
| `cbrt version` | Print the version |

### Session review

| Command | What it does |
|---|---|
| `cbrt analyze install claude [--with <skill,…\|all>] [--context-warn[=false]] [--write]` | Add the session hooks, the review rubric and the skills to Claude Code; `--context-warn` also warns before a prompt on a large or idle session ([details](docs/analyze.md#context-warning)) |
| `cbrt analyze uninstall claude [--keep-skills\|--purge] [--write]` | Remove the hooks and skills; `--keep-skills` keeps the skills, `--purge` also deletes collected data |
| `cbrt analyze run [--since 14d] [--tools claude,codex] [--project p] [--no-cache]` | Collect sessions and write the review's inputs |
| `cbrt analyze excerpt <tool>/<store>/<session> <from> <to> [--context 2]` | A redacted transcript slice, as evidence |
| `cbrt analyze rec list\|due` | Recommendations, and those due for evaluation |
| `cbrt analyze rec applied\|declined\|reverted <id> [notes]` | Your decision on a recommendation |
| `cbrt analyze rec add <file.json\|->`, `rec eval <id> <verdict> <reason> --run <run-id>` | Used by the review skill |
| `cbrt analyze run-note --review-tokens <n> [--run <run-id>]` | Record a review's token spend |
| `cbrt analyze prune` | Delete output folders past the retention |
| `cbrt hook record <adapter_id> <event>` | The hook command install adds (records one event; always exits 0) |
| `cbrt hook warn claude` | The context warning hook `--context-warn` adds (prints a warning or nothing; always exits 0) |

Skills, in Claude Code after `analyze install`:

| Skill | What it does |
|---|---|
| `/cbrt-multi-session-review [14d]` | Weekly review across sessions and tools: at most three changes with evidence, earlier ones evaluated |
| `/cbrt-session-review` | Review of the current session (optional: `--with cbrt-session-review`) |

### Projects

| Command | What it does |
|---|---|
| `cbrt projects [serve] [--listen 127.0.0.1:8091] [--no-open]` | The page with all connected projects |
| `cbrt projects connect <dir> [--dry-run] [--public\|--private] [--agent claude\|codex\|agy]` | Track a project: `PLAN.md`, README skeleton, the plan rule in `AGENTS.md` |
| `cbrt projects disconnect <name\|dir> [--remove-files]` | Stop tracking it |
| `cbrt projects list` | One line per project |
| `cbrt projects check <name\|dir> --agent claude\|codex\|agy [--dry-run] [--timeout 10m]` | Ask an agent, read-only, whether the work follows the plan |

## Documentation

- [The gateway](docs/gateway.md): running it, connecting Claude Code, Codex and other clients, subscription mode, what is translated, dropped or refused, caveats, endpoints and security.
- [Session review](docs/analyze.md): what `cbrt analyze` measures, installing it, the weekly review and what stays on your machine.
- [Project tracking](docs/projects.md): connecting projects, the page, checks and keeping plans private.
- [Deployment](docs/deploy.md): Docker, Compose with Ollama, offline setups, systemd, Windows, cloud VMs.
- [Benchmarks](docs/benchmarks.md): overhead, memory and live smoke tests.
- [Development](docs/development.md): building, testing and releasing cbrt.
