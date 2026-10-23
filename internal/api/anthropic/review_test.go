package anthropic

import (
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// manyKeys is a request body with n distinct top-level keys after the
// required members, and a late duplicate "model" that must win.
func manyKeys(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]`)
	for i := range n {
		fmt.Fprintf(&b, `,"k%d":1`, i)
	}
	b.WriteString(`,"model":"m2"}`)
	return []byte(b.String())
}

// decodeGrowth is how many times longer one decode of large takes than reps
// decodes of small: the fastest of several runs of each, interleaved and each
// after a GC, so a hiccup (another process, a collection) in some runs does
// not count. Repeating small makes both sides the same amount of work for a
// linear decoder, so neither side is a short, noisy measurement.
func decodeGrowth(small, large []byte, reps int) float64 {
	bestS, bestL := time.Duration(1<<63-1), time.Duration(1<<63-1)
	for range 7 {
		runtime.GC()
		start := time.Now()
		for range reps {
			DecodeRequest(small)
		}
		bestS = min(bestS, time.Since(start))
		runtime.GC()
		start = time.Now()
		DecodeRequest(large)
		bestL = min(bestL, time.Since(start))
	}
	return float64(bestL) / float64(bestS)
}

// A body with very many distinct top-level keys decodes in linear time. The
// check compares growth rather than wall-clock time, so it holds on slow
// machines and under -race: one body with 64x the keys costs about 1x-3x of
// 64 small decodes when linear (cache effects) and about 64x when quadratic;
// the bound of 16x leaves 4x room for noise either way.
func TestDecodeManyTopLevelKeys(t *testing.T) {
	const small, n = 3000, 192000
	body := manyKeys(n)
	ratio := decodeGrowth(manyKeys(small), body, n/small)
	t.Logf("64x the keys took %.1fx the time of 64 small decodes", ratio)
	if ratio > 16 {
		t.Fatalf("64x the keys took %.1fx the time of 64 small decodes: not linear (quadratic is about 64x)", ratio)
	}
	out, info, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "m2" || len(info.Downgraded) != n || !strings.Contains(string(out), `"model":"m2"`) {
		t.Fatalf("model %q, %d downgrades", info.Model, len(info.Downgraded))
	}
}

func TestToolStrictIsReported(t *testing.T) {
	for _, tc := range []struct {
		tools string
		want  bool
	}{
		{`[{"name":"a","input_schema":{},"strict":true}]`, true},
		{`[{"name":"a","input_schema":{},"strict":false}]`, false},
		{`[{"name":"a","input_schema":{}}]`, false},
	} {
		body := `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"tools":` + tc.tools + `}`
		out, info, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(info.Downgraded, "tools.strict"); got != tc.want {
			t.Errorf("%s: downgraded %v", tc.tools, info.Downgraded)
		}
		if strings.Contains(string(out), "strict") {
			t.Errorf("%s: strict sent upstream: %s", tc.tools, out)
		}
	}
}

func toolChunk(idx int, id, name, args string) []byte {
	return fmt.Appendf(nil, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%q,"function":{"name":%q,"arguments":%q}}]}}]}`,
		idx, id, name, args)
}

func feed(chunks ...[]byte) (*StreamEncoder, []Event, error) {
	enc := NewStreamEncoder("m", 1, true)
	evs := enc.Start()
	for _, c := range chunks {
		got, err := enc.Chunk(c)
		if err != nil {
			return enc, evs, err
		}
		evs = append(evs, got...)
	}
	got, err := enc.Finish()
	return enc, append(evs, got...), err
}

func TestStreamToolMetadataLimits(t *testing.T) {
	var many [][]byte
	for i := range maxToolCalls + 1 {
		many = append(many, toolChunk(i, fmt.Sprint("c", i), "f", "{}"))
	}
	if _, _, err := feed(many[:maxToolCalls]...); err != nil {
		t.Fatalf("%d calls: %v", maxToolCalls, err)
	}
	if _, _, err := feed(many...); err == nil || !strings.Contains(err.Error(), "tool calls") {
		t.Fatalf("%d calls: err = %v", maxToolCalls+1, err)
	}
	long := strings.Repeat("x", maxToolMeta+1)
	if _, _, err := feed(toolChunk(0, "c", long, "{}")); err == nil {
		t.Fatal("long name accepted")
	}
	if _, _, err := feed(toolChunk(0, long, "f", "{}")); err == nil {
		t.Fatal("long id accepted")
	}
	enc, _, err := feed(toolChunk(0, "abc", "fn", "{}"))
	if err != nil || enc.total != len("abc")+len("fn")+len("{}") {
		t.Fatalf("assembled %d, err %v", enc.total, err)
	}
}

func TestStreamToolNameAndIDChanges(t *testing.T) {
	if _, _, err := feed(toolChunk(0, "c1", "f", `{"a":`), toolChunk(0, "c1", "f", `1}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := feed(toolChunk(0, "c1", "f", `{"a":`), toolChunk(0, "c1", "g", `1}`)); err == nil ||
		!strings.Contains(err.Error(), "name changed") {
		t.Fatalf("name change: %v", err)
	}
	if _, _, err := feed(toolChunk(0, "c1", "f", `{"a":`), toolChunk(0, "c2", "", `1}`)); err == nil ||
		!strings.Contains(err.Error(), "id changed") {
		t.Fatalf("id change: %v", err)
	}
	// A late first id is adopted while the call is still buffered.
	_, evs, err := feed(toolChunk(0, "c0", "f", "{}"), toolChunk(1, "", "g", `{"a":`), toolChunk(1, "late", "", `1}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.Name == "content_block_start" && strings.Contains(string(e.Data), `"id":"late"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("late id not adopted")
	}
}

func TestEncodeResponseRefusal(t *testing.T) {
	for _, tc := range []struct{ msg, want string }{
		{`{"role":"assistant","content":null,"refusal":"no"}`, `[{"type":"text","text":"no"}]`},
		{`{"role":"assistant","content":"hm","refusal":"no"}`, `[{"type":"text","text":"hm"},{"type":"text","text":"no"}]`},
		{`{"role":"assistant","content":"hm","refusal":null}`, `[{"type":"text","text":"hm"}]`},
	} {
		chat := `{"choices":[{"index":0,"message":` + tc.msg + `,"finish_reason":"stop"}]}`
		out, err := EncodeResponse([]byte(chat), "m", true)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Content    json.RawMessage `json:"content"`
			StopReason string          `json:"stop_reason"`
		}
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		if string(m.Content) != tc.want || m.StopReason != "end_turn" {
			t.Errorf("%s: %s", tc.msg, out)
		}
	}
}

func TestStreamRefusal(t *testing.T) {
	_, evs, err := feed(
		[]byte(`{"choices":[{"index":0,"delta":{"content":null,"refusal":"can't"}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{"refusal":" help"},"finish_reason":"stop"}]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	var names, text []string
	for _, e := range evs {
		names = append(names, e.Name)
		if e.Name == "content_block_delta" {
			var d struct {
				Delta struct{ Type, Text string } `json:"delta"`
			}
			json.Unmarshal(e.Data, &d)
			if d.Delta.Type != "text_delta" {
				t.Fatalf("delta %s", e.Data)
			}
			text = append(text, d.Delta.Text)
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"}
	if !slices.Equal(names, want) || strings.Join(text, "") != "can't help" {
		t.Fatalf("events %v, text %q", names, text)
	}
	if !strings.Contains(string(evs[1].Data), `"type":"text"`) || !strings.Contains(string(evs[5].Data), `"end_turn"`) {
		t.Fatalf("block %s, delta %s", evs[1].Data, evs[5].Data)
	}
}

func TestStreamDroppedLateReasoning(t *testing.T) {
	early := []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`)
	text := []byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	late := []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"more"}}]}`)
	if enc, _, err := feed(early, text); err != nil || len(enc.Dropped()) != 0 {
		t.Fatalf("dropped %v, %v", enc.Dropped(), err)
	}
	if enc, _, err := feed(early, text, late, late); err != nil || !slices.Equal(enc.Dropped(), []string{DroppedLateReasoning}) {
		t.Fatalf("dropped %v, %v", enc.Dropped(), err)
	}
	hidden := NewStreamEncoder("m", 1, false)
	hidden.Chunk(text)
	hidden.Chunk(late)
	if len(hidden.Dropped()) != 0 {
		t.Fatalf("hidden: dropped %v", hidden.Dropped())
	}
}
