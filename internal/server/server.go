// Package server exposes the gateway over HTTP: API endpoints, auth, limits and the dashboard.
package server

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alex-danilin/cbrt/internal/api/anthropic"
	"github.com/alex-danilin/cbrt/internal/api/codex"
	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/gateway"
	"github.com/alex-danilin/cbrt/internal/hostcheck"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/usage"
)

//go:embed web/index.html
var dashboardHTML []byte

type Server struct {
	gw      *gateway.Gateway
	store   *usage.Store
	hub     *events.Hub
	log     *slog.Logger
	now     func() time.Time
	reload  func() (*config.Config, error)
	offline func() bool

	inflight   atomic.Int64
	reqSlots   chan struct{}
	streamSlot chan struct{}
	bucket     *tokenBucket
	sessions   *sessionStore
	logins     *loginLimiter
	loginSlots chan struct{} // bounds concurrent in-flight POST /login requests

	// afterKeyCheck, if set, runs in login between the key check and session creation (tests).
	afterKeyCheck func()
}

type Options struct {
	Gateway *gateway.Gateway
	Store   *usage.Store
	Hub     *events.Hub
	Logger  *slog.Logger
	// Reload re-reads the configuration file; nil disables /api/reload.
	Reload func() (*config.Config, error)
}

func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	lim := o.Gateway.Config().Limits
	return &Server{
		gw: o.Gateway, store: o.Store, hub: o.Hub, log: o.Logger, now: time.Now, reload: o.Reload,
		reqSlots:   make(chan struct{}, lim.MaxConcurrentRequests),
		streamSlot: make(chan struct{}, lim.MaxConcurrentStreams),
		bucket:     newTokenBucket(lim.RequestsPerMinute, time.Now),
		sessions:   newSessionStore(12 * time.Hour),
		logins:     newLoginLimiter(5, time.Minute),
		loginSlots: make(chan struct{}, maxConcurrentLogins),
	}
}

func (s *Server) cfg() *config.Config { return s.gw.Config() }

// Handler returns the root handler with host checks applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /v1/chat/completions", s.api(s.llm(s.gw.ServeChat)))
	mux.HandleFunc("POST /v1/messages", s.api(s.llm(s.gw.ServeMessages)))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.api(s.withSlot(s.countTokens)))
	mux.HandleFunc("POST /v1/responses", s.api(s.llm(s.gw.ServeResponses)))
	mux.HandleFunc("GET /v1/models", s.api(s.models))
	mux.HandleFunc("GET /api/stats", s.api(s.stats))
	mux.HandleFunc("GET /api/status", s.api(s.status))
	mux.HandleFunc("POST /api/reload", s.api(s.reloadConfig))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /{$}", s.dashboard)
	return s.hostCheck(mux)
}

func (s *Server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg() // per request: allowed_hosts and api_key are reloadable
		h := hostName(r.Host)
		if hosts := cfg.AllowedHosts; len(hosts) > 0 {
			ok := false
			for _, a := range hosts {
				if strings.EqualFold(a, h) {
					ok = true
				}
			}
			if !ok {
				http.Error(w, "host not allowed", http.StatusMisdirectedRequest)
				return
			}
		} else if cfg.APIKey == "" && !loopbackHost(h) {
			// Without a key (loopback listen only) only the Host header tells a local client
			// from a web page that reached us through a DNS-rebinding domain.
			http.Error(w, "host not allowed", http.StatusMisdirectedRequest)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostName strips the port and IPv6 brackets from a Host header value.
func hostName(h string) string { return hostcheck.Name(h) }

// loopbackHost reports whether h is localhost or a loopback IP literal.
func loopbackHost(h string) bool { return hostcheck.Loopback(h) }

// sameOrigin allows requests without an Origin header (API clients) or from the page's own origin.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// authorized accepts the API key as Bearer or x-api-key, or a dashboard session cookie.
func (s *Server) authorized(r *http.Request) bool {
	key := s.cfg().APIKey
	if key == "" {
		return true
	}
	given := r.Header.Get("X-Api-Key")
	if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		given = b
	}
	if given != "" && subtle.ConstantTimeCompare([]byte(given), []byte(key)) == 1 {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return s.sessions.valid(c.Value)
	}
	return false
}

func (s *Server) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cbrt"`)
			apiError(w, r, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
			return
		}
		h(w, r)
	}
}

// llm applies admission control (rate, concurrency, memory budget, body size)
// and hands the body to one of the gateway's client-API handlers.
func (s *Server) llm(serve func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.bucket.take() {
			w.Header().Set("Retry-After", "1")
			apiError(w, r, http.StatusTooManyRequests, "rate_limit_error", "gateway request rate limit reached")
			return
		}
		release, ok := s.takeSlot(w, r)
		if !ok {
			return
		}
		defer release()

		body, releaseBody, ok := s.readBody(w, r)
		if !ok {
			return
		}
		defer releaseBody()

		// The gateway admits streams itself once it has parsed the body (no pre-parse here).
		r = r.WithContext(gateway.WithStreamAdmission(r.Context(), func() (func(), bool) {
			select {
			case s.streamSlot <- struct{}{}:
				return func() { <-s.streamSlot }, true
			default:
				return nil, false
			}
		}))
		// The gateway sets per-write deadlines on streams. net/http (Go 1.26) also clears the
		// write deadline after an HTTP/1 handler; clearing it here keeps a stale deadline from
		// reaching the final flush or the next keep-alive request regardless of that detail.
		defer func() {
			if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
				s.log.Debug("clearing write deadline", "err", err)
			}
		}()
		serve(w, r, body)
	}
}

// takeSlot reserves one of the max_concurrent_requests slots, or answers 429.
func (s *Server) takeSlot(w http.ResponseWriter, r *http.Request) (func(), bool) {
	select {
	case s.reqSlots <- struct{}{}:
		return func() { <-s.reqSlots }, true
	default:
		w.Header().Set("Retry-After", "1")
		apiError(w, r, http.StatusTooManyRequests, "rate_limit_error", "too many concurrent requests")
		return nil, false
	}
}

// withSlot applies only the concurrency limit, not the rate bucket: for local work such
// as count_tokens, which Claude Code calls often.
func (s *Server) withSlot(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, ok := s.takeSlot(w, r)
		if !ok {
			return
		}
		defer release()
		h(w, r)
	}
}

// setReadDeadline sets (or with the zero time clears) the connection's read deadline.
// Writers that do not support deadlines (e.g. in tests) are left alone.
func setReadDeadline(rc *http.ResponseController, t time.Time) {
	_ = rc.SetReadDeadline(t) // http.ErrNotSupported is fine: the body read is then unbounded
}

// readBody reserves in-flight memory and reads the body within the size limit.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, func(), bool) {
	cfg := s.cfg()
	lim := cfg.Limits
	n := r.ContentLength
	if n < 0 || n > lim.MaxRequestBytes {
		n = lim.MaxRequestBytes // unknown length: reserve the maximum
	}
	if s.inflight.Add(n) > lim.MaxInflightBytes {
		s.inflight.Add(-n)
		w.Header().Set("Retry-After", "1")
		apiError(w, r, http.StatusServiceUnavailable, "overloaded_error", "gateway is at its in-flight memory budget")
		return nil, nil, false
	}
	release := func() { s.inflight.Add(-n) }
	// Bound the body read so a slow client cannot hold a slot and its reservation forever.
	rc := http.NewResponseController(w)
	setReadDeadline(rc, s.now().Add(cfg.Timeouts.Read.D()))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, lim.MaxRequestBytes))
	setReadDeadline(rc, time.Time{})
	if err != nil {
		release()
		var mbe *http.MaxBytesError
		var ne net.Error
		if errors.As(err, &mbe) {
			apiError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
		} else if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			w.Header().Set("Connection", "close")
			apiError(w, r, http.StatusRequestTimeout, "request_timeout", "timed out reading the request body")
		} else {
			apiError(w, r, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		}
		return nil, nil, false
	}
	return body, release, true
}

// countTokens answers Claude Code's POST /v1/messages/count_tokens locally with an estimate;
// no upstream offers it (NVIDIA has no such endpoint).
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	body, release, ok := s.readBody(w, r)
	if !ok {
		return
	}
	defer release()
	n, err := anthropic.CountTokens(body)
	if err != nil {
		apiError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, map[string]int{"input_tokens": n})
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if s.cfg().CodexCatalog && codex.IsCodexRequest(r) {
		// Codex expects its own {"models":[...]} shape with per-model metadata.
		var ms []codex.Model
		for _, mi := range s.gw.ModelInfos() {
			ms = append(ms, codex.Model{Slug: mi.ID, DisplayName: mi.ID, ContextWindow: mi.ContextWindow, SupportsReasoning: mi.SupportsReasoning})
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(codex.ModelsJSON(ms))
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	var data []model
	for _, id := range s.gw.Models() {
		owner := "cbrt"
		if p, _, ok := strings.Cut(id, ":"); ok {
			owner = p
		}
		data = append(data, model{ID: id, Object: "model", OwnedBy: owner})
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

func (s *Server) reloadConfig(w http.ResponseWriter, r *http.Request) {
	if s.reload == nil {
		writeJSONError(w, http.StatusNotImplemented, "server_error", "reload not available")
		return
	}
	old := s.cfg()
	cfg, err := s.reload()
	if err == nil {
		// Fields bound at startup cannot change live. Refusing them also keeps Validate's
		// exposed-bind rules checked against the address actually listening.
		if changed := config.RestartRequired(old, cfg); len(changed) > 0 {
			err = fmt.Errorf("configuration not applied: %s changed and require a restart", strings.Join(changed, ", "))
		}
	}
	if err == nil {
		err = s.gw.Reload(cfg)
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// Order matters: gw.Reload has already swapped the config, so reset (which bumps the
	// session epoch) runs after the new key is visible. login reads the epoch before the
	// key, so a login that saw the old key holds the old epoch: its create is refused, or,
	// if it created first, reset clears that session here.
	if cfg.APIKey != old.APIKey {
		s.sessions.reset() // sessions were granted under the old key
		s.log.Info("api key changed: dashboard sessions cleared")
	}
	s.log.Info("configuration reloaded")
	writeJSON(w, map[string]any{"reloaded": true})
}

// ProviderStatus is a provider's usage merged with its health.
type ProviderStatus struct {
	usage.ProviderStats
	Status string     `json:"status"`
	Reason string     `json:"reason,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
}

type healthCounters struct {
	usage.Health
	Downgraded    int64 `json:"downgraded"`
	RejectedTypes int64 `json:"rejected_types"`
	InflightBytes int64 `json:"inflight_bytes"`
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st := s.store.Stats()
	cfg := s.cfg()
	byProv := map[string]usage.ProviderStats{}
	for _, p := range st.Providers {
		byProv[p.Provider] = p
	}
	degraded := providerHealth(s.gw.Health().Degraded())
	downgraded, rejected := s.gw.Counters()
	provs := make([]ProviderStatus, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		ps := ProviderStatus{ProviderStats: byProv[p.ID], Status: "healthy"}
		ps.Provider, ps.Location = p.ID, p.Location
		if !p.IsEnabled() {
			ps.Status, ps.Reason = "disabled", "disabled in config"
		} else if cfg.Offline && p.Location == "cloud" {
			ps.Status, ps.Reason = "disabled", "offline mode"
		} else if d, ok := degraded[p.ID]; ok {
			ps.Status, ps.Reason, ps.Until = d.Status, string(d.Reason), d.Until
		}
		provs = append(provs, ps)
	}
	writeJSON(w, map[string]any{
		"generated_at": st.GeneratedAt,
		"offline":      cfg.Offline,
		"providers":    provs,
		"models":       st.Models,
		"daily":        st.Daily,
		"routes":       s.gw.Routes(),
		"events":       s.hub.Recent(50),
		"health":       healthCounters{Health: st.Health, Downgraded: downgraded, RejectedTypes: rejected, InflightBytes: s.inflight.Load()},
	})
}

// providerHealth keeps provider-level degradations (target-level ones stay in /api/status).
func providerHealth(states []router.State) map[string]router.State {
	out := map[string]router.State{}
	for _, st := range states {
		if st.Target == "" {
			out[st.Provider] = st
		}
	}
	return out
}

// status is the compact endpoint used by `cbrt statusline` and scripts.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"routes":   s.gw.Routes(),
		"degraded": s.gw.Health().Degraded(),
	}
	if last, ok := s.hub.Last(); ok {
		resp["last_event"] = last
	}
	if id := r.URL.Query().Get("session"); id != "" {
		if sv, ok := s.gw.Session(id); ok {
			resp["session"] = sv
		}
	}
	writeJSON(w, resp)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Write(dashboardHTML)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// apiError writes an error in the shape the calling client expects:
// Anthropic for /v1/messages*, OpenAI-style otherwise.
func apiError(w http.ResponseWriter, r *http.Request, status int, typ, msg string) {
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(anthropic.ErrorJSON(anthropic.ErrorType(status), msg))
		return
	}
	writeJSONError(w, status, typ, msg)
}

func writeJSONError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": typ, "message": msg}})
}

// tokenBucket is a simple global request-rate limiter (single-user gateway).
type tokenBucket struct {
	mu     sync.Mutex
	cap    float64
	tokens float64
	rate   float64 // tokens per second
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(perMinute int, now func() time.Time) *tokenBucket {
	return &tokenBucket{cap: float64(perMinute), tokens: float64(perMinute), rate: float64(perMinute) / 60, last: now(), now: now}
}

func (b *tokenBucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	b.tokens = min(b.cap, b.tokens+t.Sub(b.last).Seconds()*b.rate)
	b.last = t
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP identifies the caller for per-IP limits. X-Forwarded-For is honored only
// when the TCP peer is a trusted proxy (trust_proxy plus trusted_proxies, loopback by
// default): anyone else could send a fresh address with every request. The header is
// read right to left, skipping trusted hops, so the first untrusted address is the
// client; entries left of it were written by the client and are ignored.
func (s *Server) clientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		peer = h
	}
	cfg := s.cfg()
	pa, err := netip.ParseAddr(peer)
	if err != nil || !cfg.ProxyTrusted(pa) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // garbage: keep the last address a trusted hop vouched for
		}
		client = a.Unmap().WithZone("").String()
		if !cfg.ProxyTrusted(a) {
			break
		}
	}
	return client
}

func itoa(n int) string { return strconv.Itoa(n) }
