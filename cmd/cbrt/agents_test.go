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
