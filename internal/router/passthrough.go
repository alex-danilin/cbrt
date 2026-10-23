package router

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxPlanLimitCooldown caps how long a plan usage limit keeps a passthrough
// provider cooling (Claude plans reset within a week).
const MaxPlanLimitCooldown = 7 * 24 * time.Hour

const unifiedPrefix = "Anthropic-Ratelimit-Unified-"

// ClassifyPassthrough classifies a non-2xx answer from a passthrough
// (Anthropic) upstream. failover reports whether the gateway tries the next
// target; when false, the answer goes back to the client unchanged so it can
// act on it itself (refresh an expired login on 401, drop rejected thinking
// blocks on 400, and so on).
//
//   - 429 with anthropic-ratelimit-unified-* headers: the plan's usage limit.
//     Quota, cooling until the reset those headers report.
//   - 429 without them: a temporary throttle, cooling for Retry-After.
//   - 402, or a 400/403 that talks about credit or billing (API-key accounts):
//     quota, cooling for quotaCooldown.
//   - 408, 5xx and 529 (overloaded): as Classify does.
//   - Anything else: the client's business.
func ClassifyPassthrough(status int, h http.Header, body []byte, quotaCooldown time.Duration) (v Verdict, failover bool) {
	if quotaCooldown <= 0 {
		quotaCooldown = DefaultQuotaCooldown
	}
	matched := body
	if len(matched) > maxMatched {
		matched = matched[:maxMatched]
	}
	v = Verdict{Detail: extractDetail(body)}
	switch {
	case status == http.StatusTooManyRequests && planLimited(h):
		v.Class, v.Scope, v.Cooldown = QuotaExhausted, ScopeProvider, planReset(h, quotaCooldown)
	case status == http.StatusTooManyRequests:
		v.Class, v.Scope, v.Cooldown = RateLimited, ScopeProvider, retryAfter(h)
	case status == http.StatusPaymentRequired,
		(status == http.StatusBadRequest || status == http.StatusForbidden) && quotaRE.Match(matched):
		v.Class, v.Scope, v.Cooldown = QuotaExhausted, ScopeProvider, quotaCooldown
	case status == http.StatusRequestTimeout || status >= 500:
		return Classify(nil, nil, status, h, body, Patterns{}, quotaCooldown), true
	default:
		v.Class = UpstreamRejected
		return v, false
	}
	return v, true
}

// planLimited reports whether a 429 carries the unified rate-limit headers
// that mark a plan limit (a throttle has none). When they include a status,
// only "rejected" is a limit: "allowed" and "allowed_warning" are not.
func planLimited(h http.Header) bool {
	found := false
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), unifiedPrefix) {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	if s := strings.TrimSpace(h.Get(unifiedPrefix + "Status")); s != "" {
		return strings.EqualFold(s, "rejected")
	}
	return true
}

// planReset is the time until the plan limit resets: the unified reset header,
// else the earliest future one among the per-window resets (…-5h-reset, …),
// else Retry-After, else fallback. Values are Unix seconds or RFC 3339.
func planReset(h http.Header, fallback time.Duration) time.Duration {
	now := timeNow()
	if d, ok := resetIn(h.Get(unifiedPrefix+"Reset"), now); ok {
		return d
	}
	best := time.Duration(0)
	for k, vs := range h {
		k = http.CanonicalHeaderKey(k)
		if !strings.HasPrefix(k, unifiedPrefix) || !strings.HasSuffix(k, "-Reset") || len(vs) == 0 {
			continue
		}
		if d, ok := resetIn(vs[0], now); ok && (best == 0 || d < best) {
			best = d
		}
	}
	if best > 0 {
		return best
	}
	if h.Get("Retry-After") != "" {
		return retryAfter(h)
	}
	return fallback
}

func resetIn(s string, now time.Time) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var t time.Time
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		t = time.Unix(n, 0)
	} else if p, err := time.Parse(time.RFC3339, s); err == nil {
		t = p
	} else {
		return 0, false
	}
	d := t.Sub(now).Round(time.Second)
	if d <= 0 {
		return 0, false
	}
	return min(d, MaxPlanLimitCooldown), true
}
