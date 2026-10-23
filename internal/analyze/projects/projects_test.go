package projects

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/analyze"
	_ "github.com/alex-danilin/cbrt/internal/analyze/adapters/all"
)

// tree writes files (slash-separated paths -> content) under root; a path
// ending in "/" is a folder.
func tree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func claudeOnly(store string) Options {
	a, _ := analyze.Get("claude")
	return Options{TempDirs: []string{}, Adapters: []analyze.Adapter{a},
		Roots: func(analyze.Adapter) []string { return []string{store} }}
}

func TestInventoryKnownProject(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(home, "work", "inkwell")
	store := filepath.Join(base, "store")
	tree(t, home, map[string]string{".git/": ""}) // a dotfiles repository in the home folder
	tree(t, root, map[string]string{
		".git/":                       "",
		"AGENTS.md":                   "# agents\n",
		"CLAUDE.md":                   "# claude\n",
		"README.md":                   "# inkwell\n",
		"readme.de.md":                "# inkwell\n",
		"docs/deploy.md":              "x",
		"docs/internal/":              "",
		"package.json":                `{"scripts":{"test":"node t.js","build":"tsc","fixtures:gen":"node g.js"}}`,
		"Makefile":                    "VERSION := 1\n.PHONY: all\nall: build\nbuild:\n\tgo build\ntest: build\n\tgo test\nCC ::= cc\n",
		"go.mod":                      "module x\n",
		".claude/settings.json":       "{}",
		".claude/skills/release/":     "",
		".agents/rules/style.md":      "rule",
		"cmd/inkwell/main.go":         "package main\n",
		"node_modules/ignored/x.json": "{}",
	})
	slug := projectSlugOf(t, root)
	tree(t, store, map[string]string{slug + "/memory/MEMORY.md": "- a\n", slug + "/memory/topic.md": "b"})

	o := claudeOnly(store)
	o.Home = home
	cwd := filepath.Join(root, "cmd", "inkwell")
	got := Inventory(map[string]int{cwd: 3}, o)
	if len(got) != 1 {
		t.Fatalf("%d projects", len(got))
	}
	p := got[0]
	if p.Status != Known || p.Root != root || !p.Git || p.Sessions != 3 || p.Project != cwd {
		t.Fatalf("project %+v", p)
	}
	paths := func(fs []File) (out []string) {
		for _, f := range fs {
			out = append(out, f.Path)
		}
		return out
	}
	if got, want := paths(p.Instructions), []string{"AGENTS.md", ".agents/rules/style.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("instructions %q, want %q", got, want)
	}
	if got := paths(p.Readme); !reflect.DeepEqual(got, []string{"README.md", "readme.de.md"}) {
		t.Errorf("readme %q", got)
	}
	if !reflect.DeepEqual(p.Docs, []string{"deploy.md", "internal/"}) {
		t.Errorf("docs %q", p.Docs)
	}
	want := []Build{
		{File: "package.json", Names: []string{"build", "fixtures:gen", "test"}},
		{File: "Makefile", Names: []string{"all", "build", "test"}},
		{File: "go.mod"},
	}
	if !reflect.DeepEqual(p.Build, want) {
		t.Errorf("build %+v", p.Build)
	}
	kinds := map[string]ToolContext{}
	for _, c := range p.ToolContext {
		if c.Tool != "claude" {
			t.Errorf("tool context of %q", c.Tool)
		}
		kinds[c.Kind+" "+c.Path] = c
	}
	if c, ok := kinds["instructions CLAUDE.md"]; !ok || c.Bytes != int64(len("# claude\n")) {
		t.Errorf("CLAUDE.md: %+v", kinds)
	}
	if _, ok := kinds["settings .claude/settings.json"]; !ok {
		t.Errorf("settings missing: %+v", kinds)
	}
	if c := kinds["skills .claude/skills"]; !reflect.DeepEqual(c.Names, []string{"release/"}) {
		t.Errorf("skills %+v", c)
	}
	found := false
	for k, c := range kinds {
		if strings.HasPrefix(k, "memory ") {
			found = c.Files == 2
		}
	}
	if !found {
		t.Errorf("memory folder not counted: %+v", kinds)
	}
	b, _ := json.Marshal(p)
	for _, leak := range []string{"# claude", "node t.js", "go build", "module x"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("file content %q in the inventory", leak)
		}
	}
}

func projectSlugOf(t *testing.T, root string) string {
	t.Helper()
	a, _ := analyze.Get("claude")
	for _, c := range a.(analyze.ProjectContexter).ProjectContext(root, []string{"S"}) {
		if c.Kind == "memory" {
			rest := strings.TrimPrefix(c.Path, "S")
			parts := strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' })
			return parts[0]
		}
	}
	t.Fatal("no memory path")
	return ""
}

func TestInventoryUnknownAndPlain(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	plain := filepath.Join(home, "notes")
	tmp := filepath.Join(base, "tmp")
	tree(t, plain, map[string]string{"README.txt": "notes"})
	tree(t, tmp, map[string]string{"x/a.txt": "a"})
	tree(t, home, map[string]string{"scratchpad/s/": ""})
	o := Options{Home: home, TempDirs: []string{tmp}}
	keys := map[string]int{
		"":   1,
		home: 2,
		strings.ToUpper(home[:1]) + home[1:] + string(filepath.Separator): 1, // same folder, other spelling
		filepath.Join(tmp, "x"):                1,
		filepath.Join(home, "gone"):            1,
		filepath.Join(home, "scratchpad", "s"): 1,
		plain:                                  4,
	}
	got := map[string]Project{}
	for _, p := range Inventory(keys, o) {
		got[p.Project] = p
	}
	if p := got[plain]; p.Status != Known || p.Git || p.Root != plain || len(p.Readme) != 1 || len(p.Instructions) != 0 {
		t.Errorf("plain folder: %+v", p)
	}
	for k, want := range map[string]string{
		"": "no working directory", home: "home folder", filepath.Join(tmp, "x"): "temporary",
		filepath.Join(home, "gone"): "no longer exists", filepath.Join(home, "scratchpad", "s"): "scratch",
	} {
		if p := got[k]; p.Status != Unknown || !strings.Contains(p.Reason, want) || p.Root != "" {
			t.Errorf("%q: %+v, want unknown (%s)", k, p, want)
		}
	}
}

// Windows-style paths are taken apart the same way on every OS.
func TestWindowsPaths(t *testing.T) {
	for _, c := range []struct{ in, up string }{
		{`D:\Base\Me\work\p`, `D:\Base\Me\work`}, {`C:\a`, `C:\`}, {`C:\a\`, `C:\`}, {"/srv/a/b", "/srv/a"}, {"/a", "/"},
	} {
		if got, ok := parent(c.in); !ok || got != c.up {
			t.Errorf("parent(%q) = %q, %v; want %q", c.in, got, ok, c.up)
		}
	}
	if !same(`D:\Base\Me`, "d:/base/me/") || !under(`D:\Base\Me\Cache\T\x`, `d:/base/me/cache/t`) || under(`D:\Base\Me2`, `D:\Base\Me`) {
		t.Error("same/under on drive paths")
	}
	if !isRoot(`C:\`) || !isRoot("/") || isRoot(`C:\a`) {
		t.Error("isRoot")
	}
	if got := join(`C:\work\p`, ".claude/skills"); got != `C:\work\p\.claude\skills` {
		t.Errorf("join = %q", got)
	}
	if got := join("/work/p", ".claude/skills"); got != "/work/p/.claude/skills" {
		t.Errorf("join = %q", got)
	}
	if !isAbs(`C:\x`) || !isAbs("/x") || isAbs("docs/x") {
		t.Error("isAbs")
	}
	// A Windows-style project key that does not exist here is unknown, not a crash.
	p := Inventory(map[string]int{`Z:\nowhere\proj`: 1}, Options{Home: `D:\Home\Me`, TempDirs: []string{}})[0]
	if p.Status != Unknown {
		t.Errorf("missing drive path: %+v", p)
	}
	if p := Inventory(map[string]int{`d:\home\me`: 1}, Options{Home: `D:\Home\Me`, TempDirs: []string{}})[0]; !strings.Contains(p.Reason, "home") {
		t.Errorf("home in another spelling: %+v", p)
	}
}

func TestRefusal(t *testing.T) {
	home := "C:\\Users\\user"
	for dir, want := range map[string]string{
		"":                            RefuseEmpty,
		"c:\\users\\USER\\":           RefuseHome,
		`C:\Users`:                    RefuseAbove,
		"C:/Users/":                   RefuseAbove,
		`C:\`:                         RefuseRoot,
		"C:\\Users\\user\\work\\p":    "",
		`C:\Temp\x`:                   RefuseTemp,
		"C:\\Users\\user\\scratchpad": RefuseScratch,
	} {
		if got := Refusal(dir, home, []string{`C:\Temp`}); got != want {
			t.Errorf("%q: %q, want %q", dir, got, want)
		}
	}
	for dir, want := range map[string]string{"/home": RefuseAbove, "/": RefuseRoot, "/home/user/p": ""} {
		if got := Refusal(dir, "/home/user", nil); got != want {
			t.Errorf("%q: %q, want %q", dir, got, want)
		}
	}
}

func TestInventoryDeterministic(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "p")
	tree(t, root, map[string]string{".git/": "", "AGENTS.md": "a", "docs/b.md": "b", "docs/a.md": "a",
		"package.json": `{"scripts":{"z":"1","a":"2","m":"3"}}`, ".claude/skills/b/": "", ".claude/skills/a/": ""})
	o := claudeOnly(filepath.Join(base, "store"))
	o.Home = filepath.Join(base, "home")
	keys := map[string]int{root: 1, filepath.Join(root, "docs"): 2}
	a, _ := json.Marshal(Inventory(keys, o))
	b, _ := json.Marshal(Inventory(keys, o))
	if string(a) != string(b) {
		t.Fatal("inventory differs between identical runs")
	}
}
