package gateway

import (
	"net/http"

	"github.com/alex-danilin/cbrt/internal/api/responses"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/router"
)

// ServeResponses handles POST /v1/responses (OpenAI Responses, the only API Codex CLI speaks).
func (g *Gateway) ServeResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	api := responsesAPI{g: g, exposeReasoning: g.rt.Load().cfg.Visibility.ReasoningExposed()}
	chatBody, info, err := responses.DecodeRequest(body)
	if err != nil {
		g.rejectedTypes.Add(1)
		api.writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_input", err.Error())
		return
	}
	api.model, api.tools = info.Model, info.Tools
	if len(info.Downgraded) > 0 {
		g.downgraded.Add(int64(len(info.Downgraded)))
		g.noteDowngraded(info.Downgraded)
	}
	req, err := parseChat(chatBody)
	if err != nil {
		api.writeError(w, http.StatusBadRequest, "invalid_request_error", "", err.Error())
		return
	}
	g.serve(w, r, req, api)
}

// responsesAPI serves OpenAI Responses clients (Codex).
type responsesAPI struct {
	model           string
	tools           responses.ToolMap
	exposeReasoning bool
	g               *Gateway // counts upstream output dropped by the stream; nil counts nothing
}

func (responsesAPI) name() string { return "responses" }

func (responsesAPI) writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(responses.ErrorJSON(typ, code, msg))
}

func (a responsesAPI) encodeJSON(chat []byte) ([]byte, error) {
	return responses.EncodeResponse(chat, a.model, a.tools, a.exposeReasoning)
}

func (a responsesAPI) newStream(est int) clientStream {
	return &responsesStream{enc: responses.NewStreamEncoder(a.model, est, a.tools, a.exposeReasoning), drops: dropReporter{g: a.g}}
}

type responsesStream struct {
	enc   *responses.StreamEncoder
	drops dropReporter
}

func (s *responsesStream) start() []sseOut { return convertResponses(s.enc.Start()) }

func (s *responsesStream) chunk(data []byte, _ *chunk) ([]sseOut, error) {
	evs, err := s.enc.Chunk(data)
	return convertResponses(evs), err
}

func (s *responsesStream) done() ([]sseOut, error) {
	evs, err := s.enc.Finish()
	s.drops.report(s.enc.Dropped())
	return convertResponses(evs), err
}

func (s *responsesStream) fail(v router.Verdict) []sseOut {
	code := "server_error"
	if v.Class == router.RateLimited || v.Class == router.QuotaExhausted {
		code = "rate_limit_exceeded"
	}
	s.drops.report(s.enc.Dropped())
	return convertResponses(s.enc.Fail(code, "upstream stream failed: "+events.Redact(v.Detail)))
}

func convertResponses(evs []responses.Event) []sseOut {
	out := make([]sseOut, len(evs))
	for i, e := range evs {
		out[i] = sseOut{name: e.Name, data: e.Data}
	}
	return out
}
