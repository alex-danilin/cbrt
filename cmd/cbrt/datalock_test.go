package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestClaimDataDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "data")

	use1, release1, note1, err := claimDataDir(base, dir)
	if err != nil || use1 != dir || note1 != "" {
		t.Fatalf("first claim: %q %q %v", use1, note1, err)
	}
	// A second launcher while the first runs gets a temporary session dir.
	use2, release2, note2, err := claimDataDir(base, dir)
	if err != nil || use2 == dir || !strings.HasPrefix(filepath.Base(use2), "session-") || !strings.Contains(note2, "another cbrt launcher") {
		t.Fatalf("second claim: %q %q %v", use2, note2, err)
	}
	release2()
	if _, err := os.Stat(use2); !os.IsNotExist(err) {
		t.Error("session dir not removed on release")
	}
	release1()
	// Once released, the shared dir can be claimed again.
	use3, release3, _, err := claimDataDir(base, dir)
	if err != nil || use3 != dir {
		t.Fatalf("claim after release: %q %v", use3, err)
	}
	release3()
}

func TestClaimDataDirTakesOverStaleLock(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	lock := filepath.Join(dir, lockName)
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	// A PID that is certainly not running (max int32 is never assigned in practice).
	os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(1<<31-1)), 0o600)
	use, release, note, err := claimDataDir(base, dir)
	if err != nil || use != dir || note != "" {
		t.Fatalf("stale lock not taken over: %q %q %v", use, note, err)
	}
	b, _ := os.ReadFile(filepath.Join(lock, "pid"))
	if string(b) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock pid = %s", b)
	}
	release()
}

func TestClaudeSubcommandGetsNoSettings(t *testing.T) {
	got := claudeArgs(`C:\cbrt\cbrt.exe`, "http://127.0.0.1:1", "", "windows", []string{"mcp", "list"})
	if len(got) != 2 || got[0] != "mcp" {
		t.Errorf("subcommand args = %q", got)
	}
	got = claudeArgs(`C:\cbrt\cbrt.exe`, "http://127.0.0.1:1", "", "windows", []string{"-p", "hi"})
	if got[0] != "--settings" {
		t.Errorf("session args = %q", got)
	}
}
