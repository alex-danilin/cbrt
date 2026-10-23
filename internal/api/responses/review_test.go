package responses

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
	b.WriteString(`{"model":"m","input":"hi"`)
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
	if info.Model != "m2" || len(info.Downgraded) != n || info.Downgraded[0] != "k0" {
		t.Fatalf("model %q, %d downgrades", info.Model, len(info.Downgraded))
	}
	if !strings.Contains(string(out), `"model":"m2"`) {
		t.Fatalf("chat body %.200s", out)
	}
}

func TestTopLevelIndexKeepsFirstOrderLastValue(t *testing.T) {
	var top topLevel
	for i := range 40 {
		top.set(fmt.Sprint("k", i%20), []byte(fmt.Sprint(i)))
	}
	if len(top.members) != 20 || top.members[0].key != "k0" || top.members[19].key != "k19" {
		t.Fatalf("members %v", top.members)
	}
	if string(top.get("k3")) != "23" || top.get("zz") != nil {
		t.Fatalf("get k3 = %s", top.get("k3"))
	}
}

func TestFunctionStrictIsReported(t *testing.T) {
	for _, tc := range []struct {
		tools string
		want  bool
	}{
		{`[{"type":"function","name":"a","strict":true}]`, true},
		{`[{"type":"namespace","name":"n","tools":[{"type":"function","name":"a","strict":true}]}]`, true},
		{`[{"type":"function","name":"a","strict":false}]`, false},
		{`[{"type":"function","name":"a"}]`, false},
		{`[{"type":"function","name":"a","strict":null}]`, false},
	} {
		body := `{"model":"m","input":"hi","tools":` + tc.tools + `}`
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

// feed runs chunks through a new encoder and returns the first error.
func feed(chunks ...[]byte) (*StreamEncoder, error) {
	enc := NewStreamEncoder("m", 1, ToolMap{}, true)
	enc.Start()
	for _, c := range chunks {
		if _, err := enc.Chunk(c); err != nil {
			return enc, err
		}
	}
	_, err := enc.Finish()
	return enc, err
}

func TestStreamToolMetadataLimits(t *testing.T) {
	var many [][]byte
	for i := range maxToolCalls + 1 {
		many = append(many, toolChunk(i, fmt.Sprint("c", i), "f", "{}"))
	}
	if _, err := feed(many[:maxToolCalls]...); err != nil {
		t.Fatalf("%d calls: %v", maxToolCalls, err)
	}
	if _, err := feed(many...); err == nil || !strings.Contains(err.Error(), "tool calls") {
		t.Fatalf("%d calls: err = %v", maxToolCalls+1, err)
	}
	long := strings.Repeat("x", maxToolMeta+1)
	if _, err := feed(toolChunk(0, "c", long, "{}")); err == nil {
		t.Fatal("long name accepted")
	}
	if _, err := feed(toolChunk(0, long, "f", "{}")); err == nil {
		t.Fatal("long id accepted")
	}
	enc, err := feed(toolChunk(0, "abc", "fn", "{}"))
	if err != nil || enc.total != len("abc")+len("fn")+len("{}") {
		t.Fatalf("assembled %d, err %v", enc.total, err)
	}
}

func TestStreamToolNameAndIDChanges(t *testing.T) {
	// Repeated name and id: fine.
	if _, err := feed(toolChunk(0, "c1", "f", `{"a":`), toolChunk(0, "c1", "f", `1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := feed(toolChunk(0, "c1", "f", `{"a":`), toolChunk(0, "c1", "g", `1}`)); err == nil ||
		!strings.Contains(err.Error(), "name changed") {
		t.Fatalf("name change: %v", err)
	}
	if _, err := feed(toolChunk(0, "c1", "f", `{"a":`), toolChunk(0, "c2", "", `1}`)); err == nil ||
		!strings.Contains(err.Error(), "id changed") {
		t.Fatalf("id change: %v", err)
	}
	// An id arriving after an id-less first chunk, while the call is still
	// pending, is adopted.
	enc := NewStreamEncoder("m", 1, ToolMap{}, true)
	enc.Start()
	for _, c := range [][]byte{
		toolChunk(0, "c0", "f", "{}"),
		toolChunk(1, "", "g", `{"a":`),
		toolChunk(1, "late", "", `1}`),
	} {
		if _, err := enc.Chunk(c); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := enc.Finish()
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Response struct {
			Output []functionCallItem `json:"output"`
		} `json:"response"`
	}
	mustJSON(t, evs[len(evs)-1].Data, &r)
	if len(r.Response.Output) != 2 || r.Response.Output[1].CallID != "late" {
		t.Fatalf("output %+v", r.Response.Output)
	}
}

func TestEncodeResponseRefusal(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want string
	}{
		{`{"role":"assistant","content":null,"refusal":"no"}`, `[{"type":"refusal","refusal":"no"}]`},
		{`{"role":"assistant","content":"hm","refusal":"no"}`, `[{"type":"output_text","text":"hm","annotations":[]},{"type":"refusal","refusal":"no"}]`},
		{`{"role":"assistant","content":"hm","refusal":null}`, `[{"type":"output_text","text":"hm","annotations":[]}]`},
	} {
		chat := `{"choices":[{"index":0,"message":` + tc.msg + `,"finish_reason":"stop"}]}`
		out, err := EncodeResponse([]byte(chat), "m", ToolMap{}, true)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Output []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"output"`
		}
		mustJSON(t, out, &r)
		if len(r.Output) != 1 || r.Output[0].Type != "message" || string(r.Output[0].Content) != tc.want {
			t.Errorf("%s: output %s", tc.msg, out)
		}
	}
}

func TestStreamRefusal(t *testing.T) {
	enc := NewStreamEncoder("m", 1, ToolMap{}, true)
	evs := enc.Start()
	for _, c := range []string{
		`{"choices":[{"index":0,"delta":{"content":"I "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":null,"refusal":"can't"}}]}`,
		`{"choices":[{"index":0,"delta":{"refusal":" help"}}]}`,
	} {
		got, err := enc.Chunk([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, got...)
	}
	got, err := enc.Finish()
	if err != nil {
		t.Fatal(err)
	}
	evs = append(evs, got...)
	want := []string{"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.content_part.added", "response.refusal.delta", "response.refusal.delta",
		"response.refusal.done", "response.content_part.done", "response.output_item.done",
		"response.completed"}
	if !slices.Equal(names(evs), want) {
		t.Fatalf("events %v", names(evs))
	}
	var ev map[string]any
	for i, e := range evs {
		mustJSON(t, e.Data, &ev)
		if ev["sequence_number"] != float64(i) {
			t.Fatalf("sequence %v at %d", ev["sequence_number"], i)
		}
	}
	mustJSON(t, evs[7].Data, &ev)
	if ev["content_index"] != 1.0 || ev["part"].(map[string]any)["type"] != "refusal" {
		t.Fatalf("refusal part added: %s", evs[7].Data)
	}
	mustJSON(t, evs[10].Data, &ev)
	if ev["refusal"] != "can't help" || ev["content_index"] != 1.0 {
		t.Fatalf("refusal done: %s", evs[10].Data)
	}
	var done struct {
		Response struct {
			Output []struct {
				Content json.RawMessage `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	mustJSON(t, evs[len(evs)-1].Data, &done)
	wantContent := `[{"type":"output_text","text":"I ","annotations":[]},{"type":"refusal","refusal":"can't help"}]`
	if len(done.Response.Output) != 1 || string(done.Response.Output[0].Content) != wantContent {
		t.Fatalf("completed output %s", evs[len(evs)-1].Data)
	}
}

func TestStreamDroppedLateReasoning(t *testing.T) {
	early := []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`)
	text := []byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	late := []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"more"}}]}`)
	enc, err := feed(early, text)
	if err != nil || len(enc.Dropped()) != 0 {
		t.Fatalf("dropped %v, %v", enc.Dropped(), err)
	}
	enc, err = feed(early, text, late, late)
	if err != nil || !slices.Equal(enc.Dropped(), []string{DroppedLateReasoning}) {
		t.Fatalf("dropped %v, %v", enc.Dropped(), err)
	}
	// Hidden reasoning is a configuration choice, not a drop.
	hidden := NewStreamEncoder("m", 1, ToolMap{}, false)
	for _, c := range [][]byte{text, late} {
		hidden.Chunk(c)
	}
	if len(hidden.Dropped()) != 0 {
		t.Fatalf("hidden: dropped %v", hidden.Dropped())
	}
}
