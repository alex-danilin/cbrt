package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alex-danilin/cbrt/internal/api/anthropic"
	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/provider"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/stream"
	"github.com/alex-danilin/cbrt/internal/usage"
)

const (
	maxEventBytes    = 1 << 20
	maxResponseBytes = 16 << 20
	maxErrorBody     = 64 << 10
)

var errIdleTimeout = errors.New("upstream idle timeout")

// attemptLog is the per-request list of what was tried, for headers and errors.
type attemptLog []string

func (a attemptLog) String() string { return strings.Join(a, ",") }

// result of one upstream attempt.
type result struct {
	committed bool           // response bytes reached the client; no more failover
	outcome   usage.Outcome  // for the attempt record
	verdict   router.Verdict // failure verdict (Class OK when fine)
	status    int            // upstream status for rejected requests
	usage     tokenCount
	ttfb      time.Duration
	answerID  string // id of the answer the client received, if any
}

// failed reports whether the attempt ended with a failure verdict; a zero
// verdict (Class "") means success.
func (r result) failed() bool { return r.verdict.Class != "" && r.verdict.Class != router.OK }

type tokenCount struct {
	prompt, completion, reasoning, cached int64
	reported                              bool
	estCompletionChars                    int
}

// classInvalidOutput marks an upstream answer the gateway could not translate
// (e.g. tool arguments that are not JSON): fail over, but no cooldown.
const classInvalidOutput router.Class = "invalid_output"

type streamAdmitKey struct{}

// WithStreamAdmission attaches a concurrent-stream limiter to a request context.
// The gateway calls admit once it has parsed the request and knows it streams,
// so callers need not pre-parse bodies; release is called when the stream ends.
func WithStreamAdmission(ctx context.Context, admit func() (release func(), ok bool)) context.Context {
	return context.WithValue(ctx, streamAdmitKey{}, admit)
}

// ServeChat handles POST /v1/chat/completions for an already-read body.
func (g *Gateway) ServeChat(w http.ResponseWriter, r *http.Request, body []byte) {
	req, err := parseChat(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "", err.Error())
		return
	}
	g.serve(w, r, req, chatAPI{wantsUsage: req.ClientWantsUsage})
}

// ServeMessages handles POST /v1/messages (Anthropic Messages, used by Claude Code).
func (g *Gateway) ServeMessages(w http.ResponseWriter, r *http.Request, body []byte) {
	api := messagesAPI{g: g, exposeReasoning: g.rt.Load().cfg.Visibility.ReasoningExposed()}
	chatBody, info, err := anthropic.DecodeRequest(body)
	if err != nil {
		g.serveUntranslatable(w, r, body, api, err)
		return
	}
	api.model = info.Model
	req, err := parseChat(chatBody)
	if err != nil {
		g.serveUntranslatable(w, r, body, api, err)
		return
	}
	if len(info.Downgraded) > 0 {
		g.downgraded.Add(int64(len(info.Downgraded)))
		g.noteDowngraded(info.Downgraded)
	}
	req.Raw = body
	g.serve(w, r, req, api)
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, req *chatRequest, api clientAPI) {
	rt := g.rt.Load()
	route, targets, ok := rt.resolve(req.Model)
	if !ok {
		api.writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("unknown model or route %q", req.Model))
		return
	}
	if req.Stream {
		if admit, ok := r.Context().Value(streamAdmitKey{}).(func() (func(), bool)); ok {
			release, ok := admit()
			if !ok {
				w.Header().Set("Retry-After", "1")
				api.writeError(w, http.StatusTooManyRequests, "rate_limit_error", "too_many_streams", "too many concurrent streams")
				return
			}
			defer release()
		}
	}
	session := r.Header.Get("X-Claude-Code-Session-Id")
	if session == "" {
		session = r.Header.Get("Session-Id")
	}
	reqID := g.newRequestID()

	var (
		tried       attemptLog
		firstTried  string
		firstReason string
		retryAt     time.Time
		rejected    *router.Verdict
		rejStatus   int
		allRejected = true
		relayTried  bool // a passthrough target was actually tried
	)
	noteRetry := func(t time.Time) {
		if !t.IsZero() && (retryAt.IsZero() || t.Before(retryAt)) {
			retryAt = t
		}
	}
	skip := func(key, reason string) {
		tried = append(tried, key+":skipped("+reason+")")
		if firstTried == "" {
			firstTried, firstReason = key, reason
		}
		allRejected = false
	}

	for n, t := range targets {
		up := rt.ups[t.Provider]
		p := up.Config()
		switch {
		case !p.IsEnabled():
			skip(t.Key(), "disabled")
			continue
		case rt.cfg.Offline && p.Location == "cloud":
			skip(t.Key(), "offline")
			continue
		case p.Passthrough() && req.Raw == nil:
			skip(t.Key(), "not_anthropic_client") // only Claude Code's own Messages requests are relayed
			continue
		case !p.Passthrough() && req.untranslatable != nil:
			skip(t.Key(), "untranslatable")
			continue
		}
		maxCtx := g.contextFor(t)
		if !p.Passthrough() {
			if reason := req.skipReason(t, maxCtx); reason != "" {
				skip(t.Key(), reason)
				continue
			}
		}
		chk := g.health.Check(t.Provider, t.Key())
		if !chk.OK {
			skip(t.Key(), string(chk.Reason))
			noteRetry(chk.Until)
			continue
		}

		start := g.now()
		var res result
		rec := t
		if p.Passthrough() {
			relayTried = true
			res = g.forward(w, r, rt, req, t, up, route, tried)
			rec.Model = req.Model // usage is recorded under the model actually served
		} else {
			res = g.attempt(w, r, rt, req, api, t, up, maxCtx, route, tried)
		}
		g.record(reqID, n+1, route, session, api.name(), rec, p, req, res, start)

		if res.verdict.Class == router.ClientCancelled {
			g.health.Release(t.Provider, t.Key())
			return
		}
		if res.committed {
			if res.failed() {
				if res.verdict.Class != router.ClientCancelled {
					g.log.Warn("stream failed after output began; no failover possible", "route", route,
						"target", t.Key(), "class", res.verdict.Class, "detail", events.Redact(res.verdict.Detail))
				}
				g.fail(rt, route, t, res.verdict) // mid-stream failure still counts toward the breaker
			} else if g.health.Succeed(t.Provider, t.Key(), chk.Epoch) {
				g.hub.Emit(events.Event{Kind: events.KindRecovery, Provider: t.Provider, Target: t.Key(), Route: route})
			}
			g.markServed(route, session, t.Key(), len(tried) > 0, firstReason, firstTried)
			return
		}

		// failed before commit: record, cool down, try the next target
		tried = append(tried, t.Key()+":"+string(res.verdict.Class))
		if firstTried == "" {
			firstTried, firstReason = t.Key(), string(res.verdict.Class)
		}
		if res.verdict.Class == router.ContextExceeded {
			g.learnContext(t, res.verdict.LearnedContext)
		}
		if res.verdict.Class == router.UpstreamRejected || res.verdict.Class == router.ContextExceeded {
			v := res.verdict
			rejected, rejStatus = &v, res.status
		} else {
			allRejected = false
		}
		if res.verdict.Class == classInvalidOutput {
			g.health.Release(t.Provider, t.Key()) // no cooldown, but end a half-open probe
			continue
		}
		if _, until := g.fail(rt, route, t, res.verdict); !until.IsZero() {
			noteRetry(until)
		}
	}

	w.Header().Set("X-Cbrt-Attempts", tried.String())
	w.Header().Set("X-Cbrt-Request-Id", reqID)
	if req.untranslatable != nil && !relayTried && retryAt.IsZero() {
		// no passthrough target could take it, and nothing else can translate it
		// (one that was tried and failed gets the 503 below, which clients retry)
		g.rejectedTypes.Add(1)
		api.writeError(w, http.StatusBadRequest, "", "", req.untranslatable.Error())
		return
	}
	if rejected != nil && allRejected {
		// every provider refused this request itself: most likely the request is at fault
		if rejStatus == 0 {
			rejStatus = http.StatusBadRequest
		}
		api.writeError(w, rejStatus, "invalid_request_error", string(rejected.Class),
			"all providers rejected the request: "+events.Redact(rejected.Detail))
		return
	}
	if !retryAt.IsZero() {
		secs := int(retryAt.Sub(g.now()).Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
	}
	api.writeError(w, http.StatusServiceUnavailable, "server_error", "no_upstream_available",
		"no provider could serve the request; attempts: "+tried.String())
}

// fail applies a verdict to health state and emits cooldown/disabled events.
// rt is the runtime the attempt ran under: an auth failure from a request that
// started before a config reload says nothing about the new credentials, so it
// does not disable the provider (the caller still records the attempt).
func (g *Gateway) fail(rt *runtime, route string, t config.Target, v router.Verdict) (bool, time.Time) {
	if v.Class == router.AuthError && rt != g.rt.Load() {
		g.health.Release(t.Provider, t.Key())
		g.log.Info("auth failure from a request under a replaced config; provider not disabled",
			"provider", t.Provider, "target", t.Key())
		return false, time.Time{}
	}
	entered, until := g.health.Fail(t.Provider, t.Key(), v)
	if entered {
		kind := events.KindCooldown
		if v.Class == router.AuthError {
			kind = events.KindDisabled
		}
		e := events.Event{Kind: kind, Route: route, Provider: t.Provider, Reason: string(v.Class), Detail: events.Redact(v.Detail)}
		if v.Scope == router.ScopeTarget {
			e.Target = t.Key()
		}
		if !until.IsZero() {
			u := until
			e.RetryAt = &u
		}
		g.hub.Emit(e)
	}
	return entered, until
}

func (g *Gateway) record(reqID string, n int, route, session, clientAPI string, t config.Target, p config.Provider,
	req *chatRequest, res result, start time.Time) {
	if g.store == nil {
		return
	}
	rec := usage.Record{
		TS: start.UTC(), RequestID: reqID, AttemptNo: n, ClientAPI: clientAPI, Route: route,
		Provider: t.Provider, Location: p.Location, Model: t.Model, Outcome: res.outcome,
		LatencyMS: g.now().Sub(start).Milliseconds(), TTFBMS: res.ttfb.Milliseconds(), Session: session,
		ResponseID: res.answerID,
	}
	if res.failed() {
		rec.Class = string(res.verdict.Class)
	}
	switch {
	case res.usage.reported:
		rec.UsageSource = usage.SourceReported
		rec.PromptTokens, rec.CompletionTokens = res.usage.prompt, res.usage.completion
		rec.ReasoningTokens, rec.CachedTokens = res.usage.reasoning, res.usage.cached
	case res.committed:
		rec.UsageSource = usage.SourceEstimated
		rec.PromptTokens = int64(req.EstPromptTokens)
		rec.CompletionTokens = int64(estimateTokens(res.usage.estCompletionChars))
	default:
		rec.UsageSource = usage.SourceUnknown
	}
	g.store.Add(rec)
}

// attempt sends the request to one target and relays the answer if it is good.
func (g *Gateway) attempt(w http.ResponseWriter, r *http.Request, rt *runtime, req *chatRequest, api clientAPI,
	t config.Target, up *provider.Upstream, maxCtx int, route string, tried attemptLog) result {
	cfg := rt.cfg
	payload, err := req.upstreamBody(t, maxCtx)
	if err != nil {
		return result{outcome: usage.OutcomeFailed, verdict: router.Verdict{Class: router.UpstreamRejected, Detail: err.Error()}}
	}

	firstByte := firstByteBudget(cfg, t, up)

	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	wb := &watchedBody{idle: cfg.Timeouts.Idle.D()}
	wb.timer = time.AfterFunc(firstByte, func() {
		if wb.armed.Load() {
			cancel(errIdleTimeout)
		} else {
			cancel(router.ErrFirstByteTimeout)
		}
	})
	defer wb.timer.Stop()
	start := g.now()

	classify := func(err error, status int, h http.Header, body []byte) router.Verdict {
		if c := context.Cause(ctx); c != nil && (errors.Is(c, router.ErrFirstByteTimeout) || errors.Is(c, errIdleTimeout)) {
			err = c
		}
		return router.Classify(err, r.Context().Err(), status, h, body, rt.patterns[t.Provider], cfg.Cooldowns.Quota.D())
	}

	resp, err := up.Chat(ctx, payload)
	if err != nil {
		return result{outcome: failedOutcome(r), verdict: classify(err, 0, nil, nil)}
	}
	defer resp.Body.Close()

	wb.r = resp.Body
	body := wb
	if resp.StatusCode/100 != 2 {
		body.arm()
		b, _ := io.ReadAll(io.LimitReader(body, maxErrorBody))
		return result{outcome: failedOutcome(r), verdict: classify(nil, resp.StatusCode, resp.Header, b), status: resp.StatusCode}
	}

	rl := relay{g: g, ctx: ctx, w: w, r: r, req: req, api: api, t: t, tried: tried, route: route, start: start,
		body: body, classify: classify, writeTimeout: cfg.Timeouts.Write.D(), vis: cfg.Visibility}
	if req.Stream {
		return rl.stream()
	}
	return rl.json()
}

// firstByteBudget is how long a target may take to start answering: the
// target's own setting, else the provider's, else the global local/cloud one.
func firstByteBudget(cfg *config.Config, t config.Target, up *provider.Upstream) time.Duration {
	if d := t.FirstByteTimeout.D(); d > 0 {
		return d
	}
	if d := up.Config().FirstByteTimeout.D(); d > 0 {
		return d
	}
	if up.Location() == "cloud" {
		return cfg.Timeouts.FirstByteCloud.D()
	}
	return cfg.Timeouts.FirstByteLocal.D()
}

func failedOutcome(r *http.Request) usage.Outcome {
	if r.Context().Err() != nil {
		return usage.OutcomeCancelled
	}
	return usage.OutcomeFailed
}

// watchedBody arms an idle timer on every read once the response is committed.
// Before arm() the timer holds the first-byte budget; afterwards every read restarts it.
// With armOnFirstByte set, the first read that returns data arms it.
type watchedBody struct {
	r              io.Reader
	timer          *time.Timer
	idle           time.Duration
	armed          atomic.Bool
	armOnFirstByte atomic.Bool
}

func (b *watchedBody) Read(p []byte) (int, error) {
	if b.armed.Load() {
		b.timer.Reset(b.idle)
	}
	n, err := b.r.Read(p)
	if n > 0 && !b.armed.Load() && b.armOnFirstByte.Load() {
		b.arm()
	}
	return n, err
}

// arm switches from the first-byte budget to per-read idle timeouts.
func (b *watchedBody) arm() {
	b.armed.Store(true)
	b.timer.Reset(b.idle)
}

type relay struct {
	g            *Gateway
	ctx          context.Context // the attempt's context; its cause tells timeouts apart
	w            http.ResponseWriter
	r            *http.Request
	req          *chatRequest
	api          clientAPI
	t            config.Target
	tried        attemptLog
	route        string
	start        time.Time
	body         *watchedBody
	classify     func(err error, status int, h http.Header, body []byte) router.Verdict
	writeTimeout time.Duration
	vis          config.Visibility
}

func (rl *relay) commitHeaders(contentType string) {
	h := rl.w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Cbrt-Provider", rl.t.Provider)
	h.Set("X-Cbrt-Model", rl.t.Model)
	h.Set("X-Cbrt-Route", rl.route)
	h.Set("X-Cbrt-Attempts", append(append(attemptLog{}, rl.tried...), rl.t.Key()+":ok").String())
	if len(rl.tried) > 0 {
		h.Set("X-Cbrt-Fallback", "true")
	}
}

// chunk is the subset of a Chat Completions (chunk) object the gateway inspects.
type chunk struct {
	Choices []struct {
		Delta   *chunkMsg `json:"delta"`
		Message *chunkMsg `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

type chunkMsg struct {
	Content          json.RawMessage `json:"content"` // a string or an array of content parts
	Reasoning        string          `json:"reasoning"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// leakedNameRE matches a tool-call name value with a chat-template control
// token leaked into it (gpt-oss: "exec_command<|channel|>commentary"), with
// "<" literal or escaped as \u003c. Group 1 is the name before the token.
var leakedNameRE = regexp.MustCompile(`"name"\s*:\s*"([^"\\<]*)(?:<|\\u003c)\|[^"]*"`)

// repairToolNames strips such tokens from tool-call names; clients reject them.
// In a stream (delta) a name fragment that is only the leaked suffix becomes
// empty; a whole (non-delta) name is never emptied. Each repair is counted as
// a downgrade and its kind logged once.
func (g *Gateway) repairToolNames(data []byte, delta bool) []byte {
	if !bytes.Contains(data, []byte("<|")) && !bytes.Contains(data, []byte(`\u003c|`)) {
		return data // cheap pre-check: nearly every chunk has no control token
	}
	repaired := false
	out := leakedNameRE.ReplaceAllFunc(data, func(m []byte) []byte {
		name := leakedNameRE.FindSubmatch(m)[1]
		if len(name) == 0 && !delta {
			return m
		}
		repaired = true
		return append(append([]byte(`"name":"`), name...), '"')
	})
	if !repaired {
		return data
	}
	g.downgraded.Add(1)
	g.noteDowngraded([]string{"tool name with a leaked template token (suffix stripped)"})
	return out
}

func (c *chunk) count(tc *tokenCount) {
	for _, ch := range c.Choices {
		m := ch.Delta
		if m == nil {
			m = ch.Message
		}
		if m == nil {
			continue
		}
		tc.estCompletionChars += contentChars(m.Content)
		tc.estCompletionChars += len(m.Reasoning) + len(m.ReasoningContent)
		for _, t := range m.ToolCalls {
			tc.estCompletionChars += len(t.Function.Arguments)
		}
	}
	if u := c.Usage; u != nil {
		tc.reported = true
		tc.prompt, tc.completion = u.PromptTokens, u.CompletionTokens
		if u.PromptTokensDetails != nil {
			tc.cached = u.PromptTokensDetails.CachedTokens
		}
		if u.CompletionTokensDetails != nil {
			tc.reasoning = u.CompletionTokensDetails.ReasoningTokens
		}
	}
}

// contentChars counts the text of a message content: a string's length, the
// sum of the parts' "text" fields for an array, and 0 for anything else.
func contentChars(raw json.RawMessage) int {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return len(s)
		}
	case '[':
		var parts []json.RawMessage
		if json.Unmarshal(raw, &parts) != nil {
			return 0
		}
		n := 0
		for _, p := range parts {
			var part struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(p, &part) == nil {
				n += len(part.Text)
			}
		}
		return n
	}
	return 0
}

func (rl *relay) json() result {
	// Keep the first-byte budget until the body starts, then time reads as idle.
	rl.body.armOnFirstByte.Store(true)
	b, err := io.ReadAll(io.LimitReader(rl.body, maxResponseBytes+1))
	res := result{ttfb: rl.g.now().Sub(rl.start)}
	if err != nil {
		res.outcome, res.verdict = failedOutcome(rl.r), rl.classify(err, http.StatusOK, nil, nil)
		return res
	}
	if len(b) > maxResponseBytes {
		res.outcome, res.verdict = usage.OutcomeFailed, router.Verdict{Class: router.UpstreamError, Scope: router.ScopeProvider, Detail: "response too large"}
		return res
	}
	if v, isErr := router.ClassifyPayloadError(b); isErr {
		res.outcome, res.verdict = usage.OutcomeFailed, v
		return res
	}
	var c chunk
	if err := json.Unmarshal(b, &c); err != nil {
		res.outcome, res.verdict = usage.OutcomeFailed, router.Verdict{Class: router.UpstreamError, Scope: router.ScopeProvider, Detail: "malformed JSON response"}
		return res
	}
	c.count(&res.usage)
	b = rl.g.repairToolNames(b, false)
	out, err := rl.api.encodeJSON(b)
	if err != nil {
		res.outcome = usage.OutcomeFailed
		res.verdict = router.Verdict{Class: classInvalidOutput, Detail: err.Error()}
		return res
	}
	b = out
	res.answerID = answerID(b)
	if rl.vis.BodyMetadata {
		b = addMetadata(b, rl)
	}
	rl.commitHeaders("application/json")
	// Bound the write like stream writes; not every ResponseWriter supports it.
	rc := http.NewResponseController(rl.w)
	if rl.writeTimeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(rl.writeTimeout))
	}
	rl.w.WriteHeader(http.StatusOK)
	res.committed = true
	_, err = rl.w.Write(b)
	if err == nil {
		// Small bodies sit in the server's buffer until flushed; flush while the
		// deadline still applies (the server clears it when the handler returns).
		if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
			err = ferr
		}
	}
	if err != nil {
		res.outcome, res.verdict = usage.OutcomeCancelled, router.Verdict{Class: router.ClientCancelled}
		return res
	}
	res.outcome = usage.OutcomeCompleted
	return res
}

func addMetadata(b []byte, rl *relay) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	m["cbrt"], _ = json.Marshal(map[string]any{
		"provider": rl.t.Provider, "model": rl.t.Model, "route": rl.route,
		"attempts": append(append([]string{}, rl.tried...), rl.t.Key()+":ok"),
	})
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return out
}

func (rl *relay) stream() result {
	sr := stream.NewReader(rl.body, maxEventBytes)
	res := result{}

	// Before the commit point, anything wrong is a failover-able failure.
	var first stream.Event
	for {
		ev, err := sr.Next()
		if err != nil {
			res.outcome = failedOutcome(rl.r)
			if errors.Is(err, io.EOF) {
				res.verdict = router.Verdict{Class: router.UpstreamError, Scope: router.ScopeProvider, Detail: "stream ended before the first event"}
			} else if errors.Is(err, stream.ErrEventTooLarge) {
				res.verdict = router.Verdict{Class: router.UpstreamError, Scope: router.ScopeProvider, Detail: err.Error()}
			} else {
				res.verdict = rl.classify(err, http.StatusOK, nil, nil)
			}
			return res
		}
		data := bytes.TrimSpace(ev.Data)
		if string(data) == "[DONE]" {
			res.outcome = usage.OutcomeFailed
			res.verdict = router.Verdict{Class: router.UpstreamError, Scope: router.ScopeProvider, Detail: "stream finished without any data"}
			return res
		}
		if v, isErr := payloadError(data); isErr {
			res.outcome, res.verdict = usage.OutcomeFailed, v
			return res
		}
		if json.Valid(data) {
			first = ev
			break
		}
		res.outcome = usage.OutcomeFailed
		res.verdict = router.Verdict{Class: router.UpstreamError, Scope: router.ScopeProvider, Detail: "malformed stream event"}
		return res
	}

	// Commit: headers go out, and from here on there is no failover.
	res.committed = true
	res.ttfb = rl.g.now().Sub(rl.start)
	rl.body.arm()
	rl.commitHeaders("text/event-stream")
	h := rl.w.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	rl.w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(rl.w)

	write := func(evs []sseOut) bool {
		for i := 0; res.answerID == "" && i < len(evs); i++ {
			res.answerID = answerID(evs[i].data)
		}
		rc.SetWriteDeadline(time.Now().Add(rl.writeTimeout))
		for _, e := range evs {
			var err error
			if e.name == "" {
				err = stream.WriteData(rl.w, e.data)
			} else {
				err = stream.WriteEvent(rl.w, e.name, e.data)
			}
			if err != nil {
				return false
			}
		}
		// Flush only when no further upstream data is already buffered: the client
		// sees every event as soon as the gateway would otherwise wait, but a burst
		// of chunks that arrived together costs one write syscall, not one each.
		if !sr.EventBuffered() {
			// Not every ResponseWriter supports flushing; any other error is a failed write.
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return false
			}
		}
		return true
	}
	cs := rl.api.newStream(rl.req.EstPromptTokens)
	if rl.vis.SSEComments && len(rl.tried) > 0 {
		rc.SetWriteDeadline(time.Now().Add(rl.writeTimeout))
		stream.WriteComment(rl.w, fmt.Sprintf("cbrt fallback %s -> %s attempts=%s", rl.tried[0], rl.t.Key(), rl.tried))
	}
	if !write(cs.start()) {
		res.outcome, res.verdict = usage.OutcomeCancelled, router.Verdict{Class: router.ClientCancelled}
		return res
	}

	ev := first
	for {
		data := bytes.TrimSpace(ev.Data)
		if string(data) == "[DONE]" {
			evs, err := cs.done()
			if err != nil {
				return rl.midStreamFailure(res, router.Verdict{Class: classInvalidOutput, Detail: err.Error()}, cs, write)
			}
			if !write(evs) {
				res.outcome, res.verdict = usage.OutcomeCancelled, router.Verdict{Class: router.ClientCancelled}
				return res
			}
			res.outcome = usage.OutcomeCompleted
			return res
		}
		if v, isErr := payloadError(data); isErr {
			return rl.midStreamFailure(res, v, cs, write)
		}
		var c chunk
		pc := &c // decoded once, shared with the client stream
		if json.Unmarshal(data, &c) == nil {
			c.count(&res.usage)
			data = rl.g.repairToolNames(data, true)
		} else if !json.Valid(data) {
			return rl.midStreamFailure(res, router.Verdict{Class: classInvalidOutput, Detail: errMalformedEvent.Error()}, cs, write)
		} else {
			pc = nil // valid JSON the chunk struct doesn't fit (e.g. array content)
		}
		evs, err := cs.chunk(data, pc)
		if err != nil {
			return rl.midStreamFailure(res, router.Verdict{Class: classInvalidOutput, Detail: err.Error()}, cs, write)
		}
		if !write(evs) {
			res.outcome = usage.OutcomeCancelled
			res.verdict = router.Verdict{Class: router.ClientCancelled}
			return res
		}

		ev, err = sr.Next()
		if err != nil {
			if rl.r.Context().Err() != nil {
				res.outcome = usage.OutcomeCancelled
				res.verdict = router.Verdict{Class: router.ClientCancelled}
				return res
			}
			v := router.Verdict{Class: router.UpstreamError, Scope: router.ScopeProvider, Detail: "stream ended without [DONE]"}
			if c := context.Cause(rl.ctx); errors.Is(c, errIdleTimeout) {
				v.Detail = errIdleTimeout.Error()
			} else if !errors.Is(err, io.EOF) {
				v.Detail = err.Error()
			}
			return rl.midStreamFailure(res, v, cs, write)
		}
	}
}

// midStreamFailure ends a committed stream with an error event in the client's format.
func (rl *relay) midStreamFailure(res result, v router.Verdict, cs clientStream, write func([]sseOut) bool) result {
	write(cs.fail(v))
	res.outcome, res.verdict = usage.OutcomePartial, v
	return res
}

// writeError writes an OpenAI-shaped error body.
func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	body := map[string]any{"message": msg, "type": typ}
	if code != "" {
		body["code"] = code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": body})
}
