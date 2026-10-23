package projects

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConnectAgentWritesOverview(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "inkwell", map[string]string{"go.mod": "module inkwell\n"})
	var calls []Command
	e.o.Agent = "claude"
	e.o.Run = func(ctx context.Context, c Command) (string, error) {
		calls = append(calls, c)
		return "```markdown\n# inkwell\nInkwell is a fast notes app for one person.\r\n\r\nIt is early: only the editor works.\n```\n", nil
	}
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Dir != dir || !reflect.DeepEqual(calls[0].Args, []string{"-p", "--output-format", "text", "--permission-mode", "plan", "--tools", "Read,Grep,Glob", "--strict-mcp-config"}) ||
		!strings.Contains(calls[0].Stdin, "read-only") {
		t.Fatalf("calls %+v", calls)
	}
	readme := read(t, filepath.Join(dir, "README.md"))
	want := "# inkwell\n\nInkwell is a fast notes app for one person.\n\nIt is early: only the editor works.\n\n## Build and run\n"
	if !strings.HasPrefix(readme, want) || strings.Contains(readme, overviewPlaceholder) || strings.Contains(readme, "```") {
		t.Errorf("README:\n%s", readme)
	}
	plan := read(t, filepath.Join(dir, "PLAN.md"))
	if strings.Contains(plan, ReadmeTask) || !strings.Contains(plan, "Goal: Inkwell is a fast notes app for one person.") {
		t.Errorf("PLAN.md:\n%s", plan)
	}

	// connecting again: the README exists, the agent is not asked
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Errorf("agent asked for an existing README")
	}
}

func TestConnectAgentFailureKeepsSkeleton(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "inkwell", nil)
	e.o.Agent = "agy"
	e.o.Run = func(ctx context.Context, c Command) (string, error) {
		return "", errors.New("agy did not answer within 10m0s")
	}
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if readme := read(t, filepath.Join(dir, "README.md")); !strings.Contains(readme, overviewPlaceholder) {
		t.Errorf("README:\n%s", readme)
	}
	if plan := read(t, filepath.Join(dir, "PLAN.md")); !strings.Contains(plan, "- [ ] "+ReadmeTask) {
		t.Errorf("PLAN.md:\n%s", plan)
	}
	if !strings.Contains(e.out.String(), "agy wrote no overview (agy did not answer within 10m0s)") {
		t.Errorf("output:\n%s", e.out.String())
	}
	// committed after the git init, like the skeleton
	if got := gitT(t, dir, "ls-files"); !strings.Contains(got, "README.md") {
		t.Errorf("tracked: %q", got)
	}
}

func TestConnectAgentDryRunAndBadAgent(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "inkwell", nil)
	e.o.Agent, e.o.DryRun = "codex", true
	e.o.Run = func(context.Context, Command) (string, error) {
		t.Errorf("dry run ran the agent")
		return "", nil
	}
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "would ask codex for the README overview: codex exec --sandbox read-only") {
		t.Errorf("output:\n%s", e.out.String())
	}
	e.o.Agent, e.o.DryRun = "gemini", false
	if err := Connect(dir, e.o); err == nil || !strings.Contains(err.Error(), "want one of") {
		t.Errorf("bad agent: %v", err)
	}
	if len(snapshot(t, dir)) != 0 {
		t.Errorf("a bad agent still wrote files")
	}
}
