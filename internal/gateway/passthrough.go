package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/provider"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/usage"
)

// Passthrough (provider type anthropic) relays Claude Code's own Messages
// request to Anthropic unchanged, with the client's own credential, and the
// answer back unchanged. The gateway only decides whether to relay or to fail
// over; it never sends such a provider a request of its own, never changes the
// body, and never stores the credential.

// passthroughJSONBudget is the default wait for a non-stream passthrough
// answer, which arrives (headers included) only when the model has finished.
const passthroughJSONBudget = 10 * time.Minute

// serveUntranslatable handles a Messages request the translator rejected:
// a passthrough target can still relay it unchanged. Without one, the client
// gets the translator's 400 as before.
func (g *Gateway) serveUntranslatable(w http.ResponseWriter, r *http.Request, body []byte, api messagesAPI, cause error) {
	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &head)
	rt := g.rt.Load()
	if _, targets, ok := rt.resolve(head.Model); ok && head.Model != "" {
		for _, t := range targets {
			if rt.ups[t.Provider].Config().Passthrough() {
				api.model = head.Model
				g.serve(w, r, &chatRequest{Model: head.Model, Stream: head.Stream, Raw: body, untranslatable: cause}, api)
				return
			}
		}
	}
	g.rejectedTypes.Add(1)
	api.writeError(w, http.StatusBadRequest, "", "", cause.Error())
}

// forward relays the request to a passthrough target. Before the first
// response byte reaches the client a failure can still fail over; answers that
// are the client's business (401, most 4xx) are relayed unchanged instead.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, rt *runtime, req *chatRequest, t config.Target,
	up *provider.Upstream, route string, tried attemptLog) result {
	cfg := rt.cfg
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	wb := &watchedBody{idle: cfg.Timeouts.Idle.D()}
	budget := firstByteBudget(cfg, t, up)
	if !req.Stream && t.FirstByteTimeout == 0 && up.Config().FirstByteTimeout == 0 {
		// Anthropic sends a non-stream answer's headers only once it is complete.
		budget = max(budget, passthroughJSONBudget)
	}
	wb.timer = time.AfterFunc(budget, func() {
		if wb.armed.Load() {
			cancel(errIdleTimeout)
		} else {
			cancel(router.ErrFirstByteTimeout)
		}
	})
	defer wb.timer.Stop()
	start := g.now()

	transportFailure := func(err error) result {
		if c := context.Cause(ctx); c != nil && (errors.Is(c, router.ErrFirstByteTimeout) || errors.Is(c, errIdleTimeout)) {
			err = c
		}
		return result{outcome: failedOutcome(r),
			verdict: router.Classify(err, r.Context().Err(), 0, nil, nil, router.Patterns{}, cfg.Cooldowns.Quota.D())}
	}

	resp, err := up.Forward(ctx, r.URL.Path, r.URL.RawQuery, r.Header, req.Raw)
	if err != nil {
		return transportFailure(err)
	}
	defer resp.Body.Close()
	wb.r = resp.Body

	pr := passRelay{w: w, t: t, model: req.Model, route: route, tried: tried, writeTimeout: cfg.Timeouts.Write.D()}
	if resp.StatusCode/100 != 2 {
		wb.arm()
		b, err := io.ReadAll(io.LimitReader(wb, maxErrorBody))
		if err != nil {
			return transportFailure(err)
		}
		v, failover := router.ClassifyPassthrough(resp.StatusCode, resp.Header, b, cfg.Cooldowns.Quota.D())
		if failover {
			return result{outcome: failedOutcome(r), verdict: v, status: resp.StatusCode}
		}
		res := result{committed: true, outcome: usage.OutcomeFailed, status: resp.StatusCode, ttfb: g.now().Sub(start)}
		pr.commit(resp)
		if !pr.write(b) {
			res.outcome, res.verdict = usage.OutcomeCancelled, router.Verdict{Class: router.ClientCancelled}
		}
		return res
	}

	res := result{committed: true, ttfb: g.now().Sub(start)}
	wb.arm()
	pr.commit(resp)
	var sc usageScanner
	buf := make([]byte, 32<<10)
	for {
		n, rerr := wb.Read(buf)
		if n > 0 {
			sc.feed(buf[:n])
			if !pr.write(buf[:n]) {
				res.usage, res.answerID = sc.count(), sc.answerID()
				res.outcome, res.verdict = usage.OutcomeCancelled, router.Verdict{Class: router.ClientCancelled}
				return res
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			res.usage, res.answerID = sc.count(), sc.answerID()
			if r.Context().Err() != nil {
				res.outcome, res.verdict = usage.OutcomeCancelled, router.Verdict{Class: router.ClientCancelled}
				return res
			}
			v := router.Verdict{Class: router.UpstreamError, Scope: router.ScopeProvider, Detail: rerr.Error()}
			if errors.Is(context.Cause(ctx), errIdleTimeout) {
				v.Detail = errIdleTimeout.Error()
			}
			res.outcome, res.verdict = usage.OutcomePartial, v
			return res
		}
	}
	res.usage = sc.count()
	res.answerID = sc.answerID()
	res.outcome = usage.OutcomeCompleted
	return res
}

// passRelay writes a relayed answer to the client.
type passRelay struct {
	w            http.ResponseWriter
	rc           *http.ResponseController
	t            config.Target
	model        string
	route        string
	tried        attemptLog
	writeTimeout time.Duration
}

// commit copies the upstream's status and headers (rate-limit and retry
// headers included: Claude Code reads them) plus cbrt's own.
func (p *passRelay) commit(resp *http.Response) {
	h := p.w.Header()
	for k, vs := range resp.Header {
		if provider.ForwardResponseHeader(k) {
			h[k] = append([]string(nil), vs...)
		}
	}
	h.Set("X-Cbrt-Provider", p.t.Provider)
	h.Set("X-Cbrt-Model", p.model)
	h.Set("X-Cbrt-Route", p.route)
	h.Set("X-Cbrt-Attempts", append(append(attemptLog{}, p.tried...), p.t.Key()+":ok").String())
	if len(p.tried) > 0 {
		h.Set("X-Cbrt-Fallback", "true")
	}
	h.Set("X-Accel-Buffering", "no")
	p.rc = http.NewResponseController(p.w)
	p.w.WriteHeader(resp.StatusCode)
}

// write sends b and flushes it at once (SSE pings included: Claude Code's idle
// watchdog relies on them). It reports false when the client is gone.
func (p *passRelay) write(b []byte) bool {
	if p.writeTimeout > 0 {
		p.rc.SetWriteDeadline(time.Now().Add(p.writeTimeout))
	}
	if _, err := p.w.Write(b); err != nil {
		return false
	}
	if err := p.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return false
	}
	return true
}

// usageScanner reads token usage from a relayed Anthropic answer without
// holding it: SSE "data:" lines (message_start, message_delta) as they pass,
// or the "usage" object near the end of a JSON body.
type usageScanner struct {
	line    []byte // current partial line
	skip    bool   // the current line grew too long and is ignored
	tail    []byte // last bytes seen, for a non-stream JSON body
	head    []byte // first bytes seen, for a non-stream JSON body's "id"
	id      string // message id from message_start
	sse     bool   // saw an SSE data line
	u       anthropicUsage
	started bool
}

type anthropicUsage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
}

const (
	maxScanLine = 1 << 20
	maxScanTail = 8 << 10
	maxScanHead = 512
)

func (s *usageScanner) feed(b []byte) {
	if len(s.head) < maxScanHead {
		s.head = append(s.head, b[:min(len(b), maxScanHead-len(s.head))]...)
	}
	s.tail = append(s.tail, b...)
	if len(s.tail) > maxScanTail {
		s.tail = append(s.tail[:0], s.tail[len(s.tail)-maxScanTail:]...)
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			s.add(b)
			return
		}
		s.add(b[:i])
		s.endLine()
		b = b[i+1:]
	}
}

func (s *usageScanner) add(b []byte) {
	if s.skip {
		return
	}
	if len(s.line)+len(b) > maxScanLine {
		s.skip, s.line = true, s.line[:0]
		return
	}
	s.line = append(s.line, b...)
}

func (s *usageScanner) endLine() {
	line := bytes.TrimRight(s.line, "\r")
	skip := s.skip
	s.line, s.skip = s.line[:0], false
	if skip {
		return
	}
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	s.sse = true
	if !bytes.Contains(data, []byte(`"usage"`)) && !bytes.Contains(data, []byte(`"message_start"`)) {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			ID    string          `json:"id"`
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return
	}
	if ev.Type == "message_start" && ev.Message != nil && s.id == "" {
		s.id = ev.Message.ID
	}
	switch {
	case ev.Type == "message_start" && ev.Message != nil && ev.Message.Usage != nil:
		s.u, s.started = *ev.Message.Usage, true
	case ev.Type == "message_delta" && ev.Usage != nil:
		// cumulative: output always, input fields when present
		s.u.Output = ev.Usage.Output
		if ev.Usage.Input > 0 {
			s.u.Input = ev.Usage.Input
		}
		if ev.Usage.CacheCreation > 0 {
			s.u.CacheCreation = ev.Usage.CacheCreation
		}
		if ev.Usage.CacheRead > 0 {
			s.u.CacheRead = ev.Usage.CacheRead
		}
		s.started = true
	}
}

// answerID is the relayed answer's message id: from message_start in a
// stream, else the leading "id" of a JSON body.
func (s *usageScanner) answerID() string {
	if s.id != "" || s.sse {
		return s.id
	}
	return leadingID(s.head)
}

// count returns what was seen; reported is false when no usage was found.
func (s *usageScanner) count() tokenCount {
	if !s.sse && !s.started {
		// a JSON body: its top-level "usage" is the last one in it
		if i := bytes.LastIndex(s.tail, []byte(`"usage"`)); i >= 0 {
			rest := s.tail[i+len(`"usage"`):]
			if j := bytes.IndexByte(rest, '{'); j >= 0 {
				var u anthropicUsage
				if json.NewDecoder(bytes.NewReader(rest[j:])).Decode(&u) == nil {
					s.u, s.started = u, true
				}
			}
		}
	}
	if !s.started {
		return tokenCount{}
	}
	return tokenCount{
		prompt:     s.u.Input + s.u.CacheCreation + s.u.CacheRead,
		completion: s.u.Output,
		cached:     s.u.CacheRead,
		reported:   true,
	}
}
