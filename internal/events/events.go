// Package events records routing events (switches, cooldowns, recoveries,
// disabled providers) in a bounded in-memory feed, logs them and forwards
// them to an optional sink such as the usage store.
package events

import (
	"log/slog"
	"sync"
	"time"
)

// Kind classifies an event.
type Kind string

const (
	KindSwitch   Kind = "switch"   // a route is now served by a different target than before
	KindCooldown Kind = "cooldown" // a provider/target entered cooldown
	KindRecovery Kind = "recovery" // a provider/target served successfully again after cooldown
	KindDisabled Kind = "disabled" // provider disabled (e.g. auth error) until reload
)

// Event is one routing state change. It carries allow-listed fields only.
type Event struct {
	Time     time.Time  `json:"time"`
	Kind     Kind       `json:"kind"`
	Route    string     `json:"route,omitempty"`
	From     string     `json:"from,omitempty"` // target key "provider:model"
	To       string     `json:"to,omitempty"`
	Provider string     `json:"provider,omitempty"`
	Target   string     `json:"target,omitempty"`
	Reason   string     `json:"reason,omitempty"` // failure class, e.g. "quota_exhausted"
	Detail   string     `json:"detail,omitempty"` // redacted upstream message
	RetryAt  *time.Time `json:"retry_at,omitempty"`
}

// Hub keeps the most recent events in a ring buffer. It is safe for
// concurrent use.
type Hub struct {
	logger *slog.Logger
	sink   func(Event)
	now    func() time.Time

	mu   sync.Mutex
	buf  []Event
	next int // index of the slot the next event is written to
	n    int // number of stored events
}

// NewHub returns a hub holding at most capacity events (minimum 1).
// logger and sink may be nil.
func NewHub(capacity int, logger *slog.Logger, sink func(Event)) *Hub {
	if capacity < 1 {
		capacity = 1
	}
	return &Hub{
		logger: logger,
		sink:   sink,
		now:    time.Now,
		buf:    make([]Event, capacity),
	}
}

// Emit records e: it fills Time if zero, redacts Detail, stores the event
// (dropping the oldest when full), logs it and passes it to the sink.
func (h *Hub) Emit(e Event) {
	if e.Time.IsZero() {
		e.Time = h.now()
	}
	e.Detail = Redact(e.Detail)
	if e.RetryAt != nil {
		t := *e.RetryAt // do not alias the caller's value
		e.RetryAt = &t
	}

	h.mu.Lock()
	h.buf[h.next] = e
	h.next = (h.next + 1) % len(h.buf)
	if h.n < len(h.buf) {
		h.n++
	}
	h.mu.Unlock()

	h.log(e)
	if h.sink != nil {
		h.sink(e)
	}
}

// Recent returns at most n events, newest first.
func (h *Hub) Recent(n int) []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n > h.n {
		n = h.n
	}
	if n <= 0 {
		return nil
	}
	out := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, h.buf[(h.next-i+len(h.buf))%len(h.buf)])
	}
	return out
}

// Last returns the most recent event, if any.
func (h *Hub) Last() (Event, bool) {
	r := h.Recent(1)
	if len(r) == 0 {
		return Event{}, false
	}
	return r[0], true
}

func (h *Hub) log(e Event) {
	if h.logger == nil {
		return
	}
	attrs := []any{slog.String("kind", string(e.Kind)), slog.Time("time", e.Time)}
	add := func(k, v string) {
		if v != "" {
			attrs = append(attrs, slog.String(k, v))
		}
	}
	add("route", e.Route)
	add("from", e.From)
	add("to", e.To)
	add("provider", e.Provider)
	add("target", e.Target)
	add("reason", e.Reason)
	add("detail", e.Detail)
	if e.RetryAt != nil {
		attrs = append(attrs, slog.Time("retry_at", *e.RetryAt))
	}
	switch e.Kind {
	case KindCooldown, KindDisabled:
		h.logger.Warn("cbrt event", attrs...)
	default:
		h.logger.Info("cbrt event", attrs...)
	}
}
