package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	_ "github.com/alex-danilin/cbrt/internal/analyze/adapters/all"
	"github.com/alex-danilin/cbrt/internal/analyze/report"
	"github.com/alex-danilin/cbrt/internal/analyze/review"
	"github.com/alex-danilin/cbrt/internal/analyze/runner"
	"github.com/alex-danilin/cbrt/internal/analyze/state"
	"github.com/alex-danilin/cbrt/internal/skills"
)

const analyzeUsage = `usage: cbrt analyze <command>

  run [--since 14d|<RFC 3339>] [--tools claude,codex] [--project <substring>] [--no-cache]
        collect sessions and write the reviewer's inputs; prints the output folder
  excerpt <tool>/<store>/<session> <from-seq> <to-seq> [--context 2]
        a redacted transcript slice, as evidence for the review
  rec list | due
  rec add <file.json|->              add a recommendation (the review skill does this)
  rec eval <rec-id> <associated_improvement|no_change|worsened|inconclusive> <reason> --run <run-id>
  rec applied|declined|reverted <rec-id> [notes]   your decision on a recommendation
  run-note --review-tokens <n> [--run <run-id>]     record the review's token spend
  prune                              delete output folders past the retention
  install claude [--with <skill,...|all>] [--write]
        hooks, the /cbrt-multi-session-review skill and the optional skills named
        (cbrt-session-review); shows the changes, --write applies them
  uninstall claude [--keep-skills|--purge] [--write]
        --keep-skills leaves the installed skills in place; --purge also deletes
        all collected data and settings backups

Analyzer home: $CBRT_ANALYZE_HOME, default <config dir>/cbrt/analyze.
`

func runAnalyze(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, analyzeUsage)
		return 2
	}
	home := analyze.Home()
	var err error
	switch args[0] {
	case "run":
		err = analyzeRun(home, args[1:], stdout)
	case "excerpt":
		err = analyzeExcerpt(home, args[1:], stdout)
	case "rec":
		err = analyzeRec(home, args[1:], stdin, stdout)
	case "run-note":
		err = analyzeRunNote(home, args[1:])
	case "prune":
		var cfg analyze.Config
		if cfg, err = analyze.LoadConfig(home); err == nil {
			err = state.Prune(home, time.Now(), cfg.RetentionDays)
		}
	case "install", "uninstall":
		err = analyzeInstall(home, args[0] == "install", args[1:], stdout)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, analyzeUsage)
	default:
		fmt.Fprintf(stderr, "cbrt analyze: unknown command %q\n\n%s", args[0], analyzeUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "cbrt analyze:", err)
		return 1
	}
	return 0
}

func analyzeRun(home string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("analyze run", flag.ContinueOnError)
	since := fs.String("since", "", "")
	tools := fs.String("tools", "", "")
	project := fs.String("project", "", "")
	noCache := fs.Bool("no-cache", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A bare argument like "14d" is a window.
	if rest := fs.Args(); len(rest) == 1 && *since == "" {
		*since = rest[0]
	} else if len(rest) > 0 {
		return fmt.Errorf("unexpected arguments %q", rest)
	}
	var ts []string
	for _, t := range strings.Split(*tools, ",") {
		if t = strings.TrimSpace(t); t != "" {
			ts = append(ts, t)
		}
	}
	res, err := runner.Run(home, runner.Options{Since: *since, Tools: ts, Project: *project, NoCache: *noCache})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\nrun %s: %d sessions, %d events, window %s .. %s",
		res.OutDir, res.RunID, res.Sessions, res.Events,
		res.Window.Since.Format(time.RFC3339), res.Window.Until.Format(time.RFC3339))
	if res.Problems > 0 {
		fmt.Fprintf(stdout, ", %d problems (see summary.md)", res.Problems)
	}
	fmt.Fprintln(stdout)
	for _, n := range res.Notes {
		fmt.Fprintln(stdout, n)
	}
	return nil
}

func analyzeExcerpt(home string, args []string, stdout io.Writer) error {
	const usage = "usage: cbrt analyze excerpt <tool>/<store>/<session> <from-seq> <to-seq> [--context N]"
	fs := flag.NewFlagSet("analyze excerpt", flag.ContinueOnError)
	fs.SetOutput(stdout)
	ctx := fs.Int("context", report.DefaultContext, "transcript events to show before and after the range")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), usage+"\n\nPrints the events from <from-seq> to <to-seq> (seqs as in anchors.jsonl and\nexcerpt_candidates.jsonl), redacted, one header line per event and its text cut\nto a per-kind length.\n\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 3 {
		return errors.New(usage)
	}
	s, err := runner.FindSession(home, fs.Arg(0), time.Now())
	if err != nil {
		return err
	}
	red, err := report.LoadRedactor(home)
	if err != nil {
		return err
	}
	out, err := report.Excerpt(s, fs.Arg(1), fs.Arg(2), red, *ctx)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, out)
	return err
}

func analyzeRec(home string, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: cbrt analyze rec list|due|add|eval|applied|declined|reverted …")
	}
	now := time.Now()
	switch args[0] {
	case "list", "due":
		r, err := state.LoadRecs(home)
		if err != nil {
			return err
		}
		items := r.Items
		if args[0] == "due" {
			items = state.DueForEval(r, now)
		}
		return writeJSON(stdout, items)
	case "add":
		if len(args) != 2 {
			return errors.New("usage: cbrt analyze rec add <file.json|->")
		}
		var b []byte
		var err error
		if args[1] == "-" {
			b, err = io.ReadAll(io.LimitReader(stdin, 1<<20))
		} else {
			b, err = os.ReadFile(args[1])
		}
		if err != nil {
			return err
		}
		var rec state.Recommendation
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil {
			return fmt.Errorf("recommendation JSON: %w", err)
		}
		if rec.CreatedRunID == "" { // the run the review is reading
			if runs, err := state.LoadRuns(home); err == nil && len(runs) > 0 {
				rec.CreatedRunID = runs[len(runs)-1].RunID
			}
		}
		added, err := state.AddRec(home, rec, now)
		if err != nil {
			return err
		}
		if err := exportRecs(home); err != nil {
			return err
		}
		return writeJSON(stdout, added)
	case "eval":
		fs := flag.NewFlagSet("rec eval", flag.ContinueOnError)
		run := fs.String("run", "", "")
		if err := fs.Parse(reorderFlags(args[1:])); err != nil {
			return err
		}
		if fs.NArg() != 3 || *run == "" {
			return errors.New("usage: cbrt analyze rec eval <rec-id> <verdict> <reason> --run <run-id>")
		}
		if err := state.SetEval(home, fs.Arg(0), fs.Arg(1), fs.Arg(2), *run); err != nil {
			return err
		}
		return exportRecs(home)
	case "applied", "declined", "reverted":
		if len(args) < 2 {
			return fmt.Errorf("usage: cbrt analyze rec %s <rec-id> [notes]", args[0])
		}
		if err := state.SetImpl(home, args[1], args[0], strings.Join(args[2:], " "), now); err != nil {
			return err
		}
		return exportRecs(home)
	}
	return fmt.Errorf("unknown rec command %q", args[0])
}

func exportRecs(home string) error {
	r, err := state.LoadRecs(home)
	if err != nil {
		return err
	}
	return state.WriteExport(home, r)
}

func analyzeRunNote(home string, args []string) error {
	fs := flag.NewFlagSet("analyze run-note", flag.ContinueOnError)
	tokens := fs.Int("review-tokens", -1, "")
	run := fs.String("run", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tokens < 0 {
		return errors.New("usage: cbrt analyze run-note --review-tokens <n> [--run <run-id>]")
	}
	runs, err := state.LoadRuns(home)
	if err != nil {
		return err
	}
	id := *run
	if id == "" {
		if len(runs) == 0 {
			return errors.New("no run yet")
		}
		id = runs[len(runs)-1].RunID
	}
	// A note line annotates run id with the review's spend; it has no window,
	// so it never moves the next run's start (state.Run.Cursor).
	return state.AppendRun(home, state.Run{RunID: id, RanAt: time.Now().UTC(), ReviewTokensSpent: *tokens, Notes: "review"})
}

// reorderFlags moves flags after positional arguments to the front, so
// "excerpt <s> <a> <b> --context 3" parses like "excerpt --context 3 <s> <a> <b>".
func reorderFlags(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") && len(args[i]) > 1 {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, args[i])
	}
	return append(flags, pos...)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// --- install: Claude Code hooks and the review skill ---

// claudeHookEntry is one entry of settings.json "hooks.<Event>".
type claudeHookEntry struct {
	Matcher string            `json:"matcher,omitempty"`
	Hooks   []json.RawMessage `json:"hooks"`
}

type claudeCommandHook struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Timeout int      `json:"timeout,omitempty"`
}

// isCbrtHook reports whether a hook is ours: exec form running some cbrt
// binary with "hook record claude".
func isCbrtHook(raw json.RawMessage) bool {
	var h claudeCommandHook
	if json.Unmarshal(raw, &h) != nil || h.Type != "command" ||
		len(h.Args) < 3 || h.Args[0] != "hook" || h.Args[1] != "record" || h.Args[2] != "claude" {
		return false
	}
	// The command may be a Windows path read on a Unix host: split on both.
	parts := strings.FieldsFunc(h.Command, func(r rune) bool { return r == '/' || r == '\\' })
	return len(parts) > 0 && strings.HasPrefix(strings.ToLower(parts[len(parts)-1]), "cbrt")
}

// sameJSON reports whether two JSON values are equal, ignoring formatting and
// key order; a missing value equals null.
func sameJSON(a, b json.RawMessage) bool {
	var va, vb any
	if len(a) > 0 && json.Unmarshal(a, &va) != nil {
		return false
	}
	if len(b) > 0 && json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// planClaudeHooks returns settings.json's hooks object with every cbrt hook
// removed and, when install, one exec-form hook per event added.
func planClaudeHooks(current json.RawMessage, exe string, events []string, install bool) (json.RawMessage, error) {
	hooks := map[string][]claudeHookEntry{}
	if len(current) > 0 && string(current) != "null" {
		if err := json.Unmarshal(current, &hooks); err != nil {
			return nil, fmt.Errorf("settings.json hooks: %w", err)
		}
	}
	for ev, entries := range hooks {
		var keep []claudeHookEntry
		for _, e := range entries {
			var hs []json.RawMessage
			for _, h := range e.Hooks {
				if !isCbrtHook(h) {
					hs = append(hs, h)
				}
			}
			if len(hs) > 0 {
				e.Hooks = hs
				keep = append(keep, e)
			}
		}
		if len(keep) > 0 {
			hooks[ev] = keep
		} else {
			delete(hooks, ev)
		}
	}
	if install {
		for _, ev := range events {
			h, _ := json.Marshal(claudeCommandHook{Type: "command", Command: exe, Args: []string{"hook", "record", "claude", ev}, Timeout: 10})
			hooks[ev] = append(hooks[ev], claudeHookEntry{Hooks: []json.RawMessage{h}})
		}
	}
	if len(hooks) == 0 {
		return nil, nil
	}
	return json.Marshal(hooks)
}

func analyzeInstall(home string, install bool, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("analyze install", flag.ContinueOnError)
	write := fs.Bool("write", false, "")
	purge := fs.Bool("purge", false, "")
	with := fs.String("with", "", "")
	keepSkills := fs.Bool("keep-skills", false, "")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || fs.Arg(0) != "claude" {
		return errors.New("usage: cbrt analyze install claude [--with <skill,...|all>] [--write], or uninstall claude [--keep-skills|--purge] [--write] (Codex and Antigravity hooks come in a later phase)")
	}
	switch {
	case *purge && install:
		return errors.New("--purge goes with uninstall")
	case *keepSkills && install:
		return errors.New("--keep-skills goes with uninstall")
	case *with != "" && !install:
		return errors.New("--with goes with install; uninstall removes every skill cbrt installed")
	case *keepSkills && *purge:
		return errors.New("--keep-skills and --purge together would leave skills that read the rubric --purge deletes")
	}
	extra, err := optionalSkills(*with)
	if err != nil {
		return err
	}
	a, _ := analyze.Get("claude")
	cfg, err := analyze.LoadConfig(home)
	if err != nil {
		return err
	}
	var events []string
	for _, ev := range a.Capabilities().HookEvents {
		if !slices.Contains(cfg.Tools["claude"].DisabledEvents, ev) {
			events = append(events, ev)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if install && isTransient(exe) {
		return fmt.Errorf("this cbrt runs from a temporary build (%s): the hooks would break when it is deleted. "+
			"Install cbrt first (a release binary, or go install ./cmd/cbrt) and run install from that binary", exe)
	}
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		u, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		claudeDir = filepath.Join(u, ".claude")
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	top := map[string]json.RawMessage{}
	orig, err := os.ReadFile(settingsPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(orig, &top); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", settingsPath, err)
		}
	}
	newHooks, err := planClaudeHooks(top["hooks"], exe, events, install)
	if err != nil {
		return err
	}
	hooksChanged := !sameJSON(top["hooks"], newHooks)
	if newHooks == nil {
		delete(top, "hooks")
	} else {
		top["hooks"] = newHooks
	}
	out, err := marshalSettings(top)
	if err != nil {
		return err
	}
	toInstall := append(slices.Clone(skills.Required), extra...)

	switch {
	case hooksChanged:
		verb := map[bool]string{true: "add", false: "remove"}[install]
		fmt.Fprintf(stdout, "%s: %s cbrt hooks for %s (exec form: %s hook record claude <event>)\n",
			settingsPath, verb, strings.Join(events, ", "), exe)
	case install:
		fmt.Fprintf(stdout, "%s: cbrt hooks for %s already installed (exec form: %s hook record claude <event>); unchanged\n",
			settingsPath, strings.Join(events, ", "), exe)
	default:
		fmt.Fprintf(stdout, "%s: no cbrt hooks to remove; unchanged\n", settingsPath)
	}
	// Uninstall removes the skills cbrt made, never a hand-made skill of the
	// same name; install removes the ones this cbrt no longer ships.
	var removeSkills []string
	if !*keepSkills {
		list := skills.Installed
		if install {
			list = skills.Stale
		}
		names, err := list(claudeDir)
		if err != nil {
			return err
		}
		for _, name := range names {
			removeSkills = append(removeSkills, skills.Dir(claudeDir, name))
		}
	}
	switch {
	case install:
		fmt.Fprintf(stdout, "%s: write the review rubric\n", review.RubricPath(home))
		for _, d := range removeSkills {
			fmt.Fprintf(stdout, "%s: remove, a skill this cbrt no longer ships\n", d)
		}
		for _, name := range toInstall {
			p := filepath.Join(skills.Dir(claudeDir, name), "SKILL.md")
			if o, err := skills.OwnerOf(claudeDir, name); err != nil {
				return err
			} else if o == skills.Other {
				fmt.Fprintf(stdout, "%s: keep, not made by cbrt (delete it to get the cbrt /%s skill)\n", p, name)
			} else {
				fmt.Fprintf(stdout, "%s: write the /%s skill\n", p, name)
			}
		}
	case *keepSkills:
		fmt.Fprintf(stdout, "%s: skills kept\n", filepath.Join(claudeDir, "skills"))
	default:
		for _, d := range removeSkills {
			fmt.Fprintf(stdout, "%s: remove\n", d)
		}
	}
	var backups []string
	if *purge {
		if err := checkPurgeable(home); err != nil {
			return err
		}
		backups, _ = filepath.Glob(settingsPath + ".cbrt-backup-*")
		files, size := dirUsage(home)
		fmt.Fprintf(stdout, "%s: delete the analyzer home (%d files, %.1f MB: hook records, parse cache, run outputs, recommendations, archive)\n",
			home, files, float64(size)/(1<<20))
		for _, b := range backups {
			fmt.Fprintf(stdout, "%s: delete\n", b)
		}
		fmt.Fprintln(stdout, "No new settings.json backup is made. Your transcripts in ~/.claude, ~/.codex and ~/.gemini are not touched.")
	}
	if !*write {
		fmt.Fprintln(stdout, "\nNothing changed. Run again with --write to apply.")
		return nil
	}
	if hooksChanged {
		if len(orig) > 0 && !*purge {
			backup := settingsPath + ".cbrt-backup-" + time.Now().UTC().Format("20060102T150405Z")
			if err := os.WriteFile(backup, orig, 0o600); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "backup:", backup)
		}
		if err := os.MkdirAll(claudeDir, 0o700); err != nil {
			return err
		}
		if err := state.WriteFileAtomic(settingsPath, out); err != nil {
			return err
		}
	}
	if install {
		rp, note, err := review.InstallRubric(home, time.Now())
		if err != nil {
			return err
		}
		if note != "" {
			fmt.Fprintln(stdout, note)
		}
		for _, name := range toInstall {
			if _, _, err := skills.Install(claudeDir, name, exe, rp); err != nil {
				return err
			}
		}
	}
	for _, d := range removeSkills {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
	}
	if *purge {
		for _, b := range backups {
			if err := os.Remove(b); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := purgeHome(home); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "done. Hooks, skill, analyzer data and settings backups are gone.")
		return nil
	}
	if install {
		fmt.Fprintln(stdout, "done. New Claude Code sessions pick up the hooks; run /cbrt-multi-session-review weekly.")
		if slices.Contains(extra, "cbrt-session-review") {
			fmt.Fprintln(stdout, "Run /cbrt-session-review at the end of a session to review just that session.")
		}
	} else {
		fmt.Fprintln(stdout, "done. Collected data is kept in "+home+" (uninstall --purge --write deletes it).")
	}
	return nil
}

// optionalSkills parses --with: comma-separated optional skill names, or
// "all".
func optionalSkills(with string) ([]string, error) {
	known := skills.Optional()
	if with == "" {
		return nil, nil
	}
	if with == "all" {
		return known, nil
	}
	var names []string
	for n := range strings.SplitSeq(with, ",") {
		n = strings.TrimSpace(n)
		if !slices.Contains(known, n) {
			return nil, fmt.Errorf("--with: unknown skill %q (available: %s, or all)", n, strings.Join(known, ", "))
		}
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names, nil
}

// purgeable are the top-level names the analyzer creates in its home. Purge
// refuses a folder holding anything else: CBRT_ANALYZE_HOME may point at
// the wrong place, and deleting someone's folder is not undoable.
var purgeable = map[string]bool{
	"registry": true, "cache": true, "out": true, "state": true, "archive": true, "selftest": true,
	"RUBRIC.md": true, "RUBRIC.local.md": true, "recommendations.md": true,
	"config.json": true, "redaction_rules.json": true, "exclusions.json": true, "task_overrides.json": true,
}

// isAtomicTemp matches the temp files atomic writes leave behind on a crash.
func isAtomicTemp(name string) bool {
	return strings.HasPrefix(name, ".tmp-") || (strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp"))
}

func checkPurgeable(home string) error {
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var foreign []string
	for _, e := range entries {
		if !purgeable[e.Name()] && !isAtomicTemp(e.Name()) {
			foreign = append(foreign, e.Name())
		}
	}
	if len(foreign) > 0 {
		return fmt.Errorf("%s holds entries cbrt did not create (%s): not deleting it. Remove them or check %s",
			home, strings.Join(foreign, ", "), analyze.HomeEnv)
	}
	return nil
}

func purgeHome(home string) error {
	if err := checkPurgeable(home); err != nil {
		return err
	}
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(home, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(home)
}

// dirUsage counts files and bytes under dir.
func dirUsage(dir string) (files int, size int64) {
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				files++
				size += info.Size()
			}
		}
		return nil
	})
	return files, size
}

// isTransient is transientBinary; tests replace it (they run from a go-build dir).
var isTransient = transientBinary

// transientBinary reports whether exe is a go run / go test build (a
// go-build directory, or anywhere under the temp directory).
func transientBinary(exe string) bool {
	p := strings.ToLower(filepath.ToSlash(exe))
	if strings.Contains(p, "/go-build") {
		return true
	}
	tmp := strings.TrimSuffix(strings.ToLower(filepath.ToSlash(os.TempDir())), "/")
	return tmp != "" && strings.HasPrefix(p, tmp+"/")
}

// marshalSettings writes settings.json with sorted keys and two-space indent;
// other keys keep their values exactly.
func marshalSettings(top map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range keys {
		var v bytes.Buffer
		if err := json.Indent(&v, top[k], "  ", "  "); err != nil {
			return nil, err
		}
		b.WriteString("  " + strconv.Quote(k) + ": ")
		b.Write(v.Bytes())
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}
