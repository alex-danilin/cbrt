package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/skills"
)

// analyzeHome points CBRT_ANALYZE_HOME and CLAUDE_CONFIG_DIR at temp dirs,
// with a config that reads only the synthetic fixtures.
func analyzeHome(t *testing.T) (home, claudeDir string) {
	t.Helper()
	home, claudeDir = t.TempDir(), t.TempDir()
	t.Setenv("CBRT_ANALYZE_HOME", home)
	prev := isTransient
	isTransient = func(string) bool { return false } // tests run from a go-build dir
	t.Cleanup(func() { isTransient = prev })
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	abs := func(p string) string {
		a, err := filepath.Abs(filepath.Join("..", "..", "testdata", "analyze", p))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	cfg := map[string]any{
		"v": 1, "cbrt_data_dirs": []string{filepath.Join(home, "none")},
		"tools": map[string]any{
			"claude":      map[string]any{"roots": []string{abs("claude/projects")}},
			"codex":       map[string]any{"roots": []string{abs("codex/sessions")}},
			"antigravity": map[string]any{"roots": []string{abs("antigravity/antigravity/brain"), abs("antigravity/antigravity-cli/brain")}},
			"fixturetool": map[string]any{"roots": []string{home}},
		},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return home, claudeDir
}

func analyzeCmd(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runAnalyze(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestAnalyzeRunAndRecs(t *testing.T) {
	home, _ := analyzeHome(t)
	code, out, errs := analyzeCmd(t, "", "run", "--since", "2026-01-01T00:00:00Z")
	if code != 0 || !strings.Contains(out, filepath.Join(home, "out")) {
		t.Fatalf("run: %d %q %q", code, out, errs)
	}
	outDir := strings.SplitN(out, "\n", 2)[0]
	if _, err := os.Stat(filepath.Join(outDir, "summary.md")); err != nil {
		t.Fatal(err)
	}

	rec := `{"title":"Add a project map","category":"instructions","evidence_refs":["claude/default/x#1"],
	  "cause":"repeated exploration","alternatives":"new project","change":"AGENTS.md section",
	  "cost_minutes":15,"confidence":"med","scope":{"tools":["claude"],"projects":["p"]},"context_checked":["AGENTS.md","package.json"],
	  "baseline":{"metric":"exploration_before_first_edit","value":12,"window":{"since":"2026-03-01T00:00:00Z","until":"2026-03-08T00:00:00Z"}},
	  "check_method":"exploration_before_first_edit drops by a third","min_evidence":">= 5 sessions","eval_window_days":7}`
	if code, out, errs := analyzeCmd(t, rec, "rec", "add", "-"); code != 0 || !strings.Contains(out, `"rec_id"`) {
		t.Fatalf("rec add: %d %q %q", code, out, errs)
	}
	if code, _, errs := analyzeCmd(t, "", "rec", "applied", "r_1", "added", "the", "map"); code != 0 {
		t.Fatalf("rec applied: %q", errs)
	}
	if code, out, _ := analyzeCmd(t, "", "rec", "list"); code != 0 || !strings.Contains(out, `"applied"`) {
		t.Fatalf("rec list: %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(home, "recommendations.md")); err != nil || !strings.Contains(string(b), "Add a project map") {
		t.Errorf("recommendations.md not exported: %v", err)
	}
	if code, _, errs := analyzeCmd(t, "", "rec", "eval", "r_1", "no_change", "too early"); code == 0 || !strings.Contains(errs, "--run") {
		t.Errorf("eval without --run accepted: %q", errs)
	}
	if code, _, errs := analyzeCmd(t, "", "run-note", "--review-tokens", "1234"); code != 0 {
		t.Errorf("run-note: %q", errs)
	}
	if code, _, _ := analyzeCmd(t, "", "rec", "add", "-"); code == 0 {
		t.Error("empty recommendation accepted")
	}
	if code, _, _ := analyzeCmd(t, "", "bogus"); code != 2 {
		t.Error("unknown command not a usage error")
	}
}

func TestAnalyzeInstallClaude(t *testing.T) {
	_, claudeDir := analyzeHome(t)
	settings := filepath.Join(claudeDir, "settings.json")
	foreign := `{
  "model": "sonnet",
  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}],
            "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]}
}`
	if err := os.WriteFile(settings, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without --write nothing changes.
	if code, out, errs := analyzeCmd(t, "", "install", "claude"); code != 0 || !strings.Contains(out, "Nothing changed") {
		t.Fatalf("dry run: %d %q %q", code, out, errs)
	}
	if b, _ := os.ReadFile(settings); string(b) != foreign {
		t.Fatal("dry run changed settings.json")
	}

	// Install twice: one cbrt hook per event, foreign hooks and keys kept.
	// The second install finds the hooks in place: it says so, and neither
	// rewrites settings.json nor backs it up again.
	if code, _, errs := analyzeCmd(t, "", "install", "claude", "--write"); code != 0 {
		t.Fatalf("install: %q", errs)
	}
	first, _ := os.ReadFile(settings)
	if code, out, errs := analyzeCmd(t, "", "install", "claude", "--write"); code != 0 ||
		!strings.Contains(out, "already installed") || strings.Contains(out, "add cbrt hooks") || strings.Contains(out, "backup:") {
		t.Fatalf("second install: %d %q %q", code, out, errs)
	}
	if again, _ := os.ReadFile(settings); string(again) != string(first) {
		t.Fatal("second install rewrote settings.json")
	}
	var got struct {
		Model string                                  `json:"model"`
		Hooks map[string][]map[string]json.RawMessage `json:"hooks"`
	}
	b, _ := os.ReadFile(settings)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "sonnet" || len(got.Hooks["PreToolUse"]) != 1 {
		t.Fatalf("foreign settings lost: %s", b)
	}
	stop := string(b)
	if strings.Count(stop, `"hook"`) != 6 || !strings.Contains(stop, "notify-send done") {
		t.Fatalf("want 6 cbrt hooks once each and the foreign Stop hook: %s", b)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "skills", "cbrt-multi-session-review", "SKILL.md")); err != nil {
		t.Error("skill not installed")
	}
	backups, _ := filepath.Glob(settings + ".cbrt-backup-*")
	if len(backups) != 1 {
		t.Errorf("want 1 backup, got %d", len(backups))
	}

	// Uninstall leaves exactly the foreign hooks.
	if code, _, errs := analyzeCmd(t, "", "uninstall", "claude", "--write"); code != 0 {
		t.Fatalf("uninstall: %q", errs)
	}
	b, _ = os.ReadFile(settings)
	if strings.Contains(string(b), `"hook"`) || !strings.Contains(string(b), "notify-send done") || !strings.Contains(string(b), "guard.sh") {
		t.Fatalf("uninstall result: %s", b)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "skills", "cbrt-multi-session-review")); !os.IsNotExist(err) {
		t.Error("skill not removed")
	}
	if code, out, errs := analyzeCmd(t, "", "uninstall", "claude", "--write"); code != 0 || !strings.Contains(out, "no cbrt hooks to remove") {
		t.Fatalf("second uninstall: %d %q %q", code, out, errs)
	}
}

func TestAnalyzeInstallNoSettingsFile(t *testing.T) {
	_, claudeDir := analyzeHome(t)
	if code, _, errs := analyzeCmd(t, "", "install", "claude", "--write"); code != 0 {
		t.Fatalf("install: %q", errs)
	}
	b, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil || !json.Valid(b) || !strings.Contains(string(b), "SessionStart") {
		t.Fatalf("settings.json: %v %s", err, b)
	}
}

func TestTransientBinary(t *testing.T) {
	tmp := os.TempDir()
	for exe, want := range map[string]bool{
		filepath.Join(tmp, "go-build123", "b001", "exe", "cbrt.exe"): true,
		filepath.Join(tmp, "anything", "cbrt.exe"):                   true,
		filepath.Join("/opt", "tools", "cbrt"):                       false,
	} {
		if got := transientBinary(exe); got != want {
			t.Errorf("transientBinary(%q) = %v", exe, got)
		}
	}
	// The real check refuses install from a test binary.
	_, _ = analyzeHome(t)
	isTransient = transientBinary
	if code, _, errs := analyzeCmd(t, "", "install", "claude"); code == 0 || !strings.Contains(errs, "temporary build") {
		t.Errorf("install from a go-build binary: %d %q", code, errs)
	}
}

func TestAnalyzeUninstallPurge(t *testing.T) {
	home, claudeDir := analyzeHome(t)
	settings := filepath.Join(claudeDir, "settings.json")
	foreign := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}]}}`
	if err := os.WriteFile(settings, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := analyzeCmd(t, "", "install", "claude", "--write"); code != 0 {
		t.Fatalf("install: %q", errs)
	}
	if code, _, errs := analyzeCmd(t, "", "run", "--since", "2026-01-01T00:00:00Z"); code != 0 {
		t.Fatalf("run: %q", errs)
	}
	if code, _, _ := analyzeCmd(t, "", "install", "claude", "--purge"); code == 0 {
		t.Error("install --purge accepted")
	}

	// Dry run: lists everything, deletes nothing.
	code, out, errs := analyzeCmd(t, "", "uninstall", "claude", "--purge")
	if code != 0 || !strings.Contains(out, "delete the analyzer home") || !strings.Contains(out, ".cbrt-backup-") || !strings.Contains(out, "Nothing changed") {
		t.Fatalf("dry run: %d %q %q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(home, "out")); err != nil {
		t.Fatal("dry run deleted data")
	}

	// A foreign entry blocks the purge before anything changes.
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(settings)
	if code, _, errs := analyzeCmd(t, "", "uninstall", "claude", "--purge", "--write"); code == 0 || !strings.Contains(errs, "notes.txt") {
		t.Fatalf("purge with a foreign entry: %d %q", code, errs)
	}
	if after, _ := os.ReadFile(settings); string(after) != string(before) {
		t.Fatal("a refused purge changed settings.json")
	}
	os.Remove(filepath.Join(home, "notes.txt"))

	if code, _, errs := analyzeCmd(t, "", "uninstall", "claude", "--purge", "--write"); code != 0 {
		t.Fatalf("purge: %q", errs)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("analyzer home still exists: %v", err)
	}
	if b, _ := filepath.Glob(settings + ".cbrt-backup-*"); len(b) != 0 {
		t.Errorf("backups left: %v", b)
	}
	b, _ := os.ReadFile(settings)
	if strings.Contains(string(b), `"hook"`) || !strings.Contains(string(b), "notify-send done") {
		t.Errorf("settings after purge: %s", b)
	}
}

// --with installs optional skills; uninstall --keep-skills leaves every skill
// in place; plain uninstall removes the cbrt-made ones and keeps a hand-made
// skill of the same name.
func TestAnalyzeInstallOptionalSkills(t *testing.T) {
	_, claudeDir := analyzeHome(t)
	skill := func(name string) string { return filepath.Join(claudeDir, "skills", name, "SKILL.md") }
	exists := func(name string) bool { _, err := os.Stat(skill(name)); return err == nil }

	for _, args := range [][]string{
		{"install", "claude", "--with", "bogus"},
		{"uninstall", "claude", "--with", "cbrt-session-review"},
		{"install", "claude", "--keep-skills"},
		{"uninstall", "claude", "--keep-skills", "--purge"},
	} {
		if code, _, _ := analyzeCmd(t, "", args...); code == 0 {
			t.Errorf("%v accepted", args)
		}
	}

	code, out, errs := analyzeCmd(t, "", "install", "claude", "--with", "all")
	if code != 0 || !strings.Contains(out, "write the /cbrt-session-review skill") || exists("cbrt-session-review") {
		t.Fatalf("dry run: %d %q %q", code, out, errs)
	}
	if code, out, errs := analyzeCmd(t, "", "install", "claude", "--with", "cbrt-session-review", "--write"); code != 0 || !strings.Contains(out, "/cbrt-session-review at the end") {
		t.Fatalf("install: %d %q %q", code, out, errs)
	}
	if !exists("cbrt-session-review") || !exists("cbrt-multi-session-review") {
		t.Fatal("skills not installed")
	}

	if code, _, errs := analyzeCmd(t, "", "uninstall", "claude", "--keep-skills", "--write"); code != 0 {
		t.Fatalf("uninstall --keep-skills: %q", errs)
	}
	if b, _ := os.ReadFile(filepath.Join(claudeDir, "settings.json")); strings.Contains(string(b), `"hook"`) {
		t.Error("hooks left after uninstall --keep-skills")
	}
	if !exists("cbrt-session-review") || !exists("cbrt-multi-session-review") {
		t.Fatal("uninstall --keep-skills removed a skill")
	}

	if code, _, errs := analyzeCmd(t, "", "uninstall", "claude", "--write"); code != 0 {
		t.Fatalf("uninstall: %q", errs)
	}
	if exists("cbrt-session-review") || exists("cbrt-multi-session-review") {
		t.Fatal("uninstall left a cbrt skill")
	}

	// A hand-made skill of the same name survives install and uninstall.
	os.MkdirAll(filepath.Dir(skill("cbrt-session-review")), 0o700)
	os.WriteFile(skill("cbrt-session-review"), []byte("mine\n"), 0o600)
	if code, out, _ := analyzeCmd(t, "", "install", "claude", "--with", "all", "--write"); code != 0 || !strings.Contains(out, "keep, not made by cbrt") {
		t.Fatalf("install over a hand-made skill: %q", out)
	}
	if code, _, _ := analyzeCmd(t, "", "uninstall", "claude", "--write"); code != 0 {
		t.Fatal("uninstall")
	}
	if b, _ := os.ReadFile(skill("cbrt-session-review")); string(b) != "mine\n" {
		t.Errorf("hand-made skill changed: %q", b)
	}
}

// A hook stored with a Windows path is recognized on every OS: install
// replaces it instead of adding a second set.
func TestAnalyzeInstallReplacesWindowsPathHooks(t *testing.T) {
	_, claudeDir := analyzeHome(t)
	settings := filepath.Join(claudeDir, "settings.json")
	stored := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"C:\\Programs\\cbrt\\cbrt.exe","args":["hook","record","claude","Stop"]}]}]}}`
	if err := os.WriteFile(settings, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := analyzeCmd(t, "", "install", "claude", "--write"); code != 0 {
		t.Fatalf("install: %q", errs)
	}
	b, _ := os.ReadFile(settings)
	if strings.Contains(string(b), `C:\\Programs`) || strings.Count(string(b), `"hook"`) != 6 {
		t.Fatalf("stored hook kept or doubled: %s", b)
	}
}

// Install removes a cbrt-made skill this cbrt no longer ships, and keeps a
// hand-made one.
func TestAnalyzeInstallRemovesStaleSkills(t *testing.T) {
	_, claudeDir := analyzeHome(t)
	write := func(name, text string) {
		p := filepath.Join(claudeDir, "skills", name, "SKILL.md")
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(text), 0o600)
	}
	write("old-renamed", "---\nname: old-renamed\n---\n"+skills.Marker+"\n")
	write("mine", "---\nname: mine\n---\n")
	if code, out, errs := analyzeCmd(t, "", "install", "claude", "--write"); code != 0 || !strings.Contains(out, "no longer ships") {
		t.Fatalf("install: %d %q %q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "skills", "old-renamed")); !os.IsNotExist(err) {
		t.Error("stale skill kept")
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "skills", "mine", "SKILL.md")); err != nil {
		t.Error("hand-made skill removed")
	}
}
