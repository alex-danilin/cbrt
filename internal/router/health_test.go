package router

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func v(class Class, scope Scope, cd time.Duration) Verdict {
	return Verdict{Class: class, Scope: scope, Cooldown: cd}
}

func TestCooldownPerClass(t *testing.T) {
	tests := []struct {
		name      string
		verdict   Verdict
		wantScope Scope
		wantFor   time.Duration // 0 = no cooldown
	}{
		{"quota", v(QuotaExhausted, ScopeProvider, time.Hour), ScopeProvider, time.Hour},
		{"rate limited", v(RateLimited, ScopeProvider, 17*time.Second), ScopeProvider, 17 * time.Second},
		{"timeout", v(Timeout, ScopeProvider, 30*time.Second), ScopeProvider, 30 * time.Second},
		{"first-byte timeout", v(Timeout, ScopeTarget, 30*time.Second), ScopeTarget, 30 * time.Second},
		{"unreachable", v(Unreachable, ScopeProvider, 0), ScopeProvider, 30 * time.Second},
		{"model unavailable", v(ModelUnavailable, ScopeTarget, 10*time.Minute), ScopeTarget, 10 * time.Minute},
		{"upstream 4xx", v(Upstream4xx, ScopeTarget, 5*time.Minute), ScopeTarget, 5 * time.Minute},
		{"rejected", v(UpstreamRejected, ScopeNone, 0), ScopeNone, 0},
		{"context", v(ContextExceeded, ScopeNone, 0), ScopeNone, 0},
		{"single upstream error", v(UpstreamError, ScopeProvider, 0), ScopeNone, 0},
		{"cancelled", v(ClientCancelled, ScopeNone, 0), ScopeNone, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newClock()
			h := NewHealth(clk.Now)
			entered, until := h.Fail("p", "m", tt.verdict)
			if tt.wantFor == 0 {
				if entered || !h.Check("p", "m").OK || len(h.Degraded()) != 0 {
					t.Fatalf("unexpected cooldown")
				}
				return
			}
			if !entered || !until.Equal(clk.Now().Add(tt.wantFor)) {
				t.Fatalf("entered=%v until=%v", entered, until)
			}
			c := h.Check("p", "m")
			if c.OK || c.Reason != tt.verdict.Class || c.Scope != tt.wantScope || !c.Until.Equal(until) {
				t.Fatalf("check %+v", c)
			}
			clk.Advance(tt.wantFor - time.Millisecond)
			if h.Check("p", "m").OK {
				t.Fatal("OK before expiry")
			}
			clk.Advance(time.Millisecond)
			if c := h.Check("p", "m"); !c.OK || !c.Probe {
				t.Fatalf("after expiry %+v", c)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	clk := newClock()
	h := NewHealth(clk.Now)
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, d := range want {
		entered, until := h.Fail("p", "m", v(Unreachable, ScopeProvider, 0))
		if !entered || until.Sub(clk.Now()) != d {
			t.Fatalf("step %d: entered=%v backoff=%v, want %v", i, entered, until.Sub(clk.Now()), d)
		}
		// A concurrent failure during the cooldown does not escalate.
		if e, _ := h.Fail("p", "other", v(Timeout, ScopeProvider, 30*time.Second)); e {
			t.Fatalf("step %d: escalated during cooldown", i)
		}
		clk.Advance(d)
		if c := h.Check("p", "m"); !c.Probe {
			t.Fatalf("step %d: no probe %+v", i, c)
		}
	}
	if !succeedNow(h, "p", "m") {
		t.Fatal("recovered = false")
	}
	if _, until := h.Fail("p", "m", v(Timeout, ScopeProvider, 30*time.Second)); until.Sub(clk.Now()) != 30*time.Second {
		t.Fatalf("backoff not reset: %v", until.Sub(clk.Now()))
	}
}

func TestBreaker(t *testing.T) {
	ue := v(UpstreamError, ScopeProvider, 0)
	t.Run("threshold", func(t *testing.T) {
		clk := newClock()
		h := NewHealth(clk.Now)
		for i := range 2 {
			if e, _ := h.Fail("p", "m", ue); e {
				t.Fatalf("opened after %d", i+1)
			}
			clk.Advance(10 * time.Second)
		}
		if !h.Check("p", "m").OK {
			t.Fatal("blocked before threshold")
		}
		entered, until := h.Fail("p", "x", ue)
		if !entered || until.Sub(clk.Now()) != 60*time.Second {
			t.Fatalf("entered=%v until=%v", entered, until)
		}
		if c := h.Check("p", "m"); c.OK || c.Reason != UpstreamError || c.Scope != ScopeProvider {
			t.Fatalf("check %+v", c)
		}
		clk.Advance(60 * time.Second)
		c := h.Check("p", "m")
		if !c.Probe {
			t.Fatalf("no probe %+v", c)
		}
		// Failed probe reopens immediately.
		if e, _ := h.Fail("p", "m", ue); !e {
			t.Fatal("failed probe did not reopen")
		}
		clk.Advance(60 * time.Second)
		h.Check("p", "m")
		if !succeedNow(h, "p", "m") || !h.Check("p", "m").OK {
			t.Fatal("not recovered")
		}
	})
	t.Run("window expiry", func(t *testing.T) {
		clk := newClock()
		h := NewHealth(clk.Now)
		h.Fail("p", "m", ue)
		clk.Advance(30 * time.Second)
		h.Fail("p", "m", ue)
		clk.Advance(31 * time.Second) // first failure leaves the window
		if e, _ := h.Fail("p", "m", ue); e {
			t.Fatal("opened with an expired failure")
		}
		clk.Advance(time.Second)
		if e, _ := h.Fail("p", "m", ue); !e {
			t.Fatal("did not open with 3 failures in window")
		}
	})
	t.Run("window cleared on open", func(t *testing.T) {
		clk := newClock()
		h := NewHealth(clk.Now)
		for range 3 {
			h.Fail("p", "m", ue)
		}
		clk.Advance(60 * time.Second)
		if !h.Check("p", "m").Probe || !succeedNow(h, "p", "m") {
			t.Fatal("probe/succeed")
		}
		if e, _ := h.Fail("p", "m", ue); e {
			t.Fatal("window not cleared")
		}
	})
}

func TestHalfOpenSingleProbe(t *testing.T) {
	for _, scope := range []Scope{ScopeProvider, ScopeTarget} {
		t.Run(scope.String(), func(t *testing.T) {
			clk := newClock()
			h := NewHealth(clk.Now)
			class := RateLimited
			if scope == ScopeTarget {
				class = ModelUnavailable
			}
			h.Fail("p", "m", v(class, scope, time.Minute))
			clk.Advance(time.Minute)

			var probes, refused atomic.Int32
			var wg sync.WaitGroup
			for range 50 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c := h.Check("p", "m")
					switch {
					case c.OK && c.Probe:
						probes.Add(1)
					case !c.OK && c.Reason == class:
						refused.Add(1)
					}
				}()
			}
			wg.Wait()
			if probes.Load() != 1 || refused.Load() != 49 {
				t.Fatalf("probes=%d refused=%d", probes.Load(), refused.Load())
			}
			if s := h.Degraded(); len(s) != 1 || s[0].Status != "half_open" {
				t.Fatalf("degraded %+v", s)
			}

			h.Release("p", "m")
			if c := h.Check("p", "m"); !c.Probe {
				t.Fatalf("no new probe after Release: %+v", c)
			}
			if h.Check("p", "m").OK {
				t.Fatal("second probe granted")
			}
			if !succeedNow(h, "p", "m") {
				t.Fatal("recovered = false")
			}
			if succeedNow(h, "p", "m") {
				t.Fatal("recovered twice")
			}
			if c := h.Check("p", "m"); !c.OK || c.Probe {
				t.Fatalf("after recovery %+v", c)
			}
			if len(h.Degraded()) != 0 {
				t.Fatal("still degraded")
			}
		})
	}
}

func TestScopeNoneReleasesProbe(t *testing.T) {
	clk := newClock()
	h := NewHealth(clk.Now)
	h.Fail("p", "m", v(RateLimited, ScopeProvider, time.Second))
	clk.Advance(time.Second)
	if !h.Check("p", "m").Probe {
		t.Fatal("no probe")
	}
	if e, _ := h.Fail("p", "m", v(UpstreamRejected, ScopeNone, 0)); e {
		t.Fatal("entered on ScopeNone")
	}
	if !h.Check("p", "m").Probe {
		t.Fatal("probe not released")
	}
}

func TestAuthDisable(t *testing.T) {
	clk := newClock()
	h := NewHealth(clk.Now)
	entered, until := h.Fail("p", "m", v(AuthError, ScopeProvider, 0))
	if !entered || !until.IsZero() {
		t.Fatalf("entered=%v until=%v", entered, until)
	}
	if e, _ := h.Fail("p", "m", v(AuthError, ScopeProvider, 0)); e {
		t.Fatal("entered twice")
	}
	clk.Advance(24 * time.Hour)
	c := h.Check("p", "other")
	if c.OK || c.Reason != AuthError || !c.Until.IsZero() || c.Scope != ScopeProvider {
		t.Fatalf("check %+v", c)
	}
	s := h.Degraded()
	if len(s) != 1 || s[0].Status != "disabled" || s[0].Until != nil {
		t.Fatalf("degraded %+v", s)
	}
	h.ResetAuth()
	if c := h.Check("p", "m"); !c.OK {
		t.Fatalf("after reset %+v", c)
	}
}

func TestScopes(t *testing.T) {
	clk := newClock()
	h := NewHealth(clk.Now)
	h.Fail("p", "a", v(ModelUnavailable, ScopeTarget, 10*time.Minute))
	if h.Check("p", "a").OK {
		t.Fatal("target a not blocked")
	}
	if c := h.Check("p", "b"); !c.OK || c.Probe {
		t.Fatalf("target b blocked: %+v", c)
	}
	if !h.Check("q", "a").OK {
		t.Fatal("other provider's target blocked")
	}
	h.Fail("p", "b", v(QuotaExhausted, ScopeProvider, time.Hour))
	for _, tgt := range []string{"a", "b", "c"} {
		if c := h.Check("p", tgt); c.OK || c.Scope != ScopeProvider || c.Reason != QuotaExhausted {
			t.Fatalf("%s: %+v", tgt, c)
		}
	}
	if !h.Check("q", "b").OK {
		t.Fatal("provider q blocked")
	}
	// Provider expires first: target a is still cooling.
	clk.Advance(time.Hour - time.Minute)
	h.Fail("p", "a", v(ModelUnavailable, ScopeTarget, 10*time.Minute))
	clk.Advance(time.Minute)
	if c := h.Check("p", "a"); c.OK || c.Scope != ScopeTarget {
		t.Fatalf("a: %+v", c)
	}
	if c := h.Check("p", "b"); !c.Probe {
		t.Fatalf("b: %+v", c)
	}
}

func TestExtendAndReason(t *testing.T) {
	clk := newClock()
	h := NewHealth(clk.Now)
	h.Fail("p", "m", v(RateLimited, ScopeProvider, time.Minute))
	if e, _ := h.Fail("p", "m", v(RateLimited, ScopeProvider, 30*time.Second)); e {
		t.Fatal("shorter cooldown entered")
	}
	e, until := h.Fail("p", "m", v(QuotaExhausted, ScopeProvider, time.Hour))
	if !e || until.Sub(clk.Now()) != time.Hour {
		t.Fatalf("quota not applied: %v %v", e, until)
	}
	if c := h.Check("p", "m"); c.Reason != QuotaExhausted {
		t.Fatalf("reason %v", c.Reason)
	}
}

func TestDegradedSortedJSON(t *testing.T) {
	clk := newClock()
	h := NewHealth(clk.Now)
	h.Fail("z", "m", v(RateLimited, ScopeProvider, time.Minute))
	h.Fail("a", "y", v(ModelUnavailable, ScopeTarget, time.Minute))
	h.Fail("a", "x", v(Upstream4xx, ScopeTarget, time.Minute))
	h.Fail("a", "", v(Timeout, ScopeProvider, 30*time.Second))
	s := h.Degraded()
	var got []string
	for _, st := range s {
		got = append(got, st.Provider+"/"+st.Target+"/"+st.Status)
	}
	want := []string{"a//cooldown", "a/x/cooldown", "a/y/cooldown", "z//cooldown"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	b, _ := json.Marshal(s[0])
	if string(b) != `{"provider":"a","status":"cooldown","reason":"timeout","until":"2026-09-29T12:00:30Z"}` {
		t.Fatalf("json %s", b)
	}
}

func TestSucceedHealthyNotRecovered(t *testing.T) {
	h := NewHealth(nil)
	if succeedNow(h, "p", "m") {
		t.Fatal("recovered on healthy")
	}
	h.Fail("p", "m", v(UpstreamError, ScopeProvider, 0)) // breaker failure only
	if succeedNow(h, "p", "m") {
		t.Fatal("recovered without cooldown")
	}
}

// succeedNow reports a success for a request admitted at the current epoch.
func succeedNow(h *Health, provider, target string) bool {
	h.mu.Lock()
	e := h.epoch
	h.mu.Unlock()
	return h.Succeed(provider, target, e)
}

// A request admitted while healthy must not wipe a cooldown that a concurrent
// request entered while it was in flight.
func TestStaleSuccessKeepsNewerCooldown(t *testing.T) {
	for _, scope := range []Scope{ScopeProvider, ScopeTarget} {
		t.Run(scope.String(), func(t *testing.T) {
			clk := newClock()
			h := NewHealth(clk.Now)
			a, b := h.Check("p", "m"), h.Check("p", "m")
			if !a.OK || !b.OK {
				t.Fatal("not admitted")
			}
			class := QuotaExhausted
			if scope == ScopeTarget {
				class = ModelUnavailable
			}
			if e, _ := h.Fail("p", "m", v(class, scope, time.Hour)); !e {
				t.Fatal("A did not enter a cooldown")
			}
			if h.Succeed("p", "m", b.Epoch) {
				t.Fatal("stale success reported recovery")
			}
			if c := h.Check("p", "m"); c.OK || c.Reason != class {
				t.Fatalf("cooldown wiped: %+v", c)
			}
			// The half-open probe after expiry still recovers.
			clk.Advance(time.Hour)
			c := h.Check("p", "m")
			if !c.Probe {
				t.Fatalf("no probe %+v", c)
			}
			if !h.Succeed("p", "m", c.Epoch) {
				t.Fatal("probe did not recover")
			}
			if c := h.Check("p", "m"); !c.OK || c.Probe || len(h.Degraded()) != 0 {
				t.Fatalf("after recovery %+v", c)
			}
		})
	}
}

// A stale success does not reset the backoff of a cooldown entered after the
// request was admitted.
func TestStaleSuccessKeepsBackoff(t *testing.T) {
	clk := newClock()
	h := NewHealth(clk.Now)
	stale := h.Check("p", "m")
	h.Fail("p", "m", v(Unreachable, ScopeProvider, 0)) // 30s
	if h.Succeed("p", "m", stale.Epoch) {
		t.Fatal("stale success recovered")
	}
	clk.Advance(30 * time.Second)
	if !h.Check("p", "m").Probe {
		t.Fatal("no probe")
	}
	if _, until := h.Fail("p", "m", v(Unreachable, ScopeProvider, 0)); until.Sub(clk.Now()) != 60*time.Second {
		t.Fatalf("backoff reset by stale success: %v", until.Sub(clk.Now()))
	}
	// An extension of a running cooldown is newer evidence too.
	mid := h.Check("p", "x") // refused, but carries the epoch
	h.Fail("p", "m", v(QuotaExhausted, ScopeProvider, time.Hour))
	if h.Succeed("p", "m", mid.Epoch) || h.Check("p", "m").OK {
		t.Fatal("extended cooldown wiped")
	}
}

func TestSucceedNoCooldown(t *testing.T) {
	h := NewHealth(nil)
	c := h.Check("p", "m")
	if !c.OK || h.Succeed("p", "m", c.Epoch) || !h.Check("p", "m").OK {
		t.Fatal("plain success")
	}
	// A cooldown on another target of the provider is left alone.
	h.Fail("p", "x", v(ModelUnavailable, ScopeTarget, time.Hour))
	c = h.Check("p", "m")
	if !c.OK || h.Succeed("p", "m", c.Epoch) || h.Check("p", "x").OK {
		t.Fatal("other target touched")
	}
}

// A slow model (first-byte timeout, target scope) must not block other models
// of the same provider.
func TestTargetTimeoutKeepsProviderUsable(t *testing.T) {
	h := NewHealth(newClock().Now)
	h.Fail("nvidia", "nvidia:slow", v(Timeout, ScopeTarget, 30*time.Second))
	if h.Check("nvidia", "nvidia:slow").OK {
		t.Error("slow target should cool down")
	}
	if !h.Check("nvidia", "nvidia:fast").OK {
		t.Error("other targets of the provider must stay usable")
	}
}
