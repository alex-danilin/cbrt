# The gateway

How to run cbrt as a long-running gateway, connect clients to it, and what it translates, drops and refuses. For the short version, see the [README](../README.md).

## Claude Code on your own plan, free models at its limit

```sh
cbrt claude --subscription   # any extra arguments go to claude
```

Claude Code keeps your own login (a claude.ai sign-in or your API key) and its requests go to Anthropic **unchanged**: cbrt is then a plain Claude Code gateway, as described in Anthropic's [LLM gateway docs](https://code.claude.com/docs/en/llm-gateway). When your plan's usage limit is reached, the **same session** continues on the free targets until the limit resets, then returns to Anthropic.

- **Claude Code only.** Codex and other clients never reach the Anthropic target, and cbrt never sends Anthropic a request of its own. Your login only passes through memory on the way to Anthropic; it is never written to the log, usage data or dashboard.
- **Loopback only.** A config with an `anthropic` provider must listen on a loopback address with no `api_key`.
- **Context:** `CLAUDE_CODE_MAX_CONTEXT_TOKENS` is still set to the smallest free target's context, so a long conversation fits the fallback. That makes Claude Code compact earlier on your plan too.
- **After a switch back** Claude Code may get one rejected request about thinking blocks from the free model's turns; it drops them and retries by itself.
- In a `cbrt.json`: `{"id": "anthropic", "type": "anthropic"}` as a provider and `"anthropic:*"` first in the `claude-*` route.

## Running the gateway

**Configuration is optional.** Without a `cbrt.json`, cbrt detects NVIDIA and your Ollama models (with their real context sizes) and builds the routes itself. To customize, run `cbrt init` to write that configuration to `cbrt.json`, then edit it. The launchers (`cbrt claude`, `cbrt codex`) use the file named by `CBRT_CONFIG`, else `cbrt.json` in the current folder, else the one in the config folder; the env file is the config folder's `.env` (or `CBRT_ENV_FILE`), never a project's own `.env`.

```sh
cbrt serve                          # uses ./cbrt.json and ./.env, or detects providers when there is no cbrt.json
cbrt init                           # write the detected configuration to cbrt.json, to edit it
```

From source: `go run ./cmd/cbrt serve`, or `go run scripts/build.go` to build `dist/cbrt-<os>-<arch>[.exe]` for all six targets. The defaults are `-config cbrt.json -env-file .env`, and it listens on `127.0.0.1:8080`.

- **Ollama targets** need Ollama running (the Ollama app, or `ollama serve`). Otherwise they are marked unreachable and skipped.
- **Stopping:** Ctrl+C (or SIGTERM). In-flight requests get up to 15 seconds to finish, and usage data is saved.
- **After editing `cbrt.json` or `.env`:** apply the changes without a restart with `curl -X POST http://127.0.0.1:8080/api/reload` (`curl.exe` in PowerShell; add `-H "Authorization: Bearer <CBRT_API_KEY>"` if a key is set). Changes to `listen`, TLS or the limits need a restart.
- **Offline:** set `"offline": true`, or `CBRT_OFFLINE=1` in the environment. Cloud providers are then never contacted, including model discovery. The transport also enforces it, whatever a provider's `location` says: every upstream connection must go to a loopback, private (RFC 1918, IPv6 ULA) or link-local address after DNS resolution, and proxy variables are ignored. A `location: local` provider whose `base_url` host does not look local gets a warning at startup. A test (`cmd/cbrt/offline_test.go`) guards this: it fails on any connection to a non-loopback address.
- **Redirects are never followed.** A 3xx from a provider would re-send the prompt and credentials to another host, so cbrt treats it as a broken `base_url`: the attempt is logged as `unreachable` ("upstream redirected … fix base_url"), the provider backs off, and the request fails over. Passthrough providers hand the 3xx back to the client unchanged.
- **Memory:** a soft memory limit of 32 MiB is set by default. Override it with `GOMEMLIMIT`. Measured use is about 14 MB idle and 38 MB peak under 32 concurrent requests; see [benchmarks.md](benchmarks.md).
- **Healthcheck:** `cbrt healthcheck [-url …]` exits 0 when `/healthz` answers. The Docker image's `HEALTHCHECK` uses it.
- **Unset URL variables:** a `${VAR}` in a `base_url` that isn't set is a configuration error, so it never silently falls back to localhost.
- **Ollama context size:** Ollama runs models with a **4096-token context by default**. Agents need much more; Claude Code's first request is about 30K tokens. Either create a variant (`FROM <model>` plus `PARAMETER num_ctx 32768` in a Modelfile, then `ollama create <name>-32k -f Modelfile`) or set `OLLAMA_CONTEXT_LENGTH`. Then give the route target a matching `max_context`. Set it correctly: some models (gemma family) **silently drop the start of a prompt** that is longer than their context instead of returning an error, so cbrt can only skip a target that is too small if `max_context` says so.
- **Ollama in offline deployments:** disable the Ollama app's auto-update, because it checks GitHub hourly.
- **Windows Smart App Control:** it can block unsigned binaries such as `cbrt.exe`, and binaries Go builds in `%TEMP%` (which `go run` and `go test` use). Building inside your own workspace usually works: `go build -o .gotmp\cbrt.exe ./cmd/cbrt`, and `go env -w GOTMPDIR=<repo>\.gotmp` for `go run`/`go test`. For other options see [docs/deploy.md](deploy.md).

## Connecting clients

### OpenAI-compatible clients

Base URL `http://127.0.0.1:8080/v1`. Model = a route name (e.g. `default`) or `provider:model` (e.g. `ollama:gemma-4-e2b-4-32k`).

```sh
curl -si http://127.0.0.1:8080/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"hi"}]}' | grep -i x-cbrt
# X-Cbrt-Attempts: nvidia:moonshotai/kimi-k2.6:model_unavailable,ollama:gemma-4-e2b-4-32k:ok
# X-Cbrt-Fallback: true
```

In PowerShell (quoting for native `curl.exe` differs between PowerShell versions, so this uses the built-in cmdlet):

```powershell
(Invoke-WebRequest http://127.0.0.1:8080/v1/chat/completions -Method Post -ContentType application/json -UseBasicParsing `
  -Body '{"model":"default","messages":[{"role":"user","content":"hi"}]}').Headers
```

### Claude Code

The simplest way is `cbrt claude` (see [Quick start](../README.md#quick-start)): it sets everything below for that one session, including the status line. The rest of this section is for a long-running gateway (`cbrt serve`).

Add a `claude-*` route (Claude Code sends `claude-…` model names) to `cbrt.json`:

```json
"claude-*": [
  { "provider": "nvidia", "model": "openai/gpt-oss-20b", "first_byte_timeout": "30s", "capabilities": ["tools"], "max_context": 131072 },
  { "provider": "ollama", "model": "gemma-4-e2b-4-32k", "capabilities": ["tools"], "max_context": 32768,
    "token_param": "max_tokens", "tool_choice_required": false }
]
```

Then point Claude Code at cbrt. To try it in **one terminal only** (other Claude Code sessions are unaffected):

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080 ANTHROPIC_AUTH_TOKEN=local CLAUDE_CODE_MAX_CONTEXT_TOKENS=32768
claude
```

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:8080"; $env:ANTHROPIC_AUTH_TOKEN = "local"; $env:CLAUDE_CODE_MAX_CONTEXT_TOKENS = "32768"
claude
```

`ANTHROPIC_AUTH_TOKEN` is your `CBRT_API_KEY`, or any non-empty value if cbrt has no key.

To make it permanent, and to get the status line, use Claude Code's settings. A project's `.claude/settings.json` affects only that project; `~/.claude/settings.json` sends **every** Claude Code session to cbrt:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080",
    "ANTHROPIC_AUTH_TOKEN": "your CBRT_API_KEY (any non-empty value if the gateway has no key)",
    "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "32768"
  },
  "statusLine": { "type": "command", "command": "cbrt statusline" }
}
```

- **`CLAUDE_CODE_MAX_CONTEXT_TOKENS`:** set it to the **smallest** `max_context` in the route, so that Claude Code compacts the conversation before a fallback model overflows.
- **`ANTHROPIC_API_KEY`:** leave it unset or empty. `ANTHROPIC_AUTH_TOKEN` is what gets sent.
- **Windows status-line command:** use the full path and escaped backslashes, e.g.
  `"command": "C:\\Users\\you\\cbrt\\dist\\cbrt.exe statusline"`. Add `-plain` for ASCII-only terminals.
- **Status line output.** It prints the target that served *your* session, e.g. `cbrt ● claude-* → nvidia:openai/gpt-oss-20b`, plus `⚠ nvidia quota_exhausted until 13:03` when a provider is degraded. It reads the gateway URL and key from `-url`/`-key`, then `CBRT_URL`/`CBRT_API_KEY`, then `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`.
- **Trimming tools:** free models do better with fewer tools, e.g. `claude --tools "Read,Glob,Grep,Edit,Bash"`.
- **Features the gateway drops:** Claude Code features the upstreams cannot provide are dropped and counted on the dashboard. These are prompt caching, the thinking budget, context management, effort and server tools. Model reasoning is shown to Claude Code as thinking blocks; set `"visibility": {"expose_reasoning": false}` to turn this off.

### Codex CLI

The simplest way is `cbrt codex` (see [Quick start](../README.md#quick-start)): it passes the provider settings to Codex with `-c` flags, so `~/.codex/config.toml` stays untouched. The rest of this section is for a long-running gateway (`cbrt serve`).

Codex speaks only the OpenAI Responses API (`wire_api = "responses"`); cbrt translates it to Chat Completions. Add a route in `cbrt.json`, e.g.:

```json
"coder": [
  { "provider": "nvidia", "model": "openai/gpt-oss-20b", "first_byte_timeout": "60s", "capabilities": ["tools"],
    "max_context": 131072, "reasoning": "reasoning_content" },
  { "provider": "ollama", "model": "gemma-4-e2b-4-32k", "capabilities": ["tools"], "max_context": 32768,
    "token_param": "max_tokens", "tool_choice_required": false }
]
```

Then add a provider and profile to `~/.codex/config.toml`:

```toml
[model_providers.cbrt]
name = "cbrt"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "responses"
env_key = "CBRT_API_KEY"      # remove this line if the gateway has no api_key

[profiles.free]
model_provider = "cbrt"
model = "coder"
model_context_window = 32768     # the smallest max_context in the route, so compaction happens early enough
```

Run it with `codex --profile free` (or `codex exec --profile free "…"`).

**Things to know about Codex:**
- **Prompt size.** Codex sends its whole toolset every turn. With connected Codex apps and plugins, a request was about 80K tokens (206 tools) before any work had started. Models whose `max_context` is too small are skipped with `skipped(context_too_small)`. To fit free models, turn off the apps and plugins you don't need in the free profile.
- **Metadata warning.** By default Codex warns `Model metadata for "coder" not found` and uses its fallback metadata. That fallback keeps Codex's full built-in system prompt, and `model_context_window` above fixes compaction.
  - Setting `"codex_catalog": true` makes cbrt serve Codex its own `/v1/models` catalog instead. The catalog includes context windows, reasoning levels and a freeform `apply_patch` tool.
  - Codex requires a catalog to carry base instructions, so cbrt sends a short neutral prompt. That prompt **replaces** Codex's built-in one; use `model_instructions_file` to supply your own.
- **What cbrt drops or rejects:**
  - `web_search` and other built-in OpenAI tools are dropped.
  - Custom tool grammars (e.g. for `apply_patch`) are passed as text but not enforced.
  - `previous_response_id` is rejected (cbrt is stateless; Codex sends full history anyway).
- **Reasoning display.** Model reasoning is shown in Codex as reasoning summaries.
- **Model quality.** Small models may still write their reasoning into the answer or send malformed tool arguments.

## Switch visibility

| Where | What you see |
|---|---|
| Response headers | `X-Cbrt-Provider`, `-Model`, `-Route`, `-Attempts` (e.g. `nvidia:…:skipped(quota_exhausted),ollama:…:ok`), `-Fallback: true` |
| Dashboard `/` | a banner per degraded provider ("credits exhausted — routing to Ollama since …, retry at …") and a switch feed |
| Console | one log line per cooldown, switch, recovery and disable |
| Claude Code | the `cbrt statusline` status line |
| Opt-in | SSE comment lines (`visibility.sse_comments`) and a `cbrt` field in JSON bodies (`visibility.body_metadata`) |

## Compatibility

cbrt always sends OpenAI Chat Completions upstream, and translates each client format to and from it. Everything a client sends falls into one of three groups: **supported**, **downgraded** (dropped or approximated, logged once and counted on the dashboard) or **rejected** (400 naming the unsupported item). Semantic content is never dropped silently.

| Client API | Supported | Downgraded | Rejected |
|---|---|---|---|
| Chat Completions | model, messages (text; images for `vision` targets), stream, tools/tool_choice, response_format, sampling params, stop, max tokens; unknown fields pass through | — | — |
| Anthropic Messages | system (string/blocks); mid-conversation `system` messages (normalized per target); text/image blocks; tool_use / tool_result (split 1:N); custom tools; tool_choice; stop_sequences; streaming; count_tokens (local estimate); reasoning → thinking blocks | thinking (input), cache_control, metadata, context_management, output_config, top_k, server/built-in tools, billing pseudo-header | document, search_result, server_tool_use/web_search_tool_result blocks, unknown block types |
| OpenAI Responses | instructions; input messages (developer → system); function_call / _output; custom_tool_call / _output (JSON-wrapped); namespace tools (flattened); text.format → response_format; streaming; reasoning → reasoning summaries | reasoning params and items, include, store, prompt_cache_key, client_metadata, truncation, text.verbosity, custom tool grammars (sent as text only), built-in tools such as `web_search` | `previous_response_id`, built-in tool call history (web_search_call, …), input_file, unknown item types |

**Normalization for each target:**
- **System messages:** several leading or later system messages are merged into one, and late ones become `<system-reminder>` user text. Many open-model chat templates accept only one system message, and only at the start. Set `mid_system: "keep"` to turn this off.
- **Token-limit field:** named per target (`token_param`).
- **Usage in streams:** requested per target (`stream_usage`).
- **Output limit:** clamped so it fits `max_context`.

**Tested with:** Claude Code 2.1.284 and 2.1.285, Codex CLI 0.154.0 and 0.159.2, Ollama 0.33.3 and the NVIDIA build API (2026-09). The fixtures in `testdata/` are synthetic: invented conversations in the request and response formats of these versions, generated by `scripts/synth-clients` and `scripts/synth-upstream`. When a client updates, check its traffic locally with `cbrt record` and extend the generators; recordings themselves never go into the repository.

## Caveats

- **Free-tier limits.** NVIDIA's free tier is meant for prototyping and has rate and credit limits. Some models (e.g. DeepSeek V4.1 Flash) can sit in a queue for more than 2 minutes before the first byte, so choose each target's `first_byte_timeout` deliberately.
- **No failover once output has started.** When a stream fails partway, the client gets a clean error event (`event: error`, `response.failed`, or an OpenAI error chunk), and the failure is logged. Claude Code and Codex retry by themselves.
- **Token counts may be estimates.** When an upstream reports no usage, tokens are estimated at about 4 characters per token and marked "≈" on the dashboard.
- **Reasoning tokens depend on the provider.** NVIDIA reports no `reasoning_tokens` (for gpt-oss, reasoning is included in `completion_tokens`), so the dashboard shows 0 reasoning tokens for those models.
- **`apply_patch` needs the Codex catalog.** Codex only sends its `apply_patch` tool when it has model metadata, which cbrt provides with `codex_catalog: true` (see the Codex setup above). Without it, Codex edits files through shell commands.
- **Best-effort usage durability.** Usage records are flushed every second and snapshotted every 60 seconds, so a crash can lose the last few seconds of accounting.
- **Small local models are weak agents.** They make slow, error-prone agents, especially on CPU only. A fallback to them keeps a session alive, but it won't be fast.
- **Codex and apps.** Codex with many connected apps sends prompts of about 80K tokens, which is more than most local models can hold.

## Other endpoints

- `GET /api/stats`: the JSON behind the dashboard.
- `GET /api/status[?session=id]`: the compact status the status line uses.
- `POST /api/reload`: re-reads `cbrt.json` and `.env`. Changes to `listen`, TLS, `data_dir`, `retention_months` or the concurrency/rate limits are refused (400) and need a restart.
- `GET /healthz`: health check.
- `GET /v1/models`: routes plus discovered `provider:model` ids.

## Security

By default the gateway listens on loopback only. To bind to any other address you must set `api_key` (`${CBRT_API_KEY}`) **and** either set `tls_cert`/`tls_key` or put it behind a TLS reverse proxy with `trust_proxy: true`.

- **Authentication:** the API accepts `Authorization: Bearer <key>` or `x-api-key`. The dashboard uses a login form that sets an HttpOnly, SameSite=Strict session cookie.
- **Login limits:** 5 sign-in attempts per minute per client address (IPv6 by `/64`) and 30 wrong keys per minute from all clients combined. Behind a proxy, `X-Forwarded-For` is honored only from peers in `trusted_proxies` (loopback when unset); see [Client addresses behind a proxy](deploy.md#client-addresses-behind-a-proxy).
- **Host check:** without `api_key`, cbrt answers only requests addressed to `localhost` or a loopback IP (others get 421), so a web page can't reach a key-less local cbrt through a DNS-rebinding domain. Set `allowed_hosts` to accept other names, such as a LAN hostname or the public name a local reverse proxy forwards (the list then replaces this default).
- **Default limits:**
  - 32 concurrent requests and 16 concurrent streams;
  - 60 requests per minute;
  - 10 MB per request, and a 64 MB budget for all requests in flight.
- **Default timeouts** (under `timeouts`, as `"30s"`-style strings):
  - `read` 60s to receive a request body (a slower client gets 408);
  - `first_byte_cloud` 60s and `first_byte_local` 120s until the upstream's first byte (per provider or target: `first_byte_timeout`);
  - `idle` 60s between upstream stream chunks, `write` 30s per write to the client.
