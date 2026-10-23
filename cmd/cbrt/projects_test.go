package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/projects"
)

func TestProjectsCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv(projects.HomeEnv, home)
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runProjects(args, &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, out, _ := run("list"); code != 0 || !strings.Contains(out, "no connected projects") {
		t.Errorf("list: %d %q", code, out)
	}
	if code, out, _ := run("help"); code != 0 || !strings.Contains(out, "connect <dir> [--dry-run]") {
		t.Errorf("help: %d %q", code, out)
	}
	if code, _, _ := run("bogus"); code != 2 {
		t.Errorf("bogus: %d", code)
	}
	// The CLI uses the machine's rules: the home folder (here a temp folder
	// named as home) is refused before anything runs or is written.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)
	if code, _, errs := run("connect", fakeHome, "--dry-run"); code != 1 || !strings.Contains(errs, "refusing") {
		t.Errorf("connect home: %d %q", code, errs)
	}
	if code, _, errs := run("connect", filepath.Dir(fakeHome)); code != 1 || !strings.Contains(errs, "refusing") {
		t.Errorf("connect above home: %d %q", code, errs)
	}
	if code, _, errs := run("connect", t.TempDir(), "--public", "--private", "--dry-run"); code != 1 || !strings.Contains(errs, "exclude each other") {
		t.Errorf("connect --public --private: %d %q", code, errs)
	}
	if code, _, errs := run("connect"); code != 1 || !strings.Contains(errs, "usage") {
		t.Errorf("connect without dir: %d %q", code, errs)
	}
	if code, _, errs := run("disconnect", "nope"); code != 1 || !strings.Contains(errs, `no connected project "nope"`) {
		t.Errorf("disconnect: %d %q", code, errs)
	}
	if code, _, errs := run("check", "nope", "--agent", "claude"); code != 1 || !strings.Contains(errs, `no connected project "nope"`) {
		t.Errorf("check: %d %q", code, errs)
	}
	if code, _, errs := run("check", "--agent=codex", "--dry-run", "nope"); code != 1 || !strings.Contains(errs, `no connected project "nope"`) {
		t.Errorf("check --agent=: %d %q", code, errs)
	}
	if code, _, errs := run("check", "nope"); code != 1 || !strings.Contains(errs, "--agent is required") {
		t.Errorf("check without --agent: %d %q", code, errs)
	}
	if code, _, errs := run("connect", t.TempDir(), "--agent", "gemini", "--dry-run"); code != 1 || !strings.Contains(errs, "want one of") {
		t.Errorf("connect --agent gemini: %d %q", code, errs)
	}
	if _, out, _ := run("help"); !strings.Contains(out, "check <name|dir> --agent claude|codex|agy") {
		t.Errorf("help lacks check")
	}
	if _, err := os.Stat(filepath.Join(home, projects.ListFile)); err == nil {
		t.Errorf("the list was written")
	}
	for _, addr := range []string{"0.0.0.0:8091", "192.168.1.5:8091", ":8091", "example.com:80"} {
		if code, _, errs := run("serve", "--listen", addr, "--no-open"); code != 1 || !strings.Contains(errs, "listen") {
			t.Errorf("serve --listen %s: %d %q", addr, code, errs)
		}
	}
}
