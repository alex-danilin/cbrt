package projects

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseVerdict(t *testing.T) {
	for in, want := range map[string]string{
		"Verdict: on track\nreasons":      OnTrack,
		"\n\n  verdict: Drifting  \r\nx":  Drifting,
		bom + "Verdict: off track":        OffTrack,
		"**Verdict: on track**\n":         OnTrack,
		"## Verdict: drifting.\n":         Drifting,
		"Verdict: fine\n":                 VerdictUnknown,
		"I looked at the repo.\nVerdict:": VerdictUnknown,
		"":                                VerdictUnknown,
	} {
		if got, _ := ParseVerdict(in); got != want {
			t.Errorf("ParseVerdict(%q) = %q, want %q", in, got, want)
		}
	}
	v, report := checkReport("**Verdict: on track**\n\nReasons: tests pass.\n")
	if v != OnTrack || report != "Verdict: on track\n\nReasons: tests pass.\n" {
		t.Errorf("report %q", report)
	}
	// a preamble before the verdict line
	v, report = checkReport("Sure, here is my review.\n\n**Verdict: off track**\nThe plan says notes; the commits build chat.")
	if v != OffTrack || report != "Verdict: off track\n\nSure, here is my review.\n\nThe plan says notes; the commits build chat.\n" {
		t.Errorf("preamble report %q", report)
	}
	v, report = checkReport("Looks fine to me.\nVerdict: fine\n> Verdict: on track")
	if v != VerdictUnknown || report != "Verdict: unknown\n\nLooks fine to me.\nVerdict: fine\n> Verdict: on track\n" {
		t.Errorf("malformed report %q", report)
	}
}

// checkRepo is a repository with a plan and three commits: one old (before
// the previous check) and two new.
func checkRepo(t *testing.T) (*env, string) {
	t.Helper()
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "inkwell", map[string]string{
		"PLAN.md": "# Plan: inkwell\n\nGoal: fast notes.\n\nNow: sync\n\n## Core\n- [x] editor — evidence: test\n- [>] sync\n",
		"old.go":  "package old\n",
	})
	gitT(t, dir, "init", "-q")
	commit := func(date, file, subject string) {
		t.Setenv("GIT_COMMITTER_DATE", date)
		t.Setenv("GIT_AUTHOR_DATE", date)
		os.WriteFile(filepath.Join(dir, file), []byte(subject+"\n"), 0o644)
		gitT(t, dir, "add", "--", file)
		gitT(t, dir, "commit", "-q", "-m", subject)
	}
	commit("2026-01-01T10:00:00Z", "old.go", "Write the editor")
	commit("2026-07-01T10:00:00Z", "sync.go", "Start sync over webdav")
	commit("2026-07-02T10:00:00Z", "share.go", "Add sharing links")
	return e, dir
}

func TestCheckPrompt(t *testing.T) {
	_, dir := checkRepo(t)
	p, err := checkPrompt(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"read-only review", "Verdict: on track\nVerdict: drifting\nVerdict: off track",
		"## PLAN.md\n\n```\n# Plan: inkwell\n", "- [>] sync",
		"## The last 30 commits", "Write the editor", "Start sync over webdav", "Add sharing links",
		"## Diffstat of those commits", "share.go", "sync.go", "old.go",
		"## Previous check\n\n```\nThere is no previous check.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}

	// after a check: only the commits since, their diffstat, the verdict
	cd := filepath.Join(dir, ".cbrt", "checks")
	os.MkdirAll(cd, 0o755)
	os.WriteFile(filepath.Join(cd, "20260601T000000Z-codex.md"), []byte("Verdict: drifting\n\nThe editor has no tests.\n"), 0o644)
	p, err = checkPrompt(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Commits since the previous check, 2026-06-01 00:00 UTC", "Start sync over webdav", "Add sharing links",
		"share.go", "sync.go", "## Previous check (20260601T000000Z-codex.md)", "Verdict: drifting", "The editor has no tests.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt after a check lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Write the editor") || strings.Contains(p, "old.go") {
		t.Errorf("prompt has commits from before the previous check:\n%s", p)
	}

	if _, err := checkPrompt(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "no PLAN.md") {
		t.Errorf("no plan: %v", err)
	}
	// a folder that is not a repository still gets a prompt
	if p, err := checkPrompt(context.Background(), (&env{base: t.TempDir()}).project(t, "x", map[string]string{"PLAN.md": "# Plan\n"})); err != nil || !strings.Contains(p, "(none)") {
		t.Errorf("no repo: %v", err)
	}
}

func TestFence(t *testing.T) {
	if fence("a ``` b") != "````" || fence("plain") != "```" {
		t.Errorf("fence")
	}
}

func TestCheckEntrySavesReport(t *testing.T) {
	e, dir := checkRepo(t)
	entry := Entry{Name: "inkwell", Path: dir, Visibility: Private}
	now := func() time.Time { return time.Date(2026, 10, 20, 12, 30, 5, 0, time.UTC) }
	var got Command
	var out bytes.Buffer
	o := CheckOptions{Home: e.o.Home, Agent: "codex", Now: now, Out: &out, Run: func(ctx context.Context, c Command) (string, error) {
		got = c
		return "Verdict: Drifting\n\nSharing is not in the plan.\n\n- Add a task for sharing links\n", nil
	}}
	res, err := CheckEntry(context.Background(), entry, o)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "codex" || got.Dir != dir || !strings.Contains(got.Stdin, "Add sharing links") {
		t.Errorf("command %+v", got)
	}
	want := filepath.Join(dir, ".cbrt", "checks", "20261020T123005Z-codex.md")
	if res.File != want || res.Verdict != Drifting {
		t.Errorf("result %+v", res)
	}
	if s := read(t, want); s != "Verdict: drifting\n\nSharing is not in the plan.\n\n- Add a task for sharing links\n" {
		t.Errorf("report %q", s)
	}
	if !strings.Contains(out.String(), "git -C") || !strings.Contains(out.String(), ".cbrt/checks/20261020T123005Z-codex.md") {
		t.Errorf("no commit hint:\n%s", out.String())
	}
	// the page reads the verdict back
	if v, _ := latestCheck(filepath.Join(dir, ".cbrt", "checks")); v != Drifting {
		t.Errorf("latestCheck %q", v)
	}

	// a minute later, a malformed answer: saved as unknown, whole
	o.Now = func() time.Time { return now().Add(time.Minute) }
	o.Run = func(context.Context, Command) (string, error) { return "I think it is fine.", nil }
	o.Agent = "claude"
	res, err = CheckEntry(context.Background(), entry, o)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(res.File) != "20261020T123105Z-claude.md" || read(t, res.File) != "Verdict: unknown\n\nI think it is fine.\n" {
		t.Errorf("malformed: %s %q", res.File, read(t, res.File))
	}
	if v, _ := latestCheck(filepath.Join(dir, ".cbrt", "checks")); v != VerdictUnknown {
		t.Errorf("latestCheck %q", v)
	}
	res, _ = CheckEntry(context.Background(), entry, o)
	if filepath.Base(res.File) != "20261020T123105Z-claude-2.md" {
		t.Errorf("second report of a second: %s", res.File)
	}

	// a failing agent saves nothing
	before := snapshot(t, dir)
	o.Run = func(context.Context, Command) (string, error) { return "", errors.New("boom") }
	if _, err := CheckEntry(context.Background(), entry, o); err == nil {
		t.Errorf("failure not reported")
	}
	if len(snapshot(t, dir)) != len(before) {
		t.Errorf("a failed check wrote a file")
	}
	// public: the hint says it stays local
	out.Reset()
	o.Run = func(context.Context, Command) (string, error) { return "Verdict: on track", nil }
	entry.Visibility = Public
	CheckEntry(context.Background(), entry, o)
	if !strings.Contains(out.String(), "stays local") {
		t.Errorf("public hint:\n%s", out.String())
	}
}

func TestCheckDryRunAndLookup(t *testing.T) {
	e, dir := checkRepo(t)
	es := []Entry{{Name: "inkwell", Path: dir}}
	if err := Save(e.o.Home, es); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ran := false
	o := CheckOptions{Home: e.o.Home, Agent: "agy", DryRun: true, Out: &out, Run: func(context.Context, Command) (string, error) {
		ran = true
		return "", nil
	}}
	before := snapshot(t, dir)
	if _, err := Check("inkwell", o); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Errorf("dry run ran the agent")
	}
	s := out.String()
	for _, want := range []string{"dry run", "command: agy \"--print=You are reviewing", "--add-dir", "--print-timeout 9m", "prompt:\n", "Add sharing links"} {
		if !strings.Contains(s, want) {
			t.Errorf("dry run lacks %q:\n%s", want, s)
		}
	}
	if !equalMaps(before, snapshot(t, dir)) {
		t.Errorf("dry run wrote files")
	}
	if _, err := Check(dir, o); err != nil { // by folder
		t.Errorf("by folder: %v", err)
	}
	if _, err := Check("nope", o); err == nil || !strings.Contains(err.Error(), `no connected project "nope"`) {
		t.Errorf("unknown: %v", err)
	}
	o.Agent = "gpt"
	if _, err := Check("inkwell", o); err == nil || !strings.Contains(err.Error(), "want one of") {
		t.Errorf("bad agent: %v", err)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
