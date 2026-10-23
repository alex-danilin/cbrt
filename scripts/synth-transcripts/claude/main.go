// Command claude writes synthetic Claude Code transcripts for the session
// analyzer's adapter tests, under testdata/analyze/claude/projects/.
//
// Everything is invented: a small fictional project (inkwell, a word-count
// CLI written in Go), its prompts, answers, paths, ids and token numbers. Only
// the structure follows Claude Code's transcript format: record types, field
// names, one record per content block sharing a requestId, streaming
// snapshots in subagent transcripts, per-record gitBranch, compaction
// boundaries, injected context and subagent files with a meta sidecar.
//
// Run from the repository root; the output is deterministic:
//
//	go run ./scripts/synth-transcripts/claude
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type obj = map[string]any
type arr = []any

const (
	outDir = "testdata/analyze/claude/projects/-work-inkwell"
	cwd    = "/work/inkwell"
)

var base = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

func sessionID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-0000000000%02d", n) }

// session builds one transcript file.
type session struct {
	id, branch, entry, model, agentID string
	n                                 int // record counter
	sec                               float64
	recs                              []obj
	prev                              any
	sidechain                         bool
	num                               int
}

func newSession(num int, branch, entry, model string) *session {
	return &session{id: sessionID(num), num: num, branch: branch, entry: entry, model: model,
		sec: float64(num) * 3600}
}

// tick advances the clock and returns the next timestamp.
func (s *session) tick(sec float64) string {
	s.sec += sec
	return base.Add(time.Duration(s.sec * float64(time.Second))).Format("2006-01-02T15:04:05.000Z")
}

func (s *session) uuid() string {
	s.n++
	kind := 0
	if s.sidechain {
		kind = 1
	}
	return fmt.Sprintf("00000000-0000-4000-a%03d-%04d%08d", kind, s.num, s.n)
}

// add appends a message-bearing record with the common envelope fields.
func (s *session) add(typ string, after float64, fields obj) obj {
	u := s.uuid()
	r := obj{
		"type": typ, "uuid": u, "parentUuid": s.prev, "timestamp": s.tick(after),
		"sessionId": s.id, "cwd": cwd, "gitBranch": s.branch, "entrypoint": s.entry,
		"userType": "external", "isSidechain": s.sidechain, "version": "9.9.9",
	}
	if s.sidechain {
		r["agentId"] = s.agentID
	}
	for k, v := range fields {
		r[k] = v
	}
	s.prev = u
	s.recs = append(s.recs, r)
	return r
}

func (s *session) prompt(after float64, text string) {
	s.add("user", after, obj{"promptId": "prompt-fake", "message": obj{"role": "user", "content": text}})
}

func (s *session) meta(after float64, text string) {
	s.add("user", after, obj{"isMeta": true, "message": obj{"role": "user", "content": text}})
}

func (s *session) attachment(after float64, typ, text string) {
	s.add("attachment", after, obj{"attachment": obj{"type": typ, "text": text}})
}

type usage struct{ in, cw, cr, out, think int64 }

func (u usage) json(details bool) obj {
	o := obj{"input_tokens": u.in, "cache_creation_input_tokens": u.cw, "cache_read_input_tokens": u.cr,
		"output_tokens": u.out, "service_tier": "standard",
		"cache_creation": obj{"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": u.cw}}
	if details {
		o["output_tokens_details"] = obj{"thinking_tokens": u.think}
	}
	return o
}

// response writes one model response as one record per content block, all
// with the same requestId and message id and the same final usage (the shape
// of main transcripts).
func (s *session) response(after float64, req string, u usage, stop string, blocks ...obj) {
	for i, b := range blocks {
		gap := 0.2
		if i == 0 {
			gap = after
		}
		s.add("assistant", gap, obj{"requestId": req, "effort": "high", "message": obj{
			"id": "msg_" + req, "type": "message", "role": "assistant", "model": s.model,
			"content": arr{b}, "stop_reason": stop, "stop_sequence": nil, "usage": u.json(true)}})
	}
}

// streamed writes a response the way subagent transcripts do: earlier records
// are snapshots (stop_reason null, no output_tokens_details, smaller
// output_tokens); the last one carries the final usage.
func (s *session) streamed(after float64, req string, snap, final usage, stop string, blocks ...obj) {
	for i, b := range blocks {
		gap := 0.3
		if i == 0 {
			gap = after
		}
		last := i == len(blocks)-1
		u, st := snap, any(nil)
		if last {
			u, st = final, stop
		}
		s.add("assistant", gap, obj{"requestId": req, "effort": "medium", "message": obj{
			"id": "msg_" + req, "type": "message", "role": "assistant", "model": s.model,
			"content": arr{b}, "stop_reason": st, "stop_sequence": nil, "usage": u.json(last)}})
	}
}

func text(t string) obj { return obj{"type": "text", "text": t} }
func thinking(t string) obj {
	return obj{"type": "thinking", "thinking": t, "signature": "c2lnLWZha2U="}
}
func toolUse(id, name string, input obj) obj {
	return obj{"type": "tool_use", "id": id, "name": name, "input": input, "caller": obj{"type": "direct"}}
}

func (s *session) result(after float64, id, content string, isErr bool, extra obj) {
	f := obj{"message": obj{"role": "user", "content": arr{obj{"type": "tool_result", "tool_use_id": id,
		"content": content, "is_error": isErr}}}, "toolUseResult": obj{"stdout": content, "interrupted": false}}
	for k, v := range extra {
		f[k] = v
	}
	s.add("user", after, f)
}

func (s *session) system(after float64, subtype string, fields obj) {
	f := obj{"subtype": subtype, "isMeta": false}
	for k, v := range fields {
		f[k] = v
	}
	s.add("system", after, f)
}

// sidecar records without uuid/timestamp, which produce no events.
func (s *session) aside(r obj) { s.recs = append(s.recs, r) }

func encode(recs []obj) []byte {
	var b bytes.Buffer
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			panic(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
}

func main() {
	if err := os.RemoveAll(outDir); err != nil {
		panic(err)
	}

	// Session 1: tool calls with a failing result, thinking, a response split
	// over three records, and a branch change. Its token sums are asserted by
	// hand in the adapter tests.
	s := newSession(1, "main", "cli", "claude-opus-4-5")
	s.aside(obj{"type": "permission-mode", "permissionMode": "default", "sessionId": s.id})
	s.prompt(0, "Add a --words flag to inkwell that prints the word count of a file.")
	s.attachment(0.1, "date", "Today's date is 2026-03-02.")
	s.attachment(0.1, "total_tokens_reminder", "Context used: 2048 tokens.")
	s.response(1.5, "req_fake_s1_a", usage{12, 2048, 0, 180, 120}, "tool_use",
		thinking("The flag parsing lives in main.go; read it first."),
		text("I'll look at the command entry point."),
		toolUse("toolu_fake_s1_01", "Read", obj{"file_path": "/work/inkwell/cmd/inkwell/main.go"}))
	s.result(2.5, "toolu_fake_s1_01", "package main\n\nfunc main() { run(os.Args[1:]) }\n", false, nil)
	s.response(1.0, "req_fake_s1_b", usage{8, 300, 2048, 60, 0}, "tool_use",
		toolUse("toolu_fake_s1_02", "Bash", obj{"command": "go test ./...", "description": "Run the tests"}))
	s.result(4.0, "toolu_fake_s1_02", "--- FAIL: TestWords (0.00s)\n    count_test.go:9: got 0, want 3", true, nil)
	s.response(1.0, "req_fake_s1_c", usage{5, 0, 2348, 40, 10}, "end_turn",
		text("TestWords fails because countWords is not implemented yet. Shall I create a branch for it?"))
	s.system(0.1, "turn_duration", obj{"durationMs": 10300, "messageCount": 9})
	s.branch = "feature/word-count"
	s.prompt(30, "Yes, I switched to feature/word-count. Implement it.")
	s.response(1.2, "req_fake_s1_d", usage{20, 500, 2348, 300, 100}, "tool_use",
		thinking("strings.Fields splits on any whitespace, which matches the test."),
		toolUse("toolu_fake_s1_03", "Edit", obj{"file_path": "/work/inkwell/count.go",
			"old_string": "return 0", "new_string": "return len(strings.Fields(s))"}))
	s.result(0.4, "toolu_fake_s1_03", "The file /work/inkwell/count.go has been updated.", false, nil)
	s.response(0.8, "req_fake_s1_e", usage{4, 0, 2848, 25, 0}, "end_turn",
		text("Done: countWords now uses strings.Fields."))
	s.aside(obj{"type": "last-prompt", "lastPrompt": "Yes, I switched", "sessionId": s.id})
	write(filepath.Join(outDir, s.id+".jsonl"), encode(s.recs))

	// Session 2: injected context, a compaction, a model switch, an API error
	// and an interruption.
	s = newSession(2, "main", "claude-desktop", "claude-opus-4-5")
	s.prompt(0, "Explain how inkwell reads its input files.")
	s.meta(0.1, "<system-reminder>The user opened count.go in the editor.</system-reminder>")
	s.response(2.0, "req_fake_s2_a", usage{30, 4000, 0, 220, 0}, "end_turn",
		text("inkwell opens each path with os.Open and streams it through a bufio.Scanner."))
	s.system(60, "compact_boundary", obj{"content": "Conversation compacted",
		"compactMetadata": obj{"trigger": "auto", "preTokens": 150000}})
	s.add("user", 0.5, obj{"isCompactSummary": true, "message": obj{"role": "user",
		"content": "This session is being continued from a previous conversation. Summary: the user asked how input is read."}})
	s.prompt(5, "<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>haiku</command-args>")
	s.add("user", 0.1, obj{"isMeta": true, "message": obj{"role": "user",
		"content": "<local-command-stdout>Set model to haiku</local-command-stdout>"}})
	s.model = "claude-haiku-4-5"
	s.prompt(3, "Summarize that in one sentence.")
	s.response(1.0, "req_fake_s2_b", usage{6, 0, 4000, 30, 0}, "end_turn",
		text("Files are streamed line by line, never loaded whole."))
	s.prompt(20, "And the error handling?")
	s.system(0.5, "api_error", obj{"level": "error", "retryAttempt": 1, "maxRetries": 10, "retryInMs": 500,
		"error": obj{"status": 529, "message": "Overloaded"}})
	s.add("assistant", 5, obj{"isApiErrorMessage": true, "message": obj{"id": "msg_fake_s2_err", "type": "message",
		"role": "assistant", "model": "<synthetic>", "content": arr{text("API Error: 529 Overloaded")},
		"stop_reason": "stop_sequence", "usage": usage{}.json(false)}})
	s.add("user", 2, obj{"message": obj{"role": "user", "content": arr{text("[Request interrupted by user]")}}})
	write(filepath.Join(outDir, s.id+".jsonl"), encode(s.recs))

	// Session 3: a subagent, stored in its own file next to the session.
	s = newSession(3, "main", "cli", "claude-opus-4-5")
	s.prompt(0, "Find every place inkwell opens a file.")
	s.response(1.0, "req_fake_s3_a", usage{15, 1200, 0, 90, 20}, "tool_use",
		toolUse("toolu_fake_s3_01", "Agent", obj{"description": "Find file opens",
			"prompt": "List every os.Open call in the inkwell repository.", "subagent_type": "Explore"}))
	parentPrev := s.prev

	agent := "a00000000000000f1"
	sub := &session{id: s.id, num: 3, branch: "main", entry: "cli", model: "claude-haiku-4-5",
		sidechain: true, agentID: agent, sec: s.sec, n: 100}
	sub.prompt(0.5, "List every os.Open call in the inkwell repository.")
	sub.streamed(1.0, "req_fake_s3_sub_a", usage{9, 800, 0, 4, 0}, usage{9, 800, 0, 70, 30}, "tool_use",
		thinking("A grep for os.Open is enough."),
		toolUse("toolu_fake_s3_sub_01", "Grep", obj{"pattern": `os\.Open`, "path": "/work/inkwell"}))
	sub.result(0.7, "toolu_fake_s3_sub_01", "count.go:14\nmain.go:22", false, nil)
	sub.streamed(1.0, "req_fake_s3_sub_b", usage{}, usage{3, 0, 800, 35, 0}, "end_turn",
		text("Two calls: count.go line 14 and main.go line 22."))
	write(filepath.Join(outDir, s.id, "subagents", "agent-"+agent+".jsonl"), encode(sub.recs))
	meta, _ := json.Marshal(obj{"agentType": "Explore", "description": "Find file opens",
		"toolUseId": "toolu_fake_s3_01", "spawnDepth": 1})
	write(filepath.Join(outDir, s.id, "subagents", "agent-"+agent+".meta.json"), append(meta, '\n'))

	s.prev, s.sec = parentPrev, sub.sec
	s.result(0.2, "toolu_fake_s3_01", "Two calls: count.go line 14 and main.go line 22.", false,
		obj{"toolUseResult": obj{"agentId": agent, "status": "completed"}})
	s.response(1.0, "req_fake_s3_b", usage{10, 0, 1290, 50, 0}, "end_turn",
		text("inkwell opens files in count.go and main.go."))
	write(filepath.Join(outDir, s.id+".jsonl"), encode(s.recs))

	// Session 4: outside a git repository (empty gitBranch), an SDK entry
	// point, a denied tool call, MCP and web tools, and a trailing partial
	// line: the tool was still writing when the file was read.
	s = newSession(4, "", "sdk-cli", "claude-opus-4-5")
	s.prompt(0, "Check whether the notes server has an entry about release dates, then look up semver.")
	s.response(1.0, "req_fake_s4_a", usage{11, 900, 0, 45, 0}, "tool_use",
		toolUse("toolu_fake_s4_01", "mcp__notes__search", obj{"query": "release dates"}))
	s.result(0.6, "toolu_fake_s4_01", "No notes match.", false, nil)
	s.response(1.0, "req_fake_s4_b", usage{7, 0, 900, 30, 0}, "tool_use",
		toolUse("toolu_fake_s4_02", "WebFetch", obj{"url": "https://semver.example.com/", "prompt": "Summarize the rules"}))
	s.result(1.5, "toolu_fake_s4_02", "Semantic versioning: MAJOR.MINOR.PATCH.", false, nil)
	s.response(1.0, "req_fake_s4_c", usage{6, 0, 930, 25, 0}, "tool_use",
		toolUse("toolu_fake_s4_03", "Bash", obj{"command": "git tag v1.0.0"}))
	s.result(8.0, "toolu_fake_s4_03", "The user doesn't want to proceed with this tool use.", true,
		obj{"toolDenialKind": "user-rejected"})
	data := encode(s.recs)
	data = append(data, []byte(`{"type":"assistant","uuid":"00000000-0000-4000-a000-000400000099","timestamp":"2026-03-02T14:01:00.000Z","message":{"role":"assis`)...)
	write(filepath.Join(outDir, s.id+".jsonl"), data)

	// Session 5: bulky records an excerpt must not print whole: a large file
	// read, a tool result with an image block (fake base64), and a large
	// injected attachment.
	s = newSession(5, "main", "cli", "claude-opus-4-5")
	s.prompt(0, "Compare the long sample corpus with the screenshot of the output.")
	s.attachment(0.1, "skill_listing", strings.Repeat("- skill-fake: does a fictional thing.\n", 300))
	s.response(1.0, "req_fake_s5_a", usage{14, 1500, 0, 50, 0}, "tool_use",
		toolUse("toolu_fake_s5_01", "Read", obj{"file_path": "/work/inkwell/testdata/corpus.txt"}))
	s.result(0.5, "toolu_fake_s5_01", strings.Repeat("the quick brown fox jumps over the lazy dog\n", 600), false, nil)
	s.response(1.0, "req_fake_s5_b", usage{9, 0, 1500, 40, 0}, "tool_use",
		toolUse("toolu_fake_s5_02", "mcp__shots__capture", obj{"target": "inkwell --words corpus.txt"}))
	s.add("user", 2.0, obj{"message": obj{"role": "user", "content": arr{obj{"type": "tool_result", "tool_use_id": "toolu_fake_s5_02",
		"content": arr{text("Captured."), obj{"type": "image", "source": obj{"type": "base64", "media_type": "image/png",
			"data": strings.Repeat("iVBORw0KGgoFAKE", 400)}}}}}}})
	s.response(1.0, "req_fake_s5_c", usage{6, 0, 1540, 30, 0}, "end_turn",
		text("The corpus has 5400 words and the screenshot shows the same count."))
	write(filepath.Join(outDir, s.id+".jsonl"), encode(s.recs))
}
