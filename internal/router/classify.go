// Package router classifies failed upstream attempts and tracks provider and
// target health (cooldowns, backoff, circuit breaker, half-open probes).
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Class is the outcome class of one upstream attempt.
type Class string

// Outcome classes. Each maps to a cooldown scope and duration (see Classify).
const (
	OK               Class = "ok"
	ClientCancelled  Class = "client_cancelled"
	ClientError      Class = "client_error" // gateway ingress validation (not produced by Classify)
	Unreachable      Class = "unreachable"
	Timeout          Class = "timeout"
	QuotaExhausted   Class = "quota_exhausted"
	RateLimited      Class = "rate_limited"
	AuthError        Class = "auth_error"
	ModelUnavailable Class = "model_unavailable"
	ContextExceeded  Class = "context_exceeded"  // prompt too long: fail over, no cooldown, learn n_ctx
	UpstreamRejected Class = "upstream_rejected" // upstream 400/413/422: fail over, no cooldown
	Upstream4xx      Class = "upstream_4xx"      // other 4xx: fail over, target cooldown
	UpstreamError    Class = "upstream_error"    // 5xx, 200-with-error, EOF before first event: breaker
)

// Scope says what a verdict penalises.
type Scope int

// Verdict scopes.
const (
	ScopeNone Scope = iota
	ScopeTarget
	ScopeProvider
)

func (s Scope) String() string {
	switch s {
	case ScopeTarget:
		return "target"
	case ScopeProvider:
		return "provider"
	}
	return "none"
}

// ErrFirstByteTimeout is the cancellation cause the gateway uses when an
// upstream does not produce its first byte in time.
var ErrFirstByteTimeout = errors.New("upstream first-byte timeout")

// Fixed cooldowns used by Classify.
const (
	TimeoutCooldown          = 30 * time.Second
	DefaultQuotaCooldown     = time.Hour
	DefaultRateLimitCooldown = 60 * time.Second
	MaxRateLimitCooldown     = time.Hour
	ModelUnavailableCooldown = 10 * time.Minute
	Upstream4xxCooldown      = 5 * time.Minute
)

// Patterns are provider-specific wordings added to the built-in ones.
type Patterns struct {
	Quota, Auth []*regexp.Regexp
}

// Verdict is the classification of one failed attempt.
type Verdict struct {
	Class          Class
	Scope          Scope
	Cooldown       time.Duration // fixed cooldown; 0 = none, or computed by Health
	Detail         string        // short upstream message, not redacted
	LearnedContext int           // n_ctx from a context-size error, else 0
}

const (
	maxDetail  = 300
	maxMatched = 64 << 10 // only this much of a body is pattern-matched
)

var (
	quotaRE   = regexp.MustCompile(`(?i)quota|credit|insufficient[ _-]?(funds|balance)|billing|payment[ _-]?required|out[ _-]?of[ _-]?credits`)
	authRE    = regexp.MustCompile(`(?i)authori[sz]ation failed|invalid[ _-]?api[ _-]?key|invalid[ _-]?token|unauthori[sz]ed`)
	contextRE = regexp.MustCompile(`(?i)exceed_context_size_error|context[ _-]length|maximum context|too many tokens|context[ _-]window`)
	nCtxRE    = regexp.MustCompile(`n_ctx[\\"]*\s*[:=]\s*(\d+)`)
	rateRE    = regexp.MustCompile(`(?i)rate[ _-]?limit|too many requests`)
)

// timeNow is the clock used for Retry-After HTTP-dates; tests replace it.
var timeNow = time.Now

// Classify maps one failed attempt to a verdict. err is the transport error
// (nil if a response arrived); clientErr is the client's context error
// (non-nil means the client went away). The first matching rule wins.
func Classify(err, clientErr error, status int, header http.Header, body []byte, p Patterns, quotaCooldown time.Duration) Verdict {
	if clientErr != nil {
		return Verdict{Class: ClientCancelled}
	}
	if err != nil && errors.Is(err, ErrFirstByteTimeout) {
		// The gateway's own per-target timer: the provider took the request but this
		// model was slow (free-tier queues are per model), so only the target cools down.
		return Verdict{Class: Timeout, Scope: ScopeTarget, Cooldown: TimeoutCooldown, Detail: clip(err.Error())}
	}
	if err != nil && isTimeout(err) {
		return Verdict{Class: Timeout, Scope: ScopeProvider, Cooldown: TimeoutCooldown, Detail: clip(err.Error())}
	}
	if err != nil && status == 0 {
		return Verdict{Class: Unreachable, Scope: ScopeProvider, Detail: clip(err.Error())}
	}
	if quotaCooldown <= 0 {
		quotaCooldown = DefaultQuotaCooldown
	}
	detail := extractDetail(body)
	if detail == "" && err != nil {
		detail = clip(err.Error())
	}
	matched := body
	if len(matched) > maxMatched {
		matched = matched[:maxMatched]
	}
	v := Verdict{Detail: detail}
	switch {
	case status == http.StatusPaymentRequired || match(matched, quotaRE, p.Quota):
		v.Class, v.Scope, v.Cooldown = QuotaExhausted, ScopeProvider, quotaCooldown
	case status == http.StatusTooManyRequests:
		v.Class, v.Scope, v.Cooldown = RateLimited, ScopeProvider, retryAfter(header)
	case status == http.StatusUnauthorized || status == http.StatusForbidden && match(matched, authRE, p.Auth):
		v.Class, v.Scope = AuthError, ScopeProvider
	case status == http.StatusForbidden || status == http.StatusNotFound:
		v.Class, v.Scope, v.Cooldown = ModelUnavailable, ScopeTarget, ModelUnavailableCooldown
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge || status == http.StatusUnprocessableEntity:
		v.Class = UpstreamRejected
		if contextRE.Match(matched) {
			v.Class = ContextExceeded
			v.LearnedContext = learnedContext(matched)
		}
	case status >= 400 && status < 500:
		v.Class, v.Scope, v.Cooldown = Upstream4xx, ScopeTarget, Upstream4xxCooldown
	default:
		// 5xx, a 2xx with an error body, or an error after headers
		// (e.g. EOF before the first event).
		if status >= 200 && status < 300 && err == nil {
			if pv, ok := ClassifyPayloadError(body); ok {
				return pv
			}
		}
		v.Class, v.Scope = UpstreamError, ScopeProvider
	}
	return v
}

// ClassifyPayloadError classifies a JSON body or SSE event data that carries
// an "error" member despite HTTP 200. It reports false when there is none.
func ClassifyPayloadError(body []byte) (Verdict, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(body), &top) != nil {
		return Verdict{}, false
	}
	raw, ok := top["error"]
	if !ok || isNullOrEmpty(raw) {
		return Verdict{}, false
	}
	v := Verdict{Class: UpstreamError, Scope: ScopeProvider, Detail: extractDetail(body)}
	if v.Detail == "" {
		v.Detail = clip(string(raw))
	}
	matched := raw
	if len(matched) > maxMatched {
		matched = matched[:maxMatched]
	}
	switch {
	case quotaRE.Match(matched):
		v.Class, v.Cooldown = QuotaExhausted, DefaultQuotaCooldown
	case errorCode(raw) == http.StatusTooManyRequests || rateRE.Match(matched):
		v.Class, v.Cooldown = RateLimited, DefaultRateLimitCooldown
	}
	return v, true
}

func isTimeout(err error) bool {
	if errors.Is(err, ErrFirstByteTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func match(b []byte, builtin *regexp.Regexp, extra []*regexp.Regexp) bool {
	if builtin.Match(b) {
		return true
	}
	for _, re := range extra {
		if re != nil && re.Match(b) {
			return true
		}
	}
	return false
}

// retryAfter parses Retry-After (seconds or HTTP-date), defaulting to 60s and
// capped at one hour.
func retryAfter(h http.Header) time.Duration {
	s := strings.TrimSpace(h.Get("Retry-After"))
	if s == "" {
		return DefaultRateLimitCooldown
	}
	var d time.Duration
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > int64(MaxRateLimitCooldown/time.Second) {
			return MaxRateLimitCooldown
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(s); err == nil {
		d = t.Sub(timeNow()).Round(time.Second)
	} else {
		return DefaultRateLimitCooldown
	}
	return min(max(d, time.Second), MaxRateLimitCooldown)
}

func learnedContext(b []byte) int {
	m := nCtxRE.FindSubmatch(b)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// extractDetail returns a short upstream message from a problem+json body,
// an OpenAI/Ollama error object (unwrapping a JSON-encoded inner error), or
// the start of a text body.
func extractDetail(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return ""
	}
	if body[0] == '{' {
		if msg, ok := jsonMessage(body, 0); ok {
			return clip(msg)
		}
	}
	return clip(string(body))
}

func jsonMessage(b []byte, depth int) (string, bool) {
	var obj struct {
		Detail  *string         `json:"detail"`
		Title   *string         `json:"title"`
		Message *string         `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if depth > 3 || json.Unmarshal(b, &obj) != nil {
		return "", false
	}
	if len(obj.Error) > 0 && !isNullOrEmpty(obj.Error) {
		var s string
		if json.Unmarshal(obj.Error, &s) == nil {
			return unwrap(s, depth), true
		}
		var inner struct {
			Message *string `json:"message"`
		}
		if json.Unmarshal(obj.Error, &inner) == nil && inner.Message != nil {
			return unwrap(*inner.Message, depth), true
		}
		return string(obj.Error), true
	}
	for _, p := range []*string{obj.Detail, obj.Message, obj.Title} {
		if p != nil && *p != "" {
			return *p, true
		}
	}
	return "", false
}

// unwrap returns the inner message when s is itself a JSON error document.
func unwrap(s string, depth int) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "{") {
		if msg, ok := jsonMessage([]byte(t), depth+1); ok && msg != "" {
			return msg
		}
	}
	return s
}

func errorCode(raw json.RawMessage) int {
	var e struct {
		Code   json.RawMessage `json:"code"`
		Status json.RawMessage `json:"status"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return 0
	}
	for _, c := range []json.RawMessage{e.Code, e.Status} {
		if n, err := strconv.Atoi(strings.Trim(string(c), `"`)); err == nil {
			return n
		}
	}
	return 0
}

func isNullOrEmpty(raw json.RawMessage) bool {
	s := string(bytes.TrimSpace(raw))
	return s == "" || s == "null" || s == `""` || s == "false"
}

// clip trims s to maxDetail bytes without splitting a UTF-8 sequence.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxDetail {
		return s
	}
	i := maxDetail
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
