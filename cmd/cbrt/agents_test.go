package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/agents"
)

func TestAgentsImportCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv(agents.HomeEnv, home)
	src := filepath.Join("..", "..", "testdata", "agents", "codex", "test_runner.toml")
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runAgents(args, &out, &errb)
		return code, out.String(), errb.String()
	}

	if code, out, _ := run("import", src, "--dry-run"); code != 0 || !strings.Contains(out, "would import") {
		t.Fatalf("dry run: exit %d\n%s", code, out)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Fatalf("dry run wrote %d files", len(ents))
	}
	code, out, errs := run("import", src)
	if code != 0 || !strings.Contains(out, "as cbrt-test_runner") {
		t.Fatalf("import: exit %d\n%s%s", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(home, "cbrt-test_runner.md")); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run("import", src); code != 1 || !strings.Contains(errs, "--force") {
		t.Fatalf("second import: exit %d, %s", code, errs)
	}
	if code, _, _ := run("import", "--force", src); code != 0 {
		t.Fatalf("--force before the file: exit %d", code)
	}
	if code, _, errs := run("import", src, "--from", "claude"); code != 1 || !strings.Contains(errs, "Codex agent") {
		t.Fatalf("--from claude on a .toml: exit %d, %s", code, errs)
	}
	if code, _, errs := run("import"); code != 1 || !strings.Contains(errs, "usage: cbrt agents import") {
		t.Fatalf("no file: exit %d, %s", code, errs)
	}
	if code, _, _ := run("nope"); code != 2 {
		t.Fatalf("unknown command: exit %d", code)
	}
}

func TestAgentsInstallCommand(t *testing.T) {
	home, user := t.TempDir(), t.TempDir()
	t.Setenv(agents.HomeEnv, home)
	t.Setenv("HOME", user)
	t.Setenv("USERPROFILE", user)
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runAgents(args, &out, &errb)
		return code, out.String(), errb.String()
	}
	src := filepath.Join("..", "..", "testdata", "agents", "claude", "reviewer.md")
	if code, out, errs := run("import", src); code != 0 {
		t.Fatalf("import: exit %d\n%s%s", code, out, errs)
	}
	claude := filepath.Join(user, ".claude", "agents", "cbrt-reviewer.md")
	agy := filepath.Join(user, ".gemini", "config", "agents", "cbrt-reviewer", "agent.md")
	codex := filepath.Join(user, ".codex", "agents", "cbrt-reviewer.toml")

	code, out, errs := run("install", "reviewer")
	if code != 0 || !strings.Contains(out, "would write") || !strings.Contains(out, "--write") {
		t.Fatalf("dry run: exit %d\n%s%s", code, out, errs)
	}
	if ents, _ := os.ReadDir(user); len(ents) != 0 {
		t.Fatalf("dry run wrote %d entries", len(ents))
	}
	if code, out, errs = run("install", "--tools", "claude,agy", "reviewer", "--write"); code != 0 {
		t.Fatalf("--tools subset: exit %d\n%s%s", code, out, errs)
	}
	for p, want := range map[string]bool{claude: true, agy: true, codex: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists: %v, want %v", p, err == nil, want)
		}
	}
	if code, out, errs = run("install", "cbrt-reviewer", "--write"); code != 0 || strings.Count(out, "replace") != 2 {
		t.Fatalf("--write: exit %d\n%s%s", code, out, errs)
	}
	if _, err := os.Stat(codex); err != nil {
		t.Error(err)
	}
	if code, _, errs = run("install", "reviewer", "--tools", "vim"); code != 1 || !strings.Contains(errs, "unknown tool") {
		t.Fatalf("unknown tool: exit %d, %s", code, errs)
	}
	if code, _, errs = run("install", "nobody"); code != 1 || !strings.Contains(errs, "cbrt-reviewer") {
		t.Fatalf("unknown agent: exit %d, %s", code, errs)
	}

	if code, out, errs = run("list"); code != 0 || !strings.Contains(out, "cbrt-reviewer") || strings.Count(out, "installed") != 3 {
		t.Fatalf("list: exit %d\n%s%s", code, out, errs)
	}
	if code, out, _ = run("uninstall", "reviewer"); code != 0 || !strings.Contains(out, "would remove") {
		t.Fatalf("uninstall dry run: exit %d\n%s", code, out)
	}
	if code, out, errs = run("uninstall", "reviewer", "--tools", "codex", "--write"); code != 0 {
		t.Fatalf("uninstall --tools codex: exit %d\n%s%s", code, out, errs)
	}
	if _, err := os.Stat(codex); err == nil {
		t.Error("uninstall left Codex's file")
	}
	if _, err := os.Stat(claude); err != nil {
		t.Errorf("uninstall --tools codex removed Claude Code's file: %v", err)
	}
	if code, _, errs = run("list", "x"); code != 1 || !strings.Contains(errs, "unexpected arguments") {
		t.Fatalf("list with an argument: exit %d, %s", code, errs)
	}
}
