package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAnswerID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`, "chatcmpl-1"},
		{`{"id":"msg_1","type":"message","content":[]}`, "msg_1"},
		{`{"type":"message_start","message":{"id":"msg_2","usage":{}}}`, "msg_2"},
		{`{"type":"response.created","sequence_number":0,"response":{"id":"resp_3","status":"in_progress"}}`, "resp_3"},
		{`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"\"id\""}}`, ""},
		{`{"type":"ping"}`, ""},
		{`[DONE]`, ""},
		{``, ""},
	} {
		if got := answerID([]byte(tc.in)); got != tc.want {
			t.Errorf("answerID(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{`{"id":"msg_1","content":[{"type":"text","text":"cut here`, "msg_1"},
		{` { "id" : "msg_2" , `, "msg_2"},
		{`{"type":"message","id":"msg_3"}`, ""}, // only a leading id is trusted in a truncated head
	} {
		if got := leadingID([]byte(tc.in)); got != tc.want {
			t.Errorf("leadingID(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// recordedIDs returns the response_id of every persisted attempt (closes the store).
func recordedIDs(t *testing.T, e *env) []string {
	t.Helper()
	var ids []string
	for _, r := range e.records(t) {
		id, _ := r["response_id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func TestRecordsAnswerIDChat(t *testing.T) {
	a := newFake(t, okJSON("hi"))
	e := newEnv(t, "", a, newFake(t, okJSON("unused")))
	resp, body := e.post(t, simple)
	var got struct{ ID string }
	json.Unmarshal([]byte(body), &got)
	if resp.StatusCode != 200 || got.ID == "" {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ids := recordedIDs(t, e); len(ids) != 1 || ids[0] != got.ID {
		t.Fatalf("recorded %q, client got %q", ids, got.ID)
	}
}

// A translated Anthropic stream: the record carries the message id cbrt
// generated and sent in message_start, the one Claude Code stores.
func TestRecordsAnswerIDMessagesStream(t *testing.T) {
	a := newFake(t, sse(fixture(t, "nvidia/deepseek-v4.1-flash_stream.sse")))
	e := newEnv(t, "", a, a)
	resp, err := http.Post(e.messagesURL(t)+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	clientID := ""
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok && clientID == "" {
			clientID = answerID([]byte(d))
		}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(clientID, "msg_") {
		t.Fatalf("client message id %q", clientID)
	}
	if ids := recordedIDs(t, e); len(ids) != 1 || ids[0] != clientID {
		t.Fatalf("recorded %q, client got %q", ids, clientID)
	}
}

// Passthrough: the id is Anthropic's own, from message_start or a JSON body.
func TestRecordsAnswerIDPassthrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		step http.HandlerFunc
		want string
	}{
		{"stream", anthropicSSE(), "msg_1"},
		{"json", anthropicError(200, `{"id":"msg_json","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`), "msg_json"},
		{"error", anthropicError(400, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAnthropicFake(t, tc.step)
			e := newPassEnv(t, a, newFake(t, okJSON("free")))
			e.send(t, claudeBody(t))
			e.store.Close()
			ids := recordedIDs(t, &env{store: e.store, dir: e.dir})
			if len(ids) != 1 || ids[0] != tc.want {
				t.Fatalf("recorded %q, want %q", ids, tc.want)
			}
		})
	}
}
