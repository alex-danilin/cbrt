package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/alex-danilin/cbrt/internal/api/anthropic"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/router"
)

// payloadError is router.ClassifyPayloadError with a cheap pre-check: nearly every
// stream chunk has no "error" key, and fully decoding each one showed up at ~13% of
// stream CPU in profiles.
func payloadError(data []byte) (router.Verdict, bool) {
	if !bytes.Contains(data, []byte(`"error"`)) {
		return router.Verdict{}, false
	}
	return router.ClassifyPayloadError(data)
}

// sseOut is one event to send to the client; an empty name means a data-only event.
type sseOut struct {
	name string
	data []byte
}

// clientAPI adapts the Chat Completions core to one client-facing API format.
type clientAPI interface {
	name() string // "chat" | "messages", recorded with usage
	writeError(w http.ResponseWriter, status int, typ, code, msg string)
	encodeJSON(chat []byte) ([]byte, error) // non-stream upstream body -> client body
	newStream(estPromptTokens int) clientStream
}

// clientStream converts upstream chat chunks into client events.
type clientStream interface {
	start() []sseOut
	// chunk converts one upstream event (always valid JSON). c is data already
	// decoded by the relay, or nil when it doesn't fit the chunk struct (e.g. an
	// array "content"), in which case it must be handled without c.
	chunk(data []byte, c *chunk) ([]sseOut, error)
	done() ([]sseOut, error)
	fail(v router.Verdict) []sseOut
}

// chatAPI is the OpenAI Chat Completions passthrough.
type chatAPI struct{ wantsUsage bool }

func (chatAPI) name() string                           { return "chat" }
func (chatAPI) encodeJSON(chat []byte) ([]byte, error) { return chat, nil }
func (chatAPI) writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeError(w, status, typ, code, msg)
}
func (a chatAPI) newStream(int) clientStream { return &chatStream{wantsUsage: a.wantsUsage} }

type chatStream struct{ wantsUsage bool }

func (*chatStream) start() []sseOut { return nil }

var errMalformedEvent = errors.New("malformed stream event")

// chunk forwards upstream chunks verbatim, stripping usage the client did not
// ask for. A chunk the relay could not decode is valid JSON with unexpected
// field types: it is passed through untouched.
func (s *chatStream) chunk(data []byte, c *chunk) ([]sseOut, error) {
	if c == nil || c.Usage == nil || s.wantsUsage {
		return []sseOut{{data: data}}, nil
	}
	if len(c.Choices) == 0 {
		return nil, nil // usage-only chunk requested by the gateway: drop it
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return []sseOut{{data: data}}, nil
	}
	delete(m, "usage") // keep finish_reason and content of the same chunk
	out, err := json.Marshal(m)
	if err != nil {
		return []sseOut{{data: data}}, nil
	}
	return []sseOut{{data: out}}, nil
}

func (*chatStream) done() ([]sseOut, error) { return []sseOut{{data: []byte("[DONE]")}}, nil }

func (*chatStream) fail(v router.Verdict) []sseOut {
	msg, _ := json.Marshal(map[string]any{"error": map[string]string{
		"message": "upstream stream failed: " + events.Redact(v.Detail), "type": "upstream_error", "code": string(v.Class),
	}})
	return []sseOut{{data: msg}}
}

// messagesAPI serves Anthropic Messages clients (Claude Code).
type messagesAPI struct {
	model           string
	exposeReasoning bool
	g               *Gateway // counts upstream output dropped by the stream; nil counts nothing
}

func (messagesAPI) name() string { return "messages" }

func (messagesAPI) writeError(w http.ResponseWriter, status int, typ, _, msg string) {
	errType := anthropic.ErrorType(status)
	if errType == "api_error" && typ == "invalid_request_error" {
		errType = typ // an unmapped 4xx, e.g. every provider rejected the request with 422
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(anthropic.ErrorJSON(errType, msg))
}

func (a messagesAPI) encodeJSON(chat []byte) ([]byte, error) {
	return anthropic.EncodeResponse(chat, a.model, a.exposeReasoning)
}

func (a messagesAPI) newStream(est int) clientStream {
	return &messagesStream{enc: anthropic.NewStreamEncoder(a.model, est, a.exposeReasoning), drops: dropReporter{g: a.g}}
}

type messagesStream struct {
	enc   *anthropic.StreamEncoder
	drops dropReporter
}

func (s *messagesStream) start() []sseOut { return convert(s.enc.Start()) }

func (s *messagesStream) chunk(data []byte, _ *chunk) ([]sseOut, error) {
	evs, err := s.enc.Chunk(data)
	return convert(evs), err
}

func (s *messagesStream) done() ([]sseOut, error) {
	evs, err := s.enc.Finish()
	s.drops.report(s.enc.Dropped())
	return convert(evs), err
}

func (s *messagesStream) fail(v router.Verdict) []sseOut {
	typ := "api_error"
	if v.Class == router.RateLimited || v.Class == router.QuotaExhausted {
		typ = "overloaded_error"
	}
	s.drops.report(s.enc.Dropped())
	return convert(s.enc.Fail(typ, "upstream stream failed: "+events.Redact(v.Detail)))
}

// dropReporter counts and logs (once per kind, via the downgrade counters)
// upstream output that a client stream encoder dropped, e.g. reasoning that
// arrived after the answer began. A stream reports when it finishes or fails;
// a failed finish is followed by fail, so only the first report counts. A nil
// gateway counts nothing.
type dropReporter struct {
	g        *Gateway
	reported bool
}

func (r *dropReporter) report(kinds []string) {
	if r.reported {
		return
	}
	r.reported = true
	if r.g == nil || len(kinds) == 0 {
		return
	}
	r.g.downgraded.Add(int64(len(kinds)))
	r.g.noteDowngraded(kinds)
}

func convert(evs []anthropic.Event) []sseOut {
	out := make([]sseOut, len(evs))
	for i, e := range evs {
		out[i] = sseOut{name: e.Name, data: e.Data}
	}
	return out
}
