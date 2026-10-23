package server

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIPBehindProxy(t *testing.T) {
	direct, _ := newServer(t, "")
	proxied, _ := newServer(t, `, "trust_proxy": true`)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "198.51.100.66, 203.0.113.9")
	if got := direct.clientIP(r); got != "127.0.0.1" {
		t.Errorf("without trust_proxy X-Forwarded-For must be ignored, got %s", got)
	}
	if got := proxied.clientIP(r); got != "203.0.113.9" {
		t.Errorf("from a loopback proxy the proxy-appended address must be used, got %s", got)
	}
	r.Header.Set("X-Forwarded-For", "garbage")
	if got := proxied.clientIP(r); got != "127.0.0.1" {
		t.Errorf("invalid X-Forwarded-For must fall back to RemoteAddr, got %s", got)
	}
}

func TestClientIPIgnoresUntrustedPeer(t *testing.T) {
	// Default trusted_proxies is loopback only: a direct client can't pick its address.
	s, _ := newServer(t, `, "trust_proxy": true`)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "198.51.100.7:4000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := s.clientIP(r); got != "198.51.100.7" {
		t.Errorf("spoofed X-Forwarded-For from an untrusted peer was honored: %s", got)
	}
	r.RemoteAddr = "[2001:db8::5]:4000"
	if got := s.clientIP(r); got != "2001:db8::5" {
		t.Errorf("IPv6 untrusted peer: got %s", got)
	}
}

func TestClientIPTrustedProxies(t *testing.T) {
	s, _ := newServer(t, `, "trust_proxy": true, "trusted_proxies": ["172.16.0.0/12", "192.0.2.1"]`)
	cases := []struct{ peer, xff, want string }{
		{"172.18.0.3:1", "203.0.113.9", "203.0.113.9"},                    // proxy on the Docker network
		{"192.0.2.1:1", "198.51.100.1, 203.0.113.9", "203.0.113.9"},       // client-written entries ignored
		{"172.18.0.3:1", "203.0.113.9, 192.0.2.1", "203.0.113.9"},         // chain of trusted hops skipped
		{"127.0.0.1:1", "203.0.113.9", "127.0.0.1"},                       // loopback is no longer implied
		{"198.51.100.7:1", "203.0.113.9", "198.51.100.7"},                 // untrusted peer
		{"[::ffff:172.18.0.3]:1", "203.0.113.9", "203.0.113.9"},           // IPv4-mapped peer
		{"172.18.0.3:1", "", "172.18.0.3"},                                // no header
		{"172.18.0.3:1", "192.0.2.1", "192.0.2.1"},                        // only trusted hops
		{"172.18.0.3:1", "203.0.113.9, nonsense, 192.0.2.1", "192.0.2.1"}, // stop at garbage
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := s.clientIP(r); got != c.want {
			t.Errorf("peer %s xff %q: got %s, want %s", c.peer, c.xff, got, c.want)
		}
	}
}

func TestLoginKeyIPv6Slash64(t *testing.T) {
	if a, b := loginKey("2001:db8:1:2::1"), loginKey("2001:db8:1:2:ffff::9"); a != b {
		t.Errorf("same /64 must share a key: %s vs %s", a, b)
	}
	if a, b := loginKey("2001:db8:1:2::1"), loginKey("2001:db8:1:3::1"); a == b {
		t.Errorf("different /64s must not share a key: %s", a)
	}
	if got := loginKey("::ffff:192.0.2.4"); got != "192.0.2.4" {
		t.Errorf("IPv4-mapped address: got %s", got)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newTestLimiter(c *fakeClock) *loginLimiter {
	l := newLoginLimiter(5, time.Minute)
	l.now = c.now
	return l
}

func TestLoginLimiterThrottlesClient(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(c)
	for i := range 5 {
		if !l.allow("192.0.2.1") {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if l.allow("192.0.2.1") {
		t.Fatal("sixth attempt in the window must be refused")
	}
	if l.allow("2001:db8::1") != true || l.allow("192.0.2.2") != true {
		t.Fatal("other clients must not be affected")
	}
	c.add(time.Minute)
	if !l.allow("192.0.2.1") {
		t.Fatal("attempts must be allowed again after the window")
	}
}

func TestLoginLimiterMapBounded(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(c)
	l.maxKeys = 100
	// An active client uses up its bucket first.
	for range 5 {
		l.allow("192.0.2.1")
	}
	for round := range 5 {
		for i := range 500 {
			l.allow(fmt.Sprintf("10.%d.%d.%d", round, i/256, i%256))
		}
		if len(l.hits) > l.maxKeys {
			t.Fatalf("round %d: %d keys, cap %d", round, len(l.hits), l.maxKeys)
		}
		// Within the window the active client stays throttled: a full map never evicts it.
		if round == 0 && l.allow("192.0.2.1") {
			t.Fatal("a flood of new addresses reset the active client's bucket")
		}
		c.add(time.Minute)
	}
	c.add(time.Minute)
	l.allow("192.0.2.200")
	if len(l.hits) != 1 {
		t.Fatalf("stale keys not swept: %d keys", len(l.hits))
	}
}

func TestLoginLimiterGlobalFailureCap(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(c)
	allowed := 0
	for i := range 1000 { // a new address for every guess
		if l.allow(fmt.Sprintf("198.51.%d.%d", i/256, i%256)) {
			allowed++
			l.failed()
		}
	}
	if allowed != maxLoginFailures {
		t.Fatalf("address rotation got %d guesses in one window, cap %d", allowed, maxLoginFailures)
	}
	if l.allow("2001:db8::1") {
		t.Fatal("global cap must refuse every client")
	}
	c.add(time.Minute)
	if !l.allow("2001:db8::1") {
		t.Fatal("global cap must lift after the window")
	}
}
