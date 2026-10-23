package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "upstream", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestClassifyFixtures(t *testing.T) {
	tests := []struct {
		file    string
		status  int
		class   Class
		scope   Scope
		detail  string
		learned int
	}{
		{"nvidia/error_403_invalid-key.json", 403, AuthError, ScopeProvider, "Authorization failed: key rejected", 0},
		{"nvidia/error_404_not-found-for-account.json", 404, ModelUnavailable, ScopeTarget, "Function '00000000-0000-4000-8000-0000000000aa': not available for account 'example-account'", 0},
		{"nvidia/error_404_unknown-model.txt", 404, ModelUnavailable, ScopeTarget, "404: no route matches this path", 0},
		{"ollama/error_400_context-exceeded.json", 400, ContextExceeded, ScopeNone, "prompt holds 9000 tokens but the loaded context fits only 2048 tokens", 2048},
		{"ollama/error_404_unknown-model.json", 404, ModelUnavailable, ScopeTarget, "model 'missing-model' not found", 0},
		{"ollama/minicpm-v4.6_responses_context-exceeded-400.json", 400, ContextExceeded, ScopeNone, "prompt holds 15000 tokens but the loaded context fits only 2048 tokens", 2048},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		seen[tt.file] = true
		t.Run(tt.file, func(t *testing.T) {
			v := Classify(nil, nil, tt.status, http.Header{}, fixture(t, tt.file), Patterns{}, time.Hour)
			if v.Class != tt.class || v.Scope != tt.scope || v.Detail != tt.detail || v.LearnedContext != tt.learned {
				t.Fatalf("got %+v", v)
			}
		})
	}
	// Every error fixture must be covered by the table above.
	files, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "upstream", "*", "error_*"))
	for _, f := range files {
		rel := filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(f)), filepath.Base(f)))
		if !seen[rel] {
			t.Errorf("fixture %s not covered", rel)
		}
	}
}

func TestClassifyRules(t *testing.T) {
	nvidiaAuth := Patterns{Auth: []*regexp.Regexp{regexp.MustCompile(`(?i)key revoked`)}}
	custom := Patterns{Quota: []*regexp.Regexp{regexp.MustCompile(`(?i)daily limit reached`)}}
	oai := func(msg string) []byte { return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"x"}}`, msg)) }
	dnsErr := &url.Error{Op: "Post", URL: "http://x", Err: &net.DNSError{Err: "no such host", Name: "x"}}
	tests := []struct {
		name     string
		err      error
		client   error
		status   int
		header   http.Header
		body     []byte
		p        Patterns
		class    Class
		scope    Scope
		cooldown time.Duration
	}{
		{name: "client cancelled wins", err: context.Canceled, client: context.Canceled, status: 500, class: ClientCancelled},
		{name: "client cancelled over 429", client: context.Canceled, status: 429, class: ClientCancelled},
		{name: "first-byte timeout", err: fmt.Errorf("do: %w", ErrFirstByteTimeout), class: Timeout, scope: ScopeTarget, cooldown: 30 * time.Second},
		{name: "net timeout", err: &url.Error{Op: "Post", URL: "http://x", Err: timeoutErr{}}, class: Timeout, scope: ScopeProvider, cooldown: 30 * time.Second},
		{name: "deadline", err: context.DeadlineExceeded, class: Timeout, scope: ScopeProvider, cooldown: 30 * time.Second},
		{name: "timeout after headers", err: ErrFirstByteTimeout, status: 200, class: Timeout, scope: ScopeTarget, cooldown: 30 * time.Second},
		{name: "dns", err: dnsErr, class: Unreachable, scope: ScopeProvider},
		{name: "refused", err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, class: Unreachable, scope: ScopeProvider},
		{name: "eof before headers", err: io.EOF, class: Unreachable, scope: ScopeProvider},
		{name: "eof after headers", err: io.ErrUnexpectedEOF, status: 200, class: UpstreamError, scope: ScopeProvider},
		{name: "402", status: 402, class: QuotaExhausted, scope: ScopeProvider, cooldown: 2 * time.Hour},
		{name: "quota wording 403", status: 403, body: oai("You exceeded your current quota"), class: QuotaExhausted, scope: ScopeProvider, cooldown: 2 * time.Hour},
		{name: "429 credits exhausted", status: 429, body: oai("Credits exhausted for this account"), class: QuotaExhausted, scope: ScopeProvider, cooldown: 2 * time.Hour},
		{name: "insufficient_quota code", status: 429, body: []byte(`{"error":{"message":"x","code":"insufficient_quota"}}`), class: QuotaExhausted, scope: ScopeProvider, cooldown: 2 * time.Hour},
		{name: "insufficient balance 400", status: 400, body: oai("Insufficient Balance"), class: QuotaExhausted, scope: ScopeProvider, cooldown: 2 * time.Hour},
		{name: "billing 500", status: 500, body: []byte("billing hard limit"), class: QuotaExhausted, scope: ScopeProvider, cooldown: 2 * time.Hour},
		{name: "provider quota pattern", status: 429, body: []byte("Daily limit reached"), p: custom, class: QuotaExhausted, scope: ScopeProvider, cooldown: 2 * time.Hour},
		{name: "429 rate limit not quota", status: 429, body: oai("Rate limit exceeded"), class: RateLimited, scope: ScopeProvider, cooldown: 60 * time.Second},
		{name: "429 retry-after seconds", status: 429, header: http.Header{"Retry-After": {"17"}}, class: RateLimited, scope: ScopeProvider, cooldown: 17 * time.Second},
		{name: "429 retry-after date", status: 429, header: http.Header{"Retry-After": {"Tue, 29 Sep 2026 12:02:00 GMT"}}, class: RateLimited, scope: ScopeProvider, cooldown: 2 * time.Minute},
		{name: "429 retry-after past date", status: 429, header: http.Header{"Retry-After": {"Tue, 29 Sep 2026 11:00:00 GMT"}}, class: RateLimited, scope: ScopeProvider, cooldown: time.Second},
		{name: "429 retry-after capped", status: 429, header: http.Header{"Retry-After": {"999999999999"}}, class: RateLimited, scope: ScopeProvider, cooldown: time.Hour},
		{name: "429 retry-after garbage", status: 429, header: http.Header{"Retry-After": {"soon"}}, class: RateLimited, scope: ScopeProvider, cooldown: 60 * time.Second},
		{name: "401", status: 401, body: []byte("nope"), class: AuthError, scope: ScopeProvider},
		{name: "403 authorization failed", status: 403, body: []byte(`{"status":403,"title":"Forbidden","detail":"Authorization failed"}`), class: AuthError, scope: ScopeProvider},
		{name: "403 invalid api key", status: 403, body: oai("Invalid API key provided"), class: AuthError, scope: ScopeProvider},
		{name: "403 provider auth pattern", status: 403, body: []byte("Key revoked"), p: nvidiaAuth, class: AuthError, scope: ScopeProvider},
		{name: "403 plain", status: 403, body: []byte(`{"status":403,"title":"Forbidden","detail":"model not enabled"}`), class: ModelUnavailable, scope: ScopeTarget, cooldown: 10 * time.Minute},
		{name: "404 auth wording is not auth", status: 404, body: []byte("unauthorized"), class: ModelUnavailable, scope: ScopeTarget, cooldown: 10 * time.Minute},
		{name: "400 context length", status: 400, body: oai("This model's maximum context length is 8192 tokens"), class: ContextExceeded},
		{name: "413 too many tokens", status: 413, body: []byte("too many tokens"), class: ContextExceeded},
		{name: "422 context window", status: 422, body: oai("prompt exceeds context window"), class: ContextExceeded},
		{name: "400 other", status: 400, body: oai("unsupported parameter: foo"), class: UpstreamRejected},
		{name: "413 other", status: 413, class: UpstreamRejected},
		{name: "422 other", status: 422, body: []byte(`{"detail":"bad schema"}`), class: UpstreamRejected},
		{name: "409", status: 409, class: Upstream4xx, scope: ScopeTarget, cooldown: 5 * time.Minute},
		{name: "408", status: 408, class: Upstream4xx, scope: ScopeTarget, cooldown: 5 * time.Minute},
		{name: "500", status: 500, body: oai("internal"), class: UpstreamError, scope: ScopeProvider},
		{name: "503", status: 503, class: UpstreamError, scope: ScopeProvider},
		{name: "200 with error", status: 200, body: oai("boom"), class: UpstreamError, scope: ScopeProvider},
		{name: "200 with rate limit error", status: 200, body: oai("rate limit hit"), class: RateLimited, scope: ScopeProvider, cooldown: 60 * time.Second},
	}
	old := timeNow
	timeNow = func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
	defer func() { timeNow = old }()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.header
			if h == nil {
				h = http.Header{}
			}
			v := Classify(tt.err, tt.client, tt.status, h, tt.body, tt.p, 2*time.Hour)
			if v.Class != tt.class || v.Scope != tt.scope || v.Cooldown != tt.cooldown {
				t.Fatalf("got %s/%s/%v, want %s/%s/%v (%+v)", v.Class, v.Scope, v.Cooldown, tt.class, tt.scope, tt.cooldown, v)
			}
		})
	}
}

func TestClassifyDefaultQuotaCooldown(t *testing.T) {
	if v := Classify(nil, nil, 402, nil, nil, Patterns{}, 0); v.Cooldown != time.Hour {
		t.Fatalf("got %v", v.Cooldown)
	}
}

func TestDetail(t *testing.T) {
	long := strings.Repeat("é", 400)
	tests := []struct {
		name, body, want string
	}{
		{"problem+json", `{"status":403,"title":"Forbidden","detail":"Authorization failed"}`, "Authorization failed"},
		{"problem+json title only", `{"status":500,"title":"Internal"}`, "Internal"},
		{"openai", `{"error":{"message":"bad things","type":"x"}}`, "bad things"},
		{"string error", `{"error":"model is loading"}`, "model is loading"},
		{"nested", `{"error":{"message":"{\"error\":{\"message\":\"inner\"}}"}}`, "inner"},
		{"nested invalid", `{"error":{"message":"{not json"}}`, "{not json"},
		{"error without message", `{"error":{"code":1}}`, `{"code":1}`},
		{"text", "404 page not found\n", "404 page not found"},
		{"empty", "", ""},
		{"long", long, strings.Repeat("é", 150)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractDetail([]byte(tt.body)); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyPayloadError(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		ok     bool
		class  Class
		detail string
	}{
		{"chunk", `{"id":"x","choices":[{"delta":{"content":"hi"}}]}`, false, "", ""},
		{"null error", `{"error":null,"choices":[]}`, false, "", ""},
		{"not json", `[DONE]`, false, "", ""},
		{"array", `[1,2]`, false, "", ""},
		{"openai", `{"error":{"message":"overloaded","type":"server_error"}}`, true, UpstreamError, "overloaded"},
		{"string", `{"error":"boom"}`, true, UpstreamError, "boom"},
		{"anthropic", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, true, UpstreamError, "Overloaded"},
		{"quota", `{"error":{"message":"out of credits"}}`, true, QuotaExhausted, "out of credits"},
		{"code 429", `{"error":{"message":"slow down","code":429}}`, true, RateLimited, "slow down"},
		{"code 429 string", `{"error":{"message":"slow down","code":"429"}}`, true, RateLimited, "slow down"},
		{"rate wording", `{"error":{"message":"Too Many Requests"}}`, true, RateLimited, "Too Many Requests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, ok := ClassifyPayloadError([]byte(tt.body))
			if ok != tt.ok || v.Class != tt.class || v.Detail != tt.detail {
				t.Fatalf("got %+v %v", v, ok)
			}
			if ok && (v.Scope != ScopeProvider || (v.Class != UpstreamError && v.Cooldown == 0)) {
				t.Fatalf("bad scope/cooldown %+v", v)
			}
		})
	}
}

func TestClassifyTransportDetail(t *testing.T) {
	v := Classify(errors.New("dial tcp: refused"), nil, 0, nil, nil, Patterns{}, 0)
	if v.Detail != "dial tcp: refused" || v.Cooldown != 0 {
		t.Fatalf("got %+v", v)
	}
}
