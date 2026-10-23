package router

import (
	"sort"
	"sync"
	"time"
)

// Health parameters.
const (
	BackoffBase     = 30 * time.Second
	BackoffMax      = 5 * time.Minute
	BreakerFailures = 3
	BreakerWindow   = 60 * time.Second
	BreakerOpen     = 60 * time.Second
)

// Health tracks cooldowns of providers and targets. It is safe for
// concurrent use.
type Health struct {
	mu        sync.Mutex
	now       func() time.Time
	providers map[string]*provState
	targets   map[targetKey]*coolState
	// epoch counts cooldown entries and extensions. Each cooldown is stamped
	// with the epoch that started or last extended it, and Check hands out the
	// current epoch, so Succeed can tell a cooldown entered before the request
	// was admitted (cleared) from one entered while it was in flight (kept).
	epoch uint64
}

type targetKey struct{ provider, target string }

// coolState is a cooldown that stays "half-open" after it expires until a
// probe confirms health.
type coolState struct {
	until   time.Time
	reason  Class
	cooling bool   // a cooldown was entered and not yet confirmed healthy
	probing bool   // a half-open probe is in flight
	stamp   uint64 // Health.epoch when the cooldown was entered or last extended
}

type provState struct {
	coolState
	disabled bool
	backoff  time.Duration // last Unreachable/Timeout backoff
	failures []time.Time   // UpstreamError times within the breaker window
}

// Check is the result of Health.Check.
type Check struct {
	OK     bool
	Probe  bool // the caller holds the half-open probe and must report back
	Reason Class
	Until  time.Time
	Scope  Scope
	Epoch  uint64 // pass to Succeed; set on every result
}

// State describes one non-healthy provider or target.
type State struct {
	Provider string     `json:"provider"`
	Target   string     `json:"target,omitempty"`
	Status   string     `json:"status"` // "cooldown" | "half_open" | "disabled"
	Reason   Class      `json:"reason,omitempty"`
	Until    *time.Time `json:"until,omitempty"`
}

// NewHealth returns an empty Health using now as its clock (time.Now if nil).
func NewHealth(now func() time.Time) *Health {
	if now == nil {
		now = time.Now
	}
	return &Health{now: now, providers: map[string]*provState{}, targets: map[targetKey]*coolState{}}
}

// Check reports whether a target may be tried now. After a cooldown expires
// exactly one caller gets a half-open probe (OK and Probe); others are
// refused with the previous reason until the probe reports Succeed, Fail or
// Release. For a refused half-open check, Until is the expired cooldown end.
// Epoch identifies the admission for Succeed.
func (h *Health) Check(provider, target string) Check {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.check(provider, target)
	c.Epoch = h.epoch
	return c
}

func (h *Health) check(provider, target string) Check {
	now := h.now()
	p := h.providers[provider]
	t := h.targets[targetKey{provider, target}]
	if p != nil {
		if p.disabled {
			return Check{Reason: AuthError, Scope: ScopeProvider}
		}
		if p.cooling && now.Before(p.until) {
			return Check{Reason: p.reason, Until: p.until, Scope: ScopeProvider}
		}
	}
	if t != nil && t.cooling && now.Before(t.until) {
		return Check{Reason: t.reason, Until: t.until, Scope: ScopeTarget}
	}
	var half []*coolState
	if p != nil && p.cooling {
		if p.probing {
			return Check{Reason: p.reason, Until: p.until, Scope: ScopeProvider}
		}
		half = append(half, &p.coolState)
	}
	if t != nil && t.cooling {
		if t.probing {
			return Check{Reason: t.reason, Until: t.until, Scope: ScopeTarget}
		}
		half = append(half, t)
	}
	if len(half) == 0 {
		return Check{OK: true}
	}
	for _, c := range half {
		c.probing = true
	}
	return Check{OK: true, Probe: true}
}

// Fail applies a verdict. entered is true when this call started a new
// cooldown or disable (or changed its reason); until is its end (zero for a
// disable or when nothing was entered).
func (h *Health) Fail(provider, target string, v Verdict) (entered bool, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.release(provider, target)

	switch {
	case v.Class == AuthError:
		p := h.provider(provider)
		if p.disabled {
			return false, time.Time{}
		}
		p.disabled = true
		return true, time.Time{}

	case v.Class == Unreachable || (v.Class == Timeout && v.Scope != ScopeTarget):
		p := h.provider(provider)
		if p.active(now) {
			return false, time.Time{} // concurrent failures don't escalate
		}
		base := v.Cooldown
		if base <= 0 {
			base = BackoffBase
		}
		if p.backoff == 0 {
			p.backoff = base
		} else {
			p.backoff = min(p.backoff*2, BackoffMax)
		}
		p.backoff = min(max(p.backoff, base), BackoffMax)
		return p.enter(&h.epoch, now, p.backoff, v.Class)

	case v.Class == UpstreamError && v.Cooldown <= 0:
		p := h.provider(provider)
		if p.active(now) {
			return false, time.Time{}
		}
		if p.cooling { // failed half-open probe: reopen at once
			p.failures = nil
			return p.enter(&h.epoch, now, BreakerOpen, v.Class)
		}
		kept := p.failures[:0]
		for _, f := range p.failures {
			if now.Sub(f) < BreakerWindow {
				kept = append(kept, f)
			}
		}
		p.failures = append(kept, now)
		if len(p.failures) < BreakerFailures {
			return false, time.Time{}
		}
		p.failures = nil
		return p.enter(&h.epoch, now, BreakerOpen, v.Class)

	case v.Scope == ScopeProvider && v.Cooldown > 0:
		return h.provider(provider).enter(&h.epoch, now, v.Cooldown, v.Class)

	case v.Scope == ScopeTarget && v.Cooldown > 0:
		k := targetKey{provider, target}
		t := h.targets[k]
		if t == nil {
			t = &coolState{}
			h.targets[k] = t
		}
		return t.enter(&h.epoch, now, v.Cooldown, v.Class)
	}
	return false, time.Time{}
}

// Succeed reports a successful request admitted by a Check that returned
// epoch. It clears the target's and provider's cooldown state, but only a
// cooldown entered (or last extended) no later than epoch: one that a
// concurrent request entered while this request was in flight is newer
// evidence and is kept. A half-open probe is admitted after its cooldown
// began, so its success still clears it (a request that predates a cooldown
// never holds that cooldown's probe, so a kept state keeps its probe too).
// The provider's backoff is reset only when its cooldown was cleared or there
// was none. The breaker failure window and a disable are untouched.
// recovered is true if a cleared state was cooling down or half-open.
func (h *Health) Succeed(provider, target string, epoch uint64) (recovered bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := targetKey{provider, target}
	if t := h.targets[k]; t != nil && t.stamp <= epoch {
		recovered = t.cooling
		delete(h.targets, k)
	}
	if p := h.providers[provider]; p != nil && p.stamp <= epoch {
		recovered = recovered || p.cooling
		p.coolState = coolState{}
		p.backoff = 0
		if !p.disabled && len(p.failures) == 0 {
			delete(h.providers, provider)
		}
	}
	return recovered
}

// Release ends a probe without a verdict (e.g. the client cancelled) so
// another caller may probe.
func (h *Health) Release(provider, target string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.release(provider, target)
}

// ResetAuth re-enables providers disabled by AuthError (config reload).
func (h *Health) ResetAuth() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.providers {
		p.disabled = false
	}
}

// Degraded lists every non-healthy provider and target, sorted by provider
// then target (provider entries first).
func (h *Health) Degraded() []State {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	var out []State
	for name, p := range h.providers {
		if p.disabled {
			out = append(out, State{Provider: name, Status: "disabled", Reason: AuthError})
		} else if s, ok := p.state(now); ok {
			s.Provider = name
			out = append(out, s)
		}
	}
	for k, t := range h.targets {
		if s, ok := t.state(now); ok {
			s.Provider, s.Target = k.provider, k.target
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Target < out[j].Target
	})
	return out
}

func (h *Health) provider(name string) *provState {
	p := h.providers[name]
	if p == nil {
		p = &provState{}
		h.providers[name] = p
	}
	return p
}

func (h *Health) release(provider, target string) {
	if p := h.providers[provider]; p != nil {
		p.probing = false
	}
	if t := h.targets[targetKey{provider, target}]; t != nil {
		t.probing = false
	}
}

// active reports whether a cooldown is running (not yet expired).
func (c *coolState) active(now time.Time) bool {
	return c.cooling && now.Before(c.until)
}

// enter starts or extends a cooldown for d, stamping it with ++*epoch. A
// running cooldown is only extended; entered is true when none was running or
// the reason changed.
func (c *coolState) enter(epoch *uint64, now time.Time, d time.Duration, reason Class) (bool, time.Time) {
	until := now.Add(d)
	if c.active(now) {
		if !until.After(c.until) {
			return false, time.Time{}
		}
		*epoch++
		changed := c.reason != reason
		c.until, c.reason, c.stamp = until, reason, *epoch
		if !changed {
			return false, time.Time{}
		}
		return true, until
	}
	*epoch++
	c.until, c.reason, c.cooling, c.probing, c.stamp = until, reason, true, false, *epoch
	return true, until
}

func (c *coolState) state(now time.Time) (State, bool) {
	if !c.cooling {
		return State{}, false
	}
	until := c.until
	s := State{Status: "half_open", Reason: c.reason, Until: &until}
	if now.Before(c.until) {
		s.Status = "cooldown"
	}
	return s, true
}
