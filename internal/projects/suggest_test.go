package projects

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	inventory "github.com/alex-danilin/cbrt/internal/analyze/projects"
)

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAggregate(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	tmp := filepath.Join(base, "tmp")
	a, aSub, b := filepath.Join(home, "work", "a"), filepath.Join(home, "work", "a", "sub"), filepath.Join(home, "work", "b")
	mkdirs(t, aSub, b, filepath.Join(tmp, "x"))
	at := func(h int) time.Time { return time.Date(2026, 11, 4, h, 0, 0, 0, time.UTC) }
	ss := []RecentSession{
		{Tool: "claude", Dir: a, Last: at(9)},
		{Tool: "codex", Dir: aSub, Last: at(10)},
		{Tool: "claude", Dir: a, Last: at(8)},
		{Tool: "claude", Dir: b, Last: at(11)},
		{Tool: "claude", Dir: home, Last: at(12)},                        // the home folder
		{Tool: "claude", Dir: filepath.Join(tmp, "x"), Last: at(12)},     // a temp folder
		{Tool: "claude", Dir: filepath.Join(home, "gone"), Last: at(12)}, // no longer exists
		{Tool: "claude", Dir: "", Last: at(12)},                          // no folder recorded
	}
	roots := 0
	o := SuggestOptions{Home: home, TempDirs: []string{tmp}, Root: func(_ context.Context, dir string) string {
		roots++
		if dir == aSub {
			return a // a folder inside a repository maps to its main folder
		}
		return dir
	}}
	got := Aggregate(context.Background(), ss, o)
	want := []Suggestion{
		{Path: b, Name: "b", Sessions: 1, Tools: []string{"claude"}, Last: at(11)},
		{Path: a, Name: "a", Sessions: 3, Tools: []string{"claude", "codex"}, Last: at(10)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if roots != 3 {
		t.Errorf("Root called %d times, want once per existing allowed folder (3)", roots)
	}
}

func TestUnconnected(t *testing.T) {
	base := t.TempDir()
	p := func(n string) string { return filepath.Join(base, n) }
	var sg []Suggestion
	for _, n := range []string{"conn", filepath.Join("conn", "inner"), "dismissed", "s1", "s2", "s3", "s4", "s5", "s6"} {
		sg = append(sg, Suggestion{Path: p(n), Name: baseName(n)})
	}
	es := []Entry{{Name: "conn", Path: p("conn")}}
	got := Unconnected(sg, es, []string{p("dismissed")})
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	if want := []string{"s1", "s2", "s3", "s4", "s5"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("got %v, want %v (connected, inside connected and dismissed dropped, capped at %d)", names, want, MaxSuggestions)
	}
}

func TestGitRootWorktree(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	main := e.project(t, "main", map[string]string{"README.md": "x\n", "sub/f.txt": "y\n"})
	gitT(t, main, "init", "-q")
	gitT(t, main, "add", "-A")
	gitT(t, main, "commit", "-q", "-m", "init")
	wt := filepath.Join(e.base, "work", "main-wt")
	gitT(t, main, "worktree", "add", "-q", "--detach", wt)
	plain := e.project(t, "plain", map[string]string{"f.txt": "z\n"})

	ctx := context.Background()
	for _, c := range []struct{ dir, want string }{
		{main, main},
		{filepath.Join(main, "sub"), main},
		{wt, main},
		{plain, plain},
	} {
		got := gitRoot(ctx, c.dir)
		want, err := filepath.EvalSymlinks(c.want)
		if err != nil {
			t.Fatal(err)
		}
		if !inventory.SamePath(got, want) {
			t.Errorf("gitRoot(%s) = %s, want %s", c.dir, got, want)
		}
	}
}

func TestDismissKeptBySave(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "work", "x")
	if err := Save(home, []Entry{{Name: "a", Path: filepath.Join(home, "a")}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Dismiss(home, dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := Save(home, []Entry{{Name: "b", Path: filepath.Join(home, "b")}}); err != nil {
		t.Fatal(err)
	}
	ds, err := LoadDismissed(home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ds, []string{dir}) {
		t.Errorf("dismissed = %v, want [%s] once, kept by Save", ds, dir)
	}
	es, err := Load(home)
	if err != nil || len(es) != 1 || es[0].Name != "b" {
		t.Errorf("Load = %v, %v; want the saved entry b", es, err)
	}
}
