package fixturetool

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/analyzetest"
	"github.com/alex-danilin/cbrt/internal/analyze/hook"
)

// fillHome records an invented session history through the real recorder.
func fillHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	git := func(context.Context, string) (*analyze.GitInfo, error) {
		return &analyze.GitInfo{Branch: "feature/demo", Commit: "0123abcd"}, nil
	}
	base := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	calls := []struct{ adapter, event, stdin string }{
		{ID, "Start", `{"session":"alpha","cwd":"/work/demo"}`},
		{ID, "Ask", `{"session":"alpha","cwd":"/work/demo","tool":"shell"}`},
		{ID, "End", `{"session":"alpha","cwd":"/work/demo"}`},
		{ID, "Start", `{"session":"beta","workspacePaths":["/work/other"]}`},
		{ID, "Unknown", `{"session":"gamma"}`},
		{ID, "Ask", `{"no_session":true}`},
		{"othertool", "Start", `{"session":"delta"}`},
	}
	for i, c := range calls {
		now := base.Add(time.Duration(i) * 24 * time.Hour) // crosses into September
		opts := hook.Options{Now: func() time.Time { return now }, Git: git}
		if err := hook.Record(home, c.adapter, c.event, strings.NewReader(c.stdin), opts); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestRegistered(t *testing.T) {
	a, ok := analyze.Get(ID)
	if !ok {
		t.Fatal("fixturetool not registered")
	}
	if err := a.Capabilities().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestContract(t *testing.T) {
	home := fillHome(t)
	res := analyzetest.Run(t, Adapter{}, []string{home, home + "-missing"}, 1)
	var got []string
	for _, s := range res.Sources {
		got = append(got, s.NativeSessionID)
		if s.Locator != "registry:"+s.NativeSessionID || len(s.Members) != 0 || s.Store != "default" {
			t.Errorf("source %+v", s)
		}
		if len(res.Events[s.NativeSessionID]) != 0 {
			t.Errorf("session %s has transcript events", s.NativeSessionID)
		}
	}
	if strings.Join(got, ",") != "alpha,beta,gamma" {
		t.Errorf("sessions %v", got)
	}

	// Compaction changes neither the sources nor the registry events.
	before := normalizeAll(t, home)
	if err := hook.Compact(home, time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	res2 := analyzetest.Run(t, Adapter{}, []string{home}, 1)
	if len(res2.Sources) != len(res.Sources) {
		t.Errorf("%d sources after compaction, want %d", len(res2.Sources), len(res.Sources))
	}
	after := normalizeAll(t, home)
	if len(after) != len(before) {
		t.Fatalf("%d registry events after compaction, want %d", len(after), len(before))
	}
	seqs := map[string]bool{}
	for _, e := range before {
		seqs[e.Seq] = true
	}
	for _, e := range after {
		if !seqs[e.Seq] {
			t.Errorf("Seq %q changed by compaction", e.Seq)
		}
	}
}

func normalizeAll(t *testing.T, home string) []analyze.Event {
	t.Helper()
	envs, problems, err := hook.ReadSpool(home)
	if err != nil || len(problems) != 0 {
		t.Fatalf("ReadSpool: %v %v", problems, err)
	}
	var out []analyze.Event
	for _, env := range envs {
		if env.AdapterID != ID {
			continue
		}
		evs, err := Adapter{}.NormalizeHook(env)
		if err != nil {
			if !strings.Contains(env.Stdin, "no_session") {
				t.Errorf("NormalizeHook(%s): %v", env.Event, err)
			}
			continue
		}
		out = append(out, evs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) }) // spool names are random
	return out
}

func TestNormalizeHook(t *testing.T) {
	home := fillHome(t)
	evs := normalizeAll(t, home)
	want := []struct {
		session string
		kind    analyze.Kind
	}{
		{"alpha", analyze.KindSessionStart},
		{"alpha", analyze.KindPermissionReq},
		{"alpha", analyze.KindSessionEnd},
		{"beta", analyze.KindSessionStart},
	}
	if len(evs) != len(want) {
		t.Fatalf("%d events, want %d: %+v", len(evs), len(want), evs)
	}
	for _, e := range evs {
		if e.Tool != ID || e.Store != "default" || e.ObservedBy != analyze.FromRegistry || e.Origin != analyze.OriginUnknown {
			t.Errorf("event header %+v", e)
		}
		if e.TS.IsZero() || e.TS.Location() != time.UTC || !strings.HasPrefix(e.Seq, "hook:") {
			t.Errorf("TS %v Seq %q", e.TS, e.Seq)
		}
		if e.DurationCoverage != analyze.Unknown || e.Tokens != nil || e.TokensCoverage != "" {
			t.Errorf("coverage %+v", e)
		}
		if e.CorrelationID != e.NativeSessionID {
			t.Errorf("CorrelationID %q", e.CorrelationID)
		}
		switch e.Kind {
		case analyze.KindPermissionReq: // "Ask" is not PermissionRequest, so the recorder ran git
			fallthrough
		case analyze.KindSessionStart, analyze.KindSessionEnd:
			if e.Branch != "feature/demo" || e.Commit != "0123abcd" || e.BranchCoverage != analyze.Measured {
				t.Errorf("%s branch %q/%q %s", e.Kind, e.Branch, e.Commit, e.BranchCoverage)
			}
		}
	}
	for i, w := range want {
		if evs[i].NativeSessionID != w.session || evs[i].Kind != w.kind {
			t.Errorf("event %d: %s %s, want %s %s", i, evs[i].NativeSessionID, evs[i].Kind, w.session, w.kind)
		}
	}

	// No git: branch unknown.
	env := analyze.Envelope{V: 1, AdapterID: ID, Event: "Ask", RecordedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Stdin: `{"session":"x"}`}
	out, err := Adapter{}.NormalizeHook(env)
	if err != nil || len(out) != 1 || out[0].BranchCoverage != analyze.Unknown || out[0].Branch != "" || out[0].Seq == "" {
		t.Errorf("no-git envelope: %+v %v", out, err)
	}
	env.AdapterID = "othertool"
	if _, err := (Adapter{}).NormalizeHook(env); err == nil {
		t.Error("foreign envelope accepted")
	}
}
