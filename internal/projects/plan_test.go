package projects

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const samplePlan = `# Plan: inkwell

Goal: a small note-taking app
  that syncs over plain files.

Now: the sync engine

Some free text the parser keeps out of the way.

## Storage
- [x] Pick the file format — evidence: commit abc123
- [>] Sync engine
    note one
    - nested bullet is a note

    note after a blank line
- [ ] Conflict handling
- [~] Encrypted storage
- [?] Unknown marker task
* [X] Star bullet, capital X

## Release
- [ ] Packaging
-  [ ] Two spaces before the bracket
- [] not a task (no marker)
-[ ] not a task (no space)
  - [ ] indented, not a top-level task

` + "```" + `
- [ ] inside a code fence, not a task
## not a milestone either
` + "```" + `
`

func TestParseSample(t *testing.T) {
	p, err := Parse([]byte(samplePlan))
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Plan: inkwell" {
		t.Errorf("title %q", p.Title)
	}
	if p.Goal != "a small note-taking app that syncs over plain files." {
		t.Errorf("goal %q", p.Goal)
	}
	if p.Now != "the sync engine" {
		t.Errorf("now %q", p.Now)
	}
	if len(p.Milestones) != 2 || p.Milestones[0].Name != "Storage" || p.Milestones[1].Name != "Release" {
		t.Fatalf("milestones %+v", p.Milestones)
	}
	st := p.Milestones[0]
	var states, markers []string
	for _, tk := range st.Tasks {
		states = append(states, tk.State)
		markers = append(markers, tk.Marker)
	}
	if want := []string{Done, Active, Open, Parked, Unknown, Done}; !reflect.DeepEqual(states, want) {
		t.Errorf("states %v, want %v", states, want)
	}
	if want := []string{"x", ">", " ", "~", "?", "X"}; !reflect.DeepEqual(markers, want) {
		t.Errorf("markers %q, want %q", markers, want)
	}
	if st.Tasks[0].Text != "Pick the file format — evidence: commit abc123" {
		t.Errorf("text %q", st.Tasks[0].Text)
	}
	if want := []string{"note one", "- nested bullet is a note", "note after a blank line"}; !reflect.DeepEqual(st.Tasks[1].Notes, want) {
		t.Errorf("notes %q", st.Tasks[1].Notes)
	}
	if st.Tasks[1].Line != 12 {
		t.Errorf("line %d", st.Tasks[1].Line)
	}
	// Parked excluded, unknown counted as open: done 2 of 5.
	if got := st.Progress(); got != (Progress{Done: 2, Total: 5}) {
		t.Errorf("storage progress %+v", got)
	}
	rel := p.Milestones[1]
	if len(rel.Tasks) != 2 || rel.Tasks[1].Text != "Two spaces before the bracket" {
		t.Errorf("release tasks %+v", rel.Tasks)
	}
	if got := p.Progress(); got != (Progress{Done: 2, Total: 7}) || got.Percent() != 28 {
		t.Errorf("plan progress %+v", got)
	}
	var next []string
	for _, tk := range p.Next(3) {
		next = append(next, tk.Text)
	}
	if want := []string{"Sync engine", "Conflict handling", "Unknown marker task"}; !reflect.DeepEqual(next, want) {
		t.Errorf("next %q", next)
	}
}

func TestParseCRLF(t *testing.T) {
	lf, err := Parse([]byte(samplePlan))
	if err != nil {
		t.Fatal(err)
	}
	crlf, err := Parse([]byte(bom + strings.ReplaceAll(samplePlan, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lf, crlf) {
		t.Errorf("CRLF plan differs:\n%+v\n%+v", lf, crlf)
	}
}

func TestParseTasksBeforeMilestoneAndNoTasks(t *testing.T) {
	p, err := Parse([]byte("# Plan\n- [ ] loose task\n## M\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Milestones) != 2 || p.Milestones[0].Name != "" || len(p.Milestones[0].Tasks) != 1 {
		t.Errorf("%+v", p.Milestones)
	}
	if got := p.Milestones[1].Progress(); got != (Progress{}) || got.Percent() != 0 {
		t.Errorf("empty milestone %+v", got)
	}
	// Goal/Now inside a milestone are free text.
	p, err = Parse([]byte("# Plan\n## M\nGoal: not the goal\n"))
	if err != nil || p.Goal != "" {
		t.Errorf("goal %q err %v", p.Goal, err)
	}
}

func TestParseErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want error
	}{
		"no title":      {"## M\n- [ ] x\n", ErrNoTitle},
		"other title":   {"# Notes\n- [ ] x\n", ErrNoTitle},
		"binary":        {"# Plan\n\x00\x01", ErrBinary},
		"invalid utf-8": {"# Plan\n\xff\xfe", ErrBinary},
		"too large":     {"# Plan\n" + strings.Repeat("x", MaxPlanBytes), ErrTooLarge},
	} {
		if _, err := Parse([]byte(tc.in)); !errors.Is(err, tc.want) {
			t.Errorf("%s: err %v, want %v", name, err, tc.want)
		}
	}
	for _, ok := range []string{"# Plan", "# plan: x", "# Plan x", "# PLAN:"} {
		if _, err := Parse([]byte(ok + "\n")); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	if _, err := Parse([]byte("# Planning\n")); !errors.Is(err, ErrNoTitle) {
		t.Errorf("# Planning accepted")
	}
}
