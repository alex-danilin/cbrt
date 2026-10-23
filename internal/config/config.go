// Package config loads and validates the cbrt JSON configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Duration is a time.Duration that reads "30s"-style strings from JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

type Config struct {
	Listen     string `json:"listen"`
	APIKey     string `json:"api_key"`
	TLSCert    string `json:"tls_cert"`
	TLSKey     string `json:"tls_key"`
	TrustProxy bool   `json:"trust_proxy"`
	// TrustedProxies lists the peers (IPs or CIDRs) whose X-Forwarded-For is honored when
	// trust_proxy is on. Empty means loopback only (127.0.0.0/8, ::1).
	TrustedProxies []string `json:"trusted_proxies"`
	AllowedHosts   []string `json:"allowed_hosts"`
	DataDir        string   `json:"data_dir"`
	Offline        bool     `json:"offline"`
	// CodexCatalog serves Codex its own /v1/models catalog (context windows, reasoning levels).
	// Off by default: a catalog must carry base instructions, which replace Codex's built-in prompt.
	CodexCatalog bool `json:"codex_catalog"`

	RetentionMonths int        `json:"retention_months"` // 0 = default (6), -1 = keep everything
	Limits          Limits     `json:"limits"`
	Timeouts        Timeouts   `json:"timeouts"`
	Cooldowns       Cooldowns  `json:"cooldowns"`
	Visibility      Visibility `json:"visibility"`

	Providers []Provider          `json:"providers"`
	Routes    map[string][]Target `json:"routes"`

	expandErrs []error // problems found while expanding ${VAR} references
}

type Limits struct {
	MaxRequestBytes       int64 `json:"max_request_bytes"`
	MaxInflightBytes      int64 `json:"max_inflight_bytes"`
	MaxConcurrentRequests int   `json:"max_concurrent_requests"`
	MaxConcurrentStreams  int   `json:"max_concurrent_streams"`
	RequestsPerMinute     int   `json:"requests_per_minute"`
}

type Timeouts struct {
	FirstByteCloud Duration `json:"first_byte_cloud"`
	FirstByteLocal Duration `json:"first_byte_local"`
	Idle           Duration `json:"idle"`
	Write          Duration `json:"write"`
	// Read bounds the time to read a request body (default 60s).
	Read Duration `json:"read"`
}

type Cooldowns struct {
	Quota Duration `json:"quota"`
}

type Visibility struct {
	SSEComments  bool `json:"sse_comments"`
	BodyMetadata bool `json:"body_metadata"`
	// ExposeReasoning shows model reasoning to Anthropic clients as thinking blocks (default true).
	ExposeReasoning *bool `json:"expose_reasoning"`
}

// ReasoningExposed reports whether reasoning is passed to Anthropic/Responses clients.
func (v Visibility) ReasoningExposed() bool { return v.ExposeReasoning == nil || *v.ExposeReasoning }

type Provider struct {
	ID               string   `json:"id"`
	Type             string   `json:"type"`     // openai | ollama | anthropic (passthrough)
	Location         string   `json:"location"` // local | cloud
	BaseURL          string   `json:"base_url"`
	APIKey           string   `json:"api_key"`
	FirstByteTimeout Duration `json:"first_byte_timeout"`
	Enabled          *bool    `json:"enabled"`
	QuotaPatterns    []string `json:"quota_patterns"`
	AuthPatterns     []string `json:"auth_patterns"`
}

// IsEnabled reports whether the provider takes traffic (default true).
func (p Provider) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Passthrough reports whether the provider relays Anthropic Messages requests
// unchanged, with the client's own credential (type anthropic).
func (p Provider) Passthrough() bool { return p.Type == "anthropic" }

// Target is one provider/model entry in a route, with its profile.
// In JSON it may be an object or the shorthand string "provider:model".
type Target struct {
	Provider           string   `json:"provider"`
	Model              string   `json:"model"`
	Capabilities       []string `json:"capabilities"`
	MaxContext         int      `json:"max_context"`
	TokenParam         string   `json:"token_param"` // "", max_tokens, max_completion_tokens
	StreamUsage        *bool    `json:"stream_usage"`
	ToolChoiceRequired *bool    `json:"tool_choice_required"`
	FirstByteTimeout   Duration `json:"first_byte_timeout"`
	Reasoning          string   `json:"reasoning"`  // informational: reasoning_content | reasoning | none
	MidSystem          string   `json:"mid_system"` // hoist_or_user (default) | keep
}

func (t *Target) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		p, m, ok := strings.Cut(s, ":")
		if !ok || p == "" || m == "" {
			return fmt.Errorf("target shorthand must be \"provider:model\", got %q", s)
		}
		*t = Target{Provider: p, Model: m}
		return nil
	}
	type plain Target
	return json.Unmarshal(b, (*plain)(t))
}

// Key identifies the target for health tracking and display.
func (t Target) Key() string { return t.Provider + ":" + t.Model }

func (t Target) WantsStreamUsage() bool { return t.StreamUsage == nil || *t.StreamUsage }

func (t Target) SupportsToolChoiceRequired() bool {
	return t.ToolChoiceRequired == nil || *t.ToolChoiceRequired
}

// HasCapability is true when the target lists cap, or lists no capabilities at all.
func (t Target) HasCapability(cap string) bool {
	if len(t.Capabilities) == 0 {
		return true
	}
	for _, c := range t.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// Load reads path, expands ${VAR} references and applies defaults, then validates.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates a configuration document.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.expand()
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expand(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(envRef.FindStringSubmatch(m)[1])
	})
}

func (c *Config) expand() {
	for _, f := range []*string{&c.Listen, &c.APIKey, &c.TLSCert, &c.TLSKey, &c.DataDir} {
		*f = expand(*f)
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		// An unset variable in base_url must not silently fall back to the default URL
		// (inside a container that would be the container itself).
		for _, m := range envRef.FindAllStringSubmatch(p.BaseURL, -1) {
			if os.Getenv(m[1]) == "" {
				c.expandErrs = append(c.expandErrs, fmt.Errorf("provider %q: base_url references ${%s}, which is not set", p.ID, m[1]))
			}
		}
		p.BaseURL = expand(p.BaseURL)
		p.APIKey = expand(p.APIKey)
	}
	// CBRT_OFFLINE=1|true forces offline mode (handy for containers sharing one config file).
	if v := strings.ToLower(os.Getenv("CBRT_OFFLINE")); v == "1" || v == "true" || v == "yes" {
		c.Offline = true
	}
}

func (c *Config) applyDefaults() {
	def := func(v *Duration, d time.Duration) {
		if *v == 0 {
			*v = Duration(d)
		}
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.RetentionMonths == 0 {
		c.RetentionMonths = 6
	}
	l := &c.Limits
	if l.MaxRequestBytes == 0 {
		l.MaxRequestBytes = 10 << 20
	}
	if l.MaxInflightBytes == 0 {
		l.MaxInflightBytes = max(64<<20, l.MaxRequestBytes) // room for at least one maximal body
	}
	if l.MaxConcurrentRequests == 0 {
		l.MaxConcurrentRequests = 32
	}
	if l.MaxConcurrentStreams == 0 {
		l.MaxConcurrentStreams = 16
	}
	if l.RequestsPerMinute == 0 {
		l.RequestsPerMinute = 60
	}
	def(&c.Timeouts.FirstByteCloud, 60*time.Second)
	def(&c.Timeouts.FirstByteLocal, 120*time.Second)
	def(&c.Timeouts.Idle, 60*time.Second)
	def(&c.Timeouts.Write, 30*time.Second)
	def(&c.Timeouts.Read, 60*time.Second)
	def(&c.Cooldowns.Quota, time.Hour)
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Type == "" {
			p.Type = "openai"
		}
		if p.BaseURL == "" && p.Type == "ollama" {
			p.BaseURL = "http://127.0.0.1:11434/v1"
		}
		if p.Type == "anthropic" {
			if p.BaseURL == "" {
				p.BaseURL = "https://api.anthropic.com"
			}
			if p.Location == "" {
				p.Location = "cloud"
			}
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
	}
}

// Validate checks internal consistency and the security rules for exposed binds.
func (c *Config) Validate() error {
	errs := append([]error(nil), c.expandErrs...)
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	// Numeric fields, checked after defaults were applied (a zero global already got its default;
	// a zero provider/target first_byte_timeout means "inherit").
	l := c.Limits
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"limits.max_request_bytes", l.MaxRequestBytes},
		{"limits.max_inflight_bytes", l.MaxInflightBytes},
		{"limits.max_concurrent_requests", int64(l.MaxConcurrentRequests)},
		{"limits.max_concurrent_streams", int64(l.MaxConcurrentStreams)},
		{"limits.requests_per_minute", int64(l.RequestsPerMinute)},
	} {
		if f.v <= 0 {
			add("%s must be > 0, got %d", f.name, f.v)
		}
	}
	if l.MaxRequestBytes > 0 && l.MaxInflightBytes > 0 && l.MaxInflightBytes < l.MaxRequestBytes {
		add("limits.max_inflight_bytes (%d) must be >= limits.max_request_bytes (%d)", l.MaxInflightBytes, l.MaxRequestBytes)
	}
	nonNeg := func(name string, d Duration) {
		if d < 0 {
			add("%s must be >= 0, got %s", name, d.D())
		}
	}
	nonNeg("timeouts.first_byte_cloud", c.Timeouts.FirstByteCloud)
	nonNeg("timeouts.first_byte_local", c.Timeouts.FirstByteLocal)
	nonNeg("timeouts.idle", c.Timeouts.Idle)
	nonNeg("timeouts.write", c.Timeouts.Write)
	if c.Timeouts.Read <= 0 {
		add("timeouts.read must be > 0, got %s", c.Timeouts.Read.D())
	}
	nonNeg("cooldowns.quota", c.Cooldowns.Quota)
	if c.RetentionMonths < -1 {
		add("retention_months must be >= 1, or -1 to keep all usage files, got %d", c.RetentionMonths)
	}

	byID := map[string]*Provider{}
	for i := range c.Providers {
		p := &c.Providers[i]
		switch {
		case p.ID == "":
			add("providers[%d]: id is required", i)
			continue
		case strings.Contains(p.ID, ":"):
			add("provider %q: id must not contain ':'", p.ID)
		case byID[p.ID] != nil:
			add("provider %q: duplicate id", p.ID)
		}
		byID[p.ID] = p
		if p.Type != "openai" && p.Type != "ollama" && p.Type != "anthropic" {
			add("provider %q: type must be openai, ollama or anthropic", p.ID)
		}
		if p.Passthrough() {
			// The client's own credential is relayed: it must never come from the
			// config, and only this machine may send one through the gateway.
			if p.APIKey != "" {
				add("provider %q: an anthropic provider relays the client's own login; remove api_key", p.ID)
			}
			if !IsLoopback(c.Listen) || c.APIKey != "" {
				add("provider %q: an anthropic provider needs a loopback listen address and no api_key (the client's own credential is relayed)", p.ID)
			}
		}
		if p.Location != "local" && p.Location != "cloud" {
			add("provider %q: location must be local or cloud", p.ID)
		}
		if p.FirstByteTimeout < 0 {
			add("provider %q: first_byte_timeout must be >= 0, got %s", p.ID, p.FirstByteTimeout.D())
		}
		if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("provider %q: base_url %q is not an http(s) URL", p.ID, p.BaseURL)
		}
		if p.IsEnabled() && p.Location == "cloud" && !c.Offline && p.APIKey == "" && !p.Passthrough() {
			add("provider %q: api_key is empty (is the referenced environment variable set?)", p.ID)
		}
		for _, re := range append(append([]string{}, p.QuotaPatterns...), p.AuthPatterns...) {
			if _, err := regexp.Compile(re); err != nil {
				add("provider %q: bad pattern %q: %v", p.ID, re, err)
			}
		}
	}

	if len(c.Routes) == 0 {
		add("at least one route is required")
	}
	for name, targets := range c.Routes {
		if _, err := path.Match(name, ""); err != nil {
			add("route %q: invalid pattern: %v", name, err)
		}
		if len(targets) == 0 {
			add("route %q: needs at least one target", name)
		}
		for _, t := range targets {
			if byID[t.Provider] == nil {
				add("route %q: unknown provider %q", name, t.Provider)
			}
			if t.Model == "" {
				add("route %q: target for %q has no model", name, t.Provider)
			}
			if p := byID[t.Provider]; p != nil && p.Passthrough() && t.Model != "*" {
				add("route %q: target %s: an anthropic target forwards the client's model unchanged; use model \"*\"", name, t.Key())
			}
			if t.TokenParam != "" && t.TokenParam != "max_tokens" && t.TokenParam != "max_completion_tokens" {
				add("route %q: target %s: token_param must be max_tokens or max_completion_tokens", name, t.Key())
			}
			if t.MidSystem != "" && t.MidSystem != "keep" && t.MidSystem != "hoist_or_user" {
				add("route %q: target %s: mid_system must be keep or hoist_or_user", name, t.Key())
			}
			if t.FirstByteTimeout < 0 {
				add("route %q: target %s: first_byte_timeout must be >= 0, got %s", name, t.Key(), t.FirstByteTimeout.D())
			}
			if t.MaxContext < 0 {
				add("route %q: target %s: max_context must be >= 0, got %d", name, t.Key(), t.MaxContext)
			}
			for _, cp := range t.Capabilities {
				if cp != "tools" && cp != "vision" && cp != "json" {
					add("route %q: target %s: unknown capability %q", name, t.Key(), cp)
				}
			}
		}
	}

	for _, p := range c.TrustedProxies {
		if _, err := parseProxy(p); err != nil {
			add("trusted_proxies: %q is not an IP address or CIDR", p)
		}
	}
	if len(c.TrustedProxies) > 0 && !c.TrustProxy {
		add("trusted_proxies is set but trust_proxy is off: set trust_proxy or remove trusted_proxies")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		add("tls_cert and tls_key must be set together")
	}
	if !IsLoopback(c.Listen) {
		if c.APIKey == "" {
			add("listen %q is not a loopback address: api_key is required", c.Listen)
		}
		if c.TLSCert == "" && !c.TrustProxy {
			add("listen %q is not a loopback address: configure tls_cert/tls_key or set trust_proxy behind a TLS reverse proxy", c.Listen)
		}
	}
	return errors.Join(errs...)
}

// loopbackProxies is the trusted_proxies default: a reverse proxy on the same host.
var loopbackProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// parseProxy reads one trusted_proxies entry: an IP address or a CIDR.
func parseProxy(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		if a := p.Addr(); a.Is4In6() { // ::ffff:10.0.0.0/104 -> 10.0.0.0/8
			if p.Bits() < 96 {
				return netip.Prefix{}, fmt.Errorf("prefix %q is shorter than the IPv4-mapped range", s)
			}
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap().WithZone("")
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// ProxyTrusted reports whether a connection from peer may set X-Forwarded-For:
// trust_proxy is on and peer is in trusted_proxies (loopback only when that is empty).
func (c *Config) ProxyTrusted(peer netip.Addr) bool {
	if !c.TrustProxy || !peer.IsValid() {
		return false
	}
	peer = peer.Unmap().WithZone("")
	if len(c.TrustedProxies) == 0 {
		for _, p := range loopbackProxies {
			if p.Contains(peer) {
				return true
			}
		}
		return false
	}
	for _, s := range c.TrustedProxies {
		if p, err := parseProxy(s); err == nil && p.Contains(peer) {
			return true
		}
	}
	return false
}

// IsLoopback reports whether a listen address only accepts local connections.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Provider returns the provider with the given id.
func (c *Config) Provider(id string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// RouteNames returns exact (non-glob) route names, sorted.
func (c *Config) RouteNames() []string {
	var out []string
	for n := range c.Routes {
		if !strings.ContainsAny(n, "*?[") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// RestartRequired lists the fields that differ between old and new but only take effect
// on restart: they are bound at startup (listener, TLS, usage store, admission limits).
func RestartRequired(old, new *Config) []string {
	var out []string
	add := func(changed bool, name string) {
		if changed {
			out = append(out, name)
		}
	}
	add(old.Listen != new.Listen, "listen")
	add(old.TLSCert != new.TLSCert, "tls_cert")
	add(old.TLSKey != new.TLSKey, "tls_key")
	add(old.DataDir != new.DataDir, "data_dir")
	add(old.RetentionMonths != new.RetentionMonths, "retention_months")
	add(old.Limits.MaxConcurrentRequests != new.Limits.MaxConcurrentRequests, "limits.max_concurrent_requests")
	add(old.Limits.MaxConcurrentStreams != new.Limits.MaxConcurrentStreams, "limits.max_concurrent_streams")
	add(old.Limits.RequestsPerMinute != new.Limits.RequestsPerMinute, "limits.requests_per_minute")
	return out
}
