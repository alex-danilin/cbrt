package all

import (
	"testing"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// Every adapter is registered through this package (acceptance test 14: a
// new adapter needs only its package and one import line here).
func TestAllRegistered(t *testing.T) {
	var ids []string
	for _, a := range analyze.Adapters() {
		ids = append(ids, a.ID())
	}
	want := []string{"antigravity", "claude", "codex", "fixturetool"}
	if len(ids) != len(want) {
		t.Fatalf("registered %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("registered %v, want %v", ids, want)
		}
	}
}
