// Package statusline implements `cbrt statusline`, a Claude Code status-line
// command. Claude Code runs it after each turn with a JSON object on stdin and
// shows the first line of stdout. It asks the gateway's GET /api/status which
// target served the session (or, failing that, the most recent route) and
// which providers are degraded, and prints a single short line.
//
// It is built to be harmless: it never panics, is bounded in time by
// StdinTimeout+HTTPTimeout, always prints exactly one line and Main always
// returns 0, so a broken gateway can never break Claude Code.
package statusline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// DefaultURL is used when neither flags nor environment name a gateway.
	DefaultURL = "http://127.0.0.1:8080"
	// DefaultStdinTimeout bounds how long Run waits for Claude Code's stdin JSON.
	DefaultStdinTimeout = 200 * time.Millisecond
	// DefaultHTTPTimeout bounds the whole GET /api/status exchange.
	DefaultHTTPTimeout = 300 * time.Millisecond

	maxStdin    = 64 << 10
	maxBody     = 1 << 20
	maxTarget   = 32 // runes; longer target ids are truncated from the left
	maxRoute    = 20
	maxProvider = 20
	maxReason   = 24
	maxLine     = 100
)

// Options configures Run. Zero values select the documented defaults.
type Options struct {
	Stdin        io.Reader
	Stdout       io.Writer
	Getenv       func(string) string // os.Getenv by default
	Client       *http.Client        // default: &http.Client{} (timeouts come from HTTPTimeout)
	URL, Key     string              // from -url / -key flags; override env
	StdinTimeout time.Duration       // default 200ms
	HTTPTimeout  time.Duration       // default 300ms
	Now          func() time.Time    // time.Now by default; its Location formats "until"
	Plain        bool                // ASCII-only output (-plain)
}

// Main parses -url, -key and -plain, runs the status line against os.Stdin and
// os.Stdout, and always returns 0.
func Main(args []string) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	u := fs.String("url", "", "gateway base URL (default: $CBRT_URL, $ANTHROPIC_BASE_URL, "+DefaultURL+")")
	k := fs.String("key", "", "gateway API key (default: $CBRT_API_KEY, or $ANTHROPIC_AUTH_TOKEN with $ANTHROPIC_BASE_URL)")
	plain := fs.Bool("plain", false, "ASCII-only output")
	_ = fs.Parse(args) // on a bad flag, carry on with whatever was parsed
	Run(Options{Stdin: os.Stdin, Stdout: os.Stdout, URL: *u, Key: *k, Plain: *plain})
	return 0
}

// Run prints exactly one status line to o.Stdout.
func Run(o Options) {
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	sym := unicodeSymbols
	if o.Plain {
		sym = plainSymbols
	}
	defer func() {
		if recover() != nil {
			fmt.Fprintln(o.Stdout, "cbrt "+sym.cross+" error internal")
		}
	}()
	line := status(&o, sym)
	fmt.Fprintln(o.Stdout, clip(line, maxLine, sym.ellipsis))
}

type symbols struct{ dot, warn, cross, busy, arrow, ellipsis string }

var (
	unicodeSymbols = symbols{"●", "⚠", "✕", "⧗", "→", "…"}
	plainSymbols   = symbols{"*", "!", "x", "...", "->", "..."}
)

func status(o *Options, sym symbols) string {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Client == nil {
		o.Client = &http.Client{}
	}
	if o.StdinTimeout <= 0 {
		o.StdinTimeout = DefaultStdinTimeout
	}
	if o.HTTPTimeout <= 0 {
		o.HTTPTimeout = DefaultHTTPTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}

	session := sessionID(readStdin(o.Stdin, o.StdinTimeout))
	base, key := discover(o)
	st, errLine := fetch(o, base, key, session, sym)
	if errLine != "" {
		return errLine
	}
	return render(st, o.Now().Location(), sym)
}

// readStdin reads at most maxStdin bytes, giving up after d. Whatever arrived
// before the deadline is returned; the reader goroutine is abandoned (it ends
// when the reader is closed or the process exits).
func readStdin(r io.Reader, d time.Duration) []byte {
	if r == nil {
		return nil
	}
	type chunk struct {
		b   []byte
		end bool
	}
	ch := make(chan chunk)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer func() { _ = recover() }()
		lr := io.LimitReader(r, maxStdin)
		for {
			buf := make([]byte, 8<<10)
			n, err := lr.Read(buf)
			select {
			case ch <- chunk{buf[:n], err != nil}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	var data []byte
	for {
		select {
		case c := <-ch:
			data = append(data, c.b...)
			if c.end {
				return data
			}
		case <-timer.C:
			return data
		}
	}
}

// sessionID extracts the top-level "session_id" string. It walks tokens rather
// than unmarshalling so that truncated or partly malformed input still yields
// the id if it appears before the damage.
func sessionID(b []byte) string {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	depth, wantKey := 1, true
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if depth == 1 && wantKey {
			key, ok := tok.(string)
			if !ok { // '}' closes the top-level object
				return ""
			}
			if key == "session_id" {
				v, _ := dec.Token()
				s, _ := v.(string)
				return s
			}
			wantKey = false
			continue
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 1 {
			wantKey = true // a value at the top level just finished
		}
	}
}

// discover resolves the gateway URL and key: flags, then CBRT_*, then
// ANTHROPIC_*, then the default URL and no key. ANTHROPIC_AUTH_TOKEN is only
// used when the URL itself came from ANTHROPIC_BASE_URL, so a real Anthropic
// token is never sent to a gateway configured some other way.
func discover(o *Options) (base, key string) {
	fromAnthropic := false
	switch {
	case o.URL != "":
		base = o.URL
	case o.Getenv("CBRT_URL") != "":
		base = o.Getenv("CBRT_URL")
	case o.Getenv("ANTHROPIC_BASE_URL") != "":
		base = o.Getenv("ANTHROPIC_BASE_URL")
		fromAnthropic = true
	default:
		base = DefaultURL
	}
	switch {
	case o.Key != "":
		key = o.Key
	case o.Getenv("CBRT_API_KEY") != "":
		key = o.Getenv("CBRT_API_KEY")
	case fromAnthropic:
		key = o.Getenv("ANTHROPIC_AUTH_TOKEN")
	}
	return trimBase(base), strings.TrimSpace(key)
}

func trimBase(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, "/v1")
	return strings.TrimRight(s, "/")
}

type routeState struct {
	Route      string `json:"route"`
	LastTarget string `json:"last_target"`
	LastAt     string `json:"last_at"`
	Fallback   bool   `json:"fallback"`
	Reason     string `json:"reason"`
}

type degraded struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Until    string `json:"until"`
}

type statusResp struct {
	Routes   []routeState `json:"routes"`
	Degraded []degraded   `json:"degraded"`
	Session  *routeState  `json:"session"`
}

// fetch performs GET <base>/api/status. On failure it returns the finished
// error line instead of a status.
func fetch(o *Options, base, key, session string, sym symbols) (*statusResp, string) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "cbrt " + sym.cross + " bad url (" + clip(base, 40, sym.ellipsis) + ")"
	}
	endpoint := base + "/api/status"
	if session != "" {
		endpoint += "?session=" + url.QueryEscape(session)
	}

	ctx, cancel := context.WithTimeout(context.Background(), o.HTTPTimeout)
	defer cancel()
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "cbrt " + sym.cross + " bad url (" + clip(base, 40, sym.ellipsis) + ")"
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	unreachable := "cbrt " + sym.cross + " unreachable (" + hostPort(u) + ")"
	busy := "cbrt " + sym.busy + " busy"
	resp, err := o.Client.Do(req)
	if err != nil {
		// A timeout after the connection was made is a busy gateway; one
		// during the dial is a gateway that is not there (Windows retries
		// SYNs to a closed port for ~2s instead of failing at once).
		if isTimeout(err) && connected.Load() {
			return nil, busy
		}
		return nil, unreachable
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		if isTimeout(err) {
			return nil, busy
		}
		return nil, unreachable
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, "cbrt " + sym.cross + " auth (set CBRT_API_KEY)"
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Sprintf("cbrt %s error %d", sym.cross, resp.StatusCode)
	}
	var st statusResp
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, "cbrt " + sym.cross + " error bad response"
	}
	return &st, ""
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func hostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := "80"
	if u.Scheme == "https" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// render builds the line from three parts: what served (head), why it was a
// fallback, and the first degraded provider. When the result is too long the
// fallback reason is dropped first, so the warning survives; Run then clips.
func render(st *statusResp, loc *time.Location, sym symbols) string {
	head := "cbrt " + sym.dot + " "
	var fb, fbShort string
	switch {
	case st.Session != nil && (st.Session.Route != "" || st.Session.LastTarget != ""):
		s := st.Session
		head += routeTarget(s, sym)
		if s.Fallback {
			fbShort = " (fallback)"
			fb = fbShort
			if s.Reason != "" {
				fb = " (fallback: " + clip(s.Reason, maxReason, sym.ellipsis) + ")"
			}
		}
	default:
		if r := latest(st.Routes); r != nil {
			head += routeTarget(r, sym) + " (route-wide)"
		} else {
			head += "ready"
		}
	}

	var warn string
	var provs []degraded
	for _, d := range st.Degraded {
		if d.Target == "" && d.Provider != "" {
			provs = append(provs, d)
		}
	}
	if len(provs) > 0 {
		d := provs[0]
		reason := d.Reason
		if reason == "" {
			reason = d.Status
		}
		warn = " " + sym.warn + " " + clip(d.Provider, maxProvider, sym.ellipsis)
		if reason != "" {
			warn += " " + clip(reason, maxReason, sym.ellipsis)
		}
		if t, err := time.Parse(time.RFC3339, d.Until); err == nil {
			if loc == nil {
				loc = time.Local
			}
			warn += " until " + t.In(loc).Format("15:04")
		}
		if len(provs) > 1 {
			warn += fmt.Sprintf(" +%d", len(provs)-1)
		}
	}
	line := head + fb + warn
	if utf8.RuneCountInString(line) > maxLine {
		line = head + fbShort + warn
	}
	return strings.TrimRight(line, " ")
}

func routeTarget(r *routeState, sym symbols) string {
	route := clip(r.Route, maxRoute, sym.ellipsis)
	target := shortTarget(r.LastTarget, sym.ellipsis)
	switch {
	case route == "":
		return target
	case target == "":
		return route
	}
	return route + " " + sym.arrow + " " + target
}

// latest returns the route with the greatest last_at (unparseable times sort
// first, ties keep gateway order); nil if no route has served anything.
func latest(rs []routeState) *routeState {
	idx := make([]int, 0, len(rs))
	for i, r := range rs {
		if r.Route != "" || r.LastTarget != "" {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	at := func(i int) time.Time {
		t, _ := time.Parse(time.RFC3339, rs[i].LastAt)
		return t
	}
	sort.SliceStable(idx, func(a, b int) bool { return at(idx[a]).After(at(idx[b])) })
	return &rs[idx[0]]
}

// shortTarget keeps the tail of long target ids: "nvidia:deepseek-ai/deepseek-v4.1-flash"
// becomes "…/deepseek-v4.1-flash".
func shortTarget(s, ellipsis string) string {
	s = sanitize(s)
	if utf8.RuneCountInString(s) <= maxTarget {
		return s
	}
	if i := strings.LastIndex(s, "/"); i >= 0 && utf8.RuneCountInString(s[i:]) < maxTarget {
		return ellipsis + s[i:]
	}
	r := []rune(s)
	return ellipsis + string(r[len(r)-(maxTarget-1):])
}

// clip sanitizes s and cuts it to n runes (the ellipsis counts as one).
func clip(s string, n int, ellipsis string) string {
	s = sanitize(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + ellipsis
}

// sanitize drops control characters (a hostile or buggy response must not
// inject newlines or terminal escapes into the status line).
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
}
