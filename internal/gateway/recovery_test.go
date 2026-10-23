package gateway

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/router"
)

// A provider that recovers after its cooldown must be marked healthy again and
// announced with a recovery event (regression: successes used to be treated as
// failures with an empty class, so Succeed was never called).
func TestRecoveryAfterCooldown(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.Header().Set("Retry-After", "1")
			status(429, `{"error":{"message":"slow down"}}`)(w, r)
			return
		}
		okJSON("a is back")(w, r)
	})
	e := newEnv(t, "", a, newFake(t, okJSON("b")))
	now := time.Now()
	e.gw.health = router.NewHealth(func() time.Time { return now })

	e.post(t, simple) // a: rate limited -> b serves
	fail.Store(false)
	now = now.Add(2 * time.Second) // cooldown over: next request probes a

	resp, _ := e.post(t, simple)
	if resp.Header.Get("X-Cbrt-Provider") != "a" {
		t.Fatalf("a should serve again after its cooldown: %q", resp.Header.Get("X-Cbrt-Attempts"))
	}
	var recovered bool
	for _, ev := range e.hub.Recent(20) {
		recovered = recovered || (ev.Kind == events.KindRecovery && ev.Provider == "a")
	}
	if !recovered {
		t.Errorf("no recovery event: %+v", e.hub.Recent(20))
	}
	if d := e.gw.health.Degraded(); len(d) != 0 {
		t.Errorf("a still degraded after a successful probe: %+v", d)
	}
}
