package projects

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const parkPlan = "# Plan: inkwell\n\nGoal: notes.\n\n## Core\n- [x] a — evidence: test\n- [>] sync\n  a note: keep [ ] as is\n- [ ] export   \n* [~] archive\n\n```\n- [ ] in a fence\n```\nfree text - [ ] not a task\n## Later\n-   [ ] spaced\n- [?] maybe"

// planDir writes PLAN.md with content into a fresh folder.
func planDir(t *testing.T, content string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "PLAN.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, p
}

// lineOf is the 1-based line of the first line containing s.
func lineOf(t *testing.T, content, s string) int {
	t.Helper()
	for i, l := range strings.Split(content, "\n") {
		if strings.Contains(l, s) {
			return i + 1
		}
	}
	t.Fatalf("no line with %q", s)
	return 0
}

func TestToggleRewritesOneByte(t *testing.T) {
	for _, nl := range []string{"\n", "\r\n"} {
		for _, prefix := range []string{"", bom} {
			orig := prefix + strings.ReplaceAll(parkPlan, "\n", nl)
			dir, p := planDir(t, orig)
			for _, c := range []struct {
				text, from, to string
			}{
				{"export", "- [ ] export", "- [~] export"},
				{"sync", "- [>] sync", "- [~] sync"},
				{"archive", "* [~] archive", "* [ ] archive"},
				{"spaced", "-   [ ] spaced", "-   [~] spaced"},
			} {
				before := read(t, p)
				line := lineOf(t, before, c.from)
				m, err := Toggle(dir, line, c.text, PlanHash([]byte(before)))
				if err != nil {
					t.Fatalf("%q %s: %v", nl, c.text, err)
				}
				want := strings.Replace(before, c.from, c.to, 1)
				if got := read(t, p); got != want {
					t.Fatalf("%q %s: rewrote\n%q\nwant\n%q", nl, c.text, got, want)
				}
				if m != c.to[strings.Index(c.to, "[")+1:strings.Index(c.to, "]")] {
					t.Errorf("%s: marker %q", c.text, m)
				}
			}
			// unpark and re-mark: back to the original bytes apart from sync
			// (an active task unparks to open)
			for _, c := range []struct{ text, from string }{{"export", "- [~] export"}, {"archive", "* [ ] archive"}, {"spaced", "-   [~] spaced"}} {
				cur := read(t, p)
				if _, err := Toggle(dir, lineOf(t, cur, c.from), c.text, PlanHash([]byte(cur))); err != nil {
					t.Fatal(err)
				}
			}
			want := strings.Replace(orig, "- [>] sync", "- [~] sync", 1)
			if got := read(t, p); got != want {
				t.Errorf("%q bom=%v: round trip\n%q\nwant\n%q", nl, prefix != "", got, want)
			}
			// the parser sees what was written
			pl, err := Parse([]byte(read(t, p)))
			if err != nil {
				t.Fatal(err)
			}
			if got := pl.Milestones[0].Tasks[1]; got.State != Parked || got.Text != "sync" {
				t.Errorf("parsed %+v", got)
			}
		}
	}
}

// TestToggleConcurrent: two tabs toggle from the same page: one wins, the
// other gets the 409, no update is lost.
func TestToggleConcurrent(t *testing.T) {
	for range 20 {
		dir, p := planDir(t, parkPlan)
		hash := PlanHash([]byte(parkPlan))
		errs := make(chan error, 2)
		for _, c := range []struct {
			line int
			text string
		}{{9, "export"}, {10, "archive"}} {
			go func() { _, err := Toggle(dir, c.line, c.text, hash); errs <- err }()
		}
		e1, e2 := <-errs, <-errs
		if (e1 == nil) == (e2 == nil) {
			t.Fatalf("both or neither succeeded: %v, %v", e1, e2)
		}
		if err := errors.Join(e1, e2); !strings.Contains(err.Error(), "changed since") {
			t.Fatalf("loser: %v", err)
		}
		got := read(t, p)
		one := strings.Replace(parkPlan, "- [ ] export", "- [~] export", 1)
		other := strings.Replace(parkPlan, "* [~] archive", "* [ ] archive", 1)
		if got != one && got != other {
			t.Fatalf("PLAN.md:\n%q", got)
		}
	}
}

func TestToggleTaskOnBOMLine(t *testing.T) {
	orig := bom + "- [ ] first\n# Plan: x\n"
	dir, p := planDir(t, orig)
	if _, err := Toggle(dir, 1, "first", PlanHash([]byte(orig))); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != bom+"- [~] first\n# Plan: x\n" {
		t.Errorf("%q", got)
	}
}

func TestToggleRefuses(t *testing.T) {
	plan := parkPlan + "\n- [ ] twice\n- [ ] twice\n"
	for _, c := range []struct {
		name, text string
		line       int
		hash       string // "" = the right one
		want       string
	}{
		{"changed file", "export", 9, "0000", "changed since the page was loaded"},
		{"no hash", "export", 9, "-", "changed since"},
		{"wrong line", "export", 8, "", `no task "export" at line 8`},
		{"wrong text", "exports", 9, "", "no task"},
		{"past the end", "export", 999, "", "no task"},
		{"in a fence", "in a fence", lineOf(t, plan, "in a fence"), "", "no task"},
		{"ambiguous", "twice", lineOf(t, plan, "twice"), "", `2 tasks read "twice"`},
		{"done", "a — evidence: test", 6, "", "is done"},
		{"unknown marker", "maybe", lineOf(t, plan, "maybe"), "", "is unknown"},
	} {
		dir, p := planDir(t, plan)
		hash := PlanHash([]byte(plan))
		switch c.hash {
		case "-":
			hash = ""
		case "":
		default:
			hash = c.hash
		}
		_, err := Toggle(dir, c.line, c.text, hash)
		var te *ToggleError
		if !errors.As(err, &te) || te.Code != http.StatusConflict || !strings.Contains(te.Msg, c.want) {
			t.Errorf("%s: %v, want 409 %q", c.name, err, c.want)
		}
		if read(t, p) != plan {
			t.Errorf("%s: the file changed", c.name)
		}
	}
	// a file edited after the page was rendered
	dir, p := planDir(t, plan)
	hash := PlanHash([]byte(plan))
	os.WriteFile(p, []byte(plan+"- [ ] new\n"), 0o644)
	if _, err := Toggle(dir, 9, "export", hash); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("edited file: %v", err)
	}
	if _, err := Toggle(t.TempDir(), 1, "x", "h"); err == nil || !strings.Contains(err.Error(), "no PLAN.md") {
		t.Errorf("no plan: %v", err)
	}
}
