package projects

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	inventory "github.com/alex-danilin/cbrt/internal/analyze/projects"
)

// Markers around the rule block cbrt adds to AGENTS.md.
const (
	BlockStart = "<!-- cbrt:plan -->"
	BlockEnd   = "<!-- /cbrt:plan -->"
)

// RuleBlock is the AGENTS.md block that teaches agents the PLAN.md format.
const RuleBlock = BlockStart + `
## Plan and progress
- The project plan is in PLAN.md. Read it before starting work.
- Plan new work there first: milestones as ` + "`##`" + ` headings, tasks as ` + "`- [ ]`" + ` lines.
- Mark the task you start ` + "`- [>]`" + ` and set the ` + "`Now:`" + ` line. Mark it ` + "`- [x]`" + ` only with evidence (a passing test, a commit, command output) written after an em dash.
- Only the user parks tasks: never write ` + "`- [~]`" + ` yourself (deferred or future work stays ` + "`- [ ]`" + `), and never delete or reword a parked task.
- Keep PLAN.md short: done milestones may be collapsed to one line.
` + BlockEnd + "\n"

// agentsHeader starts an AGENTS.md that connect creates.
const agentsHeader = "# Agent instructions\n\n"

// claudeImport is the CLAUDE.md line that makes Claude Code read AGENTS.md
// when a CLAUDE.md exists: Claude Code then reads CLAUDE.md instead of
// AGENTS.md. Without a CLAUDE.md, Claude Code (v2.1.277+), Codex CLI and
// Antigravity all read AGENTS.md themselves, so connect creates none.
const claudeImport = "@AGENTS.md"

// ReadmeTask is the first PLAN.md task when connect wrote the README skeleton.
const ReadmeTask = "Write the README overview"

// commitMessage is the message of the commit after a git init.
const commitMessage = "Connect to cbrt projects: PLAN.md and agent instructions"

// commitGuard, when set (tests), is called before connect runs git add and
// git commit in dir; an error stops the commit.
var commitGuard func(dir string) error

// Options are the machine facts and switches connect and disconnect use;
// tests set them.
type Options struct {
	Home     string   // the folder holding projects.json; "" = Home()
	UserHome string   // the user's home folder; "" = os.UserHomeDir
	TempDirs []string // nil = the usual temp roots
	DryRun   bool
	Out      io.Writer // every action is printed here; nil = discard
	Now      func() time.Time
	// Visibility is Public or Private to override the detection (the
	// --public and --private flags); "" detects it from the remotes.
	Visibility string
	// HTTP asks GitHub and GitLab whether a repository is public; nil = a
	// plain client. Tests inject a fake transport.
	HTTP *http.Client
	// Agent (claude, codex or agy), when set, writes the overview of a
	// README connect creates, headless and read-only; "" leaves the
	// skeleton's placeholder and adds the PLAN.md task ReadmeTask.
	Agent        string
	AgentTimeout time.Duration // 0 = DefaultAgentTimeout
	Run          RunFunc       // nil = ExecRun
}

func (o *Options) defaults() {
	if o.Home == "" {
		o.Home = Home()
	}
	if o.UserHome == "" {
		o.UserHome, _ = os.UserHomeDir()
	}
	if o.TempDirs == nil {
		o.TempDirs = inventory.DefaultTempDirs()
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// refusal applies the analyzer's folder rules to dir, both as written and
// with links (symlinks, junctions) resolved, against the home folder and
// temp roots as written and resolved.
func (o *Options) refusal(dir string) string {
	homes := withResolved([]string{o.UserHome})
	temps := withResolved(o.TempDirs)
	for _, d := range withResolved([]string{dir}) {
		for _, h := range homes {
			if why := inventory.Refusal(d, h, temps); why != "" {
				return why
			}
		}
	}
	return ""
}

// withResolved is ps plus each one's link-resolved forms where they differ.
func withResolved(ps []string) []string {
	out := slices.Clone(ps)
	for _, p := range ps {
		if p == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
			out = append(out, r)
		}
		if r := resolveJunctions(p); r != p {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

// resolveJunctions resolves every link along an absolute path p, including
// Windows junctions, which filepath.EvalSymlinks does not follow (os.Lstat
// reports them as irregular files; os.Readlink reads their target). It
// returns p when nothing resolves.
func resolveJunctions(p string) string {
	cur := filepath.Clean(p)
	for range 40 { // a bound against link loops
		vol := filepath.VolumeName(cur)
		parts := strings.FieldsFunc(cur[len(vol):], func(r rune) bool { return r == '/' || r == '\\' })
		prefix := vol + string(filepath.Separator)
		changed := false
		for i, part := range parts {
			next := filepath.Join(prefix, part)
			st, err := os.Lstat(next)
			if err != nil {
				return cur
			}
			if st.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
				if t, err := os.Readlink(next); err == nil {
					if !filepath.IsAbs(t) {
						t = filepath.Join(prefix, t)
					}
					cur = filepath.Join(append([]string{t}, parts[i+1:]...)...)
					changed = true
					break
				}
			}
			prefix = next
		}
		if !changed {
			return cur
		}
	}
	return cur
}

// Clean replaces control characters (tabs, newlines, escape sequences) in
// text from a repository with spaces before it reaches a terminal.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// actor prints actions, in the past tense or, in a dry run, as "would …".
type actor struct {
	out  io.Writer
	dry  bool
	done int
}

func (a *actor) act(past, would string, args ...any) {
	a.done++
	if a.dry {
		fmt.Fprintf(a.out, "  would "+would+"\n", args...)
		return
	}
	fmt.Fprintf(a.out, "  "+past+"\n", args...)
}

func (a *actor) note(format string, args ...any) {
	fmt.Fprintf(a.out, "  "+format+"\n", args...)
}

// Repository situations of a folder being connected.
type repoKind int

const (
	repoNone    repoKind = iota // not inside a git work tree
	repoOwn                     // the folder is the top level of its repository
	repoOuter                   // a subfolder of a repository (a monorepo)
	repoIgnored                 // inside a repository that ignores it
	repoRefused                 // inside a repository whose top level cannot be a project (dotfiles in home)
)

// Connect makes dir a tracked project: git init when it is not inside a git
// work tree, PLAN.md, a README.md skeleton, the AGENTS.md rule block, the
// @AGENTS.md import in an existing CLAUDE.md, and an entry in the project
// list. Existing files are
// never overwritten. After a git init it commits only the files it created.
// A second run changes nothing and says so.
func Connect(dir string, o Options) error {
	o.defaults()
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if o.Agent != "" {
		if err := validAgent(o.Agent); err != nil {
			return err
		}
	}
	if why := o.refusal(abs); why != "" {
		return fmt.Errorf("refusing to connect %s: it is %s", Clean(abs), why)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("connect %s: %w", Clean(abs), errors.Unwrap(err))
	}
	if !st.IsDir() {
		return fmt.Errorf("connect %s: not a folder", Clean(abs))
	}
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git is not on PATH")
	}
	es, err := Load(o.Home)
	if err != nil {
		return err
	}
	idx := findPath(es, abs)
	name := uniqueName(es, abs)
	if idx >= 0 {
		name = es[idx].Name
	}
	if o.DryRun {
		fmt.Fprintf(o.Out, "%s (%s), dry run: nothing is written\n", Clean(name), Clean(abs))
	} else {
		fmt.Fprintf(o.Out, "%s (%s)\n", Clean(name), Clean(abs))
	}
	a := &actor{out: o.Out, dry: o.DryRun}

	// git
	kind, top, err := inspectRepo(abs, &o)
	if err != nil {
		return err
	}
	needInit := kind == repoNone || kind == repoIgnored || kind == repoRefused
	switch kind {
	case repoIgnored:
		a.note("the folder is inside the git repository at %s, which ignores it: it gets a repository of its own", Clean(top))
	case repoOuter:
		a.note("the folder is inside the git repository at %s: files are created there and not committed", Clean(top))
	}
	if needInit {
		if !o.DryRun {
			if _, err := git(abs, "init", "-q"); err != nil {
				return err
			}
		}
		a.act("git init", "git init")
	}

	// visibility: a repository cbrt just initialised has no remote yet
	switch o.Visibility {
	case "", Public, Private:
	default:
		return fmt.Errorf("visibility %q: want %q or %q", o.Visibility, Public, Private)
	}
	haveRepo := !(needInit && o.DryRun) // a dry run has no repository to ask
	var urls []string
	if !needInit {
		if urls, err = remotes(abs); err != nil {
			return err
		}
	}
	vis := decideVisibility(context.Background(), o.Visibility, urls, o.HTTP)
	a.note("visibility: %s (%s)", vis.Visibility, vis.How)
	target := excludeTarget{File: filepath.Join(abs, ".git", "info", "exclude")}
	if haveRepo {
		if target, err = findExclude(abs); err != nil {
			return err
		}
	}
	public := vis.Visibility == Public

	var created []string
	create := func(rel string, data []byte) error {
		if !o.DryRun {
			if err := createFile(filepath.Join(abs, rel), data); err != nil {
				return err
			}
		}
		created = append(created, rel)
		a.act("created %s", "create %s", rel)
		return nil
	}

	// README.md
	readmeName, readme := findReadme(abs)
	readmeTask := false
	if readmeName == "" {
		overview := ""
		if o.Agent != "" {
			overview = o.readmeOverview(name, abs, a)
		}
		if err := create("README.md", readmeSkeleton(name, abs, overview)); err != nil {
			return err
		}
		readmeTask = overview == ""
		readme = []byte(overview)
	}

	// PLAN.md
	if _, err := os.Lstat(filepath.Join(abs, "PLAN.md")); errors.Is(err, fs.ErrNotExist) {
		if err := create("PLAN.md", planTemplate(name, readmeGoal(readme), readmeTask)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if readmeTask {
		a.note("PLAN.md exists and is left as is; add a task to it: %q", ReadmeTask)
	}

	// AGENTS.md (a link is followed: the file it points to is edited)
	agentsLink := filepath.Join(abs, "AGENTS.md")
	agents, err := resolveLink(agentsLink)
	if err != nil {
		return err
	}
	switch b, err := os.ReadFile(agents); {
	case agents != agentsLink && outside(agents, abs):
		a.note("AGENTS.md links to %s, outside the project: not edited; add the plan rule block there yourself if you want it", Clean(agents))
	case errors.Is(err, fs.ErrNotExist):
		if err := create("AGENTS.md", []byte(agentsHeader+RuleBlock)); err != nil {
			return err
		}
	case err != nil:
		return err
	case bytes.Contains(b, []byte(BlockStart)):
	default:
		if !o.DryRun {
			if err := replaceFile(agents, appendBlock(b, RuleBlock)); err != nil {
				return err
			}
		}
		a.act("appended the plan rule block to AGENTS.md", "append the plan rule block to AGENTS.md")
	}

	// CLAUDE.md
	claudeLink := filepath.Join(abs, "CLAUDE.md")
	if sameFile(claudeLink, agentsLink, agents) {
		// CLAUDE.md is AGENTS.md: Claude Code reads the block directly.
	} else {
		claude, err := resolveLink(claudeLink)
		if err != nil {
			return err
		}
		switch b, err := os.ReadFile(claude); {
		case claude != claudeLink && outside(claude, abs):
			a.note("CLAUDE.md links to %s, outside the project: not edited; add the line %s there yourself if you want it", Clean(claude), claudeImport)
		case errors.Is(err, fs.ErrNotExist):
			// No CLAUDE.md: Claude Code reads AGENTS.md itself (v2.1.277+).
		case err != nil:
			return err
		case hasImport(b):
		default:
			if !o.DryRun {
				if err := replaceFile(claude, appendBlock(b, claudeImport+"\n")); err != nil {
					return err
				}
			}
			a.act("added the line %s to CLAUDE.md", "add the line %s to CLAUDE.md", claudeImport)
		}
	}

	// public repository: the plan files stay out of git, locally
	// (an entry from before created_agents existed: an AGENTS.md that holds
	// exactly what connect writes was created by connect)
	createdAgents := slices.Contains(created, "AGENTS.md") || (idx >= 0 && es[idx].CreatedAgents) || onlyOurAgents(agents)
	local := excludeFiles(createdAgents)
	if public {
		if err := keepLocal(abs, target, local, haveRepo, a); err != nil {
			return err
		}
	} else if haveRepo && hasExcludeBlock(target) {
		a.note("the local exclude block from an earlier public verdict stays in %s (you may want it);", Clean(target.File))
		a.note("  to remove it, delete the lines from %q to %q there", excludeStart+excludePattern(target.Prefix), excludeEnd)
	}
	// committable drops the files a public repository keeps local
	committable := func(fs []string) []string {
		if !public {
			return fs
		}
		return slices.DeleteFunc(slices.Clone(fs), func(f string) bool { return slices.Contains(local, f) })
	}

	// commit what a repository cbrt initialised got from it; retry an
	// earlier commit that did not happen while there are still no commits
	var pending []string
	var commitErr error
	switch {
	case needInit && len(committable(created)) > 0:
		pending, commitErr = commitFiles(abs, committable(created), a)
	case idx >= 0 && len(es[idx].PendingCommit) > 0 && kind == repoOwn && !o.DryRun:
		if _, err := git(abs, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
			break // the user committed since: nothing to retry
		}
		if retry := committable(untracked(abs, es[idx].PendingCommit)); len(retry) > 0 {
			a.note("retrying the commit of the files connect created earlier")
			pending, commitErr = commitFiles(abs, retry, a)
		}
	case idx >= 0 && len(es[idx].PendingCommit) > 0 && kind == repoOwn:
		if _, err := git(abs, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
			if retry := committable(untracked(abs, es[idx].PendingCommit)); len(retry) > 0 {
				a.act("", "retry the commit of %s", strings.Join(retry, ", "))
			}
		}
	}

	// project list, saved after the commit
	if !o.DryRun {
		changed := false
		if idx < 0 {
			es = append(es, Entry{Name: name, Path: abs, ConnectedAt: o.Now().UTC().Truncate(time.Second), PendingCommit: pending,
				Visibility: vis.Visibility, VisibilityHow: vis.How, CreatedAgents: createdAgents})
			changed = true
		} else {
			e := &es[idx]
			if !slices.Equal(e.PendingCommit, pending) || e.Visibility != vis.Visibility || e.VisibilityHow != vis.How || e.CreatedAgents != createdAgents {
				e.PendingCommit, e.Visibility, e.VisibilityHow, e.CreatedAgents = pending, vis.Visibility, vis.How, createdAgents
				changed = true
			}
		}
		if changed {
			if err := Save(o.Home, es); err != nil {
				return err
			}
		}
	}
	if idx < 0 {
		a.act("added %s to the project list (%s)", "add %s to the project list (%s)", Clean(name), Clean(filepath.Join(o.Home, ListFile)))
	}
	if commitErr != nil {
		return commitErr
	}
	if a.done == 0 && len(pending) == 0 {
		fmt.Fprintln(o.Out, "  already connected; nothing to change")
	}
	return nil
}

// inspectRepo finds the repository dir is in. Only git's "not a git
// repository" means there is none; any other failure (a timeout, dubious
// ownership) is an error.
func inspectRepo(dir string, o *Options) (repoKind, string, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return repoNone, "", nil
		}
		return 0, "", err
	}
	top := filepath.FromSlash(out)
	if sameDir(top, dir) {
		return repoOwn, top, nil
	}
	if o.refusal(top) != "" {
		return repoRefused, top, nil
	}
	ignored, err := gitStatus(dir, "check-ignore", "-q", ".")
	if err != nil {
		return 0, "", err
	}
	if ignored {
		return repoIgnored, top, nil
	}
	return repoOuter, top, nil
}

func sameDir(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	if err1 == nil && err2 == nil {
		return os.SameFile(sa, sb)
	}
	return inventory.SamePath(a, b)
}

// commitFiles commits exactly files in dir. It returns the files still to
// commit when the commit was skipped (no git identity) or failed, and
// prints the commands that finish it by hand.
func commitFiles(dir string, files []string, a *actor) ([]string, error) {
	if a.dry {
		a.act("", "commit %s (only these files)", strings.Join(files, ", "))
		return nil, nil
	}
	name, _ := git(dir, "config", "--get", "user.name")
	email, _ := git(dir, "config", "--get", "user.email")
	if name == "" || email == "" {
		a.note("not committed: git user.name and user.email are not configured")
		manualCommit(dir, files, a)
		return files, nil
	}
	err := func() error {
		if commitGuard != nil {
			if err := commitGuard(dir); err != nil {
				return err
			}
		}
		if _, err := git(dir, append([]string{"add", "--"}, files...)...); err != nil {
			return err
		}
		_, err := git(dir, append([]string{"commit", "-q", "-m", commitMessage, "--"}, files...)...)
		return err
	}()
	if err != nil {
		a.note("not committed: %s", Clean(err.Error()))
		manualCommit(dir, files, a)
		return files, err
	}
	sha, _ := git(dir, "rev-parse", "--short", "HEAD")
	a.act("committed %s (%s)", "commit %s (%s)", strings.Join(files, ", "), sha)
	return nil, nil
}

func manualCommit(dir string, files []string, a *actor) {
	a.note("to commit them yourself, run:")
	a.note(`  git -C "%s" add -- %s`, Clean(dir), strings.Join(files, " "))
	a.note(`  git -C "%s" commit -m "%s" -- %s`, Clean(dir), commitMessage, strings.Join(files, " "))
	a.note("or run connect again once git can commit")
}

// untracked is the files that exist in dir and git does not track.
func untracked(dir string, files []string) []string {
	out, err := git(dir, append([]string{"ls-files", "--"}, files...)...)
	if err != nil {
		return nil
	}
	tracked := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		tracked[strings.TrimSpace(l)] = true
	}
	var rest []string
	for _, f := range files {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil && !tracked[f] {
			rest = append(rest, f)
		}
	}
	return rest
}

// git runs git -C dir args and returns its trimmed output; the error
// carries git's message.
func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return gitCtx(ctx, dir, args...)
}

func gitCmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	if args[0] != "commit" {
		// English messages: inspectRepo tells "not a git repository" apart.
		// A commit keeps the user's locale: their hooks run under it.
		cmd.Env = append(cmd.Env, "LC_ALL=C", "LANGUAGE=")
	}
	return cmd
}

func gitCtx(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := gitCmd(ctx, dir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitStatus runs a git command that answers with its exit status: 0 is
// true, 1 is false, anything else an error.
func gitStatus(dir string, args ...string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := gitCmd(ctx, dir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, nil
	}
	return false, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()+" "+err.Error()))
}

// resolveLink returns the file a link at p points to (p itself when it is
// not a link or does not exist); a broken link is an error.
func resolveLink(p string) (string, error) {
	st, err := os.Lstat(p)
	if err != nil || st.Mode()&fs.ModeSymlink == 0 {
		return p, nil
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("%s is a link that cannot be followed: %w", filepath.Base(p), errors.Unwrap(err))
	}
	return r, nil
}

// onlyOurAgents reports an AGENTS.md at p that holds exactly what connect
// writes into a new one (line endings aside).
func onlyOurAgents(p string) bool {
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	return norm(string(b)) == norm(agentsHeader+RuleBlock)
}

// outside reports a link target that is not inside the project folder root
// (a shared file such as ~/.claude/CLAUDE.md): connect and disconnect never
// edit or delete those, since other projects may read them too.
// Both paths are compared as given and resolved (macOS /var is /private/var;
// Windows short names such as RUNNER~1), so either spelling counts as inside.
func outside(target, root string) bool {
	roots, targets := []string{root}, []string{target}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		roots = append(roots, r)
	}
	if t, err := filepath.EvalSymlinks(target); err == nil {
		targets = append(targets, t)
	} else if d, err := filepath.EvalSymlinks(filepath.Dir(target)); err == nil {
		// a file not created yet: resolve its folder
		targets = append(targets, filepath.Join(d, filepath.Base(target)))
	}
	for _, r := range roots {
		for _, t := range targets {
			rel, err := filepath.Rel(r, t)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
				return false
			}
		}
	}
	return true
}

// sameFile reports the CLAUDE.md at claude being the AGENTS.md (a link to
// it, a link to the same target, or a hard link), also when AGENTS.md does
// not exist yet.
func sameFile(claude, agentsLink, agentsTarget string) bool {
	if a, err := os.Stat(agentsTarget); err == nil {
		if c, err := os.Stat(claude); err == nil && os.SameFile(a, c) {
			return true
		}
	}
	t, err := os.Readlink(claude)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(claude), t)
	}
	return inventory.SamePath(t, agentsLink) || inventory.SamePath(t, agentsTarget)
}

// createFile writes a new file and fails when it already exists.
func createFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// replaceFile replaces an existing regular file's content through a temp
// file and a rename, keeping its permissions. path must not be a link
// (resolveLink first).
func replaceFile(path string, data []byte) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, st.Mode().Perm())
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// lineEnding is "\r\n" when b uses CRLF, else "\n".
func lineEnding(b []byte) string {
	if bytes.Contains(b, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// appendBlock appends block to b after a blank line, in b's line endings.
func appendBlock(b []byte, block string) []byte {
	nl := lineEnding(b)
	block = strings.ReplaceAll(block, "\n", nl)
	out := bytes.TrimRight(b, "\r\n")
	if len(out) > 0 {
		out = append(out, nl+nl...)
	}
	return append(out, block...)
}

// removeBlock undoes appendBlock: it removes the rule block, its line end
// and the blank line before it. ok is false when b has no complete block.
func removeBlock(b []byte) ([]byte, bool) {
	s := string(b)
	i := strings.Index(s, BlockStart)
	if i < 0 {
		return b, false
	}
	j := strings.Index(s[i:], BlockEnd)
	if j < 0 {
		return b, false
	}
	nl := lineEnding(b)
	end := i + j + len(BlockEnd)
	if strings.HasPrefix(s[end:], "\r\n") {
		end += 2
	} else if strings.HasPrefix(s[end:], "\n") {
		end++
	}
	before := s[:i]
	if strings.HasSuffix(before, nl+nl) {
		before = before[:len(before)-len(nl)]
	}
	return []byte(before + s[end:]), true
}

func hasImport(b []byte) bool {
	for _, l := range strings.Split(string(b), "\n") {
		switch strings.TrimSpace(l) {
		case claudeImport, "@./AGENTS.md":
			return true
		}
	}
	return false
}

// findReadme returns the name and content of the README at dir, if any.
func findReadme(dir string) (string, []byte) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(strings.ToLower(n), "readme") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil || len(b) > MaxPlanBytes {
			return n, nil
		}
		return n, b
	}
	return "", nil
}

// readmeGoal is the first prose paragraph of a README (no headings, badges,
// HTML or code), on one line and at most 400 characters; "" when none.
func readmeGoal(b []byte) string {
	if !utf8.Valid(b) {
		return ""
	}
	var para []string
	fence := false
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(strings.TrimSuffix(l, "\r"))
		if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~") {
			fence = !fence
			continue
		}
		skip := fence || l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "<") ||
			strings.HasPrefix(l, "![") || strings.HasPrefix(l, "[![") || strings.HasPrefix(l, "|") ||
			strings.HasPrefix(l, "---") || strings.HasPrefix(l, "===")
		if skip {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, l)
	}
	g := strings.Join(para, " ")
	if utf8.RuneCountInString(g) > 400 {
		g = string([]rune(g)[:400]) + "…"
	}
	return g
}

func planTemplate(name, goal string, readmeTask bool) []byte {
	if goal == "" {
		goal = `<one paragraph: what this project is for and what "done" means>`
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Plan: %s\n\nGoal: %s\n\nNow: nothing\n\n## First steps\n", name, goal)
	if readmeTask {
		fmt.Fprintf(&b, "- [ ] %s\n", ReadmeTask)
	}
	b.WriteString("- [ ] Plan the first milestone\n")
	return []byte(b.String())
}

// overviewPlaceholder stands in the README skeleton for the overview.
const overviewPlaceholder = "<!-- Overview: what this project is, who it is for and how far along it is. -->"

// maxOverview bounds the overview an agent writes into a README.
const maxOverview = 4000

// readmeOverview asks the agent for a short overview of the project in dir
// for the README connect creates. Any failure is printed and gives "": the
// README keeps its placeholder and PLAN.md gets the ReadmeTask.
func (o *Options) readmeOverview(name, dir string, a *actor) string {
	prompt := "Write a short overview of the project in this folder (" + name + ") for its README.md: what it is, who it is for and how far along it is, in one or two paragraphs of plain Markdown. " +
		"Answer with the paragraphs only: no heading, no preamble, no code fences. " +
		"This is read-only: do not edit, create or delete any file and do not run commands that change anything. " +
		"If the folder is empty or says too little, answer with one sentence saying what can be told."
	timeout := o.AgentTimeout
	if timeout <= 0 {
		timeout = DefaultAgentTimeout
	}
	if o.DryRun {
		if c, err := newCommand(o.Agent, dir, prompt, timeout, "<temp folder>"); err == nil {
			a.note("would ask %s for the README overview: %s", o.Agent, Clean(c.String()))
		}
		return ""
	}
	a.note("asking %s for the README overview (read-only, up to %v)…", o.Agent, timeout)
	out, err := runAgent(context.Background(), o.Run, o.Agent, dir, prompt, timeout)
	if err == nil {
		out = cleanOverview(out)
		if out == "" {
			err = errors.New("the answer held no text")
		}
	}
	if err != nil {
		a.note("%s wrote no overview (%s): the README keeps the placeholder and PLAN.md gets the task %q", o.Agent, Clean(err.Error()), ReadmeTask)
		return ""
	}
	return out
}

// cleanOverview drops a heading line or a code fence an agent may wrap the
// overview in, normalises line endings and bounds the length.
func cleanOverview(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") || (strings.HasPrefix(t, "#") && len(keep) == 0) {
			continue
		}
		keep = append(keep, strings.TrimRight(l, " \t"))
	}
	s = strings.TrimSpace(strings.Join(keep, "\n"))
	if utf8.RuneCountInString(s) > maxOverview {
		s = string([]rune(s)[:maxOverview]) + "…"
	}
	return s
}

// readmeSkeleton is the README written when there is none: the name, the
// overview (or a placeholder for it) and the build facts cbrt can detect.
// overview is the overview the agent wrote, or "" for the placeholder.
func readmeSkeleton(name, dir, overview string) []byte {
	var b strings.Builder
	if overview == "" {
		overview = overviewPlaceholder
	}
	fmt.Fprintf(&b, "# %s\n\n%s\n\n## Build and run\n\n", name, overview)
	builds := inventory.Builds(dir)
	if len(builds) == 0 {
		b.WriteString("No build files detected yet.\n")
	}
	for _, bd := range builds {
		switch bd.File {
		case "package.json":
			if len(bd.Names) == 0 {
				b.WriteString("- Node.js project (package.json): `npm install`\n")
				continue
			}
			fmt.Fprintf(&b, "- package.json scripts (`npm run <name>`): %s\n", codeList(bd.Names, bd.More))
		case "go.mod":
			b.WriteString("- Go module (go.mod): `go build ./...`, `go test ./...`\n")
		case "Cargo.toml":
			b.WriteString("- Rust crate (Cargo.toml): `cargo build`, `cargo test`\n")
		case "pyproject.toml":
			b.WriteString("- Python project (pyproject.toml)\n")
		default: // a Makefile
			if len(bd.Names) == 0 {
				fmt.Fprintf(&b, "- %s: `make`\n", bd.File)
				continue
			}
			fmt.Fprintf(&b, "- %s targets (`make <target>`): %s\n", bd.File, codeList(bd.Names, bd.More))
		}
	}
	return []byte(b.String())
}

func codeList(names []string, more int) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "`" + n + "`"
	}
	s := strings.Join(q, ", ")
	if more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}

// Disconnect removes the project named key (or whose folder is key) from
// the list. With removeFiles it also removes the AGENTS.md rule block
// (and AGENTS.md itself when nothing else is left in it); PLAN.md, the
// README and checks are the user's history and stay.
func Disconnect(key string, removeFiles bool, o Options) error {
	o.defaults()
	es, err := Load(o.Home)
	if err != nil {
		return err
	}
	i := Find(es, key)
	if i < 0 {
		if abs, err := filepath.Abs(key); err == nil {
			i = findPath(es, abs)
		}
	}
	if i < 0 {
		return fmt.Errorf("no connected project %q (see cbrt projects list)", Clean(key))
	}
	e := es[i]
	fmt.Fprintf(o.Out, "%s (%s)\n", Clean(e.Name), Clean(e.Path))
	a := &actor{out: o.Out}
	if removeFiles {
		if err := removeRuleBlock(filepath.Join(e.Path, "AGENTS.md"), a); err != nil {
			return err
		}
		if err := removeExclude(e.Path, a); err != nil {
			return err
		}
	}
	es = append(es[:i:i], es[i+1:]...)
	if err := Save(o.Home, es); err != nil {
		return err
	}
	a.act("removed %s from the project list", "", Clean(e.Name))
	if !removeFiles {
		a.note("PLAN.md, the AGENTS.md rule block and any .git/info/exclude block stay; --remove-files removes the blocks")
	}
	return nil
}

func removeRuleBlock(link string, a *actor) error {
	agents, err := resolveLink(link)
	if err != nil {
		return err
	}
	if agents != link && outside(agents, filepath.Dir(link)) {
		a.note("AGENTS.md links to %s, outside the project: not edited", Clean(agents))
		return nil
	}
	b, err := os.ReadFile(agents)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		a.note("no AGENTS.md; no files changed")
		return nil
	case err != nil:
		return err
	}
	out, ok := removeBlock(b)
	if !ok {
		a.note("AGENTS.md has no plan rule block; no files changed")
		return nil
	}
	if rest := strings.TrimSpace(string(out)); rest == "" || rest == strings.TrimSpace(agentsHeader) {
		if err := os.Remove(agents); err != nil {
			return err
		}
		if agents != link {
			os.Remove(link) // the link would dangle
		}
		a.act("removed AGENTS.md (it held only the plan rule block)", "")
		return nil
	}
	if err := replaceFile(agents, out); err != nil {
		return err
	}
	a.act("removed the plan rule block from AGENTS.md", "")
	return nil
}
