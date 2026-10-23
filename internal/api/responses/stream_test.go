package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// sseChunks splits an upstream SSE fixture into data payloads, without [DONE].
func sseChunks(t *testing.T, path string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	var out [][]byte
	for _, ev := range strings.Split(string(raw), "\n\n") {
		ev = strings.TrimSpace(ev)
		if ev == "" {
			continue
		}
		data := strings.TrimPrefix(ev, "data: ")
		if data == "[DONE]" {
			continue
		}
		out = append(out, []byte(data))
	}
	return out
}

// runStream feeds chunks through a StreamEncoder and returns all events.
func runStream(t *testing.T, chunks [][]byte, tools ToolMap, expose bool) []Event {
	t.Helper()
	enc := NewStreamEncoder("codex-test", 42, tools, expose)
	ev := enc.Start()
	for _, c := range chunks {
		got, err := enc.Chunk(c)
		if err != nil {
			t.Fatalf("Chunk(%s): %v", c, err)
		}
		ev = append(ev, got...)
	}
	got, err := enc.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return append(ev, got...)
}

func mustJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}

func names(events []Event) []string {
	var n []string
	for _, e := range events {
		n = append(n, e.Name)
	}
	return n
}

// assemble checks the event grammar, rebuilds every output item from the
// deltas, checks them against the done events and response.completed, and
// returns the terminal response object.
func assemble(t *testing.T, events []Event) map[string]any {
	t.Helper()
	if len(events) < 3 || events[0].Name != "response.created" || events[1].Name != "response.in_progress" {
		t.Fatalf("bad envelope: %v", names(events))
	}
	terminal := events[len(events)-1].Name
	if terminal != "response.completed" && terminal != "response.incomplete" && terminal != "response.failed" {
		t.Fatalf("last event %q", terminal)
	}
	var respID string
	var items []any
	var open map[string]any // item being built
	var buf strings.Builder // text / arguments / input of the open item
	partOpen := false
	for i, e := range events {
		var ev map[string]any
		mustJSON(t, e.Data, &ev)
		if ev["type"] != e.Name {
			t.Fatalf("event name %q != type %v", e.Name, ev["type"])
		}
		if ev["sequence_number"] != float64(i) {
			t.Fatalf("event %d (%s): sequence_number %v", i, e.Name, ev["sequence_number"])
		}
		idx, _ := ev["output_index"].(float64)
		itemID, _ := ev["item_id"].(string)
		checkOpen := func() {
			t.Helper()
			if open == nil || itemID != open["id"] || int(idx) != len(items) {
				t.Fatalf("event %d %s for item %q/%v while open=%v (%d closed)", i, e.Name, itemID, idx, open, len(items))
			}
		}
		switch e.Name {
		case "response.created", "response.in_progress":
			if i > 1 {
				t.Fatalf("%s at %d", e.Name, i)
			}
			r := ev["response"].(map[string]any)
			if r["status"] != "in_progress" || len(r["output"].([]any)) != 0 {
				t.Fatalf("%s response = %v", e.Name, r)
			}
			if respID == "" {
				respID = r["id"].(string)
			} else if r["id"] != respID {
				t.Fatal("response id changed")
			}
		case "response.output_item.added":
			if open != nil || int(idx) != len(items) {
				t.Fatalf("added at %v while open=%v", idx, open)
			}
			open = ev["item"].(map[string]any)
			buf.Reset()
			switch open["type"] {
			case "message":
				if len(open["content"].([]any)) != 0 || open["status"] != "in_progress" {
					t.Fatalf("added message %v", open)
				}
			case "reasoning":
				if len(open["summary"].([]any)) != 0 {
					t.Fatalf("added reasoning %v", open)
				}
			case "function_call":
				if open["arguments"] != "" || open["status"] != "in_progress" {
					t.Fatalf("added function_call %v", open)
				}
			case "custom_tool_call":
				if open["input"] != "" || open["status"] != "in_progress" {
					t.Fatalf("added custom_tool_call %v", open)
				}
			default:
				t.Fatalf("added item %v", open)
			}
		case "response.content_part.added", "response.reasoning_summary_part.added":
			itemID = ev["item_id"].(string)
			checkOpen()
			if partOpen {
				t.Fatal("part already open")
			}
			partOpen = true
		case "response.output_text.delta", "response.reasoning_summary_text.delta",
			"response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			checkOpen()
			want := map[string]string{"response.output_text.delta": "message", "response.reasoning_summary_text.delta": "reasoning",
				"response.function_call_arguments.delta": "function_call", "response.custom_tool_call_input.delta": "custom_tool_call"}[e.Name]
			if open["type"] != want {
				t.Fatalf("%s for %v", e.Name, open["type"])
			}
			if want == "message" || want == "reasoning" {
				if !partOpen {
					t.Fatal("delta outside part")
				}
			}
			buf.WriteString(ev["delta"].(string))
		case "response.output_text.done", "response.reasoning_summary_text.done":
			checkOpen()
			if ev["text"] != buf.String() {
				t.Fatalf("%s text %q != deltas %q", e.Name, ev["text"], buf.String())
			}
		case "response.content_part.done", "response.reasoning_summary_part.done":
			checkOpen()
			if !partOpen || ev["part"].(map[string]any)["text"] != buf.String() {
				t.Fatalf("%s part %v", e.Name, ev["part"])
			}
			partOpen = false
		case "response.function_call_arguments.done":
			checkOpen()
			if ev["arguments"] != buf.String() {
				t.Fatalf("arguments %q != deltas %q", ev["arguments"], buf.String())
			}
		case "response.custom_tool_call_input.done":
			checkOpen()
			if ev["input"] != buf.String() {
				t.Fatalf("input %q != deltas %q", ev["input"], buf.String())
			}
		case "response.output_item.done":
			itemID, _ = ev["item"].(map[string]any)["id"].(string)
			checkOpen()
			if partOpen {
				t.Fatal("part left open")
			}
			item := ev["item"].(map[string]any)
			if item["id"] != open["id"] || item["type"] != open["type"] {
				t.Fatalf("done %v for %v", item, open)
			}
			var got string
			switch item["type"] {
			case "message":
				got = item["content"].([]any)[0].(map[string]any)["text"].(string)
			case "reasoning":
				got = item["summary"].([]any)[0].(map[string]any)["text"].(string)
			case "function_call":
				got = item["arguments"].(string)
			case "custom_tool_call":
				got = item["input"].(string)
			}
			if got != buf.String() || (item["status"] != nil && item["status"] != "completed") {
				t.Fatalf("done item %v, deltas %q", item, buf.String())
			}
			items = append(items, item)
			open = nil
		case "response.completed", "response.incomplete", "response.failed":
			if i != len(events)-1 || open != nil {
				t.Fatalf("%s at %d, open=%v", e.Name, i, open)
			}
			r := ev["response"].(map[string]any)
			if r["id"] != respID {
				t.Fatal("terminal response id changed")
			}
			if items == nil {
				items = []any{}
			}
			if !reflect.DeepEqual(r["output"], items) {
				t.Fatalf("response.output %v != done items %v", r["output"], items)
			}
			wantStatus := strings.TrimPrefix(e.Name, "response.")
			if r["status"] != wantStatus {
				t.Fatalf("%s status %v", e.Name, r["status"])
			}
			return r
		default:
			t.Fatalf("unexpected event %q", e.Name)
		}
	}
	t.Fatal("no terminal event")
	return nil
}

func TestStreamFixtures(t *testing.T) {
	files, _ := filepath.Glob("../../../testdata/upstream/*/*.sse")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		for _, expose := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/expose=%v", filepath.Base(f), expose), func(t *testing.T) {
				chunks := sseChunks(t, f)
				resp := assemble(t, runStream(t, chunks, ToolMap{}, expose))

				// Independently assemble the upstream stream, also as a
				// non-stream chat response for EncodeResponse.
				var text, reasoning strings.Builder
				type call struct{ id, name, args string }
				calls := map[int]*call{}
				maxIdx := -1
				finish := ""
				var usage *chatUsage
				for _, c := range chunks {
					var ch chatChunk
					mustJSON(t, c, &ch)
					if ch.Usage != nil {
						usage = ch.Usage
					}
					for _, choice := range ch.Choices {
						if choice.Delta.Content != nil {
							text.WriteString(*choice.Delta.Content)
						}
						reasoning.WriteString(pickReasoning(choice.Delta.ReasoningContent, choice.Delta.Reasoning))
						for _, tc := range choice.Delta.ToolCalls {
							i := *tc.Index
							if calls[i] == nil {
								calls[i] = &call{id: tc.ID, name: tc.Function.Name}
							}
							a, _ := argString(tc.Function.Arguments)
							calls[i].args += a
							maxIdx = max(maxIdx, i)
						}
						if choice.FinishReason != nil {
							finish = *choice.FinishReason
						}
					}
				}

				var gotText string
				for _, m := range itemsOf(resp, "message") {
					gotText += m["content"].([]any)[0].(map[string]any)["text"].(string)
				}
				if gotText != text.String() {
					t.Errorf("text = %q, want %q", gotText, text.String())
				}
				rs := itemsOf(resp, "reasoning")
				if !expose && len(rs) > 0 {
					t.Error("reasoning item with expose=false")
				}
				if expose && reasoning.Len() > 0 && (len(rs) != 1 || !strings.HasPrefix(reasoning.String(), rs[0]["summary"].([]any)[0].(map[string]any)["text"].(string))) {
					t.Errorf("reasoning items = %v", rs)
				}
				fcs := itemsOf(resp, "function_call")
				if len(fcs) != len(calls) {
					t.Fatalf("function_call items = %d, want %d", len(fcs), len(calls))
				}
				var msgCalls []any
				for i := 0; i <= maxIdx; i++ {
					if fcs[i]["call_id"] != calls[i].id || fcs[i]["name"] != calls[i].name || fcs[i]["arguments"] != calls[i].args {
						t.Errorf("call %d = %v, want %+v", i, fcs[i], calls[i])
					}
					msgCalls = append(msgCalls, map[string]any{"id": calls[i].id, "type": "function",
						"function": map[string]any{"name": calls[i].name, "arguments": calls[i].args}})
				}
				usg := resp["usage"].(map[string]any)
				if usg["output_tokens"].(float64) <= 0 || usg["input_tokens"] == 42.0 {
					t.Errorf("usage = %v (upstream usage not applied)", usg)
				}

				// response.completed equals the non-stream encoding when
				// the stream has the non-stream layout (text before tools).
				msg := map[string]any{"role": "assistant", "content": text.String(), "reasoning_content": reasoning.String()}
				if msgCalls != nil {
					msg["tool_calls"] = msgCalls
				}
				body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
				want, err := EncodeResponse(body, "codex-test", ToolMap{}, expose)
				if err != nil {
					t.Fatal(err)
				}
				var wantResp map[string]any
				mustJSON(t, want, &wantResp)
				if g, w := stripIDs(resp), stripIDs(wantResp); !reflect.DeepEqual(g, w) {
					gj, _ := json.Marshal(g)
					wj, _ := json.Marshal(w)
					t.Errorf("stream != non-stream\nstream:     %s\nnon-stream: %s", gj, wj)
				}
			})
		}
	}
}

// stripIDs removes generated ids and timestamps for comparison.
func stripIDs(r map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range r {
		if k != "id" && k != "created_at" {
			out[k] = v
		}
	}
	var items []any
	for _, it := range r["output"].([]any) {
		m := map[string]any{}
		for k, v := range it.(map[string]any) {
			if k != "id" {
				m[k] = v
			}
		}
		items = append(items, m)
	}
	out["output"] = items
	return out
}

// Tools offered to the synthetic answers: a plain function, a custom tool
// and a namespaced function.
const synthTools = `[{"type":"function","name":"fn"},{"type":"custom","name":"patch"},
  {"type":"namespace","name":"ns","tools":[{"type":"function","name":"nsfn"}]}]`

// synth is a random upstream answer rendered both as a stream and as a
// non-stream response.
type synth struct {
	reasoning, text string
	calls           []synthCall
	finish          string
	usage           *chatUsage
	usageSeparate   bool
	reasoningField  string
}

type synthCall struct{ id, name, args string }

func randText(r *rand.Rand, n int) string {
	const alphabet = "abc xyz\n\"\\é日🙂{}"
	runes := []rune(alphabet)
	var b strings.Builder
	for range n {
		b.WriteRune(runes[r.IntN(len(runes))])
	}
	return b.String()
}

func newSynth(seed uint64) synth {
	r := rand.New(rand.NewPCG(seed, 7))
	s := synth{reasoningField: []string{"reasoning_content", "reasoning"}[r.IntN(2)]}
	if r.IntN(3) > 0 {
		s.reasoning = randText(r, 1+r.IntN(40))
	}
	if r.IntN(3) > 0 {
		s.text = randText(r, 1+r.IntN(60))
	}
	n := r.IntN(4)
	if seed%4 == 1 {
		n = 3 // always cover function + custom + namespaced together
	}
	kinds := r.Perm(3)
	for i := range n {
		var name, args string
		switch kinds[i%3] {
		case 0, 2:
			name = "fn"
			if kinds[i%3] == 2 {
				name = "ns__nsfn"
			}
			switch r.IntN(6) {
			case 0:
				args = ""
			case 1:
				args = "{}"
			default:
				obj := map[string]any{"s": randText(r, r.IntN(30)), "n": r.IntN(1000), "nested": map[string]any{"list": []int{1, 2, i}}}
				b, _ := json.Marshal(obj)
				args = string(b)
			}
		case 1:
			name = "patch"
			input := "*** Begin Patch\n" + randText(r, r.IntN(40))
			var b []byte
			if r.IntN(4) == 0 {
				b, _ = json.Marshal(input) // bare JSON string
			} else {
				b, _ = json.Marshal(map[string]string{"input": input})
			}
			args = string(b)
		}
		s.calls = append(s.calls, synthCall{id: fmt.Sprintf("call_%d_%d", seed, i), name: name, args: args})
	}
	switch {
	case len(s.calls) > 0:
		s.finish = "tool_calls"
	case r.IntN(2) == 0:
		s.finish = "length"
	default:
		s.finish = "stop"
	}
	if r.IntN(5) > 0 {
		s.usage = &chatUsage{PromptTokens: 100 + r.IntN(1000), CompletionTokens: 1 + r.IntN(500)}
		if r.IntN(2) == 0 {
			c := r.IntN(100)
			s.usage.PromptTokensDetails = &struct {
				CachedTokens *int `json:"cached_tokens"`
			}{&c}
		}
		if r.IntN(2) == 0 {
			rt := r.IntN(50)
			s.usage.CompletionTokensDetails = &struct {
				ReasoningTokens *int `json:"reasoning_tokens"`
			}{&rt}
		}
		s.usageSeparate = r.IntN(2) == 0
	}
	return s
}

// split cuts s into 1..n random non-empty pieces on rune boundaries.
func split(r *rand.Rand, str string, n int) []string {
	s := []rune(str)
	var out []string
	for len(s) > 0 && len(out) < n-1 {
		k := 1 + r.IntN(len(s))
		out = append(out, string(s[:k]))
		s = s[k:]
	}
	if len(s) > 0 {
		out = append(out, string(s))
	}
	return out
}

func (s synth) nonStream() []byte {
	msg := map[string]any{"role": "assistant", "content": nil}
	if s.text != "" {
		msg["content"] = s.text
	}
	if s.reasoning != "" {
		msg[s.reasoningField] = s.reasoning
	}
	var calls []any
	for _, c := range s.calls {
		calls = append(calls, map[string]any{"id": c.id, "type": "function", "function": map[string]any{"name": c.name, "arguments": c.args}})
	}
	if calls != nil {
		msg["tool_calls"] = calls
	}
	resp := map[string]any{"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": s.finish}}}
	if s.usage != nil {
		resp["usage"] = s.usage
	}
	b, _ := json.Marshal(resp)
	return b
}

func (s synth) chunks(seed uint64) [][]byte {
	r := rand.New(rand.NewPCG(seed, 99))
	var deltas []map[string]any
	for _, p := range split(r, s.reasoning, 5) {
		d := map[string]any{s.reasoningField: p}
		if r.IntN(2) == 0 {
			d["content"] = "" // Ollama style
		}
		deltas = append(deltas, d)
	}
	for _, p := range split(r, s.text, 6) {
		deltas = append(deltas, map[string]any{"content": p})
	}

	// Fragment every call; the first fragment carries id and name.
	type frag struct {
		call int
		body map[string]any
	}
	queues := make([][]frag, len(s.calls))
	for i, c := range s.calls {
		pieces := split(r, c.args, 4)
		if r.IntN(2) == 0 || len(pieces) == 0 {
			pieces = append([]string{""}, pieces...) // NVIDIA: empty arguments first
		}
		for j, p := range pieces {
			fn := map[string]any{"arguments": p}
			tc := map[string]any{"index": i, "function": fn}
			if j == 0 {
				tc["id"] = c.id
				tc["type"] = "function"
				fn["name"] = c.name
			}
			queues[i] = append(queues[i], frag{i, tc})
		}
	}
	// Interleave: calls start in index order, fragments then interleave.
	started := 0
	var toolDeltas [][]any
	for {
		var ready []int
		for i := range queues {
			if len(queues[i]) > 0 && i <= started {
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			break
		}
		i := ready[r.IntN(len(ready))]
		f := queues[i][0]
		queues[i] = queues[i][1:]
		if i == started {
			started++
		}
		// Sometimes pack several calls into one chunk (minicpm style).
		if n := len(toolDeltas); n > 0 && r.IntN(3) == 0 && !containsCall(toolDeltas[n-1], i) {
			toolDeltas[n-1] = append(toolDeltas[n-1], f.body)
		} else {
			toolDeltas = append(toolDeltas, []any{f.body})
		}
	}
	for _, td := range toolDeltas {
		deltas = append(deltas, map[string]any{"tool_calls": td})
	}

	var out [][]byte
	for i, d := range deltas {
		if i == 0 {
			d["role"] = "assistant"
		}
		out = append(out, chunkJSON(d, nil, nil))
	}
	finish := s.finish
	if s.usage != nil && !s.usageSeparate {
		out = append(out, chunkJSON(map[string]any{}, &finish, s.usage))
	} else {
		out = append(out, chunkJSON(map[string]any{}, &finish, nil))
		if s.usage != nil {
			b, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": s.usage})
			out = append(out, b)
		}
	}
	return out
}

func containsCall(list []any, i int) bool {
	for _, x := range list {
		if x.(map[string]any)["index"] == i {
			return true
		}
	}
	return false
}

func chunkJSON(delta map[string]any, finish *string, usage *chatUsage) []byte {
	c := map[string]any{"object": "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	if usage != nil {
		c["usage"] = usage
	}
	b, _ := json.Marshal(c)
	return b
}

func TestStreamMatchesNonStream(t *testing.T) {
	tools := toolsFor(t, synthTools)
	kinds := map[string]int{}
	for seed := uint64(0); seed < 400; seed++ {
		s := newSynth(seed)
		for _, c := range s.calls {
			kinds[c.name]++
		}
		expose := seed%3 != 0
		chunks := s.chunks(seed)

		got := assemble(t, runStream(t, chunks, tools, expose))
		body, err := EncodeResponse(s.nonStream(), "codex-test", tools, expose)
		if err != nil {
			t.Fatalf("seed %d: EncodeResponse: %v", seed, err)
		}
		var want map[string]any
		mustJSON(t, body, &want)
		if s.usage == nil {
			// Without upstream usage the stream reports its own estimates.
			delete(got, "usage")
			delete(want, "usage")
		}
		if g, w := stripIDs(got), stripIDs(want); !reflect.DeepEqual(g, w) {
			gj, _ := json.Marshal(g)
			wj, _ := json.Marshal(w)
			t.Fatalf("seed %d mismatch\nstream: %s\nnon-stream: %s\nchunks: %s", seed, gj, wj, bytes.Join(chunks, []byte("\n")))
		}
	}
	for _, k := range []string{"fn", "patch", "ns__nsfn"} {
		if kinds[k] < 50 {
			t.Errorf("tool %s covered only %d times", k, kinds[k])
		}
	}
}

func TestStreamCustomAndNamespace(t *testing.T) {
	tools := toolsFor(t, synthTools)
	tc := func(idx int, id, name, args string) []byte {
		return chunkJSON(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "id": id, "function": map[string]any{"name": name, "arguments": args}}}}, nil, nil)
	}
	chunks := [][]byte{
		chunkJSON(map[string]any{"content": "a"}, nil, nil),
		tc(0, "c0", "patch", `{"inp`),
		tc(1, "c1", "ns__nsfn", `{"q":`),
		tc(0, "", "", `ut":"x\ny"}`),
		tc(1, "", "", `1}`),
		chunkJSON(map[string]any{"content": "b"}, ptr("tool_calls"), nil),
	}
	ev := runStream(t, chunks, tools, true)
	resp := assemble(t, ev)
	if got := itemTypes(resp); !reflect.DeepEqual(got, []string{"message", "custom_tool_call", "message", "function_call"}) {
		t.Fatalf("types = %v (%v)", got, names(ev))
	}
	o := resp["output"].([]any)
	if c := o[1].(map[string]any); c["input"] != "x\ny" || c["call_id"] != "c0" || c["name"] != "patch" {
		t.Errorf("custom = %v", c)
	}
	if f := o[3].(map[string]any); f["name"] != "nsfn" || f["namespace"] != "ns" || f["arguments"] != `{"q":1}` {
		t.Errorf("function = %v", f)
	}
	// The custom call's input arrives as a single delta.
	n := 0
	for _, e := range ev {
		if e.Name == "response.custom_tool_call_input.delta" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("custom input deltas = %d", n)
	}
}

func TestStreamErrors(t *testing.T) {
	tools := toolsFor(t, synthTools)
	tc := func(idx int, id, name, args string) []byte {
		return chunkJSON(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "id": id, "function": map[string]any{"name": name, "arguments": args}}}}, nil, nil)
	}
	t.Run("invalid args", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		enc.Start()
		if _, err := enc.Chunk(tc(0, "a", "fn", `{"x":`)); err != nil {
			t.Fatal(err)
		}
		if _, err := enc.Finish(); err == nil {
			t.Fatal("want error for truncated JSON")
		}
	})
	t.Run("array args buffered", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		_, _ = enc.Chunk(tc(0, "a", "fn", `{}`))
		_, _ = enc.Chunk(tc(1, "b", "fn", `[1]`))
		if _, err := enc.Finish(); err == nil {
			t.Fatal("want error for non-object buffered args")
		}
	})
	t.Run("custom without input", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		_, _ = enc.Chunk(tc(0, "a", "patch", `{"patch":"x"}`))
		if _, err := enc.Finish(); err == nil {
			t.Fatal("want error for custom args without input")
		}
	})
	t.Run("custom invalid when closed by text", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		_, _ = enc.Chunk(tc(0, "a", "patch", `{"input":`))
		if _, err := enc.Chunk(chunkJSON(map[string]any{"content": "x"}, nil, nil)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("oversized buffered args", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		_, _ = enc.Chunk(tc(0, "a", "fn", `{}`))
		piece := strings.Repeat("a", 256<<10)
		var err error
		if _, err = enc.Chunk(tc(1, "b", "fn", `{"s":"`)); err != nil {
			t.Fatal(err)
		}
		for range 5 {
			if _, err = enc.Chunk(tc(1, "", "", piece)); err != nil {
				break
			}
		}
		if err == nil {
			t.Fatal("want size cap error")
		}
	})
	t.Run("assembled cap", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		piece := strings.Repeat("a", 1<<20)
		var err error
		for range 10 {
			if _, err = enc.Chunk(chunkJSON(map[string]any{"content": piece}, nil, nil)); err != nil {
				break
			}
		}
		if err == nil {
			t.Fatal("want assembled cap error")
		}
	})
	t.Run("invalid chunk", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		if _, err := enc.Chunk([]byte(`{"choices":`)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("upstream error", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		if _, err := enc.Chunk([]byte(`{"error":{"message":"boom"}}`)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("missing name", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		if _, err := enc.Chunk(tc(0, "a", "", `{}`)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("after finish", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		if _, err := enc.Finish(); err != nil {
			t.Fatal(err)
		}
		if _, err := enc.Chunk(chunkJSON(map[string]any{"content": "x"}, nil, nil)); err == nil {
			t.Fatal("want error")
		}
		if _, err := enc.Finish(); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("empty choices without usage", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, tools, true)
		ev, err := enc.Chunk([]byte(`{"choices":[]}`))
		if err != nil || len(ev) != 0 {
			t.Fatalf("ev=%v err=%v", ev, err)
		}
	})
}

func TestStreamReasoningOnly(t *testing.T) {
	chunks := [][]byte{
		chunkJSON(map[string]any{"reasoning": "hmm", "content": ""}, nil, nil),
		chunkJSON(map[string]any{}, ptr("length"), &chatUsage{PromptTokens: 5, CompletionTokens: 9}),
	}
	for _, expose := range []bool{true, false} {
		ev := runStream(t, chunks, ToolMap{}, expose)
		resp := assemble(t, ev)
		out := resp["output"].([]any)
		last := out[len(out)-1].(map[string]any)
		if last["type"] != "message" || last["content"].([]any)[0].(map[string]any)["text"] != "" {
			t.Errorf("expose=%v: output = %v", expose, out)
		}
		if rs := itemsOf(resp, "reasoning"); (len(rs) == 1) != expose {
			t.Errorf("expose=%v: reasoning = %v", expose, rs)
		}
		if ev[len(ev)-1].Name != "response.incomplete" || resp["status"] != "incomplete" ||
			!reflect.DeepEqual(resp["incomplete_details"], map[string]any{"reason": "max_output_tokens"}) {
			t.Errorf("terminal = %s %v", ev[len(ev)-1].Name, resp)
		}
	}
}

func TestStreamLateReasoningAndText(t *testing.T) {
	chunks := [][]byte{
		chunkJSON(map[string]any{"content": "a"}, nil, nil),
		chunkJSON(map[string]any{"reasoning_content": "late"}, nil, nil),
		chunkJSON(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "x", "function": map[string]any{"name": "f", "arguments": ""}}}}, nil, nil),
		chunkJSON(map[string]any{"content": "b"}, nil, nil),
	}
	resp := assemble(t, runStream(t, chunks, ToolMap{}, true))
	if got := itemTypes(resp); !reflect.DeepEqual(got, []string{"message", "function_call", "message"}) {
		t.Errorf("types = %v", got)
	}
	if fc := itemsOf(resp, "function_call")[0]; fc["arguments"] != "{}" {
		t.Errorf("empty arguments = %v", fc)
	}
}

func TestFail(t *testing.T) {
	enc := NewStreamEncoder("m", 1, ToolMap{}, true)
	start := enc.Start()
	ev1, _ := enc.Chunk(chunkJSON(map[string]any{"content": "partial"}, nil, nil))
	ev := enc.Fail("rate_limit_exceeded", "gone")
	all := append(append(start, ev1...), ev...)
	if len(ev) != 1 || ev[0].Name != "response.failed" {
		t.Fatalf("Fail = %v", names(ev))
	}
	var f struct {
		Type           string `json:"type"`
		SequenceNumber int    `json:"sequence_number"`
		Response       struct {
			ID     string            `json:"id"`
			Status string            `json:"status"`
			Error  map[string]string `json:"error"`
			Output []any             `json:"output"`
		} `json:"response"`
	}
	mustJSON(t, ev[0].Data, &f)
	if f.Type != "response.failed" || f.SequenceNumber != len(all)-1 || f.Response.Status != "failed" ||
		!reflect.DeepEqual(f.Response.Error, map[string]string{"code": "rate_limit_exceeded", "message": "gone"}) || f.Response.ID == "" {
		t.Errorf("failed event = %s", ev[0].Data)
	}
	if got := string(ev[0].Bytes()); !strings.HasPrefix(got, "event: response.failed\ndata: {") || !strings.HasSuffix(got, "}\n\n") {
		t.Errorf("Bytes = %q", got)
	}
	if _, err := enc.Finish(); err == nil {
		t.Error("Finish after Fail must error")
	}
}

// TestShapesMatchCapture compares event names and field names with the
// Ollama-produced stream that Codex 0.154.0 accepted and rendered.
func TestShapesMatchCapture(t *testing.T) {
	c := loadCapture(t, "0003")
	type shape map[string][]string // event name -> sorted keys (with nested item./part. keys)
	keysOf := func(data []byte) []string {
		var m map[string]json.RawMessage
		mustJSON(t, data, &m)
		var keys []string
		for k, v := range m {
			keys = append(keys, k)
			if k == "item" || k == "part" {
				var sub map[string]json.RawMessage
				mustJSON(t, v, &sub)
				for sk := range sub {
					keys = append(keys, k+"."+sk)
				}
			}
		}
		sort.Strings(keys)
		return keys
	}
	capShapes := shape{}
	var capNames []string
	body := strings.ReplaceAll(c.ResponseBody, "\r\n", "\n")
	for _, raw := range strings.Split(body, "\n\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		lines := strings.SplitN(raw, "\n", 2)
		name := strings.TrimPrefix(lines[0], "event: ")
		data := []byte(strings.TrimPrefix(lines[1], "data: "))
		if _, ok := capShapes[name]; !ok {
			capNames = append(capNames, name)
		}
		// Union over all events of the same name (reasoning + message items).
		capShapes[name] = union(capShapes[name], keysOf(data))
	}

	ours := shape{}
	ev := runStream(t, sseChunks(t, "../../../testdata/upstream/ollama/minicpm-v4.6_stream.sse"), ToolMap{}, true)
	for _, e := range ev {
		ours[e.Name] = union(ours[e.Name], keysOf(e.Data))
	}
	// Fields Ollama sends that Codex does not read: opaque reasoning state
	// (we have none to send) and logprobs inside parts.
	ignore := []string{"item.encrypted_content", "part.logprobs"}
	for _, name := range capNames {
		got, ok := ours[name]
		if !ok {
			t.Errorf("event %q (seen in capture) not produced", name)
			continue
		}
		for _, k := range capShapes[name] {
			if !slices.Contains(got, k) && !slices.Contains(ignore, k) {
				t.Errorf("%s: field %q missing (ours: %v)", name, k, got)
			}
		}
	}
	// The response object carries what Codex parses in response.completed.
	var done struct {
		Response map[string]json.RawMessage `json:"response"`
	}
	mustJSON(t, ev[len(ev)-1].Data, &done)
	for _, k := range []string{"id", "object", "created_at", "status", "model", "output", "usage", "error", "incomplete_details", "parallel_tool_calls", "tool_choice", "tools"} {
		if _, ok := done.Response[k]; !ok {
			t.Errorf("response.completed lacks %q", k)
		}
	}
	var usage map[string]json.RawMessage
	mustJSON(t, done.Response["usage"], &usage)
	for _, k := range []string{"input_tokens", "input_tokens_details", "output_tokens", "output_tokens_details", "total_tokens"} {
		if _, ok := usage[k]; !ok {
			t.Errorf("usage lacks %q", k)
		}
	}
}

func union(a, b []string) []string {
	for _, k := range b {
		if !slices.Contains(a, k) {
			a = append(a, k)
		}
	}
	sort.Strings(a)
	return a
}

func ptr[T any](v T) *T { return &v }
