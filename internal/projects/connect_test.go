package projects

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// needGit skips the test when git is not on PATH and isolates git from the
// machine's configuration; identity sets user.name/user.email globally.
func needGit(t *testing.T, identity bool) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH: connect needs git")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	body := "[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n[core]\n\tautocrlf = false\n"
	if identity {
		body += "[user]\n\tname = Test Person\n\temail = test@example.com\n"
	}
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL", "GIT_DIR", "GIT_WORK_TREE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	// Every t.TempDir of this test is inside root. The temp folder may itself
	// be inside a repository (GOTMPDIR or TMP pointing into a checkout): git
	// must not look above root, and nothing may ever be committed outside it.
	root := filepath.Dir(filepath.Dir(cfg))
	t.Setenv("GIT_CEILING_DIRECTORIES", root)
	testRoot = root
	commitGuard = func(dir string) error { return insideRoot(dir) }
	t.Cleanup(func() { commitGuard, testRoot = nil, "" })
}

// testRoot is the running test's temp root (needGit).
var testRoot string

// insideRoot fails unless dir's repository top level is inside testRoot.
func insideRoot(dir string) error {
	if testRoot == "" {
		return errors.New("test guard: needGit was not called")
	}
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	rr, err := filepath.EvalSymlinks(testRoot)
	if err != nil {
		return err
	}
	rt, err := filepath.EvalSymlinks(filepath.FromSlash(top))
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(rr, rt); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("test guard: repository %s is outside the test's temp root %s", rt, rr)
	}
	return nil
}

type env struct {
	base string // holds home/, list/ and the projects
	out  *bytes.Buffer
	o    Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{base: base, out: &bytes.Buffer{}}
	e.o = Options{
		Home:     filepath.Join(base, "list"),
		UserHome: filepath.Join(base, "home"),
		TempDirs: []string{}, // t.TempDir is under the system temp folder
		Now:      func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
	e.o.Out = e.out
	return e
}

// project creates a folder with files (slash paths -> content).
func (e *env) project(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(e.base, "work", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if args[0] != "init" {
		if err := insideRoot(dir); err != nil {
			t.Fatal(err)
		}
	}
	out, err := git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// snapshot is every file under dir (outside .git) with its content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				m[".git/"] = "present"
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		m[filepath.ToSlash(rel)] = read(t, p)
		return nil
	})
	return m
}

func TestConnectNonGitFolder(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "inkwell", map[string]string{
		"go.mod":   "module example.invalid/inkwell\n",
		"Makefile": "build:\n\tgo build\ntest:\n\tgo test\n",
	})
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	for _, want := range []string{"git init", "created README.md", "created PLAN.md", "created AGENTS.md", "added inkwell to the project list", "committed README.md, PLAN.md, AGENTS.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	readme := read(t, filepath.Join(dir, "README.md"))
	for _, want := range []string{"# inkwell\n", "Go module (go.mod)", "Makefile targets (`make <target>`): `build`, `test`"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q:\n%s", want, readme)
		}
	}
	p, err := Parse([]byte(read(t, filepath.Join(dir, "PLAN.md"))))
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Plan: inkwell" || len(p.Milestones) != 1 || p.Milestones[0].Tasks[0].Text != ReadmeTask {
		t.Errorf("plan %+v", p)
	}
	if got := read(t, filepath.Join(dir, "AGENTS.md")); got != "# Agent instructions\n\n"+RuleBlock {
		t.Errorf("AGENTS.md:\n%s", got)
	}
	// No CLAUDE.md: Claude Code reads AGENTS.md itself when there is none.
	if _, err := os.Lstat(filepath.Join(dir, "CLAUDE.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("CLAUDE.md created: %v", err)
	}
	// Only the created files are committed; go.mod and the Makefile stay untracked.
	files := strings.Fields(gitT(t, dir, "ls-files"))
	if want := []string{"AGENTS.md", "PLAN.md", "README.md"}; !reflect.DeepEqual(files, want) {
		t.Errorf("committed %v, want %v", files, want)
	}
	es, err := Load(e.o.Home)
	if err != nil || len(es) != 1 || es[0].Name != "inkwell" || es[0].Path != dir || !es[0].ConnectedAt.Equal(e.o.Now()) {
		t.Fatalf("list %+v %v", es, err)
	}

	// Idempotent: a second connect changes nothing and says so.
	before := snapshot(t, dir)
	listBefore := read(t, filepath.Join(e.o.Home, ListFile))
	e.out.Reset()
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "nothing to change") {
		t.Errorf("second connect said:\n%s", e.out)
	}
	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("second connect changed files")
	}
	if read(t, filepath.Join(e.o.Home, ListFile)) != listBefore {
		t.Errorf("second connect changed the list")
	}
	if n := gitT(t, dir, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("%s commits", n)
	}
}

func TestConnectWithoutGitIdentitySkipsCommit(t *testing.T) {
	needGit(t, false)
	e := newEnv(t)
	dir := e.project(t, "plain", nil)
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "not committed: git user.name and user.email are not configured") {
		t.Errorf("output:\n%s", e.out)
	}
	if _, err := git(dir, "rev-parse", "--verify", "HEAD"); err == nil {
		t.Errorf("a commit was made")
	}
	if _, err := os.Stat(filepath.Join(dir, "PLAN.md")); err != nil {
		t.Error(err)
	}
}

func TestConnectExistingRepo(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	agents := "# Rules\r\n\r\n- Be brief.\r\n"
	claude := "# Claude notes\nUse tabs.\n"
	dir := e.project(t, "toolbox", map[string]string{
		"README.md": "# toolbox\n\n[![ci](x.svg)](y)\n\nA command-line tool that\nsorts photos by date.\n\n## Install\n",
		"AGENTS.md": agents,
		"CLAUDE.md": claude,
	})
	gitT(t, dir, "init", "-q")
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)

	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	if strings.Contains(out, "git init") || strings.Contains(out, "commit") || strings.Contains(out, "README.md") {
		t.Errorf("unexpected actions:\n%s", out)
	}
	if _, err := git(dir, "rev-parse", "--verify", "HEAD"); err == nil {
		t.Errorf("connect committed in an existing repository")
	}
	p, err := Parse([]byte(read(t, filepath.Join(dir, "PLAN.md"))))
	if err != nil {
		t.Fatal(err)
	}
	if p.Goal != "A command-line tool that sorts photos by date." {
		t.Errorf("goal %q", p.Goal)
	}
	for _, tk := range p.Milestones[0].Tasks {
		if tk.Text == ReadmeTask {
			t.Errorf("README task added although a README exists")
		}
	}
	wantAgents := "# Rules\r\n\r\n- Be brief.\r\n\r\n" + strings.ReplaceAll(RuleBlock, "\n", "\r\n")
	if got := read(t, filepath.Join(dir, "AGENTS.md")); got != wantAgents {
		t.Errorf("AGENTS.md:\n%q\nwant\n%q", got, wantAgents)
	}
	if got := read(t, filepath.Join(dir, "CLAUDE.md")); got != claude+"\n"+claudeImport+"\n" {
		t.Errorf("CLAUDE.md %q", got)
	}

	// A folder inside the repository needs no git init either.
	e.out.Reset()
	if err := Connect(sub, e.o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.out.String(), "git init") {
		t.Errorf("git init inside a work tree:\n%s", e.out)
	}
}

func TestConnectKeepsExistingBlockAndImport(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	agents := "# Rules\n\n" + BlockStart + "\nmy edited block\n" + BlockEnd + "\n\nMore rules.\n"
	claude := "Intro.\n  @AGENTS.md  \n"
	dir := e.project(t, "kept", map[string]string{
		"README.md": "# kept\n",
		"PLAN.md":   "# Plan: kept\n",
		"AGENTS.md": agents,
		"CLAUDE.md": claude,
	})
	gitT(t, dir, "init", "-q")
	before := snapshot(t, dir)
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("files changed:\n%v\n%v", before, after)
	}
	if !strings.Contains(e.out.String(), "added kept to the project list") {
		t.Errorf("output:\n%s", e.out)
	}
}

func TestConnectRefuses(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	home := e.o.UserHome
	os.MkdirAll(home, 0o755)
	tmp := filepath.Join(e.base, "tmp")
	scratch := filepath.Join(e.base, "x", "scratchpad", "p")
	os.MkdirAll(filepath.Join(tmp, "p"), 0o755)
	os.MkdirAll(scratch, 0o755)
	o := e.o
	o.TempDirs = []string{tmp}
	root := filepath.VolumeName(e.base) + string(filepath.Separator)
	for dir, want := range map[string]string{
		home:                            "the home folder",
		filepath.Join(tmp, "p"):         "a temporary folder",
		scratch:                         "a scratch folder",
		root:                            "a filesystem root",
		home + string(os.PathSeparator): "the home folder",
		e.base:                          "a folder above the home folder",
	} {
		err := Connect(dir, o)
		if err == nil || !strings.Contains(err.Error(), "refusing") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", dir, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(e.o.Home, ListFile)); err == nil {
		t.Errorf("a refused connect wrote the list")
	}
	if _, err := os.Stat(filepath.Join(home, "PLAN.md")); err == nil {
		t.Errorf("a refused connect wrote PLAN.md")
	}
	if err := Connect(filepath.Join(e.base, "missing"), o); err == nil {
		t.Errorf("a missing folder was connected")
	}
}

func TestConnectRefusesLinkToHome(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	os.MkdirAll(e.o.UserHome, 0o755)
	link := filepath.Join(e.base, "work", "looks-fine")
	os.MkdirAll(filepath.Dir(link), 0o755)
	dirLinkOrSkip(t, e.o.UserHome, link)
	if err := Connect(link, e.o); err == nil || !strings.Contains(err.Error(), "the home folder") {
		t.Errorf("err %v", err)
	}
	// A link to a temp folder too.
	tmp := filepath.Join(e.base, "tmp")
	os.MkdirAll(filepath.Join(tmp, "p"), 0o755)
	link2 := filepath.Join(e.base, "work", "tmp-link")
	dirLinkOrSkip(t, filepath.Join(tmp, "p"), link2)
	o := e.o
	o.TempDirs = []string{tmp}
	if err := Connect(link2, o); err == nil || !strings.Contains(err.Error(), "a temporary folder") {
		t.Errorf("err %v", err)
	}
}

func TestConnectDryRunWritesNothing(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "dry", map[string]string{"AGENTS.md": "# Rules\n", "CLAUDE.md": "x\n"})
	before := snapshot(t, dir)
	o := e.o
	o.DryRun = true
	if err := Connect(dir, o); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("dry run changed files: %v", after)
	}
	if _, err := os.Stat(e.o.Home); err == nil {
		t.Errorf("dry run created the list folder")
	}
	out := e.out.String()
	for _, want := range []string{"dry run", "would git init", "would create README.md", "would create PLAN.md", "would append the plan rule block to AGENTS.md", "would add the line @AGENTS.md to CLAUDE.md", "would add dry to the project list", "would commit README.md, PLAN.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDisconnect(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	a := e.project(t, "app", nil)
	b := filepath.Join(e.base, "other", "app")
	os.MkdirAll(b, 0o755)
	os.WriteFile(filepath.Join(b, "AGENTS.md"), []byte("# Rules\n\n- Be brief.\n"), 0o644)
	for _, d := range []string{a, b} {
		if err := Connect(d, e.o); err != nil {
			t.Fatal(err)
		}
	}
	es, _ := Load(e.o.Home)
	if len(es) != 2 || es[0].Name != "app" || es[1].Name != "app-2" {
		t.Fatalf("names %+v", es)
	}

	// By name, keeping files.
	e.out.Reset()
	if err := Disconnect("app", false, e.o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, filepath.Join(a, "AGENTS.md")), BlockStart) {
		t.Errorf("block removed without --remove-files")
	}
	if !strings.Contains(e.out.String(), "removed app from the project list") {
		t.Errorf("output:\n%s", e.out)
	}

	// By folder, removing the block; PLAN.md and README stay.
	if err := Disconnect(b, true, e.o); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(b, "AGENTS.md")); got != "# Rules\n\n- Be brief.\n" {
		t.Errorf("AGENTS.md after removal %q (not the exact inverse of connect)", got)
	}
	for _, f := range []string{"PLAN.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(b, f)); err != nil {
			t.Errorf("%s removed: %v", f, err)
		}
	}
	if es, _ := Load(e.o.Home); len(es) != 0 {
		t.Errorf("list %+v", es)
	}
	if err := Disconnect("app", false, e.o); err == nil {
		t.Errorf("disconnecting an unknown project succeeded")
	}

	// An AGENTS.md that connect created holds nothing else: it is deleted.
	if err := Connect(a, e.o); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	if err := Disconnect(a, true, e.o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a, "AGENTS.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("AGENTS.md left behind: %v", err)
	}
	if !strings.Contains(e.out.String(), "removed AGENTS.md") {
		t.Errorf("output:\n%s", e.out)
	}
}

func TestBlockRoundTrip(t *testing.T) {
	for _, in := range []string{"# R\n", "# R\r\n\r\n- x\r\n", "# R\n\n- x\n", "", "\n"} {
		got, ok := removeBlock(appendBlock([]byte(in), RuleBlock))
		want := strings.TrimRight(in, "\r\n")
		if want != "" {
			want += lineEnding([]byte(in))
		}
		if !ok || string(got) != want {
			t.Errorf("%q: round trip gave %q, want %q", in, got, want)
		}
	}
}

// symlinkOrSkip creates link -> target or skips the test where links need
// privileges (Windows without Developer Mode).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here (%v)", err)
	}
}

// dirLinkOrSkip links the folder link to target: a symlink, or on Windows
// a junction when symlinks need privileges.
func dirLinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if jerr == nil {
			return
		}
		t.Skipf("cannot create a symlink (%v) or a junction (%v: %s)", err, jerr, out)
	}
	t.Skipf("cannot create symlinks here (%v)", err)
}

func TestConnectFollowsLinks(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)

	// CLAUDE.md -> AGENTS.md: the block goes into AGENTS.md, no import line.
	one := e.project(t, "one", map[string]string{"AGENTS.md": "# Rules\n", "README.md": "# one\n"})
	symlinkOrSkip(t, "AGENTS.md", filepath.Join(one, "CLAUDE.md"))
	if err := Connect(one, e.o); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(filepath.Join(one, "CLAUDE.md")); err != nil || st.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("CLAUDE.md is no longer a link: %v", err)
	}
	if got := read(t, filepath.Join(one, "AGENTS.md")); got != "# Rules\n\n"+RuleBlock {
		t.Errorf("AGENTS.md %q", got)
	}

	// AGENTS.md -> docs/rules.md: the target is edited, the link stays.
	two := e.project(t, "two", map[string]string{"docs/rules.md": "# Rules\n", "CLAUDE.md": "@AGENTS.md\n"})
	symlinkOrSkip(t, filepath.Join("docs", "rules.md"), filepath.Join(two, "AGENTS.md"))
	if err := Connect(two, e.o); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(filepath.Join(two, "AGENTS.md")); err != nil || st.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("AGENTS.md is no longer a link: %v", err)
	}
	if got := read(t, filepath.Join(two, "docs", "rules.md")); !strings.Contains(got, BlockStart) {
		t.Errorf("target not edited: %q", got)
	}
	if err := Disconnect(two, true, e.o); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(two, "docs", "rules.md")); got != "# Rules\n" {
		t.Errorf("target after disconnect %q", got)
	}
}

func TestConnectInsideAnotherRepo(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	outer := e.project(t, "outer", map[string]string{".gitignore": "ignored/\n", "keep.txt": "x\n"})
	gitT(t, outer, "init", "-q")
	gitT(t, outer, "add", "--", ".gitignore", "keep.txt")
	gitT(t, outer, "commit", "-q", "-m", "outer")
	outerStatus := func() string {
		return gitT(t, outer, "status", "--porcelain", "--untracked-files=no") + "|" + gitT(t, outer, "rev-list", "--count", "HEAD")
	}
	before := outerStatus()

	// Ignored by the outer repository: a repository of its own, committed.
	nested := filepath.Join(outer, "ignored", "proj")
	os.MkdirAll(nested, 0o755)
	if err := Connect(nested, e.o); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	if !strings.Contains(out, "which ignores it") || !strings.Contains(out, "git init") || !strings.Contains(out, "committed") {
		t.Errorf("output:\n%s", out)
	}
	if top := gitT(t, nested, "rev-parse", "--show-toplevel"); !sameDir(filepath.FromSlash(top), nested) {
		t.Errorf("nested top level %s", top)
	}
	if n := gitT(t, nested, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("nested repo has %s commits", n)
	}

	// A monorepo subfolder: files created, nothing committed or staged outside.
	e.out.Reset()
	sub := filepath.Join(outer, "packages", "app")
	os.MkdirAll(sub, 0o755)
	if err := Connect(sub, e.o); err != nil {
		t.Fatal(err)
	}
	out = e.out.String()
	if !strings.Contains(out, "files are created there and not committed") || strings.Contains(out, "git init") || strings.Contains(out, "committed ") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(sub, "PLAN.md")); err != nil {
		t.Error(err)
	}
	if after := outerStatus(); after != before {
		t.Errorf("outer repository changed: %q -> %q", before, after)
	}
}

func TestConnectGitErrorIsNotInit(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "broken", map[string]string{".git": "garbage\n"})
	err := Connect(dir, e.o)
	if err == nil || strings.Contains(e.out.String(), "git init") {
		t.Errorf("err %v, output:\n%s", err, e.out)
	}
	if _, err := os.Stat(filepath.Join(dir, "PLAN.md")); err == nil {
		t.Errorf("files created after a git failure")
	}
}

func TestConnectRetriesCommit(t *testing.T) {
	needGit(t, false)
	e := newEnv(t)
	dir := e.project(t, "later", map[string]string{"notes.txt": "mine\n"})
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "git -C") || !strings.Contains(e.out.String(), "add -- README.md PLAN.md AGENTS.md") {
		t.Errorf("no manual command:\n%s", e.out)
	}
	es, _ := Load(e.o.Home)
	if len(es) != 1 || len(es[0].PendingCommit) != 3 {
		t.Fatalf("list %+v", es)
	}
	// Still no identity: the rerun does not claim there is nothing to do.
	e.out.Reset()
	Connect(dir, e.o)
	if strings.Contains(e.out.String(), "nothing to change") || !strings.Contains(e.out.String(), "not committed") {
		t.Errorf("rerun:\n%s", e.out)
	}
	// With an identity, the rerun commits exactly the created files.
	needGit(t, true)
	e.out.Reset()
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "committed README.md, PLAN.md, AGENTS.md") {
		t.Errorf("retry:\n%s", e.out)
	}
	if files := strings.Fields(gitT(t, dir, "ls-files")); !reflect.DeepEqual(files, []string{"AGENTS.md", "PLAN.md", "README.md"}) {
		t.Errorf("committed %v", files)
	}
	if es, _ := Load(e.o.Home); len(es[0].PendingCommit) != 0 {
		t.Errorf("pending left: %+v", es)
	}
	e.out.Reset()
	Connect(dir, e.o)
	if !strings.Contains(e.out.String(), "nothing to change") {
		t.Errorf("third run:\n%s", e.out)
	}
}

func TestConnectFailingHookKeepsPending(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	hooks := filepath.Join(e.base, "hooks")
	os.MkdirAll(hooks, 0o755)
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\necho refused by hook >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := os.Getenv("GIT_CONFIG_GLOBAL")
	f, _ := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintf(f, "[core]\n\thooksPath = %s\n", filepath.ToSlash(hooks))
	f.Close()
	dir := e.project(t, "hooked", nil)
	err := Connect(dir, e.o)
	if err == nil || !strings.Contains(e.out.String(), "to commit them yourself") {
		t.Fatalf("err %v, output:\n%s", err, e.out)
	}
	if es, _ := Load(e.o.Home); len(es) != 1 || len(es[0].PendingCommit) != 3 {
		t.Errorf("list %+v", es)
	}
}

func TestClean(t *testing.T) {
	if got := Clean("a\tb\nc\x1b[31md\u0085e"); got != "a b c [31md e" {
		t.Errorf("%q", got)
	}
}

func TestRemoveBlock(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"end":    {"# A\n\n" + RuleBlock, "# A\n"},
		"middle": {"# A\n\n" + RuleBlock + "\nMore.\n", "# A\n\nMore.\n"},
		"crlf":   {"# A\r\n\r\n" + strings.ReplaceAll(RuleBlock, "\n", "\r\n"), "# A\r\n"},
		"only":   {RuleBlock, ""},
	} {
		got, ok := removeBlock([]byte(tc.in))
		if !ok || string(got) != tc.want {
			t.Errorf("%s: %q, %v; want %q", name, got, ok, tc.want)
		}
	}
	if _, ok := removeBlock([]byte("# A\n" + BlockStart + "\n")); ok {
		t.Errorf("an unterminated block was removed")
	}
}

func TestLoadRejectsNewerList(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ListFile), []byte(`{"v":99,"projects":[]}`), 0o600)
	if _, err := Load(home); err == nil {
		t.Errorf("a newer list version was accepted")
	}
	if es, err := Load(filepath.Join(home, "none")); err != nil || es != nil {
		t.Errorf("missing list: %v %v", es, err)
	}
}

func TestHomeEnv(t *testing.T) {
	d := t.TempDir()
	t.Setenv(HomeEnv, d)
	if Home() != filepath.Clean(d) {
		t.Errorf("Home %q, want %q", Home(), filepath.Clean(d))
	}
}

func TestOutside(t *testing.T) {
	root := t.TempDir()
	for target, want := range map[string]bool{
		filepath.Join(root, "AGENTS.md"):                           false,
		filepath.Join(root, "docs", "AGENTS.md"):                   false,
		filepath.Join(root, "..", "shared", "x.md"):                true,
		filepath.Join(filepath.Dir(root), "AGENTS.md"):             true,
		root + "-other" + string(filepath.Separator) + "AGENTS.md": true,
	} {
		if got := outside(filepath.Clean(target), root); got != want {
			t.Errorf("outside(%q) = %v, want %v", target, got, want)
		}
	}
	// The resolved spelling of the same folder (macOS /private/var, Windows
	// long names) is inside too, whichever side is resolved.
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if outside(filepath.Join(real, "AGENTS.md"), root) || outside(filepath.Join(root, "AGENTS.md"), real) {
		t.Errorf("resolved spelling %q of %q counted as outside", real, root)
	}
}

// A link to a file outside the project (a shared instruction file) is
// neither edited by connect nor touched by disconnect.
func TestConnectLeavesSharedLinkTarget(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	shared := filepath.Join(e.base, "shared.md")
	if err := os.WriteFile(shared, []byte("# Shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := e.project(t, "linked", nil)
	if err := os.Symlink(shared, filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if got := read(t, shared); got != "# Shared\n" {
		t.Errorf("shared target edited: %q", got)
	}
	if !strings.Contains(e.out.String(), "outside the project") {
		t.Errorf("output:\n%s", e.out)
	}
	if err := Disconnect(dir, true, e.o); err != nil {
		t.Fatal(err)
	}
	if got := read(t, shared); got != "# Shared\n" {
		t.Errorf("shared target changed by disconnect: %q", got)
	}
}
