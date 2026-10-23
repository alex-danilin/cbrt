package projects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Verdicts of a direction check, from the first line of its report.
const (
	OnTrack        = "on track"
	Drifting       = "drifting"
	OffTrack       = "off track"
	VerdictUnknown = "unknown" // the agent's answer did not start with a verdict
)

// checkStamp is the UTC timestamp a check report's name starts with.
const checkStamp = "20060102T150405Z"

// Bounds of the prompt's sections.
const (
	promptPlanMax  = 12000
	promptLogMax   = 6000
	promptStatMax  = 3000
	promptPrevMax  = 2000
	fallbackCommit = 30
)

// CheckOptions are the switches of a direction check.
type CheckOptions struct {
	Home    string // the folder holding projects.json; "" = Home()
	Agent   string // claude, codex or agy
	Timeout time.Duration
	DryRun  bool
	Out     io.Writer // nil = discard
	Now     func() time.Time
	Run     RunFunc // nil = ExecRun
}

func (o *CheckOptions) defaults() {
	if o.Home == "" {
		o.Home = Home()
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultAgentTimeout
	}
}

// CheckResult is a saved check.
type CheckResult struct {
	Verdict string
	File    string // the report, inside the project
}

// Check asks an agent whether the project named key (or in folder key) is
// heading where its plan says, and saves the answer in .cbrt/checks.
func Check(key string, o CheckOptions) (CheckResult, error) {
	o.defaults()
	es, err := Load(o.Home)
	if err != nil {
		return CheckResult{}, err
	}
	i := Find(es, key)
	if i < 0 {
		if abs, err := filepath.Abs(key); err == nil {
			i = findPath(es, abs)
		}
	}
	if i < 0 {
		return CheckResult{}, fmt.Errorf("no connected project %q (see cbrt projects list)", Clean(key))
	}
	return CheckEntry(context.Background(), es[i], o)
}

// CheckEntry runs the direction check of one project.
func CheckEntry(ctx context.Context, e Entry, o CheckOptions) (CheckResult, error) {
	o.defaults()
	if err := validAgent(o.Agent); err != nil {
		return CheckResult{}, err
	}
	if st, err := os.Stat(e.Path); err != nil || !st.IsDir() {
		return CheckResult{}, fmt.Errorf("%s: the folder %s is missing", Clean(e.Name), Clean(e.Path))
	}
	prompt, err := checkPrompt(ctx, e.Path)
	if err != nil {
		return CheckResult{}, err
	}
	if o.DryRun {
		c, err := newCommand(o.Agent, e.Path, prompt, o.Timeout, "<temp folder>")
		if err != nil {
			return CheckResult{}, err
		}
		fmt.Fprintf(o.Out, "%s, dry run: nothing runs or is written\n\ncommand: %s\ntimeout: %v\n\nprompt:\n%s\n", Clean(e.Name), Clean(c.String()), o.Timeout, prompt)
		return CheckResult{}, nil
	}
	fmt.Fprintf(o.Out, "%s: asking %s (read-only, up to %v)…\n", Clean(e.Name), o.Agent, o.Timeout)
	answer, err := runAgent(ctx, o.Run, o.Agent, e.Path, prompt, o.Timeout)
	if err != nil {
		return CheckResult{}, err
	}
	verdict, report := checkReport(answer)
	file, err := saveCheck(e.Path, o.Now(), o.Agent, report)
	if err != nil {
		return CheckResult{}, err
	}
	rel := filepath.ToSlash(filepath.Join(ChecksDir, filepath.Base(file)))
	fmt.Fprintf(o.Out, "  Verdict: %s\n  saved %s\n", verdict, rel)
	if verdict == VerdictUnknown {
		fmt.Fprintf(o.Out, "  the answer did not start with a verdict line; it is saved as is\n")
	}
	if e.Visibility == Public {
		fmt.Fprintf(o.Out, "  it stays local: .cbrt/ is excluded from git in this public repository\n")
	} else {
		fmt.Fprintf(o.Out, "  not committed; to keep it in the history: git -C \"%s\" add %s\n", Clean(e.Path), rel)
	}
	return CheckResult{Verdict: verdict, File: file}, nil
}

// ParseVerdict finds the first line of an answer that is a verdict
// ("Verdict: on track", case and Markdown emphasis aside; a preamble before
// it is allowed) and returns it with the answer without that line;
// VerdictUnknown and the whole answer when no line is one.
func ParseVerdict(answer string) (string, string) {
	s := strings.TrimLeft(strings.TrimPrefix(answer, bom), " \t\r\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if v := verdictLine(l); v != "" {
			rest := strings.Join(append(lines[:i:i], lines[i+1:]...), "\n")
			return v, rest
		}
	}
	return VerdictUnknown, s
}

// verdictLine is the verdict a line states, or "".
func verdictLine(l string) string {
	line := strings.Trim(strings.TrimSpace(l), "*_#` ")
	v, ok := cutPrefixFold(line, "verdict:")
	if !ok {
		return ""
	}
	v = strings.ToLower(strings.Trim(strings.TrimSpace(v), "*_`. "))
	switch v {
	case OnTrack, Drifting, OffTrack:
		return v
	}
	return ""
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// checkReport is the file content: the canonical verdict line, then the
// answer (all of it when the verdict is unknown).
func checkReport(answer string) (string, string) {
	v, rest := ParseVerdict(answer)
	body := strings.TrimSpace(rest)
	var b strings.Builder
	b.WriteString("Verdict: " + v + "\n")
	if body != "" {
		b.WriteString("\n" + body + "\n")
	}
	return v, b.String()
}

// saveCheck writes .cbrt/checks/<UTC yyyymmddThhmmssZ>-<agent>.md in dir
// (with -2, -3… when a report of that second exists) and returns its path.
func saveCheck(dir string, now time.Time, agent, report string) (string, error) {
	cd := filepath.Join(dir, filepath.FromSlash(ChecksDir))
	if err := os.MkdirAll(cd, 0o755); err != nil {
		return "", err
	}
	base := now.UTC().Format(checkStamp) + "-" + agent
	for i := 1; i < 100; i++ {
		name := base + ".md"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.md", base, i)
		}
		p := filepath.Join(cd, name)
		err := createFile(p, []byte(report))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return p, err
	}
	return "", fmt.Errorf("too many check reports for %s", base)
}

// previousCheck is the newest report's name, time (from its name) and
// content.
func previousCheck(dir string) (string, time.Time, string) {
	cd := filepath.Join(dir, filepath.FromSlash(ChecksDir))
	name := newestCheck(cd)
	if name == "" {
		return "", time.Time{}, ""
	}
	var t time.Time
	if len(name) >= len(checkStamp) {
		t, _ = time.Parse(checkStamp, name[:len(checkStamp)])
	}
	b, _ := os.ReadFile(filepath.Join(cd, name))
	return name, t, string(b)
}

// checkPrompt assembles the direction-check prompt from the project's
// PLAN.md, the commits since the last check (else the last 30), their
// diffstat and the previous check.
func checkPrompt(ctx context.Context, dir string) (string, error) {
	plan, err := os.ReadFile(filepath.Join(dir, "PLAN.md"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", errors.New("the project has no PLAN.md: nothing to check against")
	case err != nil:
		return "", err
	}
	prevName, prevTime, prevText := previousCheck(dir)
	log, stat := commitsSince(ctx, dir, prevTime)

	var b strings.Builder
	b.WriteString(`You are reviewing whether the work in this repository is heading where its plan says it should go. This is a read-only review: do not edit, create or delete any file, and do not run commands that change anything. You may read files in the repository.

Answer in Markdown. The first line must be exactly one of:
Verdict: on track
Verdict: drifting
Verdict: off track

Then give your reasons, citing commits, files or tasks. Then list suggested changes to PLAN.md as proposals only (do not edit it): tasks to add, park, reword or mark done with evidence, tasks marked done without evidence, and whether the Goal still describes the work.

`)
	section(&b, "PLAN.md", clipBytes(string(plan), promptPlanMax))
	if prevTime.IsZero() {
		section(&b, fmt.Sprintf("The last %d commits (newest first)", fallbackCommit), clipBytes(log, promptLogMax))
	} else {
		section(&b, "Commits since the previous check, "+prevTime.UTC().Format("2006-01-02 15:04 UTC")+" (newest first)", clipBytes(log, promptLogMax))
	}
	section(&b, "Diffstat of those commits", clipBytes(stat, promptStatMax))
	if prevName == "" {
		section(&b, "Previous check", "There is no previous check.")
	} else {
		section(&b, "Previous check ("+prevName+")", clipBytes(prevText, promptPrevMax))
	}
	return b.String(), nil
}

func section(b *strings.Builder, title, body string) {
	body = strings.TrimRight(body, "\r\n ")
	if body == "" {
		body = "(none)"
	}
	f := fence(body)
	fmt.Fprintf(b, "## %s\n\n%s\n%s\n%s\n\n", title, f, body, f)
}

// fence is a backtick fence longer than any backtick run in s.
func fence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (cut)"
}

// commitsSince lists the commits after since (zero: the last 30) and their
// diffstat. A folder without commits gives empty strings.
func commitsSince(ctx context.Context, dir string, since time.Time) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"log", "--format=%h %cs %s", "--no-color"}
	var base string
	if since.IsZero() {
		args = append(args, fmt.Sprintf("-n%d", fallbackCommit))
		base, _ = gitCtx(ctx, dir, "rev-parse", "--verify", "-q", fmt.Sprintf("HEAD~%d", fallbackCommit))
	} else {
		at := since.UTC().Format(time.RFC3339)
		args = append(args, "-n300", "--since="+at)
		base, _ = gitCtx(ctx, dir, "rev-list", "-1", "--before="+at, "HEAD")
	}
	log, err := gitCtx(ctx, dir, args...)
	if err != nil || log == "" {
		return "", ""
	}
	if base == "" { // the range starts at the first commit
		if base, err = gitCtx(ctx, dir, "hash-object", "-t", "tree", "--stdin"); err != nil {
			return log, ""
		}
	}
	stat, _ := gitCtx(ctx, dir, "diff", "--no-color", "--stat=100", "--stat-count=40", base, "HEAD")
	return log, stat
}
