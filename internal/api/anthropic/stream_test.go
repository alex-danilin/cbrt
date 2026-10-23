package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
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
func runStream(t *testing.T, chunks [][]byte, expose bool) []Event {
	t.Helper()
	enc := NewStreamEncoder("claude-test", 42, expose)
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

// assemble checks the event grammar and rebuilds the final message.
func assemble(t *testing.T, events []Event) map[string]any {
	t.Helper()
	if len(events) < 3 || events[0].Name != "message_start" ||
		events[len(events)-2].Name != "message_delta" || events[len(events)-1].Name != "message_stop" {
		t.Fatalf("bad envelope: %v", names(events))
	}
	var start struct {
		Message map[string]any `json:"message"`
	}
	mustJSON(t, events[0].Data, &start)
	msg := start.Message
	var content []any
	var partial strings.Builder
	open := -1
	for _, e := range events[1 : len(events)-2] {
		var ev struct {
			Type         string         `json:"type"`
			Index        int            `json:"index"`
			ContentBlock map[string]any `json:"content_block"`
			Delta        map[string]any `json:"delta"`
		}
		mustJSON(t, e.Data, &ev)
		if ev.Type != e.Name {
			t.Fatalf("event name %q != type %q", e.Name, ev.Type)
		}
		switch e.Name {
		case "content_block_start":
			if open != -1 || ev.Index != len(content) {
				t.Fatalf("start index %d with open=%d, have %d blocks", ev.Index, open, len(content))
			}
			open = ev.Index
			content = append(content, ev.ContentBlock)
			partial.Reset()
		case "content_block_delta":
			if ev.Index != open {
				t.Fatalf("delta for %d while %d open", ev.Index, open)
			}
			b := content[open].(map[string]any)
			switch ev.Delta["type"] {
			case "text_delta":
				requireType(t, b, "text")
				b["text"] = b["text"].(string) + ev.Delta["text"].(string)
			case "thinking_delta":
				requireType(t, b, "thinking")
				b["thinking"] = b["thinking"].(string) + ev.Delta["thinking"].(string)
			case "input_json_delta":
				requireType(t, b, "tool_use")
				partial.WriteString(ev.Delta["partial_json"].(string))
			default:
				t.Fatalf("unknown delta %v", ev.Delta)
			}
		case "content_block_stop":
			if ev.Index != open {
				t.Fatalf("stop for %d while %d open", ev.Index, open)
			}
			b := content[open].(map[string]any)
			if b["type"] == "tool_use" {
				s := partial.String()
				if s == "" {
					s = "{}"
				}
				var input map[string]any
				if err := json.Unmarshal([]byte(s), &input); err != nil {
					t.Fatalf("tool input %q: %v", s, err)
				}
				b["input"] = input
			}
			open = -1
		default:
			t.Fatalf("unexpected event %q", e.Name)
		}
	}
	if open != -1 {
		t.Fatal("block left open")
	}
	var md struct {
		Delta map[string]any `json:"delta"`
		Usage map[string]any `json:"usage"`
	}
	mustJSON(t, events[len(events)-2].Data, &md)
	msg["content"] = content
	msg["stop_reason"] = md.Delta["stop_reason"]
	msg["stop_sequence"] = md.Delta["stop_sequence"]
	msg["usage"] = md.Usage
	return msg
}

func requireType(t *testing.T, b map[string]any, typ string) {
	t.Helper()
	if b["type"] != typ {
		t.Fatalf("delta for %s block %v", typ, b)
	}
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

func blocksOf(msg map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, b := range msg["content"].([]any) {
		if bm := b.(map[string]any); bm["type"] == typ {
			out = append(out, bm)
		}
	}
	return out
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
				msg := assemble(t, runStream(t, chunks, expose))

				// Independently assemble the upstream stream.
				var text, reasoning strings.Builder
				type call struct{ id, name, args string }
				calls := map[int]*call{}
				maxIdx := -1
				for _, c := range chunks {
					var ch chatChunk
					mustJSON(t, c, &ch)
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
					}
				}

				var gotText string
				for _, b := range blocksOf(msg, "text") {
					gotText += b["text"].(string)
				}
				if gotText != text.String() {
					t.Errorf("text = %q, want %q", gotText, text.String())
				}
				thinking := blocksOf(msg, "thinking")
				if !expose && len(thinking) > 0 {
					t.Error("thinking block with expose=false")
				}
				if expose && reasoning.Len() > 0 && (len(thinking) != 1 || !strings.HasPrefix(reasoning.String(), thinking[0]["thinking"].(string))) {
					t.Errorf("thinking blocks = %v", thinking)
				}
				tools := blocksOf(msg, "tool_use")
				if len(tools) != len(calls) {
					t.Fatalf("tool_use blocks = %d, want %d", len(tools), len(calls))
				}
				for i := 0; i <= maxIdx; i++ {
					var want map[string]any
					mustJSON(t, []byte(calls[i].args), &want)
					if tools[i]["id"] != calls[i].id || tools[i]["name"] != calls[i].name || !reflect.DeepEqual(tools[i]["input"], want) {
						t.Errorf("tool %d = %v, want %+v", i, tools[i], calls[i])
					}
				}
				wantStop := "end_turn"
				if len(calls) > 0 {
					wantStop = "tool_use"
				}
				if msg["stop_reason"] != wantStop || msg["stop_sequence"] != nil {
					t.Errorf("stop_reason = %v, want %s", msg["stop_reason"], wantStop)
				}
				usage := msg["usage"].(map[string]any)
				if usage["output_tokens"].(float64) <= 0 || usage["input_tokens"] == 42.0 {
					t.Errorf("usage = %v (upstream usage not applied)", usage)
				}
			})
		}
	}
}

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
	for i := range r.IntN(4) {
		var args string
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
		s.calls = append(s.calls, synthCall{id: fmt.Sprintf("call_%d_%d", seed, i), name: fmt.Sprintf("fn_%d", i), args: args})
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
	for seed := uint64(0); seed < 300; seed++ {
		s := newSynth(seed)
		expose := seed%3 != 0
		chunks := s.chunks(seed)

		got := assemble(t, runStream(t, chunks, expose))
		body, err := EncodeResponse(s.nonStream(), "claude-test", expose)
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
		delete(got, "id")
		delete(want, "id")
		if !reflect.DeepEqual(got, want) {
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(want)
			t.Fatalf("seed %d mismatch\nstream: %s\nnon-stream: %s\nchunks: %s", seed, g, w, bytes.Join(chunks, []byte("\n")))
		}
	}
}

func TestStreamErrors(t *testing.T) {
	tc := func(idx int, id, name, args string) []byte {
		return chunkJSON(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "id": id, "function": map[string]any{"name": name, "arguments": args}}}}, nil, nil)
	}
	t.Run("invalid args", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, true)
		enc.Start()
		if _, err := enc.Chunk(tc(0, "a", "f", `{"x":`)); err != nil {
			t.Fatal(err)
		}
		if _, err := enc.Finish(); err == nil {
			t.Fatal("want error for truncated JSON")
		}
	})
	t.Run("array args", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, true)
		_, _ = enc.Chunk(tc(0, "a", "f", `{}`))
		_, _ = enc.Chunk(tc(1, "b", "g", `[1]`))
		if _, err := enc.Finish(); err == nil {
			t.Fatal("want error for non-object buffered args")
		}
	})
	t.Run("oversized buffered args", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, true)
		_, _ = enc.Chunk(tc(0, "a", "f", `{}`))
		piece := strings.Repeat("a", 256<<10)
		var err error
		if _, err = enc.Chunk(tc(1, "b", "g", `{"s":"`)); err != nil {
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
	t.Run("invalid chunk", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, true)
		if _, err := enc.Chunk([]byte(`{"choices":`)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("missing name", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, true)
		if _, err := enc.Chunk(tc(0, "a", "", `{}`)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("empty choices without usage", func(t *testing.T) {
		enc := NewStreamEncoder("m", 1, true)
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
		msg := assemble(t, runStream(t, chunks, expose))
		content := msg["content"].([]any)
		last := content[len(content)-1].(map[string]any)
		if last["type"] != "text" || last["text"] != "" {
			t.Errorf("expose=%v: content = %v", expose, content)
		}
		if th := blocksOf(msg, "thinking"); (len(th) == 1) != expose {
			t.Errorf("expose=%v: thinking = %v", expose, th)
		}
		if msg["stop_reason"] != "max_tokens" {
			t.Errorf("stop_reason = %v", msg["stop_reason"])
		}
	}
}

func TestStreamLateReasoningAndText(t *testing.T) {
	chunks := [][]byte{
		chunkJSON(map[string]any{"content": "a"}, nil, nil),
		chunkJSON(map[string]any{"reasoning_content": "late"}, nil, nil),
		chunkJSON(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "x", "function": map[string]any{"name": "f", "arguments": "{}"}}}}, nil, nil),
		chunkJSON(map[string]any{"content": "b"}, nil, nil),
	}
	msg := assemble(t, runStream(t, chunks, true))
	var types []string
	for _, b := range msg["content"].([]any) {
		types = append(types, b.(map[string]any)["type"].(string))
	}
	if !reflect.DeepEqual(types, []string{"text", "tool_use", "text"}) || msg["stop_reason"] != "tool_use" {
		t.Errorf("types = %v stop = %v", types, msg["stop_reason"])
	}
}

func TestFail(t *testing.T) {
	enc := NewStreamEncoder("m", 1, true)
	ev := enc.Fail("overloaded_error", "gone")
	if len(ev) != 1 || ev[0].Name != "error" ||
		string(ev[0].Data) != `{"type":"error","error":{"type":"overloaded_error","message":"gone"}}` {
		t.Errorf("Fail = %v", ev)
	}
	if got := string(ev[0].Bytes()); !strings.HasPrefix(got, "event: error\ndata: {") || !strings.HasSuffix(got, "}\n\n") {
		t.Errorf("Bytes = %q", got)
	}
}

func ptr[T any](v T) *T { return &v }
