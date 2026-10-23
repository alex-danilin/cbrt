package collect

import (
	"reflect"
	"testing"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

func TestBranchFlagsCountStartOnce(t *testing.T) {
	ev := func(branch string, cov analyze.Coverage) analyze.Event {
		return analyze.Event{Branch: branch, BranchCoverage: cov}
	}
	cases := []struct {
		name string
		evs  []analyze.Event
		want []string
	}{
		{"start repeated is one observation", []analyze.Event{
			ev("feat", analyze.MeasuredAtStart), ev("feat", analyze.MeasuredAtStart), ev("feat", analyze.Inferred)}, []string{FlagBranchPartial}},
		{"start plus a hook observation", []analyze.Event{
			ev("feat", analyze.MeasuredAtStart), ev("feat", analyze.MeasuredAtStart), ev("feat", analyze.Measured)}, nil},
		{"per-event branches", []analyze.Event{ev("a", analyze.Measured), ev("a", analyze.Measured)}, nil},
		{"drift", []analyze.Event{ev("a", analyze.Measured), ev("b", analyze.Measured)}, []string{FlagBranchDrift}},
		{"none", []analyze.Event{ev("", analyze.Unknown)}, []string{FlagBranchPartial}},
	}
	for _, c := range cases {
		if got := branchFlags(c.evs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
