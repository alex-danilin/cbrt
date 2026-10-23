package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"html"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"time"
)

const sessionCookie = "cbrt_session"

// maxConcurrentLogins bounds POST /login requests in flight (each may hold a slow body read).
const maxConcurrentLogins = 8

// maxLoginRead caps the time to read the login form.
const maxLoginRead = 10 * time.Second

type sessionStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time
	now func() time.Time
	ep  uint64 // bumped by reset; a create under an older epoch is refused
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, m: map[string]time.Time{}, now: time.Now}
}

// epoch is the current session epoch; read it before checking the key a session is granted for.
func (s *sessionStore) epoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ep
}

// create issues a session token, unless reset ran since epoch was read (ok=false).
func (s *sessionStore) create(epoch uint64) (tok string, ok bool) {
	b := make([]byte, 32)
	rand.Read(b)
	tok = hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != s.ep {
		return "", false
	}
	now := s.now()
	for k, exp := range s.m { // opportunistic cleanup
		if now.After(exp) {
			delete(s.m, k)
		}
	}
	s.m[tok] = now.Add(s.ttl)
	return tok, true
}

func (s *sessionStore) valid(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	return ok && s.now().Before(exp)
}

// reset drops every session and starts a new epoch (after the API key changes).
func (s *sessionStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.m)
	s.ep++
}

// Login limiter bounds.
const (
	// maxLoginFailures caps failed logins per window from all clients combined, so
	// rotating addresses (or IPv6 /64s) can't buy unlimited guesses.
	maxLoginFailures = 30
	// maxLoginKeys caps the per-client map. Keys are only admitted while the global
	// failure cap has room, so it is rarely reached; see allow for what happens then.
	maxLoginKeys = 10000
)

// loginLimiter allows n login attempts (failed or not) per client per window, and
// refuses every attempt while maxFails failures (all clients) fall inside the window.
type loginLimiter struct {
	mu        sync.Mutex
	n         int
	window    time.Duration
	maxFails  int
	maxKeys   int
	hits      map[string][]time.Time // client key -> attempts inside the window, oldest first
	fails     []time.Time            // failures from all clients, oldest first
	lastSweep time.Time
	now       func() time.Time
}

func newLoginLimiter(n int, window time.Duration) *loginLimiter {
	return &loginLimiter{n: n, window: window, maxFails: maxLoginFailures, maxKeys: maxLoginKeys,
		hits: map[string][]time.Time{}, now: time.Now}
}

// loginKey is the limiter key for a client address: the address itself for IPv4,
// its /64 for IPv6 (one host usually controls a whole /64).
func loginKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}

// recent drops times that fell out of the window (ts is oldest first).
func (l *loginLimiter) recent(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= l.window {
		i++
	}
	return ts[i:]
}

// sweep deletes every key with no attempt inside the window.
func (l *loginLimiter) sweep(now time.Time) {
	for k, ts := range l.hits {
		if len(l.recent(ts, now)) == 0 {
			delete(l.hits, k)
		}
	}
	l.lastSweep = now
}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastSweep) >= l.window {
		l.sweep(now) // amortized: one full pass per window
	}
	l.fails = l.recent(l.fails, now)
	if len(l.fails) >= l.maxFails {
		return false
	}
	key := loginKey(ip)
	kept, ok := l.hits[key]
	kept = l.recent(kept, now)
	if !ok && len(l.hits) >= l.maxKeys {
		l.sweep(now)
		if len(l.hits) >= l.maxKeys {
			// Every key still has an attempt inside the window. Refuse the newcomer rather
			// than evict: evicting would hand a throttled client (or the attacker who
			// filled the map) a fresh bucket. Room returns as entries age out.
			return false
		}
	}
	if len(kept) >= l.n {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// failed records a wrong key toward the global failure cap.
func (l *loginLimiter) failed() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.fails = append(l.recent(l.fails, now), now)
}

const loginHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>cbrt login</title><style>
:root{color-scheme:light dark;--bg:#fafafa;--fg:#1a1a1a;--muted:#666;--card:#fff;--line:#ddd}
@media (prefers-color-scheme:dark){:root{--bg:#141414;--fg:#eee;--muted:#aaa;--card:#1e1e1e;--line:#333}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh}
form{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:24px;width:min(320px,90vw)}
input,button{width:100%;box-sizing:border-box;padding:8px;margin-top:8px;font:inherit}
p{color:var(--muted);margin:8px 0 0}</style></head><body>
<form method="post" action="/login"><strong>cbrt</strong><label><p>API key</p><input type="password" name="key" autocomplete="current-password" required autofocus></label>
<button type="submit">Sign in</button>%s</form></body></html>`

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	renderLogin(w, http.StatusOK, "")
}

func renderLogin(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	note := ""
	if msg != "" {
		note = "<p role=\"alert\">" + html.EscapeString(msg) + "</p>"
	}
	w.Write([]byte(sprintfOne(loginHTML, note)))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	// The epoch is read before the key (see reloadConfig): a session can only be created
	// for the key that was current when this epoch was.
	ep := s.sessions.epoch()
	cfg := s.cfg()
	key := cfg.APIKey
	if key == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	select {
	case s.loginSlots <- struct{}{}:
		defer func() { <-s.loginSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		renderLogin(w, http.StatusTooManyRequests, "Too many sign-ins in progress. Try again shortly.")
		return
	}
	if !s.logins.allow(s.clientIP(r)) {
		w.Header().Set("Retry-After", "60")
		renderLogin(w, http.StatusTooManyRequests, "Too many attempts. Try again in a minute.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	rc := http.NewResponseController(w)
	setReadDeadline(rc, s.now().Add(min(cfg.Timeouts.Read.D(), maxLoginRead)))
	err := r.ParseForm()
	setReadDeadline(rc, time.Time{})
	if err != nil {
		var ne net.Error
		if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			w.Header().Set("Connection", "close")
			renderLogin(w, http.StatusRequestTimeout, "Timed out reading the form.")
			return
		}
		renderLogin(w, http.StatusBadRequest, "Could not read the form.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("key")), []byte(key)) != 1 {
		s.logins.failed()
		renderLogin(w, http.StatusUnauthorized, "Wrong key.")
		return
	}
	if s.afterKeyCheck != nil {
		s.afterKeyCheck()
	}
	tok, ok := s.sessions.create(ep)
	if !ok {
		renderLogin(w, http.StatusUnauthorized, "Key changed; sign in again.")
		return
	}
	secure := r.TLS != nil || (cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: int((12 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sprintfOne substitutes the single %s in a template without fmt's other verbs.
func sprintfOne(tpl, v string) string {
	for i := 0; i+1 < len(tpl); i++ {
		if tpl[i] == '%' && tpl[i+1] == 's' {
			return tpl[:i] + v + tpl[i+2:]
		}
	}
	return tpl
}
