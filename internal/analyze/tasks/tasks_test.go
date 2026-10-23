package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

var t0 = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

var all = analyze.Window{Since: t0.Add(-24 * time.Hour), Until: t0.Add(60 * 24 * time.Hour)}

type sb struct {
	s      *analyze.Session
	branch string
}

// sess makes a synthetic session; branch "" means no measured branch.
func sess(id, project, branch string, flags ...string) *sb {
	return &sb{branch: branch, s: &analyze.Session{
		Key:     analyze.SessionKey{AdapterID: "fake", Store: "cli", NativeID: id},
		Project: project,
		Flags:   flags,
	}}
}

func (b *sb) add(hours float64, kind analyze.Kind, text string, tool string, files ...string) *sb {
	e := analyze.Event{
		Seq: fmt.Sprint(len(b.s.Events) + 1), TS: t0.Add(time.Duration(hours * float64(time.Hour))),
		Kind: kind, Text: text, ToolName: tool, Files: files, ObservedBy: analyze.FromTranscript,
		BranchCoverage: analyze.Unknown,
	}
	if b.branch != "" {
		e.Branch, e.BranchCoverage = b.branch, analyze.Measured
	}
	b.s.Events = append(b.s.Events, e)
	return b
}

func (b *sb) prompt(h float64, text string) *sb { return b.add(h, analyze.KindUserPrompt, text, "") }
func (b *sb) say(h float64) *sb                 { return b.add(h, analyze.KindAssistantText, "ok", "") }
func (b *sb) edit(h float64, files ...string) *sb {
	return b.add(h, analyze.KindToolCall, "", analyze.ToolEdit, files...)
}
func (b *sb) read(h float64, files ...string) *sb {
	return b.add(h, analyze.KindToolCall, "", analyze.ToolReadFile, files...)
}

// work is a short session on one branch starting at hour h.
func work(id, project, branch string, h float64, flags ...string) *analyze.Session {
	return sess(id, project, branch, flags...).prompt(h, "do it").say(h + 0.5).s
}

func run(t *testing.T, ss []*analyze.Session, o Overrides, ids analyze.TaskIdentity) ([]analyze.Task, analyze.TaskIdentity) {
	t.Helper()
	out, next := Group(ss, all, 4, o, ids)
	again, next2 := Group(ss, all, 4, o, ids)
	if !reflect.DeepEqual(out, again) || !reflect.DeepEqual(next, next2) {
		t.Fatalf("Group is not deterministic")
	}
	return out, next
}

func shape(ts []analyze.Task) string {
	var parts []string
	for _, t := range ts {
		var ms []string
		for _, m := range t.Members {
			ms = append(ms, m.Session.NativeID+"#"+m.FromSeq)
		}
		parts = append(parts, t.Confidence+"["+strings.Join(ms, ",")+"]")
	}
	return strings.Join(parts, " ")
}

func wantShape(t *testing.T, ts []analyze.Task, want string) {
	t.Helper()
	if got := shape(ts); got != want {
		t.Errorf("tasks = %s\nwant    %s", got, want)
	}
}

func TestTagSplit(t *testing.T) {
	a := sess("a", "/p", "").
		prompt(0, "start #task:alpha").say(0.1).
		prompt(0.2, "more on it").say(0.3). // stays in alpha
		prompt(0.4, "now #task:beta").say(0.5).
		prompt(0.6, "again #task:beta").say(0.7).s // same tag: no split
	b := sess("b", "/elsewhere", "").prompt(30, "follow-up #task:alpha").s
	ts, _ := run(t, []*analyze.Session{a, b}, Overrides{}, analyze.TaskIdentity{})
	wantShape(t, ts, "explicit[a#1,b#1] explicit[a#5]")
	if ts[0].Members[0].ToSeq != "4" || ts[1].Members[0].ToSeq != "8" {
		t.Errorf("range ends: %+v", ts)
	}
}

func TestGapSplit(t *testing.T) {
	s := sess("a", "/p", "").
		prompt(0, "first").
		add(2, analyze.KindToolCall, "", analyze.ToolShell).
		add(5, analyze.KindToolResult, "", ""). // a long agent turn: no 4 h without events
		prompt(6, "second").say(6.1).
		prompt(10.2, "third"). // 4.1 h idle
		say(10.3).s
	ts, _ := run(t, []*analyze.Session{s}, Overrides{}, analyze.TaskIdentity{})
	wantShape(t, ts, "single[a#1] single[a#6]")

	// A larger gap setting keeps one range; 0 means the default 4 h.
	if ts, _ := Group([]*analyze.Session{s}, all, 5, Overrides{}, analyze.TaskIdentity{}); len(ts) != 1 {
		t.Errorf("gap 5 h: %s", shape(ts))
	}
	if ts, _ := Group([]*analyze.Session{s}, all, 0, Overrides{}, analyze.TaskIdentity{}); len(ts) != 2 {
		t.Errorf("gap default: %s", shape(ts))
	}

	// Window restriction: only the second range has events in it.
	w := analyze.Window{Since: t0.Add(10 * time.Hour), Until: t0.Add(11 * time.Hour)}
	ts, _ = Group([]*analyze.Session{s}, w, 4, Overrides{}, analyze.TaskIdentity{})
	wantShape(t, ts, "single[a#6]")
}

func TestStrongLink(t *testing.T) {
	ss := []*analyze.Session{
		work("a", "/p", "feat", 0),
		work("b", "/p", "feat", 20),  // within 24 h of a
		work("c", "/p", "feat", 100), // too far
		work("d", "/q", "feat", 1),   // other project
		work("e", "/p", "main", 2),   // default branch
		work("f", "/p", "master", 3), // default branch
		work("g", "/p", "feat", 4, "branch_drift"),
		work("h", "/p", "feat", 5, "branch_partial"),
		work("i", "/p", "other", 6), // other branch
		work("j", "/p", "feat", 21), // links to b, so to a
	}
	ts, _ := run(t, ss, Overrides{}, analyze.TaskIdentity{})
	wantShape(t, ts, "strong[a#1,b#1,j#1] single[d#1] single[e#1] single[f#1] single[g#1] single[h#1] single[i#1] single[c#1]")
	if ts[0].Branch != "feat" || ts[0].Project != "/p" {
		t.Errorf("task 0: %+v", ts[0])
	}
}

func TestWeakLink(t *testing.T) {
	a := sess("a", "/p", "").prompt(0, "x").edit(0.1, "x.go", "y.go").read(0.2, "z.go").s
	b := sess("b", "/p", "").prompt(3, "y").read(3.1, "x.go").read(3.2, "./y.go").s // 2 shared, 2.8 h later
	c := sess("c", "/p", "").prompt(4, "z").edit(4.1, "x.go").s                     // 1 shared only
	d := sess("d", "/p", "").prompt(9, "w").edit(9.1, "x.go", "y.go").s             // too late (> 4 h after b)
	e := sess("e", "/p", "main").prompt(2, "v").edit(2.1, "x.go", "z.go").s         // default branch: allowed
	f := sess("f", "/p", "feat").prompt(2, "u").edit(2.1, "x.go", "z.go").s         // measured feature branch: rule 4 does not apply
	g := sess("g", "/q", "").prompt(1, "t").edit(1.1, "x.go", "y.go").s             // other project
	ts, _ := run(t, []*analyze.Session{a, b, c, d, e, f, g}, Overrides{}, analyze.TaskIdentity{})
	wantShape(t, ts, "weak[a#1,e#1,b#1] single[g#1] single[f#1] single[c#1] single[d#1]")
}

func TestTagsBlockAutomaticLinks(t *testing.T) {
	a := sess("a", "/p", "feat").prompt(0, "#task:one").s
	b := sess("b", "/p", "feat").prompt(1, "#task:two").s
	c := sess("c", "/p", "feat").prompt(2, "untagged").s
	ts, _ := run(t, []*analyze.Session{a, b, c}, Overrides{}, analyze.TaskIdentity{})
	// c links to the first tagged range only; a and b never link.
	wantShape(t, ts, "strong[a#1,c#1] explicit[b#1]")
}

func TestSubagentsNotGrouped(t *testing.T) {
	p := work("p", "/p", "", 0)
	c := work("c", "/p", "", 0.1)
	c.Parent = &p.Key
	ts, _ := run(t, []*analyze.Session{p, c}, Overrides{}, analyze.TaskIdentity{})
	wantShape(t, ts, "single[p#1]")
}

func TestIDStability(t *testing.T) {
	a := work("a", "/p", "feat", 0)
	ts, ids := run(t, []*analyze.Session{a}, Overrides{}, analyze.TaskIdentity{})
	id := ts[0].ID
	if id != NewID("fake/cli/a#1") || !strings.HasPrefix(id, "t_") || len(id) != 14 {
		t.Fatalf("id %q", id)
	}
	if ids.V != IdentityVersion || ids.ByFound["fake/cli/a#1"] != id {
		t.Fatalf("identity %+v", ids)
	}

	// A new linked range does not change the id, nor does an older one found later.
	b := work("b", "/p", "feat", 5)
	old := work("o", "/p", "feat", -3)
	ts, ids2 := run(t, []*analyze.Session{a, b, old}, Overrides{}, ids)
	if len(ts) != 1 || ts[0].ID != id || len(ts[0].Aliases) != 0 {
		t.Fatalf("after adding ranges: %+v", ts)
	}
	if ids2.ByFound["fake/cli/b#1"] != id || ids.ByFound["fake/cli/b#1"] != "" {
		t.Errorf("identity not updated or input mutated: %+v %+v", ids2, ids)
	}

	// The founding range leaves the window: the task keeps its id.
	w := analyze.Window{Since: t0.Add(4 * time.Hour), Until: t0.Add(10 * time.Hour)}
	ts, _ = Group([]*analyze.Session{a, b}, w, 4, Overrides{}, ids2)
	if len(ts) != 1 || ts[0].ID != id {
		t.Errorf("windowed: %+v", ts)
	}
}

func TestMergeAlias(t *testing.T) {
	a := work("a", "/p", "feat", 0)
	b := work("b", "/p", "feat", 40)
	ts, ids := run(t, []*analyze.Session{a, b}, Overrides{}, analyze.TaskIdentity{})
	if len(ts) != 2 {
		t.Fatalf("%s", shape(ts))
	}
	idA, idB := ts[0].ID, ts[1].ID

	// A range in between links both: the older id survives.
	mid := work("m", "/p", "feat", 20)
	ts, ids = run(t, []*analyze.Session{a, b, mid}, Overrides{}, ids)
	if len(ts) != 1 || ts[0].ID != idA || !reflect.DeepEqual(ts[0].Aliases, []string{idB}) {
		t.Fatalf("merged: %+v", ts)
	}
	if ids.Aliases[idB] != idA {
		t.Errorf("aliases %v", ids.Aliases)
	}
	// Overrides may name the absorbed id.
	ts, _ = run(t, []*analyze.Session{a, b, mid}, Overrides{Rename: map[string]string{idB: "widget"}}, ids)
	if ts[0].Label != "widget" {
		t.Errorf("rename via alias: %+v", ts[0])
	}
}

func TestManualOverrides(t *testing.T) {
	a := work("a", "/p", "", 0)
	b := work("b", "/q", "", 50)
	c := work("c", "/r", "", 60)
	ts, ids := run(t, []*analyze.Session{a, b, c}, Overrides{}, analyze.TaskIdentity{})
	idA, idB, idC := ts[0].ID, ts[1].ID, ts[2].ID

	o := Overrides{V: 1, Merge: [][]string{{idB, idA}}, Rename: map[string]string{idA: "parser"}, Exclude: []string{idC}}
	ts, ids = run(t, []*analyze.Session{a, b, c}, o, ids)
	wantShape(t, ts, "manual[a#1,b#1]")
	if ts[0].ID != idA || ts[0].Label != "parser" || ids.Aliases[idB] != idA || ids.ByFound["fake/cli/c#1"] != idC {
		t.Errorf("merge: %+v %+v", ts, ids)
	}
}

func TestSplit(t *testing.T) {
	ss := []*analyze.Session{work("a", "/p", "feat", 0), work("b", "/p", "feat", 2), work("c", "/p", "feat", 4)}
	ts, ids := run(t, ss, Overrides{}, analyze.TaskIdentity{})
	id := ts[0].ID
	o := Overrides{Split: []SplitRule{{Task: id, Members: []string{"fake/cli/a#1", "fake/cli/c#1"}}}}
	ts, ids2 := run(t, ss, o, ids)
	wantShape(t, ts, "manual[a#1,c#1] manual[b#1]")
	var kept, detached analyze.Task
	for _, x := range ts {
		if x.ID == id {
			kept = x
		} else {
			detached = x
		}
	}
	// The detached part gets a new id even though it holds the founding range.
	if kept.Members[0].Session.NativeID != "b" || detached.SplitFrom != id || detached.ID == id || !strings.HasPrefix(detached.ID, "t_") {
		t.Fatalf("split: %+v", ts)
	}
	// Stable on the next run, and no alias is created.
	again, ids3 := run(t, ss, o, ids2)
	if !reflect.DeepEqual(again, ts) || len(ids3.Aliases) != 0 {
		t.Errorf("split not stable: %+v\n%+v", again, ids3)
	}
	if detached.ID != SplitID(o.Split[0]) || ids3.ByFound["fake/cli/a#1"] != id {
		t.Errorf("split id %q, identity %+v", detached.ID, ids3)
	}
	// Dropping the rule merges back under the original id, without an alias.
	back, _ := run(t, ss, Overrides{}, ids3)
	if len(back) != 1 || back[0].ID != id || len(back[0].Aliases) != 0 {
		t.Errorf("unsplit: %+v", back)
	}
}

func TestOrder(t *testing.T) {
	// Same First: ordered by id.
	a := work("a", "/p", "", 0)
	b := work("b", "/q", "", 0)
	ts, _ := run(t, []*analyze.Session{b, a}, Overrides{}, analyze.TaskIdentity{})
	if len(ts) != 2 || ts[0].ID > ts[1].ID {
		t.Errorf("order: %+v", ts)
	}
}

func TestLoadOverrides(t *testing.T) {
	dir := t.TempDir()
	o, err := LoadOverrides(filepath.Join(dir, "missing.json"))
	if err != nil || len(o.Merge)+len(o.Split)+len(o.Exclude)+len(o.Rename) != 0 {
		t.Fatalf("missing: %+v %v", o, err)
	}
	write := func(s string) string {
		p := filepath.Join(dir, "o.json")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	o, err = LoadOverrides(write(`{"v":1,"merge":[["t_a","t_b"]],"split":[{"task":"t_a","members":["x/y/z#1"]}],"rename":{"t_a":"n"},"exclude":["t_c"]}`))
	if err != nil || len(o.Merge) != 1 || o.Split[0].Members[0] != "x/y/z#1" || o.Rename["t_a"] != "n" || o.Exclude[0] != "t_c" {
		t.Errorf("valid: %+v %v", o, err)
	}
	if _, err := LoadOverrides(write(`{"v":2}`)); err == nil {
		t.Error("newer version accepted")
	}
	if _, err := LoadOverrides(write(`{"v":1,"merges":[]}`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := LoadOverrides(write(`{`)); err == nil {
		t.Error("bad JSON accepted")
	}
}
