package router

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestClassifyPassthrough(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	old := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = old }()

	unix := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	cases := []struct {
		name     string
		status   int
		h        http.Header
		body     string
		class    Class
		cooldown time.Duration
		failover bool
	}{
		{"plan limit, unified reset", 429, hdr("anthropic-ratelimit-unified-status", "rejected", "anthropic-ratelimit-unified-reset", unix(3*time.Hour)), "", QuotaExhausted, 3 * time.Hour, true},
		{"plan limit, earliest window reset", 429, hdr("anthropic-ratelimit-unified-5h-reset", unix(90*time.Minute), "anthropic-ratelimit-unified-7d-reset", unix(48*time.Hour)), "", QuotaExhausted, 90 * time.Minute, true},
		{"plan limit, RFC 3339 reset", 429, hdr("anthropic-ratelimit-unified-reset", now.Add(time.Hour).Format(time.RFC3339)), "", QuotaExhausted, time.Hour, true},
		{"plan limit, no reset: Retry-After", 429, hdr("anthropic-ratelimit-unified-status", "rejected", "Retry-After", "600"), "", QuotaExhausted, 10 * time.Minute, true},
		{"plan limit, no reset at all", 429, hdr("anthropic-ratelimit-unified-status", "rejected"), "", QuotaExhausted, 2 * time.Hour, true},
		{"plan limit, reset capped", 429, hdr("anthropic-ratelimit-unified-reset", unix(30*24*time.Hour)), "", QuotaExhausted, MaxPlanLimitCooldown, true},
		{"plan limit, reset in the past", 429, hdr("anthropic-ratelimit-unified-reset", unix(-time.Hour)), "", QuotaExhausted, 2 * time.Hour, true},
		{"unified says allowed: throttle", 429, hdr("anthropic-ratelimit-unified-status", "allowed", "Retry-After", "5"), "", RateLimited, 5 * time.Second, true},
		{"warning level is not a limit", 429, hdr("anthropic-ratelimit-unified-status", "allowed_warning", "anthropic-ratelimit-unified-reset", unix(3*time.Hour), "Retry-After", "5"), "", RateLimited, 5 * time.Second, true},
		{"throttle", 429, hdr("Retry-After", "5"), `{"error":{"type":"rate_limit_error"}}`, RateLimited, 5 * time.Second, true},
		{"API-key credit", 400, nil, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low"}}`, QuotaExhausted, 2 * time.Hour, true},
		{"payment required", 402, nil, "", QuotaExhausted, 2 * time.Hour, true},
		{"overloaded", 529, nil, `{"type":"error","error":{"type":"overloaded_error"}}`, UpstreamError, 0, true},
		{"server error", 500, nil, "", UpstreamError, 0, true},
		{"expired login: client refreshes", 401, nil, `{"error":{"type":"authentication_error"}}`, UpstreamRejected, 0, false},
		{"forbidden", 403, nil, `{"error":{"type":"permission_error"}}`, UpstreamRejected, 0, false},
		{"thinking signature: client recovers", 400, nil, `{"error":{"message":"Invalid signature in thinking block"}}`, UpstreamRejected, 0, false},
		{"too large", 413, nil, "", UpstreamRejected, 0, false},
		{"not found", 404, nil, "", UpstreamRejected, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.h
			if h == nil {
				h = http.Header{}
			}
			v, failover := ClassifyPassthrough(tc.status, h, []byte(tc.body), 2*time.Hour)
			if v.Class != tc.class || v.Cooldown != tc.cooldown || failover != tc.failover {
				t.Fatalf("got %s %v failover=%v, want %s %v failover=%v", v.Class, v.Cooldown, failover, tc.class, tc.cooldown, tc.failover)
			}
		})
	}
}
