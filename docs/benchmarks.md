# cbrt benchmarks and smoke tests

Benchmark, load-test and live smoke-test results, checked against the project's resource targets.
Measured on 2026-09-30.

## Machine

| | |
|---|---|
| CPU | mid-range laptop CPU, 4 cores / 8 threads (`GOMAXPROCS=8`) |
| RAM | 16 GB |
| OS | Windows 11, amd64 |
| Go | go1.26.0 windows/amd64 |
| Binary | `dist/cbrt.exe` (stripped release build, 2026-09-30) |

The laptop was also running other work (Ollama, other build agents) during the runs, so the absolute numbers vary by about ±30% from run to run. Read them as orders of magnitude.

## Summary against the resource targets

| Target | Result | Verdict |
|---|---|---|
| Idle RSS < 20 MB | **13.9–14.2 MB** | met |
| < 40 MB at 16 streams + 16 non-stream, ≈50 KB bodies | **peak 48–50 MB**, p95 47–49 MB, with default GC settings. With `GOMEMLIMIT=30MiB`: peak 36.6 MB, p95 36.1 MB | **missed** with defaults; met with `GOMEMLIMIT` |
| Gateway overhead < 2 ms p50, including translation | chat, small body: **0.2 ms**. Messages (Claude Code capture, 6 KB): **1.3 ms**. Chat with a 50 KB body: **2.1–3.3 ms**. Responses (Codex capture, 262 KB): **22 ms** | met for small bodies; **missed** for 50 KB chat bodies and for the Codex request |
| Binary < 15 MB | windows-amd64 **8.1 MB**; linux-amd64 7.9 MB; darwin-arm64 7.5 MB | met |

## 1. Gateway overhead (`internal/gateway/bench_test.go`)

```sh
export GOTMPDIR="$PWD/.gotmp"
go test -run '^$' -bench . -benchmem -count 3 ./internal/gateway
```

In each case the client posts over loopback HTTP to a fake upstream (`httptest`) that answers instantly from memory:

- `BenchmarkBaseline/*` posts the body straight to the fake.
- `BenchmarkGateway/*` posts the same body to the gateway (`ServeChat`, `ServeMessages` or `ServeResponses` behind an `httptest` server), which parses, translates, forwards, relays and records usage.

Overhead is Gateway minus Baseline. The table shows the median of 3 runs (mean ns/op).

| Case | Request / upstream | Baseline | Gateway | **Overhead** | B/op (gw) | allocs/op (gw) |
|---|---|---:|---:|---:|---:|---:|
| ChatNonStream | 60 B chat / JSON | 0.11 ms | 0.31 ms | **0.20 ms** | 24 KB | 312 |
| ChatNonStream50KB | 51 KB chat / JSON | 0.21 ms | 3.39 ms | **3.2 ms** | 470 KB | 360 |
| ChatStream | 70 B chat / 148-chunk SSE (`gemma-4-e2b-4_toolsstream.sse`) | 0.47 ms | 10.1 ms | **9.7 ms** | 465 KB | 8 590 |
| MessagesNonStream | Claude Code 2.1.284 capture 0016 (6.3 KB) / tool-call JSON | 0.13 ms | 1.41 ms | **1.3 ms** | 128 KB | 958 |
| MessagesStream | same capture, streamed / 148-chunk SSE | 0.47 ms | 11.4 ms | **10.9 ms** | 585 KB | 9 982 |
| ResponsesNonStream | Codex 0.154.0 capture 0003 (262 KB, 180 tools) / tool-call JSON | 0.71 ms | 22.9 ms | **22.2 ms** | 3.4 MB | 7 715 |
| ResponsesStream | same capture, streamed / 148-chunk SSE | 1.0 ms | 32.2 ms | **31.2 ms** | 4.0 MB | 19 298 |

The benchmarks also report `p50-ms` and `p99-ms` per request. On this Windows machine `time.Now` has a resolution of about 0.5 ms, so a p50 below 0.5 ms shows up as `0`. That is why the table uses the mean. The p50 values agree with the means: ChatNonStream50KB p50 was 2.0–3.5 ms, MessagesNonStream 1.1–1.4 ms, and ResponsesNonStream 23–25 ms.

### Where the time goes (CPU profiles)

**Chat with a 50 KB body (≈3 ms).** About 7 passes of `encoding/json` over the body:

- `parseChat` takes 32% of CPU:
  - it unmarshals the whole body into `map[string]json.RawMessage`, copying every field (`RawMessage.UnmarshalJSON` accounts for 37% of all allocated bytes);
  - it unmarshals `messages` again;
  - it runs `json.Unmarshal(m.Content, &parts)` on every message to look for images. For plain-string content this scans the whole 50 KB string before failing (560 ms of 1.56 s in `parseChat`).
- `upstreamBody` takes 19%. `normalizeSystem` parses `messages` once more, and `json.Marshal(map[string]json.RawMessage)` then re-validates and compacts every raw field.
- In the real server, `server.llm` adds one more full unmarshal of the body to read `stream`. The in-process benchmark skips that pass.
- Allocations come to about 470 KB, 9× the body size.

**Codex Responses request (≈22 ms).**

- `responses.DecodeRequest` takes 57% of CPU. `decoder.tools` alone takes 35%: it unmarshals the tools array, then for each of the ~180 tools it unmarshals `parameters` into a map (only to check that it is an object) and runs `compactJSON` on it.
- The resulting chat JSON is marshalled, then parsed again by `parseChat` and marshalled again by `upstreamBody`.
- `appendCompact` and `checkValid` from `encoding/json` together take about 40% of CPU.

This is a cost of the design: translate to canonical chat JSON, then reuse the chat pipeline. It is not a correctness bug. In absolute terms it is small next to the seconds a free model takes per turn, and Codex sends this request once per turn. It still misses the target of "< 2 ms including translation".

**Streams (≈10 ms for 148 chunks, ≈65 µs per chunk).**

- About 45% of the time goes to one `ResponseController.Flush` per SSE event, which is a `WSASend` syscall per chunk. The baseline writes the whole fixture in one go, so part of this overhead is inherent to streaming. Streams also relay at the pace of the upstream, so the per-chunk cost is what matters, not the total.
- About 13.5% goes to `router.ClassifyPayloadError`, which the gateway calls on **every** stream chunk (`chat.go:524`, `chat.go:589`). It fully unmarshals each chunk into `map[string]json.RawMessage` just to check whether an `"error"` key exists. Each chunk is also parsed separately for token counting. A cheap `bytes.Contains(data, []byte(`"error"`))` check before the unmarshal would remove most of this cost. **Reported, not fixed** (outside this task's files).
- That comes to about 58 allocations per chunk on the chat path, 67 on Messages and 130 on Responses.

## 2. Memory under load (`scripts/loadtest`)

```sh
go build -o dist/cbrt.exe ./cmd/cbrt          # or use the release build
go run ./scripts/loadtest -bin dist/cbrt.exe -duration 20s
GOMEMLIMIT=30MiB go run ./scripts/loadtest -duration 20s   # the child inherits the environment
GODEBUG=gctrace=1 go run ./scripts/loadtest -duration 10s -show-log   # heap trace of the child
```

How the script works:

1. It starts an in-process fake upstream. Non-stream requests get an instant JSON answer; streams get 50 chunks spaced 5 ms apart, which is about 330 ms per stream on Windows because of timer granularity.
2. It writes a temporary config: loopback provider, `offline: true`, temp `data_dir`.
3. It launches the real binary as a child process, waits for `/healthz`, waits 3 s, and samples the idle RSS.
4. It runs **16 streaming + 16 non-stream clients in closed loops**. Each client sends a ≈51 KB Chat Completions body and sends the next request as soon as the previous one completes.
5. It samples the child's memory every 500 ms. On Windows that is `tasklist` "Mem Usage", which is the working set; on Linux it is `/proc/<pid>/status` VmRSS, and on macOS `ps -o rss=`.

The temporary config raises the admission limits (`requests_per_minute` 1 000 000, `max_concurrent_requests` 64, `max_concurrent_streams` 32). With the defaults, the 60 requests/minute rate limit would reject almost all of the workload, and slots are released a moment after the client sees the last byte. So the test measures memory, not 429s.

| Run (20 s) | Idle | Peak | p95 | p50 | 2 s after | req/s | Errors | Non-stream p50 / p95 | Stream p50 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| default GC | 14.2 MB | **49.7 MB** | 48.6 MB | 43.8 MB | 45.3 MB | 1 385 (26 794 + 905 streams) | 0 | 10.8 / 21.3 ms | 344 ms |
| earlier default run | 13.9 MB | 48.2 MB | 46.7 MB | 43.2 MB | 44.7 MB | 1 474 | 0 | 10.4 / 19.1 ms | 332 ms |
| `GOMEMLIMIT=30MiB` | 13.9 MB | **36.6 MB** | 36.1 MB | 35.3 MB | 36.2 MB | 1 258 | 0 | 11.7 / 24.6 ms | 335 ms |
| `GOMEMLIMIT=24MiB` (15 s) | 13.9 MB | 30.3 MB | 30.2 MB | 29.9 MB | 30.3 MB | 989 | 0 | 14.7 / 33.0 ms | 311 ms |

Notes:

- **The workload is a stress test.** The 16 non-stream clients never pause, so the gateway accepts about 68 MB/s of request bodies and every CPU is saturated. The client and the fake upstream run on the same 8 threads. The 10 ms non-stream latency here is queueing under saturation, not gateway overhead; section 1 has the overhead figures.
- **Heap trace.** `gctrace` of the child shows a live heap of 10–14 MB and a GC goal of 23–27 MB under this load. The working set sits about 20 MB above the goal because of runtime overhead, stacks and memory the Go scavenger has not yet returned; it had not dropped 2 s after the load stopped. 12 MB live for 32 in-flight 51 KB requests is about 375 KB per request, matching the ≈470 KB/op the benchmark reports for a 50 KB chat body.
- **Why the 40 MB target is missed.** Heap churn from the repeated JSON passes plus GOGC=100 doubling. The live heap itself is fine. `GOMEMLIMIT` of 24–30 MiB keeps the process under 40 MB, at a cost of roughly 10–30% throughput from the extra GC. The binary does not set a memory limit itself; `debug.SetMemoryLimit` is not called anywhere. Setting `GOMEMLIMIT` in the Dockerfile, systemd unit or README, or reducing per-request copies, would close the gap.
- **Worst case.** It stays bounded by the 64 MB in-flight budget in `server.readBody`, which is outside this workload.

## 3. Live smoke tests (`internal/live`, build tag `live`)

```sh
go test -tags live -count=1 -v ./internal/live
```

The tests load the repo's `.env` without overriding existing variables. NVIDIA tests are skipped when `NVIDIA_API_KEY` is empty. Ollama tests are skipped when `OLLAMA_URL` (default `http://127.0.0.1:11434`) is unreachable or lacks `minicpm-v4.6-32k`. This was verified with `NVIDIA_API_KEY= OLLAMA_URL=http://127.0.0.1:1`: all 6 skipped. The tests run in-process through `gateway.New` + `server.New` (full HTTP handler, admission control included), with token use capped at 300 or fewer.

| Test | Path | Result (2026-09-30) |
|---|---|---|
| TestNvidiaChat | `/v1/chat/completions` non-stream → `nvidia:openai/gpt-oss-20b` | PASS, 0.57 s, content `"pong"`, usage 72/30 |
| TestNvidiaChatStream | chat stream | PASS, 1.7 s, 33 chunks, content `"pong"` + 240 reasoning chars, ends with `[DONE]` |
| TestNvidiaMessagesStream | `/v1/messages` stream, `claude-sonnet-4-5` → route `claude-*` | PASS, 1.1–8.6 s, 52–111 events, `message_start` … `message_stop`, text `"pong"` |
| TestNvidiaResponsesToolCall | `/v1/responses` stream, `get_weather` tool, `tool_choice: required` | PASS, 0.8–4 s, `response.completed`, one `function_call` `get_weather` `{"city":"Paris"}` |
| TestOllamaChat | chat non-stream → `ollama:minicpm-v4.6-32k` (CPU) | PASS, 2.2–7.2 s, content `"pong"` (+ reasoning) |
| TestNvidiaFailoverOnUnavailableModel | route `[nvidia:moonshotai/kimi-k2.6, nvidia:openai/gpt-oss-20b]` | PASS, 0.84 s, `X-Cbrt-Attempts: nvidia:moonshotai/kimi-k2.6:model_unavailable,nvidia:openai/gpt-oss-20b:ok`, `X-Cbrt-Fallback: true` |

Whole package: `ok cbrt/internal/live 16.4s`. Without `-tags live` the package is excluded, so `go test ./...` does not run it.

## Hygiene

- `gofmt -l cmd internal scripts` returns nothing.
- `go vet ./...` and `go vet -tags live ./internal/live` both pass.
- `go test -count=1 ./...` passes for all packages.

## Follow-ups (not done here)

1. **Memory target.** Ship `GOMEMLIMIT` (for example 30MiB) in the Docker, systemd and README defaults, or call `debug.SetMemoryLimit` from a config value. The longer-term fix is to cut per-request JSON copies.
2. **Per-chunk error sniffing.** Add a `bytes.Contains(…, "error")` check before `router.ClassifyPayloadError` in the stream relay.
3. **`parseChat`.** Only try `[]part` for content whose first byte is `[`. Reuse the parsed messages in `normalizeSystem`.
4. **Responses translation.** Avoid the double pass (Responses → chat JSON → `parseChat` → `upstreamBody`). Validate tool `parameters` with a first-byte check instead of a full unmarshal into a map.

## Update: single-pass request pipeline (2026-09-30)

These are the follow-ups from this report that have been applied:

1. **Stream chunks:** they are pre-checked with `bytes.Contains(data, "\"error\"")` before `ClassifyPayloadError` decodes them.
2. **`parseChat`:** it only decodes a message's content as an array (to look for images) when the content starts with `[`.
3. **Default soft memory limit:** `cbrt serve` calls `debug.SetMemoryLimit(32 MiB)` unless `GOMEMLIMIT` is set. The limit is soft: the GC works harder near it, but it never causes an OOM.

Results on the same machine:

| Measure | Before | After | Target |
|---|---|---|---|
| Peak RSS, 16 streams + 16 non-stream, 50 KB bodies, 20 s | 48–50 MB | **38.1 MB** (p95 37.9) | < 40 MB ✅ |
| Idle RSS | 13.9–14.2 MB | 13.8 MB | < 20 MB ✅ |
| Load-test throughput | ~1,400 req/s | 1,692 req/s, 0 errors | — |
| Overhead, ChatNonStream50KB | 3.2 ms | 2.8 ms | < 2 ms ❌ (0.2 ms for small bodies ✅) |
| Overhead, ChatStream (148 chunks) | 9.7 ms | 8.1 ms | — |
| Overhead, ResponsesNonStream (Codex 262 KB, 180 tools) | 22.2 ms | ~24.6 ms (run-to-run noise) | — |

**Still open:**
- Large-body overhead comes from the repeated JSON passes (items 3 and 4 above). A single-pass request pipeline would remove most of it.
- The difference is milliseconds, against provider latencies of seconds (free-tier queues run up to about 145 s), so this was not done at first.

## Single-pass request pipeline (2026-09-30)

**What changed:**
- **`internal/jsonspan`** (new): a stdlib scanner that validates JSON once, at about 1 GB/s, and returns the raw byte spans of an object's members or an array's elements. A 60 s fuzz run checked it against `encoding/json`.
- **`parseChat`:**
  - It scans the body once and reads only the small fields it needs from their spans.
  - It indexes messages by span (role, content).
  - Image detection skips string content entirely.
- **`upstreamBody`:**
  - It assembles each target's request by splicing the original member bytes and substituting only `model`, `messages` (when normalized), `stream_options` and the token-limit field.
  - It does no `json.Marshal` of the whole request, so nothing is re-validated or re-compacted.
  - System-message normalization is computed once per request and reused across failover attempts.
- **Server:** it no longer pre-parses the body to find `stream`. The gateway admits the stream (the concurrent-stream limit) once it has parsed the request (`gateway.WithStreamAdmission`).
- **Translators** (`internal/api/anthropic`, `internal/api/responses`): the decoders validate once and copy tool schemas, texts and tool outputs verbatim into a single output buffer, with no `map[string]any` decoding. They are 12–30× faster on the real Claude Code and Codex captures.
- **Stream flush coalescing:** the relay flushes only when no complete further upstream event is already buffered (`stream.Reader.EventBuffered`). Latency is unchanged, because each event is flushed as soon as the gateway would wait, but a burst of chunks costs one write syscall instead of one each. Flushing had been about 1/3 of stream CPU on Windows loopback.
- **Correctness safeguards:**
  - The old implementations are kept as reference oracles in `_test.go` files, and equivalence tests compare new against old on every real capture, on edge cases, and under fuzzing (2.2M execs for the chat pipeline; 90 s per translator).
  - Fuzzing found that the old `encoding/json` struct decoding matched keys such as `"Content"` and `"tYpe"` case-insensitively. The new chat pipeline uses exact keys, like the provider APIs. The translators keep the old semantics exactly.

**End-to-end gateway benchmarks** (loopback fake upstream, mean per request):

| Benchmark | Before | After | Change |
|---|---|---|---|
| ChatNonStream (small) | 0.29 ms | 0.28 ms | — |
| ChatNonStream50KB | 3.07 ms | **0.67 ms** | 4.6× |
| ChatStream (148 chunks) | 8.5 ms | **1.46 ms** | 5.8× |
| MessagesNonStream (Claude Code 0016) | 1.54 ms | **0.34 ms** | 4.5× |
| MessagesStream | 10.2 ms | **1.75 ms** | 5.8× |
| ResponsesNonStream (Codex 262 KB, 180 tools) | 25.4 ms | **2.2 ms** | 11.5× |
| ResponsesStream | 30.4 ms | **3.8 ms** | 8× |

The 50 KB chat overhead is now about 0.33 ms (gateway minus baseline), so the target of < 2 ms p50 is **met**.

**Load test** (16 streams + 16 non-stream, 50 KB bodies, 20 s):

| | Before | After |
|---|---|---|
| Throughput | 1,692 req/s | **3,741 req/s**, 0 errors |
| Non-stream latency p50 / p95 | 9.2 / 16.0 ms | **4.1 / 7.5 ms** |
| Peak RSS | 38.1 MB | **37.0 MB** (< 40 MB ✅) |
| Idle RSS | 13.8 MB | 13.9 MB |
