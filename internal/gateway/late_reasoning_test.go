package gateway

import (
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/router"
)

// Reasoning that arrives after the answer began cannot be sent in either
// client format; it is dropped and counted as a downgrade.

var lateReasoningChunks = []string{
	`{"choices":[{"index":0,"delta":{"reasoning_content":"plan"}}]}`,
	`{"choices":[{"index":0,"delta":{"content":"answer"}}]}`,
	`{"choices":[{"index":0,"delta":{"reasoning_content":"late"}}]}`,
	`{"choices":[{"index":0,"delta":{"reasoning_content":"later"},"finish_reason":"stop"}]}`,
}

func lateReasoningSSE(chunks []string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String() + "data: [DONE]\n\n"
}

func TestResponsesLateReasoningCountsDowngrade(t *testing.T) {
	for _, tc := range []struct {
		chunks []string
		want   int64
	}{
		{lateReasoningChunks, 1},
		{lateReasoningChunks[:2], 0},
	} {
		a := newFake(t, sse(lateReasoningSSE(tc.chunks)))
		e := newEnv(t, "", a, newFake(t, okJSON("unused")))
		before, _ := e.gw.Counters()
		resp, out := postTo(t, e.responsesURL(t), `{"model":"r","stream":true,"input":"hi"}`)
		if resp.StatusCode != 200 || !strings.Contains(out, "response.completed") {
			t.Fatalf("%d %s", resp.StatusCode, out)
		}
		if strings.Contains(out, "late") {
			t.Errorf("late reasoning was sent: %s", out)
		}
		if after, _ := e.gw.Counters(); after-before != tc.want {
			t.Errorf("%d chunks: downgraded %d -> %d, want +%d", len(tc.chunks), before, after, tc.want)
		}
	}
}

// messagesStream reports through the gateway it was created with, on done and
// on fail, once.
func TestMessagesLateReasoningCountsDowngrade(t *testing.T) {
	for _, failAfter := range []bool{false, true} {
		e := newEnv(t, "", newFake(t, okJSON("unused")), newFake(t, okJSON("unused")))
		before, _ := e.gw.Counters()
		cs := messagesAPI{model: "m", exposeReasoning: true, g: e.gw}.newStream(1)
		cs.start()
		for _, c := range lateReasoningChunks {
			if _, err := cs.chunk([]byte(c), nil); err != nil {
				t.Fatal(err)
			}
		}
		if failAfter {
			cs.fail(router.Verdict{Class: router.UpstreamError, Detail: "x"})
		} else if _, err := cs.done(); err != nil {
			t.Fatal(err)
		}
		cs.fail(router.Verdict{Class: router.UpstreamError, Detail: "again"}) // a second report is ignored
		if after, _ := e.gw.Counters(); after-before != 1 {
			t.Errorf("fail=%v: downgraded %d -> %d, want +1", failAfter, before, after)
		}
	}
}

// End to end through ServeMessages (Claude Code's path), not just the stream type.
func TestServeMessagesLateReasoningCountsDowngrade(t *testing.T) {
	a := newFake(t, sse(lateReasoningSSE(lateReasoningChunks)))
	e := newEnv(t, "", a, newFake(t, okJSON("unused")))
	before, _ := e.gw.Counters()
	resp, out := postTo(t, e.messagesURL(t), `{"model":"r","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(out, "message_stop") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if after, _ := e.gw.Counters(); after-before != 1 {
		t.Errorf("downgraded %d -> %d, want +1", before, after)
	}
}
