package agents

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var validFixtures = []string{
	"claude/reviewer.md", "claude/db-analyst.md", "claude/release-helper.md", "claude/minimal.md",
	"claude/cbrt-changelog.md", "antigravity/triage/agent.md", "antigravity/notes-writer.md",
	"codex/test_runner.toml", "codex/doc-writer.toml",
}

// importAll imports every valid fixture into a temporary canonical folder.
func importAll(t *testing.T) (home string, agents []Agent) {
	t.Helper()
	home = t.TempDir()
	for _, f := range validFixtures {
		r, err := Import(fixture(f), ImportOptions{Home: home})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		agents = append(agents, r.Agent)
	}
	return home, agents
}

// written reports whether a field Plan warns about is still written: a
// mapped tool list for Antigravity, or the shell switch for Codex.
func written(n Note, t Tool) bool {
	return t == Antigravity && n.Field == "tools"
}

func TestInstallRoundTrip(t *testing.T) {
	home, all := importAll(t)
	user := t.TempDir()
	var out bytes.Buffer
	for _, a := range all {
		if err := Install(a.Name, InstallOptions{Home: home, UserHome: user, Write: true, Out: &out}); err != nil {
			t.Fatalf("%s: %v\n%s", a.Name, err, out.String())
		}
		canon, err := Read(filepath.Join(home, a.Name+".md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range Tools {
			path := TargetPath(tool, user, a.Name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s %s: %v", a.Name, tool, err)
			}
			if strings.Count(string(raw), Marker) != 1 {
				t.Errorf("%s: marker missing or repeated", path)
			}
			if tool == Codex {
				checkCodex(t, canon, path, string(raw))
				continue
			}
			got, _, err := Parse(path, tool)
			if err != nil {
				t.Fatalf("%s: %v\n%s", path, err, raw)
			}
			if got.Name != canon.Name || got.Description != canon.Description || strings.TrimSpace(got.Body) != strings.TrimSpace(canon.Body) {
				t.Errorf("%s: name, description or body changed:\n%s", path, raw)
			}
			for _, n := range Plan(canon, tool)[3:] {
				_, has := got.Get(n.Field)
				switch {
				case carried(n) || written(n, tool):
					if !has {
						t.Errorf("%s: %s (%c) missing", path, n.Field, n.Code)
					}
					if tool == Claude {
						want, _ := canon.Get(n.Field)
						if v, _ := got.Get(n.Field); !reflect.DeepEqual(v, want) {
							t.Errorf("%s: %s = %+v, want %+v", path, n.Field, v, want)
						}
					}
				case has && !(tool == Antigravity && n.Field == "inheritMcp"):
					t.Errorf("%s: %s (%c) written", path, n.Field, n.Code)
				}
			}
			if tool == Antigravity {
				for _, k := range []string{"mainAgent", "subagent"} {
					if _, ok := got.Get(k); !ok {
						t.Errorf("%s: no %s", path, k)
					}
				}
				if _, mcp := canon.Get("mcpServers"); mcp {
					if v, _ := got.Get("inheritMcp"); v.Str != "true" {
						t.Errorf("%s: mcpServers without inheritMcp: true", path)
					}
				}
			}
		}
	}
}

func checkCodex(t *testing.T, canon Agent, path, raw string) {
	t.Helper()
	fields, err := parseTOML(raw)
	if err != nil {
		t.Fatalf("%s: %v\n%s", path, err, raw)
	}
	got := map[string]Value{}
	for _, f := range fields {
		got[f.Key] = f.Value
	}
	if got["name"].Str != codexName(canon.Name) || got["description"].Str != canon.Description ||
		strings.TrimSpace(got["developer_instructions"].Str) != strings.TrimSpace(canon.Body) {
		t.Errorf("%s: name, description or instructions changed:\n%s", path, raw)
	}
	allowed := map[string]bool{"name": true, "description": true, "developer_instructions": true}
	shellOff := false
	for _, n := range Plan(canon, Codex)[3:] {
		if strings.Contains(n.Detail, "features.shell_tool = false") {
			shellOff = true
		}
		if n.Field == "effort" && carried(n) {
			allowed["model_reasoning_effort"] = true
			_, want, _ := strings.Cut(n.Detail, "= ")
			want, _, _ = strings.Cut(want, " ")
			if got["model_reasoning_effort"].Str != want {
				t.Errorf("%s: model_reasoning_effort %q, want %q", path, got["model_reasoning_effort"].Str, want)
			}
		}
	}
	if shellOff {
		allowed["features.shell_tool"] = true
		if got["features.shell_tool"].Str != "false" {
			t.Errorf("%s: shell not switched off", path)
		}
	}
	for k := range got {
		if !allowed[k] {
			t.Errorf("%s: unexpected key %s", path, k)
		}
	}
	for k := range allowed {
		if _, ok := got[k]; !ok {
			t.Errorf("%s: missing key %s", path, k)
		}
	}
}

func TestRenderCodexDetails(t *testing.T) {
	home, _ := importAll(t)
	db, err := Read(filepath.Join(home, "cbrt-db-analyst.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := RenderCodex(db)
	for _, want := range []string{"# " + Marker + "\n", `model_reasoning_effort = "high"`, "features.shell_tool = false", `\"How many tasks are overdue?\"`} {
		if !strings.Contains(s, want) {
			t.Errorf("db-analyst for Codex lacks %q:\n%s", want, s)
		}
	}
	if !strings.HasPrefix(s, "# "+Marker) {
		t.Error("the marker is not the first line")
	}
	ag := RenderAntigravity(db)
	if !strings.HasPrefix(ag, "---\n# "+Marker+"\n") {
		t.Errorf("Antigravity marker:\n%s", ag)
	}
	if strings.Contains(strings.ToLower(Marker), "claude") || strings.Contains(strings.ToLower(Marker), "codex") || strings.Contains(strings.ToLower(Marker), "antigravity") {
		t.Error("the marker names a tool")
	}
}

func TestInstallFiles(t *testing.T) {
	home, _ := importAll(t)
	user := t.TempDir()
	install := func(write bool, tools ...Tool) (string, error) {
		var out bytes.Buffer
		err := Install("reviewer", InstallOptions{Home: home, UserHome: user, Write: write, Tools: tools, Out: &out})
		return out.String(), err
	}
	claude := TargetPath(Claude, user, "cbrt-reviewer")
	codex := TargetPath(Codex, user, "cbrt-reviewer")
	agy := TargetPath(Antigravity, user, "cbrt-reviewer")

	out, err := install(false)
	if err != nil || !strings.Contains(out, "would write") || !strings.Contains(out, "add --write to apply") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if ents, _ := os.ReadDir(user); len(ents) != 0 {
		t.Fatalf("dry run wrote %d entries", len(ents))
	}
	if !strings.Contains(out, "D model") {
		t.Errorf("dropped fields not shown:\n%s", out)
	}

	if out, err = install(true); err != nil || strings.Contains(out, "dry run") {
		t.Fatalf("write: %v\n%s", err, out)
	}
	for _, p := range []string{claude, codex, agy} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	if out, err = install(true); err != nil || strings.Count(out, "replace") != 3 {
		t.Fatalf("second install: %v\n%s", err, out)
	}

	// The user's own file is backed up once.
	if err := os.WriteFile(claude, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err = install(true, Claude); err != nil || !strings.Contains(out, "back up to cbrt-reviewer.md.bak") {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(claude + ".bak"); string(b) != "mine" {
		t.Errorf("backup holds %q", b)
	}
	if b, _ := os.ReadFile(claude); !strings.Contains(string(b), Marker) {
		t.Error("not replaced after the backup")
	}

	// With a backup in the way, that tool is refused and the others install.
	if err := os.WriteFile(claude, []byte("mine again"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(codex)
	out, err = install(true)
	if err == nil || !strings.Contains(err.Error(), "claude") || !strings.Contains(out, "refused") {
		t.Fatalf("existing backup: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(claude + ".bak"); string(b) != "mine" {
		t.Errorf("backup overwritten: %q", b)
	}
	if b, _ := os.ReadFile(claude); string(b) != "mine again" {
		t.Errorf("refused file changed: %q", b)
	}
	if _, err := os.Stat(codex); err != nil {
		t.Errorf("codex not installed beside the refusal: %v", err)
	}

	var buf bytes.Buffer
	err = Install("nope", InstallOptions{Home: home, UserHome: user, Out: &buf})
	if err == nil || !strings.Contains(err.Error(), "cbrt-reviewer") {
		t.Errorf("missing agent: %v", err)
	}
	if err := Install("cbrt-minimal", InstallOptions{Home: home, UserHome: user, Out: &buf}); err != nil {
		t.Errorf("name with the prefix: %v", err)
	}
}

// A canonical file whose name was edited to a path is refused: the name
// becomes a path in each tool's folder.
func TestInstallRefusesEditedName(t *testing.T) {
	home, user := t.TempDir(), t.TempDir()
	if _, err := Import(fixture("claude/minimal.md"), ImportOptions{Home: home}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "cbrt-minimal.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "name: cbrt-minimal", "name: ../../escape", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Install("minimal", InstallOptions{Home: home, UserHome: user, Write: true})
	if err == nil || !strings.Contains(err.Error(), "the file name") {
		t.Fatalf("edited name: %v", err)
	}
	if ents, _ := os.ReadDir(user); len(ents) != 0 {
		t.Fatalf("wrote %d entries into the user home", len(ents))
	}
}
