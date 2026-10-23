// Command antigravity writes the synthetic Antigravity transcripts the
// session-analyzer adapter is tested on. Every prompt, path, command and
// output is invented (a fictional "orchard-ledger" project); only the file
// layout and record shapes follow the tool's format. Run from the repo root:
//
//	go run ./scripts/synth-transcripts/antigravity
//
// It rewrites testdata/analyze/antigravity/{antigravity,antigravity-cli}/ and
// is deterministic: a second run changes nothing.
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

type toolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type step struct {
	StepIndex int    `json:"step_index"`
	Source    string `json:"source"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	// Usage, on planner steps of newer IDE versions only, in the tool's field
	// order. The numbers are invented.
	InputTokens     *int64     `json:"input_tokens,omitempty"`
	CacheReadTokens *int64     `json:"cache_read_tokens,omitempty"`
	OutputTokens    *int64     `json:"output_tokens,omitempty"`
	Content         string     `json:"content,omitempty"`
	Thinking        string     `json:"thinking,omitempty"`
	ToolCalls       []toolCall `json:"tool_calls,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// conv builds one conversation's steps with a clock that advances per step.
type conv struct {
	t     time.Time
	steps []step
}

func newConv(start string) *conv {
	t, err := time.Parse(time.RFC3339, start)
	if err != nil {
		panic(err)
	}
	return &conv{t: t}
}

func (c *conv) add(s step) *conv {
	s.StepIndex = len(c.steps)
	if s.Status == "" {
		s.Status = "DONE"
	}
	if s.Source == "" {
		s.Source = "MODEL"
	}
	s.CreatedAt = c.t.UTC().Format(time.RFC3339)
	c.t = c.t.Add(3 * time.Second)
	c.steps = append(c.steps, s)
	return c
}

func (c *conv) user(text, settings string) *conv {
	content := "<USER_REQUEST>\n" + text + "\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is a fictional moment in the orchard-ledger demo.\n</ADDITIONAL_METADATA>"
	if settings != "" {
		content += "\n<USER_SETTINGS_CHANGE>\n" + settings + "\n</USER_SETTINGS_CHANGE>"
	}
	return c.add(step{Source: "USER_EXPLICIT", Type: "USER_INPUT", Content: content})
}

func (c *conv) planner(text, thinking string, calls ...toolCall) *conv {
	return c.add(step{Type: "PLANNER_RESPONSE", Content: text, Thinking: thinking, ToolCalls: calls})
}

// usage sets invented usage counts on the last step (a planner step).
func (c *conv) usage(in, cacheRead, out int64) *conv {
	s := &c.steps[len(c.steps)-1]
	s.InputTokens, s.CacheReadTokens, s.OutputTokens = &in, &cacheRead, &out
	return c
}

// timing is the header tool steps embed: Created At, and Completed At when
// the step finished. layout picks the zone form (UTC "Z" or an offset).
func (c *conv) timing(d time.Duration, offset bool, done bool) string {
	start := c.t.Add(time.Second)
	f := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	if offset {
		zone := time.FixedZone("demo", 3600)
		f = func(t time.Time) string { return t.In(zone).Format(time.RFC3339) }
	}
	s := "Created At: " + f(start) + "\n"
	if done {
		s += "Completed At: " + f(start.Add(d)) + "\n"
	}
	return s
}

func (c *conv) tool(typ, status, body string, d time.Duration, offset bool) *conv {
	return c.add(step{Type: typ, Status: status, Content: c.timing(d, offset, status != "RUNNING") + body})
}

func call(name string, args map[string]any) toolCall {
	args["toolAction"] = "demo action"
	args["toolSummary"] = "demo summary"
	return toolCall{Name: name, Args: args}
}

func intp(n int) *int { return &n }

const proj = "/workspace/orchard-ledger"

func settingsNote(from, to string) string {
	return "The user changed setting `Model Selection` from " + from + " to " + to + ". Please use it from now on; this note is not part of the user's request."
}

// ideMain: a full IDE conversation with thinking, usage counts on the planner
// steps after the model switch only, embedded timings in UTC and
// offset form, a checkpoint between a call and its result, a permission
// denial, a model switch, a failed command, an invalid-tool-call error
// answering a call, an MCP call, a failing generic tool, and a trailing
// partial line.
func ideMain() *conv {
	c := newConv("2026-03-02T10:00:00Z")
	c.user("Add a total_weight field to the harvest report in orchard-ledger.", settingsNote("None", "Gemini 3 Pro (High)"))
	c.add(step{Source: "SYSTEM", Type: "CONVERSATION_HISTORY", Content: "# Conversation history\nNo earlier conversations in this workspace."})
	c.planner("", "The report lives in report.go; read it and look for the weight helpers first.",
		call("view_file", map[string]any{"AbsolutePath": proj + "/report.go", "StartLine": 1, "EndLine": 80}),
		call("grep_search", map[string]any{"Query": "Weight", "SearchPath": proj, "MatchPerLine": true}))
	c.tool("VIEW_FILE", "DONE", "File Path: `"+proj+"/report.go`\nTotal Lines: 42\npackage ledger\n\ntype HarvestReport struct {\n\tCrates int\n}\n", time.Second, false)
	c.tool("GREP_SEARCH", "DONE", "Found 2 matches for Weight in "+proj+"\n", 2*time.Second, true)
	c.planner("I will add the field and a helper that sums crate weights.", "",
		call("replace_file_content", map[string]any{"TargetFile": proj + "/report.go", "TargetContent": "\tCrates int\n", "ReplacementContent": "\tCrates int\n\tTotalWeight float64\n"}))
	c.add(step{Source: "SYSTEM", Type: "CHECKPOINT", Content: "{{ CHECKPOINT 1 }}\nPath: " + proj + "\nLast Edited: report.go"})
	c.tool("CODE_ACTION", "DONE", "Edited "+proj+"/report.go: 1 line added.\n", 2*time.Second, false)
	c.planner("", "", call("run_command", map[string]any{"CommandLine": "go test ./...", "Cwd": proj, "WaitMsBeforeAsync": 5000}))
	c.tool("RUN_COMMAND", "ERROR", "\nEncountered error in step execution: user denied permission to run command:\ngo test ./...\n", 4*time.Second, false)
	c.user("You may run the tests now. Switch to the faster model for this.", settingsNote("Gemini 3 Pro (High)", "Gemini 3 Flash"))
	c.planner("", "", call("run_command", map[string]any{"CommandLine": "go test ./...", "Cwd": proj, "WaitMsBeforeAsync": 5000}),
		call("view_file", map[string]any{"AbsolutePath": proj + "/report_test.go"})).
		usage(5200, 0, 310) // the IDE was updated mid-conversation: usage from here on
	s := step{Type: "RUN_COMMAND", Content: c.timing(6*time.Second, false, true) + "Output:\n--- FAIL: TestHarvestTotals\nFAIL\n", ExitCode: intp(1)}
	c.add(s)
	c.add(step{Source: "SYSTEM", Type: "ERROR_MESSAGE", Content: c.timing(0, true, false) + "Error Message: invalid tool call: view_file needs AbsolutePath\nRetries remaining: 2", Error: "invalid tool call"})
	c.planner("Checking the forecast service and the docs before fixing the test.", "",
		call("call_mcp_tool", map[string]any{"ServerName": "weather-demo", "ToolName": "forecast", "Arguments": map[string]any{"query": "orchard frost"}}),
		call("search_web", map[string]any{"query": "go float64 rounding for weights"})).
		usage(640, 5248, 455)
	c.tool("GENERIC", "DONE", "Forecast: no frost expected in the demo valley.\n", 700*time.Millisecond, false)
	c.tool("SEARCH_WEB", "DONE", "Sources:\n1. example.com rounding guide\n", 3*time.Second, false)
	c.planner("", "", call("manage_task", map[string]any{"TaskId": "orchard-demo/task-9", "Action": "status"})).
		usage(410, 5888, 120)
	c.add(step{Type: "GENERIC", Status: "ERROR", Content: c.timing(200*time.Millisecond, false, true) + "task orchard-demo/task-9 not found", Error: "task not found"})
	c.planner("Done: HarvestReport now has TotalWeight, and the failing test is fixed.", "").
		usage(220, 6298, 95)
	return c
}

// ideInterrupt: background commands the model keeps working past (no
// interrupt), then a RUNNING command the user cuts off with a new prompt
// (interrupt), and a cancellation system message.
func ideInterrupt() *conv {
	c := newConv("2026-03-03T09:30:00Z")
	c.user("Start the orchard-ledger dev server.", "")
	c.planner("", "", call("run_command", map[string]any{"CommandLine": "go run ./cmd/ledger serve", "Cwd": proj, "WaitMsBeforeAsync": 500}))
	c.tool("RUN_COMMAND", "RUNNING", "Output so far:\nlistening on :8080\n", 0, false)
	c.planner("The server is starting in the background; checking its log.", "",
		call("run_command", map[string]any{"CommandLine": "go run ./cmd/ledger tail-log", "Cwd": proj, "WaitMsBeforeAsync": 500}))
	c.tool("RUN_COMMAND", "RUNNING", "Output so far:\nwaiting for log lines\n", 0, false)
	c.user("Stop, use port 9090 instead.", "")
	c.add(step{Source: "SYSTEM", Type: "SYSTEM_MESSAGE", Content: "The following is a <SYSTEM_MESSAGE> generated by the system, not sent by the user.\nTask id \"orchard-demo/task-4\" was canceled by the user."})
	c.add(step{Source: "SYSTEM", Type: "SYSTEM_MESSAGE", Content: "The following is a <SYSTEM_MESSAGE> generated by the system, not sent by the user.\nThe workspace index was refreshed."})
	c.planner("Restarting the server on port 9090.", "The user wants port 9090.")
	return c
}

// cliChunked: 25 steps spread over two chunk files and the live file. A
// record straddles the chunk boundary, the live file repeats steps 15-19,
// and steps 0-14 exist only in chunks.
func cliChunked() *conv {
	c := newConv("2026-03-04T14:00:00Z")
	c.user("Summarize every module in orchard-ledger.", settingsNote("None", "Gemini 3 Flash"))
	for k := 1; k < 25; k++ {
		file := fmt.Sprintf("%s/module%02d.go", proj, k)
		if k%2 == 1 {
			if k%4 == 1 {
				c.planner("", "Reading the next module.", call("view_file", map[string]any{"AbsolutePath": file}))
			} else {
				c.planner(fmt.Sprintf("Module %d holds the crate counters.", k), "", call("list_dir", map[string]any{"DirectoryPath": proj}))
			}
			continue
		}
		if k%4 == 2 {
			c.tool("VIEW_FILE", "DONE", "File Path: `"+file+"`\npackage ledger\n", time.Duration(k%5+1)*time.Second, false)
		} else {
			c.tool("LIST_DIRECTORY", "DONE", "Summary: 25 files in "+proj+"\n", time.Duration(k%5+1)*time.Second, true)
		}
	}
	return c
}

// cliTools: no settings note (model unknown), every remaining tool family,
// and steps written out of step order.
func cliTools() *conv {
	c := newConv("2026-03-05T08:00:00Z")
	c.user("Draft a README for orchard-ledger and check the hosted docs.", "")
	c.planner("", "", call("list_dir", map[string]any{"DirectoryPath": proj}), call("find_by_name", map[string]any{"Pattern": "*.md", "SearchDirectory": proj}))
	c.tool("LIST_DIRECTORY", "DONE", "Summary: 25 files\n", 300*time.Millisecond, false)
	c.tool("GENERIC", "DONE", "Found 0 results for *.md\n", 250*time.Millisecond, false)
	c.planner("", "", call("write_to_file", map[string]any{"TargetFile": proj + "/README.md", "CodeContent": "# orchard-ledger\n", "Overwrite": false}))
	c.tool("CODE_ACTION", "DONE", "Created file file:///workspace/orchard-ledger/README.md with 1 line.\n", 400*time.Millisecond, false)
	c.planner("", "", call("read_url_content", map[string]any{"Url": "https://docs.example.com/orchard-ledger"}),
		call("browser_subagent", map[string]any{"Task": "Open the demo docs page and report its title.", "RecordingName": "docs_check"}))
	c.tool("READ_URL_CONTENT", "DONE", "Title: Orchard Ledger demo docs\n", 900*time.Millisecond, false)
	c.tool("BROWSER_SUBAGENT", "DONE", "Browser subagent result: the page title is Orchard Ledger demo docs.\n", 12*time.Second, false)
	c.planner("", "", call("ask_question", map[string]any{"questions": []any{map[string]any{"question": "Add a license section?", "options": []any{"yes", "no"}, "is_multi_select": false}}}))
	c.tool("ASK_QUESTION", "DONE", "The user answered: yes\n", 20*time.Second, false)
	c.planner("", "", call("schedule", map[string]any{"TimerCondition": "in 1 hour", "Prompt": "Check the README again."}))
	c.tool("GENERIC", "DONE", "Scheduled a reminder.\n", 100*time.Millisecond, false)
	c.planner("The README is drafted with a license section; a reminder is scheduled.", "")
	return c
}

func lines(steps []step) [][]byte {
	var out [][]byte
	for _, s := range steps {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(s); err != nil {
			log.Fatal(err)
		}
		out = append(out, b.Bytes()) // ends with '\n'
	}
	return out
}

// compact is the shape of transcript.jsonl: content cut short. The adapter
// must never read it.
func compact(steps []step) []byte {
	var cut []step
	for _, s := range steps {
		if len(s.Content) > 40 {
			s.Content = s.Content[:40]
		}
		s.Thinking = ""
		cut = append(cut, s)
	}
	return bytes.Join(lines(cut), nil)
}

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
}

func main() {
	out := flag.String("out", filepath.Join("testdata", "analyze", "antigravity"), "output directory")
	flag.Parse()
	ide := filepath.Join(*out, "antigravity", "brain")
	cli := filepath.Join(*out, "antigravity-cli", "brain")
	for _, d := range []string{filepath.Dir(ide), filepath.Dir(cli)} {
		if err := os.RemoveAll(d); err != nil {
			log.Fatal(err)
		}
	}
	logs := func(root, id string) string { return filepath.Join(root, id, ".system_generated", "logs") }

	// IDE, main conversation: live file only, ending in a partial line.
	m := ideMain()
	l := logs(ide, "00000000-0000-4000-8000-00000000a001")
	partial := lines([]step{{StepIndex: len(m.steps), Source: "USER_EXPLICIT", Type: "USER_INPUT", Status: "DONE", CreatedAt: "2026-03-02T10:30:00Z", Content: "<USER_REQUEST>\nNow add"}})[0]
	write(filepath.Join(l, "transcript_full.jsonl"), append(bytes.Join(lines(m.steps), nil), partial[:60]...))
	write(filepath.Join(l, "transcript.jsonl"), compact(m.steps))

	// IDE, interrupts.
	i := ideInterrupt()
	l = logs(ide, "00000000-0000-4000-8000-00000000a002")
	write(filepath.Join(l, "transcript_full.jsonl"), bytes.Join(lines(i.steps), nil))
	write(filepath.Join(l, "transcript.jsonl"), compact(i.steps))

	// IDE, compact transcript only: not a source.
	write(filepath.Join(logs(ide, "00000000-0000-4000-8000-00000000a003"), "transcript.jsonl"), compact(ideInterrupt().steps))

	// CLI, chunked: chunk 0 = steps 0-9 plus the first part of step 10,
	// chunk 1 = the rest of step 10 plus steps 11-19, live = steps 15-24.
	ch := cliChunked()
	ls := lines(ch.steps)
	l = logs(cli, "00000000-0000-4000-8000-00000000c001")
	split := len(ls[10]) / 2
	chunk0 := append(bytes.Join(ls[:10], nil), ls[10][:split]...)
	chunk1 := append(append([]byte{}, ls[10][split:]...), bytes.Join(ls[11:20], nil)...)
	write(filepath.Join(l, "chunks", "transcript_full", "00000000.jsonl"), chunk0)
	write(filepath.Join(l, "chunks", "transcript_full", "00000001.jsonl"), chunk1)
	write(filepath.Join(l, "chunks", "transcript", "00000000.jsonl"), compact(ch.steps[:20]))
	write(filepath.Join(l, "transcript_full.jsonl"), bytes.Join(ls[15:], nil))
	write(filepath.Join(l, "transcript.jsonl"), compact(ch.steps[15:]))

	// CLI, tool families, two steps swapped in the file.
	tl := cliTools()
	ls = lines(tl.steps)
	ls[5], ls[6] = ls[6], ls[5]
	l = logs(cli, "00000000-0000-4000-8000-00000000c002")
	write(filepath.Join(l, "transcript_full.jsonl"), bytes.Join(ls, nil))
	write(filepath.Join(l, "transcript.jsonl"), compact(tl.steps))
}
