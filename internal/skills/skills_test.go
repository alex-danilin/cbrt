package skills

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const exe, rubric = `C:\Program Files\cbrt\cbrt.exe`, `C:\cbrt\analyze\RUBRIC.md`

// Every skill renders without placeholders, names itself after its folder and
// carries the marker; required skills are not optional.
func TestRender(t *testing.T) {
	if !slices.Contains(Names(), "cbrt-multi-session-review") || !slices.Contains(Optional(), "cbrt-session-review") {
		t.Fatalf("names %v, optional %v", Names(), Optional())
	}
	for _, r := range Required {
		if slices.Contains(Optional(), r) || !slices.Contains(Names(), r) {
			t.Errorf("required %q: missing or listed as optional", r)
		}
	}
	if _, ok := Render("bogus", exe, rubric); ok {
		t.Error("unknown skill rendered")
	}
	for _, name := range Names() {
		s, ok := Render(name, exe, rubric)
		if !ok {
			t.Fatalf("%s: not rendered", name)
		}
		if !strings.Contains(s, "\nname: "+name+"\n") || !strings.Contains(s, Marker) || !strings.Contains(s, filepath.ToSlash(rubric)) {
			t.Errorf("%s: lacks its name, the marker or the rubric path", name)
		}
		for _, p := range []string{"{cbrt}", "{rubric}", "{marker}", "{args}"} {
			if strings.Contains(s, p) {
				t.Errorf("%s: unreplaced %s", name, p)
			}
		}
	}
	if s, _ := Render("cbrt-multi-session-review", exe, rubric); !strings.Contains(s, filepath.ToSlash(exe)+`" analyze run $ARGUMENTS`) {
		t.Error("cbrt-multi-session-review does not run this cbrt")
	}
}

// Install overwrites a cbrt-made copy and keeps a hand-made one; Installed
// lists only cbrt-made skills.
func TestInstall(t *testing.T) {
	claude := t.TempDir()
	for _, name := range Names() {
		if o, _ := OwnerOf(claude, name); o != Absent {
			t.Fatalf("%s: owner %v before install", name, o)
		}
		p, kept, err := Install(claude, name, exe, rubric)
		if err != nil || kept {
			t.Fatalf("%s: %v kept=%v", name, err, kept)
		}
		if o, _ := OwnerOf(claude, name); o != Ours {
			t.Errorf("%s: owner %v after install", name, o)
		}
		b, _ := os.ReadFile(p)
		os.WriteFile(p, append(b, "old\n"...), 0o600)
		if _, kept, _ := Install(claude, name, exe, rubric); kept {
			t.Errorf("%s: cbrt-made copy kept", name)
		}
		if b, _ := os.ReadFile(p); strings.HasSuffix(string(b), "old\n") {
			t.Errorf("%s: cbrt-made copy not overwritten", name)
		}
	}
	if got, _ := Installed(claude); !slices.Equal(got, Names()) {
		t.Errorf("installed %v", got)
	}

	name := Names()[0]
	p := filepath.Join(Dir(claude, name), "SKILL.md")
	os.WriteFile(p, []byte("my own skill\n"), 0o600)
	if _, kept, err := Install(claude, name, exe, rubric); err != nil || !kept {
		t.Errorf("hand-made copy not kept: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "my own skill\n" {
		t.Error("hand-made copy overwritten")
	}
	if got, _ := Installed(claude); slices.Contains(got, name) {
		t.Errorf("hand-made %s listed as installed", name)
	}
}

// Every skill cbrt ships is named cbrt-…, so its origin is clear in Claude
// Code's skill list.
func TestNamesStartWithCbrt(t *testing.T) {
	for _, n := range Names() {
		if !strings.HasPrefix(n, "cbrt-") {
			t.Errorf("skill %q: name it cbrt-%s", n, n)
		}
	}
}

// Installed finds cbrt-made skills by their marker, including ones this cbrt
// no longer ships; Stale lists only those.
func TestStale(t *testing.T) {
	claude := t.TempDir()
	if _, _, err := Install(claude, Names()[0], exe, rubric); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		os.MkdirAll(Dir(claude, name), 0o700)
		os.WriteFile(filepath.Join(Dir(claude, name), "SKILL.md"), []byte(text), 0o600)
	}
	write("old-renamed", "---\nname: old-renamed\n---\n"+Marker+"\n")
	write("mine", "---\nname: mine\n---\n")
	got, _ := Installed(claude)
	if !slices.Equal(got, []string{Names()[0], "old-renamed"}) {
		t.Errorf("installed %v", got)
	}
	if got, _ := Stale(claude); !slices.Equal(got, []string{"old-renamed"}) {
		t.Errorf("stale %v", got)
	}
	if got, err := Stale(t.TempDir()); err != nil || len(got) != 0 {
		t.Errorf("no skills folder: %v %v", got, err)
	}
}
