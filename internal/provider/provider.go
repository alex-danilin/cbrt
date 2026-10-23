// Package provider talks to OpenAI-compatible upstreams (NVIDIA build, Ollama, ...).
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alex-danilin/cbrt/internal/config"
)

// DialFunc opens a network connection, like (*net.Dialer).DialContext.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

var defaultDial DialFunc = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext

// dialHook, when set, replaces defaultDial for every upstream (including ones
// already built): upstreams are created deep inside the gateway, so a
// constructor option could not reach them from an end-to-end test.
var dialHook atomic.Pointer[DialFunc]

func dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if h := dialHook.Load(); h != nil {
		return (*h)(ctx, network, addr)
	}
	return defaultDial(ctx, network, addr)
}

// SetDialContextForTest routes every upstream connection (including proxy
// connections) through fn until the returned restore func is called.
// It exists so tests can assert which hosts the gateway contacts.
func SetDialContextForTest(fn DialFunc) (restore func()) {
	prev := dialHook.Swap(&fn)
	return func() { dialHook.Store(prev) }
}

// Upstream is one configured provider with its own connection pool.
type Upstream struct {
	cfg    config.Provider
	client *http.Client
}

// Option adjusts an upstream built by New.
type Option func(*options)

type options struct{ offline bool }

// Offline makes the upstream's transport refuse every connection to an
// address that is not loopback, private (RFC 1918, ULA) or link-local, after
// DNS resolution, and ignore proxy variables. It enforces offline mode at the
// network level, whatever location label the provider carries.
func Offline(on bool) Option { return func(o *options) { o.offline = on } }

// New builds an upstream client. The HTTP client has no overall timeout:
// the gateway bounds each attempt with first-byte and idle timers instead.
// It never follows a redirect: Chat and Models report a 3xx as a
// *RedirectError, Forward hands it back to the caller unchanged.
func New(p config.Provider, opts ...Option) *Upstream {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dial,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if o.offline {
		// A proxy would carry the request off the machine whatever address it has.
		tr.Proxy = nil
		tr.DialContext = offlineDial
	}
	// Never follow a redirect: it would re-send the prompt and the provider's
	// credential (or, on passthrough, the client's own login) to another host,
	// and that host's reply would be used as the answer.
	c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Upstream{cfg: p, client: c}
}

// RedirectError is returned by Chat and Models when the upstream answers with
// a redirect. Redirects are never followed; the base_url needs fixing.
type RedirectError struct {
	Provider string
	Status   int
	Host     string // the redirect target's host, empty if none was given
}

func (e *RedirectError) Error() string {
	to := e.Host
	if to == "" {
		to = "an unknown location"
	}
	return fmt.Sprintf("%s: upstream redirected (HTTP %d) to %s; redirects are not followed, fix base_url", e.Provider, e.Status, to)
}

// redirected turns a 3xx response into a *RedirectError, closing its body.
func (u *Upstream) redirected(resp *http.Response) error {
	if resp.StatusCode/100 != 3 {
		return nil
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	e := &RedirectError{Provider: u.cfg.ID, Status: resp.StatusCode}
	if loc, err := resp.Location(); err == nil {
		e.Host = loc.Host
	}
	return e
}

// lookupIP resolves a host name for the offline dialer; tests replace it.
var lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, nil
}

// LocalAddr reports whether ip stays on this machine or its local network:
// loopback, private (RFC 1918, ULA fc00::/7), link-local or unspecified.
func LocalAddr(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// offlineDial resolves addr itself and dials only the resolved IPs that are
// LocalAddr. Dialling the checked IP (not the name again) means a second,
// different DNS answer cannot slip a public address past the check.
func offlineDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if ips, err = lookupIP(ctx, host); err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		if !LocalAddr(ip) {
			continue
		}
		c, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("offline mode: refusing to connect to %s: not a loopback, private or link-local address", addr)
}

// LocalLooking reports whether a base URL's host looks like it stays on the
// local network: a LocalAddr literal, localhost, a single-label name, or a
// name under a local-only suffix. It does no DNS lookup: it only feeds a
// startup warning (offline mode itself checks the resolved addresses).
func LocalLooking(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip := net.ParseIP(h); ip != nil {
		return LocalAddr(ip)
	}
	if h == "" {
		return false
	}
	if h == "localhost" || !strings.Contains(h, ".") {
		return true
	}
	for _, sfx := range []string{".localhost", ".local", ".lan", ".internal", ".home.arpa"} {
		if strings.HasSuffix(h, sfx) {
			return true
		}
	}
	return false
}

func (u *Upstream) ID() string              { return u.cfg.ID }
func (u *Upstream) Location() string        { return u.cfg.Location }
func (u *Upstream) Config() config.Provider { return u.cfg }

// Chat sends an already-encoded Chat Completions body.
func (u *Upstream) Chat(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	u.auth(req)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	if err := u.redirected(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// Forward relays a client's Anthropic Messages request unchanged: the same
// path, query and body, and the client's headers (its own credential included)
// minus those that only concern the hop to cbrt. The provider's own key is
// never added. Used only for passthrough (type anthropic) providers.
func (u *Upstream) Forward(ctx context.Context, path, rawQuery string, h http.Header, body []byte) (*http.Response, error) {
	target := u.cfg.BaseURL + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range h {
		if !ForwardRequestHeader(k) {
			continue
		}
		req.Header[k] = append([]string(nil), vs...)
	}
	return u.client.Do(req)
}

// ForwardRequestHeader reports whether a client request header is relayed by
// Forward. Everything is relayed (Claude Code adds anthropic-* and
// x-claude-code-* headers over releases) except hop-by-hop headers, headers
// about the connection to cbrt, browser context, and cbrt's own.
// Accept-Encoding is dropped so the transport negotiates (and decodes)
// compression itself; the gateway reads the usage from the body.
func ForwardRequestHeader(name string) bool {
	k := http.CanonicalHeaderKey(name)
	switch k {
	case "Connection", "Keep-Alive", "Proxy-Authorization", "Proxy-Authenticate", "Proxy-Connection",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Host", "Content-Length", "Accept-Encoding",
		"Cookie", "Origin", "Referer", "Forwarded", "X-Real-Ip":
		return false
	}
	for _, p := range []string{"X-Forwarded-", "Sec-", "X-Cbrt-"} {
		if strings.HasPrefix(k, p) {
			return false
		}
	}
	return true
}

// ForwardResponseHeader reports whether an upstream response header is relayed
// to the client on passthrough. Content-Length and Content-Encoding no longer
// describe the (decoded, re-framed) body; cookies are the upstream's business.
func ForwardResponseHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Connection", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade", "Content-Length", "Content-Encoding", "Set-Cookie", "Alt-Svc":
		return false
	}
	return true
}

// Models lists the model ids the upstream advertises (GET /models).
// Being listed does not guarantee the account may use a model.
func (u *Upstream) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.cfg.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	u.auth(req)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	if err := u.redirected(resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("%s: list models: HTTP %d", u.cfg.ID, resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: list models: %w", u.cfg.ID, err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// CloseIdle releases pooled connections (used when a config reload replaces the upstream).
func (u *Upstream) CloseIdle() { u.client.CloseIdleConnections() }

func (u *Upstream) auth(req *http.Request) {
	if u.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.cfg.APIKey)
	}
}
