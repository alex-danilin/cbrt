package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/hook"
)

func TestHookRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv(analyze.HomeEnv, home)
	var stderr bytes.Buffer
	stdin := "{\"session\":\"s1\",\n\"note\":\"invented\"}\n"
	if code := runHook([]string{"record", "fixturetool", "Ask"}, strings.NewReader(stdin), &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr %q", stderr.String())
	}
	envs, problems, err := hook.ReadSpool(home)
	if err != nil || len(problems) != 0 || len(envs) != 1 {
		t.Fatalf("envs %+v problems %v err %v", envs, problems, err)
	}
	if e := envs[0]; e.AdapterID != "fixturetool" || e.Event != "Ask" || e.Stdin != stdin {
		t.Errorf("envelope %+v", e)
	}
}

func TestHookUsage(t *testing.T) {
	t.Setenv(analyze.HomeEnv, t.TempDir())
	for _, args := range [][]string{nil, {"record"}, {"record", "x"}, {"play", "x", "y"}, {"record", "x", "y", "z"}, {"record", "", "y"}} {
		var stderr bytes.Buffer
		if code := runHook(args, strings.NewReader(""), &stderr); code != 2 || !strings.Contains(stderr.String(), "usage") {
			t.Errorf("%q: exit %d, stderr %q", args, code, stderr.String())
		}
	}
}

// A failing write still exits 0, prints nothing and lands in errors.log.
func TestHookRecordFailureIsSilent(t *testing.T) {
	home := t.TempDir()
	t.Setenv(analyze.HomeEnv, home)
	if err := os.MkdirAll(analyze.RegistryDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	// A file where the spool directory should be.
	if err := os.WriteFile(analyze.SpoolDir(home), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := runHook([]string{"record", "fixturetool", "End"}, strings.NewReader("{}"), &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr %q", stderr.String())
	}
	log, err := os.ReadFile(filepath.Join(analyze.RegistryDir(home), "errors.log"))
	if err != nil || !strings.Contains(string(log), "hook record fixturetool End") {
		t.Fatalf("errors.log %q (%v)", log, err)
	}
}

func TestHookErrorLogBounded(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(analyze.RegistryDir(home), "errors.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), hookErrorLogMax), 0o600); err != nil {
		t.Fatal(err)
	}
	logHookError(home, "a", "e", os.ErrPermission)
	st, err := os.Stat(path)
	if err != nil || st.Size() >= hookErrorLogMax {
		t.Fatalf("errors.log not rotated: %v %v", st, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("rotated log: %v", err)
	}
}
