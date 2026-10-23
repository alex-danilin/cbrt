// Package analyzetest checks an adapter against the analyze contract. Every
// adapter's tests call Run on its synthetic fixtures, and its local
// (-tags local) tests call it on the user's real stores.
package analyzetest

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// Result is what Run read, for adapter-specific assertions.
type Result struct {
	Sources []analyze.Source
	Events  map[string][]analyze.Event // by Source.NativeSessionID
}

// Run discovers every source under roots, reads each one twice, and fails t
// on any contract violation (analyze.CheckEvents) and on a difference between
// the two reads or two discoveries. Every bodiesEvery-th source (0 means 10)
// is also read through Bodies, which must emit the same events, each with
// TextVolume(Body) == TextVolumeUnits, while Events leaves every Body empty.
//
// Failure messages name sessions, event indexes and field names only: never
// event text, so running it on real stores prints nothing of their content.
func Run(t *testing.T, a analyze.Adapter, roots []string, bodiesEvery int) Result {
	t.Helper()
	if bodiesEvery <= 0 {
		bodiesEvery = 10
	}
	srcs, err := a.Discover(roots)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	again, err := a.Discover(roots)
	if err != nil || !reflect.DeepEqual(srcs, again) {
		t.Fatalf("Discover is not deterministic (err %v)", err)
	}
	res := Result{Sources: srcs, Events: map[string][]analyze.Event{}}
	for si, src := range srcs {
		if src.AdapterID != a.ID() {
			t.Errorf("source %s: AdapterID %q", src.NativeSessionID, src.AdapterID)
		}
		if _, dup := res.Events[src.NativeSessionID]; dup {
			t.Errorf("source %s discovered twice", src.NativeSessionID)
			continue
		}
		before := memberStats(src)
		evs, err := collect(a, src)
		if err != nil {
			t.Errorf("source %s: Events: %v", src.NativeSessionID, err)
			continue
		}
		evs2, err := collect(a, src)
		switch {
		case err != nil:
			t.Errorf("source %s: second read: %v", src.NativeSessionID, err)
		case !reflect.DeepEqual(before, memberStats(src)):
			// A live session's transcript grew between the reads (local runs on
			// real stores): the two reads may differ legitimately.
			t.Logf("source %s changed while it was read; determinism not compared", src.NativeSessionID)
		case !sameEvents(evs, evs2):
			t.Errorf("source %s: Events is not deterministic", src.NativeSessionID)
		}
		for _, e := range analyze.CheckEvents(a, src, evs) {
			t.Error(e)
		}
		for i, e := range evs {
			if e.Body != "" {
				t.Errorf("source %s event %d: Events set Body", src.NativeSessionID, i)
				break
			}
		}
		if si%bodiesEvery == 0 {
			checkBodies(t, a, src, evs, before)
		}
		res.Events[src.NativeSessionID] = evs
	}
	return res
}

// checkBodies reads src through Bodies and compares with evs (from Events).
func checkBodies(t *testing.T, a analyze.Adapter, src analyze.Source, evs []analyze.Event, before []string) {
	t.Helper()
	var bodies []analyze.Event
	err := a.Bodies(src, func(e analyze.Event) error {
		bodies = append(bodies, e)
		return nil
	})
	switch {
	case err != nil:
		t.Errorf("source %s: Bodies: %v", src.NativeSessionID, err)
		return
	case !reflect.DeepEqual(before, memberStats(src)):
		return // grew while read
	case !sameEvents(evs, bodies): // JSON leaves Body out
		t.Errorf("source %s: Bodies emits other events than Events", src.NativeSessionID)
		return
	}
	for i, e := range bodies {
		if got := analyze.TextVolume(e.Body); got != e.TextVolumeUnits {
			t.Errorf("source %s event %d (%s): TextVolume(Body) %d, TextVolumeUnits %d",
				src.NativeSessionID, i, e.Kind, got, e.TextVolumeUnits)
		}
	}
}

// memberStats is each member's size and modification time.
func memberStats(src analyze.Source) []string {
	var out []string
	for _, m := range src.Members {
		if st, err := os.Stat(m.Path); err == nil {
			out = append(out, fmt.Sprintf("%d@%d", st.Size(), st.ModTime().UnixNano()))
		} else {
			out = append(out, "missing")
		}
	}
	return out
}

func collect(a analyze.Adapter, src analyze.Source) ([]analyze.Event, error) {
	var evs []analyze.Event
	err := a.Events(src, func(e analyze.Event) error {
		evs = append(evs, e)
		return nil
	})
	return evs, err
}

// sameEvents compares through JSON so that equal times with different
// monotonic readings, and nil vs empty slices, count as equal.
func sameEvents(a, b []analyze.Event) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

// Kinds counts events by kind, for adapter assertions.
func Kinds(evs []analyze.Event) map[analyze.Kind]int {
	m := map[analyze.Kind]int{}
	for _, e := range evs {
		m[e.Kind]++
	}
	return m
}

// TokenSum adds up each token bucket over the model_request events (nil
// buckets count as zero) and returns how many requests had measured or
// partial usage out of how many requests there were.
func TokenSum(evs []analyze.Event) (sum analyze.Tokens, withUsage, requests int) {
	add := func(dst **int64, v *int64) {
		if v == nil {
			return
		}
		if *dst == nil {
			*dst = analyze.Int64(0)
		}
		**dst += *v
	}
	for _, e := range evs {
		if e.Kind != analyze.KindModelRequest {
			continue
		}
		requests++
		if t := e.Tokens; t != nil {
			withUsage++
			add(&sum.InUncached, t.InUncached)
			add(&sum.InCacheRead, t.InCacheRead)
			add(&sum.InCacheWrite, t.InCacheWrite)
			add(&sum.OutVisible, t.OutVisible)
			add(&sum.OutThinking, t.OutThinking)
		}
	}
	return sum, withUsage, requests
}
