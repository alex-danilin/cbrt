package agents

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installTo(t *testing.T, home, user, name string, tools ...Tool) {
	t.Helper()
	if err := Install(name, InstallOptions{Home: home, UserHome: user, Tools: tools, Write: true}); err != nil {
		t.Fatal(err)
	}
}

func TestUninstall(t *testing.T) {
	home, user := t.TempDir(), t.TempDir()
	if _, err := Import(fixture("claude/reviewer.md"), ImportOptions{Home: home}); err != nil {
		t.Fatal(err)
	}
	// A file of the user's at Claude Code's path: install backs it up.
	claude := TargetPath(Claude, user, "cbrt-reviewer")
	if err := os.MkdirAll(filepath.Dir(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "---\nname: cbrt-reviewer\ndescription: mine\n---\nMy own.\n"
	if err := os.WriteFile(claude, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	installTo(t, home, user, "reviewer")
	agy, codex := TargetPath(Antigravity, user, "cbrt-reviewer"), TargetPath(Codex, user, "cbrt-reviewer")

	var out bytes.Buffer
	if err := Uninstall("reviewer", UninstallOptions{UserHome: user, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would restore cbrt-reviewer.md.bak") || !strings.Contains(out.String(), "--write") {
		t.Errorf("dry run report:\n%s", out.String())
	}
	for _, p := range []string{claude, agy, codex, claude + ".bak"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("dry run changed %s: %v", p, err)
		}
	}

	if err := Uninstall("cbrt-reviewer", UninstallOptions{UserHome: user, Write: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(claude); err != nil || string(got) != mine {
		t.Errorf("the user's file is not restored: %q, %v", got, err)
	}
	for _, p := range []string{claude + ".bak", agy, filepath.Dir(agy), codex} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s still exists", p)
		}
	}

	// Again: the user's file is not cbrt's and is refused; the rest is not installed.
	out.Reset()
	err := Uninstall("reviewer", UninstallOptions{UserHome: user, Write: true, Out: &out})
	if err == nil || !strings.Contains(out.String(), "not cbrt's") || strings.Count(out.String(), "not installed") != 2 {
		t.Errorf("second uninstall: %v\n%s", err, out.String())
	}
	if got, _ := os.ReadFile(claude); string(got) != mine {
		t.Error("a refused uninstall changed the user's file")
	}

	// --tools limits it.
	installTo(t, home, user, "reviewer", Codex, Antigravity)
	if err := Uninstall("reviewer", UninstallOptions{UserHome: user, Write: true, Tools: []Tool{Codex}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agy); err != nil {
		t.Errorf("--tools codex removed Antigravity's file: %v", err)
	}
	if _, err := os.Stat(codex); err == nil {
		t.Error("--tools codex left Codex's file")
	}
	if err := Uninstall("../x", UninstallOptions{UserHome: user}); err == nil {
		t.Error("a path as the name was accepted")
	}
}

func TestList(t *testing.T) {
	home, user := t.TempDir(), t.TempDir()
	es, err := ListAgents(home, user)
	if err != nil || len(es) != 0 {
		t.Fatalf("empty: %v, %v", es, err)
	}
	var b bytes.Buffer
	WriteList(&b, es)
	if !strings.Contains(b.String(), "no agents imported yet") {
		t.Errorf("empty list: %q", b.String())
	}

	for _, f := range []string{"claude/reviewer.md", "claude/minimal.md", "codex/doc-writer.toml"} {
		if _, err := Import(fixture(f), ImportOptions{Home: home}); err != nil {
			t.Fatal(err)
		}
	}
	installTo(t, home, user, "reviewer", Claude, Codex)
	installTo(t, home, user, "doc-writer")
	if err := os.Remove(filepath.Join(home, "cbrt-doc-writer.md")); err != nil { // canonical copy gone
		t.Fatal(err)
	}
	mine := TargetPath(Antigravity, user, "cbrt-minimal")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("---\nname: cbrt-minimal\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	es, err = ListAgents(home, user)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][4]string{ // canonical, claude, antigravity, codex
		"cbrt-doc-writer": {"false", "installed", "installed", "installed"},
		"cbrt-minimal":    {"true", "-", "not cbrt's", "-"},
		"cbrt-reviewer":   {"true", "installed", "-", "installed"},
	}
	if len(es) != len(want) {
		t.Fatalf("want %d rows, got %+v", len(want), es)
	}
	for _, e := range es {
		got := [4]string{map[bool]string{true: "true", false: "false"}[e.Canonical], string(e.States[Claude]), string(e.States[Antigravity]), string(e.States[Codex])}
		if got != want[e.Name] {
			t.Errorf("%s: got %v, want %v", e.Name, got, want[e.Name])
		}
	}
	b.Reset()
	WriteList(&b, es)
	if !strings.Contains(b.String(), "cbrt-doc-writer (no canonical copy)") {
		t.Errorf("list output:\n%s", b.String())
	}
}

// Every agent cbrt installs is named cbrt-…: the file or folder name and the
// name inside the file, for every fixture and every tool.
func TestInstalledNamesStartWithCbrt(t *testing.T) {
	home, agents := importAll(t)
	user := t.TempDir()
	for _, a := range agents {
		installTo(t, home, user, a.Name)
	}
	files := 0
	err := filepath.WalkDir(user, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		rel, _ := filepath.Rel(user, path)
		segs := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
		// .claude/agents/<name>.md, .codex/agents/<name>.toml, .gemini/config/agents/<name>/agent.md
		fileName := strings.TrimSuffix(strings.TrimSuffix(segs[len(segs)-1], ".md"), ".toml")
		if segs[0] == ".gemini" {
			fileName = segs[len(segs)-2]
		}
		if !strings.HasPrefix(fileName, Prefix) || strings.HasPrefix(fileName, Prefix+Prefix) {
			t.Errorf("%s: file name %q", rel, fileName)
		}
		var inside string
		if strings.HasSuffix(path, ".toml") {
			raw, _ := os.ReadFile(path)
			fields, err := parseTOML(string(raw))
			if err != nil {
				return err
			}
			for _, f := range fields {
				if f.Key == "name" {
					inside = f.Value.Str
				}
			}
		} else {
			a, _, err := Parse(path, "")
			if err != nil {
				return err
			}
			inside = a.Name
		}
		if inside != fileName {
			t.Errorf("%s: name inside is %q, file name %q", rel, inside, fileName)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := len(agents) * len(Tools); files != want {
		t.Errorf("want %d installed files, got %d", want, files)
	}
}
