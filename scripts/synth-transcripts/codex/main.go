// Command codex writes synthetic Codex CLI rollouts for the session-analyzer
// adapter tests (internal/analyze/adapters/codex). Every value is invented:
// a fictional project ("lighthouse", a weather-station dashboard), fake ids
// (UUIDs start 00000000-0000-), made-up commands and token counts. Only the
// record format follows Codex CLI. Output is deterministic.
//
//	go run ./scripts/synth-transcripts/codex [-out testdata/analyze/codex/sessions]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

type obj = map[string]any

type line struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Ordinal   *int   `json:"ordinal,omitempty"`
	Payload   any    `json:"payload"`
}

type builder struct {
	t        time.Time
	ordinals bool
	cli      string // session_meta cli_version; "" means 0.154.0
	n        int
	buf      bytes.Buffer
}

func newBuilder(start string, ordinals bool) *builder {
	t, err := time.Parse(time.RFC3339, start)
	if err != nil {
		log.Fatal(err)
	}
	return &builder{t: t, ordinals: ordinals}
}

// tick advances the clock by ms milliseconds.
func (b *builder) tick(ms int) { b.t = b.t.Add(time.Duration(ms) * time.Millisecond) }

func (b *builder) add(typ string, payload any) {
	l := line{Timestamp: b.t.UTC().Format("2006-01-02T15:04:05.000Z"), Type: typ, Payload: payload}
	if b.ordinals {
		o := b.n
		l.Ordinal = &o
	}
	b.n++
	enc := json.NewEncoder(&b.buf) // writes the line and its newline
	enc.SetEscapeHTML(false)       // Codex writes <, > and & as is
	if err := enc.Encode(l); err != nil {
		log.Fatal(err)
	}
	b.tick(250)
}

func id(n int) string     { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
func turnID(n int) string { return fmt.Sprintf("00000000-0000-4000-9000-%012d", n) }
func callID(n int) string { return fmt.Sprintf("call_fake_%04d", n) }
func itemID(n int) string { return fmt.Sprintf("item_fake_%04d", n) }
func jsonStr(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

func (b *builder) meta(sid, originator, source string, git obj) {
	cli := b.cli
	if cli == "" {
		cli = "0.154.0"
	}
	p := obj{"id": sid, "timestamp": b.t.UTC().Format(time.RFC3339), "cwd": "/srv/projects/lighthouse",
		"originator": originator, "cli_version": cli, "source": source, "model_provider": "openai"}
	if git != nil {
		p["git"] = git
	}
	b.add("session_meta", p)
}

func (b *builder) turnContext(turn int, model, effort string) {
	b.add("turn_context", obj{"turn_id": turnID(turn), "cwd": "/srv/projects/lighthouse", "approval_policy": "on-request",
		"sandbox_policy": obj{"type": "workspace-write"}, "model": model, "effort": effort, "summary": "auto"})
}

func (b *builder) event(typ string, fields obj) {
	p := obj{"type": typ}
	for k, v := range fields {
		p[k] = v
	}
	b.add("event_msg", p)
}

func (b *builder) msg(role, partType, text string) {
	b.add("response_item", obj{"type": "message", "role": role, "content": []obj{{"type": partType, "text": text}}})
}

func (b *builder) userPrompt(text string) {
	b.event("user_message", obj{"message": text, "images": []any{}, "text_elements": []any{}})
	b.msg("user", "input_text", text)
}

func (b *builder) reasoning(summary string) {
	b.add("response_item", obj{"type": "reasoning", "id": itemID(b.n), "summary": []obj{{"type": "summary_text", "text": summary}},
		"encrypted_content": "ZmFrZS1lbmNyeXB0ZWQ="})
}

func (b *builder) fcall(name, ns string, args obj, call int) {
	p := obj{"type": "function_call", "name": name, "arguments": jsonStr(args), "call_id": callID(call), "id": itemID(b.n)}
	if ns != "" {
		p["namespace"] = ns
	}
	b.add("response_item", p)
}

func (b *builder) fout(call int, output any) {
	b.add("response_item", obj{"type": "function_call_output", "call_id": callID(call), "output": output})
}

func (b *builder) custom(name, input string, call int) {
	b.add("response_item", obj{"type": "custom_tool_call", "status": "completed", "name": name, "input": input,
		"call_id": callID(call), "id": itemID(b.n)})
}

func (b *builder) customOut(call int, output any) {
	b.add("response_item", obj{"type": "custom_tool_call_output", "call_id": callID(call), "output": output})
}

type u struct{ in, cached, out, reasoning int }

func usage(x u) obj {
	return obj{"input_tokens": x.in, "cached_input_tokens": x.cached, "cache_write_input_tokens": 0,
		"output_tokens": x.out, "reasoning_output_tokens": x.reasoning, "total_tokens": x.in + x.out}
}

// tokens writes a token_count: last is this response's usage, total the
// running total.
func (b *builder) tokens(last, total u) {
	b.event("token_count", obj{"info": obj{"last_token_usage": usage(last), "total_token_usage": usage(total),
		"model_context_window": 272000}, "rate_limits": nil})
}

func (b *builder) tokensNull() { b.event("token_count", obj{"info": nil, "rate_limits": nil}) }

func respID(n int) string { return fmt.Sprintf("resp_fake_%04d", n) }

// usageRecord writes a token_usage_record (Codex 0.153+), which precedes its
// response's token_count: usage is the response's, turnUsage the turn's
// running sum, thread the session's running total.
func (b *builder) usageRecord(sid string, turn, resp int, usg, turnUsage, thread u) {
	b.add("token_usage_record", obj{"session_id": sid, "thread_id": sid, "turn_id": turnID(turn), "root_turn_id": turnID(turn),
		"response_id": respID(resp), "usage": usage(usg), "turn_token_usage": usage(turnUsage), "thread_token_usage": usage(thread)})
}

const envContext = "<environment_context>\n  <cwd>/srv/projects/lighthouse</cwd>\n  <shell>bash</shell>\n</environment_context>"
const agentsMD = "# AGENTS.md instructions for /srv/projects/lighthouse\n\n<INSTRUCTIONS>\nRun the tests before you finish.\n</INSTRUCTIONS>"

const patch1 = "*** Begin Patch\n*** Update File: src/forecast/smoothing.go\n@@\n-func Smooth(xs []float64) []float64 {\n+func Smooth(xs []float64, window int) []float64 {\n*** Add File: src/forecast/smoothing_test.go\n+package forecast\n*** End Patch"

// session1: git info, two measured turns, shell reads/searches/edits,
// intermediate and repeated token_count events.
func session1() *builder {
	b := newBuilder("2026-01-05T09:00:00Z", false)
	b.meta(id(1), "codex_cli_rs", "cli", obj{"commit_hash": "1111111111111111111111111111111111111111", "branch": "feature/rain-gauge",
		"repository_url": "https://example.com/lighthouse.git"})
	b.msg("developer", "input_text", "<permissions instructions>\nWorkspace writes are allowed.\n</permissions instructions>")
	b.msg("user", "input_text", agentsMD)
	b.msg("user", "input_text", envContext)

	// Turn 1: 2 responses. Running total 0 → (2500 in, 1100 cached, 170 out, 60 reasoning).
	b.event("task_started", obj{"turn_id": turnID(1), "model_context_window": 272000})
	b.turnContext(1, "gpt-5-codex", "medium")
	b.userPrompt("The rain gauge chart is jumpy. Add a smoothing window to the forecast package.")
	b.reasoning("Look for the smoothing helper first.")
	b.fcall("shell_command", "", obj{"command": "rg -n Smooth src/forecast", "workdir": "/srv/projects/lighthouse", "timeout_ms": 10000}, 1)
	b.tick(1250)
	b.fout(1, "Exit code: 0\nWall time: 0.2 seconds\nOutput:\nsrc/forecast/smoothing.go:7:func Smooth(xs []float64) []float64 {\n")
	b.tokens(u{1000, 200, 50, 20}, u{1000, 200, 50, 20})
	b.fcall("shell_command", "", obj{"command": "cat src/forecast/smoothing.go", "workdir": "/srv/projects/lighthouse"}, 2)
	b.tick(500)
	b.fout(2, "Exit code: 0\nWall time: 0.1 seconds\nOutput:\npackage forecast\n")
	b.custom("apply_patch", patch1, 3)
	b.event("patch_apply_end", obj{"call_id": callID(3), "turn_id": turnID(1), "stdout": "", "stderr": "", "success": true, "status": "completed"})
	b.customOut(3, jsonStr(obj{"output": "Success. Updated the following files:\nM src/forecast/smoothing.go\nA src/forecast/smoothing_test.go\n",
		"metadata": obj{"exit_code": 0, "duration_seconds": 0.1}}))
	b.msg("assistant", "output_text", "Smooth now takes a window size; I added a test file for it.")
	b.tokens(u{1500, 900, 120, 40}, u{2500, 1100, 170, 60})
	b.event("task_complete", obj{"turn_id": turnID(1), "last_agent_message": "Smooth now takes a window size."})

	// Turn 2: one response, its token_count written twice.
	b.tick(30000)
	b.event("task_started", obj{"turn_id": turnID(2), "model_context_window": 272000})
	b.turnContext(2, "gpt-5-codex", "high")
	b.userPrompt("Run the forecast tests.")
	b.fcall("shell_command", "", obj{"command": "go test ./src/forecast/...", "workdir": "/srv/projects/lighthouse"}, 4)
	b.tick(4000)
	b.fout(4, "Exit code: 1\nWall time: 4.0 seconds\nOutput:\n--- FAIL: TestSmoothWindow\n")
	b.msg("assistant", "output_text", "One test fails: the window is off by one.")
	b.tokens(u{3000, 2400, 80, 0}, u{5500, 3500, 250, 60})
	b.tokens(u{3000, 2400, 80, 0}, u{5500, 3500, 250, 60})
	b.event("task_complete", obj{"turn_id": turnID(2), "last_agent_message": "One test fails."})
	return b
}

// session2: no git, IDE source. A turn whose usage is null, an interrupted
// turn (one response counted, no closing token_count), then a measured turn.
func session2() *builder {
	b := newBuilder("2026-01-06T14:30:00Z", false)
	b.meta(id(2), "codex_vscode", "vscode", nil)
	b.msg("user", "input_text", envContext)

	b.event("task_started", obj{"turn_id": turnID(11)})
	b.turnContext(11, "local-model", "low")
	b.userPrompt("Explain what the station poller does.")
	b.msg("assistant", "output_text", "It polls each station every minute and caches the readings.")
	b.tokensNull()
	b.event("task_complete", obj{"turn_id": turnID(11)})

	b.event("task_started", obj{"turn_id": turnID(12)})
	b.turnContext(12, "gpt-5-codex", "medium")
	b.userPrompt("Rewrite the poller to use a ticker.")
	b.reasoning("Start with the poll loop.")
	b.tokens(u{400, 0, 30, 10}, u{400, 0, 30, 10})
	b.fcall("shell_command", "", obj{"command": "sed -n 1,80p src/poller/poll.go"}, 21)
	b.tick(300)
	b.event("turn_aborted", obj{"turn_id": turnID(12), "reason": "interrupted"})
	b.msg("developer", "input_text", "<turn_aborted>\nThe user interrupted the previous turn.\n</turn_aborted>")

	b.event("task_started", obj{"turn_id": turnID(13)})
	b.turnContext(13, "gpt-5-codex", "medium")
	b.userPrompt("Never mind, just list the poller files.")
	b.fcall("shell_command", "", obj{"command": "ls src/poller"}, 22)
	b.tick(200)
	b.fout(22, "Exit code: 0\nWall time: 0.0 seconds\nOutput:\npoll.go\npoll_test.go\n")
	b.msg("assistant", "output_text", "There are two files: poll.go and poll_test.go.")
	b.tokens(u{600, 400, 40, 5}, u{1000, 400, 70, 15})
	b.event("task_complete", obj{"turn_id": turnID(13)})
	return b
}

// session3: ordinals on every record, git info, a compaction mid-turn, the
// world_state injection, and a trailing partial line (a write in progress).
func session3() *builder {
	b := newBuilder("2026-01-07T20:15:00Z", true)
	b.meta(id(3), "codex_cli_rs", "cli", obj{"commit_hash": "2222222222222222222222222222222222222222", "branch": "main"})
	b.add("world_state", obj{"full": true, "state": obj{"environments": obj{"current_date": "2026-01-07", "timezone": "UTC"}}})
	b.event("task_started", obj{"turn_id": turnID(21)})
	b.turnContext(21, "gpt-5-codex", "high")
	b.userPrompt("Summarize the alerting module, it has grown a lot.")
	b.tokens(u{2000, 0, 100, 0}, u{2000, 0, 100, 0})
	b.add("compacted", obj{"message": "", "replacement_history": []obj{{"type": "message", "role": "user",
		"content": []obj{{"type": "input_text", "text": "Summarize the alerting module, it has grown a lot."}}}}})
	b.event("context_compacted", obj{})
	b.msg("assistant", "output_text", "The alerting module has three parts: thresholds, routing and quiet hours.")
	b.tokens(u{300, 100, 20, 0}, u{2300, 100, 120, 0})
	b.event("task_complete", obj{"turn_id": turnID(21)})
	b.buf.WriteString(`{"timestamp":"2026-01-07T20:20:00.000Z","type":"event_msg","ordinal":99,"payload":{"type":"task_star`)
	return b
}

// session4: `codex exec` with the free-form exec tool, a wait on its cell, the
// web namespace, a plan update, a question to the user and an orphan output.
func session4() *builder {
	b := newBuilder("2026-01-08T07:45:00Z", false)
	b.meta(id(4), "codex_exec", "exec", nil)
	b.event("task_started", obj{"turn_id": turnID(31)})
	b.turnContext(31, "gpt-5-codex", "medium")
	b.userPrompt("Check whether the forecast API changed its rate limits this year.")
	b.fcall("run", "web", obj{"search_query": []obj{{"q": "lighthouse forecast api rate limits"}}, "response_length": "short"}, 41)
	b.tick(2000)
	b.fout(41, []obj{{"type": "input_text", "text": "No change found in the public changelog."}})
	b.custom("exec", "const r = await tools.shell_command({command: \"grep -rn RATE src/api\"});\ntext(r);", 42)
	b.fcall("wait", "", obj{"cell_id": "1", "yield_time_ms": 1000, "max_tokens": 2000}, 43)
	b.tick(1000)
	b.fout(43, "Process exited with code 0\nsrc/api/client.go:12:const RATE = 60\n")
	b.customOut(42, []obj{{"type": "input_text", "text": "src/api/client.go:12:const RATE = 60"}})
	b.fcall("update_plan", "", obj{"plan": []obj{{"step": "check limits", "status": "completed"}}}, 44)
	b.fout(44, "Plan updated")
	b.fcall("request_user_input", "", obj{"questions": []obj{{"question": "Raise the client limit too?"}}}, 45)
	b.fout(45, "yes")
	b.fout(99, "an output whose call is not in this rollout")
	b.msg("assistant", "output_text", "The limit is still 60 requests per minute.")
	b.tokens(u{800, 0, 60, 30}, u{800, 0, 60, 30})
	b.event("task_complete", obj{"turn_id": turnID(31)})
	b.event("error", obj{"message": "stream disconnected before completion"})
	return b
}

// session5: Codex 0.159 with token_usage_record before each response.
//   - Turn 41: three responses with records; the second's token_count is
//     written twice.
//   - Turn 42: opens with a repeat of turn 41's last token_count (same running
//     total, different last_token_usage); then a response whose running total
//     grows by more than its own usage, so the turn is merged: one request for
//     the turn's increase (2000, 1300 cached, 70 out, 15 reasoning).
//   - Turn 43: interrupted before any usage.
//   - Turn 44: a record whose usage does not match the response that follows.
//
// Final running total (7200, 4300, 280, 45) = the sum over all requests.
func session5() *builder {
	b := newBuilder("2026-01-09T16:00:00Z", false)
	b.cli = "0.159.2"
	sid := id(5)
	b.meta(sid, "codex_cli_rs", "cli", obj{"commit_hash": "5555555555555555555555555555555555555555", "branch": "fix/wind-units"})

	b.event("task_started", obj{"turn_id": turnID(41)})
	b.turnContext(41, "gpt-5-codex", "medium")
	b.userPrompt("Wind speed shows m/s on the dashboard but the station sends knots. Convert it.")
	b.tokensNull()
	b.reasoning("Find where wind readings are parsed.")
	b.fcall("shell_command", "", obj{"command": "rg -n Wind src/station"}, 51)
	b.tick(400)
	b.fout(51, "Exit code: 0\nWall time: 0.1 seconds\nOutput:\nsrc/station/parse.go:30:\tWind float64\n")
	b.usageRecord(sid, 41, 1, u{1200, 0, 40, 10}, u{1200, 0, 40, 10}, u{1200, 0, 40, 10})
	b.tokens(u{1200, 0, 40, 10}, u{1200, 0, 40, 10})
	b.custom("apply_patch", "*** Begin Patch\n*** Update File: src/station/parse.go\n@@\n-\tr.Wind = v\n+\tr.Wind = v * 0.514444\n*** End Patch", 52)
	b.event("patch_apply_end", obj{"call_id": callID(52), "turn_id": turnID(41), "stdout": "", "stderr": "", "success": true, "status": "completed"})
	b.customOut(52, jsonStr(obj{"output": "Success. Updated the following files:\nM src/station/parse.go\n", "metadata": obj{"exit_code": 0}}))
	b.usageRecord(sid, 41, 2, u{1500, 1100, 60, 20}, u{2700, 1100, 100, 30}, u{2700, 1100, 100, 30})
	b.tokens(u{1500, 1100, 60, 20}, u{2700, 1100, 100, 30})
	b.tokens(u{1500, 1100, 60, 20}, u{2700, 1100, 100, 30})
	b.msg("assistant", "output_text", "Readings are now converted from knots to m/s when parsed.")
	b.usageRecord(sid, 41, 3, u{1800, 1400, 90, 0}, u{4500, 2500, 190, 30}, u{4500, 2500, 190, 30})
	b.tokens(u{1800, 1400, 90, 0}, u{4500, 2500, 190, 30})
	b.event("task_complete", obj{"turn_id": turnID(41), "last_agent_message": "Readings are now converted."})

	b.tick(20000)
	b.event("task_started", obj{"turn_id": turnID(42)})
	b.turnContext(42, "gpt-5-codex", "medium")
	b.userPrompt("Also convert the gust field.")
	b.tokens(u{0, 0, 90, 0}, u{4500, 2500, 190, 30})
	b.usageRecord(sid, 42, 4, u{1000, 800, 50, 10}, u{1000, 800, 50, 10}, u{6000, 3400, 250, 45})
	b.tokens(u{1000, 800, 50, 10}, u{6000, 3400, 250, 45})
	b.msg("assistant", "output_text", "Gusts are converted the same way now.")
	b.usageRecord(sid, 42, 5, u{500, 400, 10, 0}, u{1500, 1200, 60, 10}, u{6500, 3800, 260, 45})
	b.tokens(u{500, 400, 10, 0}, u{6500, 3800, 260, 45})
	b.event("task_complete", obj{"turn_id": turnID(42)})

	b.event("task_started", obj{"turn_id": turnID(43)})
	b.turnContext(43, "gpt-5-codex", "medium")
	b.userPrompt("Run the station tests.")
	b.tokensNull()
	b.event("turn_aborted", obj{"turn_id": turnID(43), "reason": "interrupted"})

	b.event("task_started", obj{"turn_id": turnID(44)})
	b.turnContext(44, "gpt-5-codex", "low")
	b.userPrompt("Just summarize the change.")
	b.usageRecord(sid, 44, 6, u{900, 500, 20, 0}, u{900, 500, 20, 0}, u{7400, 4300, 280, 45})
	b.tokens(u{700, 500, 20, 0}, u{7200, 4300, 280, 45})
	b.msg("assistant", "output_text", "Wind and gust readings are converted from knots to m/s at parse time.")
	b.event("task_complete", obj{"turn_id": turnID(44)})
	return b
}

func main() {
	out := flag.String("out", filepath.Join("testdata", "analyze", "codex", "sessions"), "output root")
	flag.Parse()
	files := []struct {
		day, clock string
		sid        int
		b          *builder
	}{
		{"05", "09-00-00", 1, session1()},
		{"06", "14-30-00", 2, session2()},
		{"07", "20-15-00", 3, session3()},
		{"08", "07-45-00", 4, session4()},
		{"09", "16-00-00", 5, session5()},
	}
	for _, f := range files {
		dir := filepath.Join(*out, "2026", "01", f.day)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
		name := fmt.Sprintf("rollout-2026-01-%sT%s-%s.jsonl", f.day, f.clock, id(f.sid))
		if err := os.WriteFile(filepath.Join(dir, name), f.b.buf.Bytes(), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
