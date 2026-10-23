package hook

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

var fixedNow = time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

// fakeGit returns a fixed branch and counts its calls.
type fakeGit struct {
	mu    sync.Mutex
	dirs  []string
	info  *analyze.GitInfo
	err   error
	block bool
	// deadline seen by the last call, and whether it had one
	deadline    time.Time
	hasDeadline bool
}

func (f *fakeGit) run(ctx context.Context, dir string) (*analyze.GitInfo, error) {
	f.mu.Lock()
	f.dirs = append(f.dirs, dir)
	f.deadline, f.hasDeadline = ctx.Deadline()
	f.mu.Unlock()
	if f.block {
		select {} // ignores the context: Record must not wait for it
	}
	return f.info, f.err
}

func (f *fakeGit) seen() (dirs []string, deadline time.Time, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dirs...), f.deadline, f.hasDeadline
}

func record(t *testing.T, home, adapter, event, stdin string, opts Options) {
	t.Helper()
	if err := Record(home, adapter, event, strings.NewReader(stdin), opts); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func readAll(t *testing.T, home string) ([]analyze.Envelope, []string) {
	t.Helper()
	envs, problems, err := ReadSpool(home)
	if err != nil {
		t.Fatalf("ReadSpool: %v", err)
	}
	return envs, problems
}

func TestRecordEnvelope(t *testing.T) {
	home := t.TempDir()
	g := &fakeGit{info: &analyze.GitInfo{Branch: "feature/x", Commit: "abc123"}}
	local := time.Date(2026, 9, 14, 12, 30, 0, 0, time.FixedZone("x", 2*3600))
	stdin := "{\n  \"session\": \"s1\",\n  \"cwd\": \"/work/proj\"\n}\n"
	record(t, home, "tool", "Start", stdin, Options{Now: at(local), Git: g.run})

	envs, problems := readAll(t, home)
	if len(problems) != 0 || len(envs) != 1 {
		t.Fatalf("envs %d problems %v", len(envs), problems)
	}
	e := envs[0]
	if e.V != analyze.EnvelopeVersion || e.AdapterID != "tool" || e.Event != "Start" {
		t.Errorf("envelope header %+v", e)
	}
	if !e.RecordedAt.Equal(local) || e.RecordedAt.Location() != time.UTC {
		t.Errorf("RecordedAt %v, want %v in UTC", e.RecordedAt, local)
	}
	if e.Stdin != stdin {
		t.Errorf("multi-line stdin not preserved: %q", e.Stdin)
	}
	if e.Git == nil || *e.Git != *g.info {
		t.Errorf("Git %+v", e.Git)
	}
	if len(g.dirs) != 1 || g.dirs[0] != "/work/proj" {
		t.Errorf("git ran in %v", g.dirs)
	}
	if filepath.Dir(e.File) != analyze.SpoolDir(home) || !strings.HasSuffix(e.File, ".json") || SpoolName(e) != filepath.Base(e.File) {
		t.Errorf("File %q", e.File)
	}
}

func TestRecordInvalidUTF8(t *testing.T) {
	home := t.TempDir()
	raw := "{\"text\":\"a\xffb\"}\n\xc3"
	record(t, home, "tool", "Ask", raw, Options{Now: at(fixedNow)})
	envs, _ := readAll(t, home)
	if len(envs) != 1 {
		t.Fatalf("%d envelopes", len(envs))
	}
	if want := "{\"text\":\"a�b\"}\n�"; envs[0].Stdin != want {
		t.Errorf("Stdin %q, want %q", envs[0].Stdin, want)
	}
}

func TestRecordStdinCap(t *testing.T) {
	home := t.TempDir()
	record(t, home, "tool", "Ask", strings.Repeat("x", 100), Options{Now: at(fixedNow), MaxStdin: 10})
	envs, _ := readAll(t, home)
	if len(envs) != 1 || envs[0].Stdin != strings.Repeat("x", 10) {
		t.Fatalf("over-cap stdin: %+v", envs)
	}
}

func TestRecordWorkspacePaths(t *testing.T) {
	home := t.TempDir()
	g := &fakeGit{info: &analyze.GitInfo{Branch: "main", Commit: "c"}}
	record(t, home, "tool", "Start", `{"workspacePaths":["/a","/b"]}`, Options{Now: at(fixedNow), Git: g.run})
	record(t, home, "tool", "Start", `{"cwd":"","workspacePaths":"not a list"}`, Options{Now: at(fixedNow), Git: g.run})
	record(t, home, "tool", "Start", `not json`, Options{Now: at(fixedNow), Git: g.run})
	if len(g.dirs) != 1 || g.dirs[0] != "/a" {
		t.Errorf("git ran in %v, want only /a", g.dirs)
	}
}

func TestRecordGitFailures(t *testing.T) {
	cases := map[string]*fakeGit{
		"error":   {err: errors.New("not a repository")},
		"nil":     {},
		"absent":  {err: &os.PathError{Op: "exec", Path: "git", Err: os.ErrNotExist}},
		"timeout": {block: true},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			record(t, home, "tool", "End", `{"cwd":"/w"}`, Options{Now: at(fixedNow), Git: g.run})
			envs, _ := readAll(t, home)
			if len(envs) != 1 || envs[0].Git != nil {
				t.Fatalf("want one envelope with nil Git, got %+v", envs)
			}
			if _, _, ok := g.seen(); !ok {
				t.Fatal("git ran without a deadline")
			}
		})
	}
}

// The git budget is one deadline at most GitBudget after the call started.
func TestRecordGitDeadline(t *testing.T) {
	g := &fakeGit{info: &analyze.GitInfo{Branch: "b", Commit: "c"}}
	start := time.Now()
	record(t, t.TempDir(), "tool", "Start", `{"cwd":"/w"}`, Options{Git: g.run})
	end := time.Now()
	_, deadline, ok := g.seen()
	set := deadline.Add(-GitBudget) // when the budget started
	if !ok || set.Before(start) || set.After(end) {
		t.Fatalf("deadline %v for a call during [%v, %v] (budget %v)", deadline, start, end, GitBudget)
	}
}

func TestRecordPermissionRequestSkipsGit(t *testing.T) {
	home := t.TempDir()
	g := &fakeGit{info: &analyze.GitInfo{Branch: "b", Commit: "c"}}
	record(t, home, "tool", "PermissionRequest", `{"cwd":"/w"}`, Options{Now: at(fixedNow), Git: g.run})
	envs, _ := readAll(t, home)
	if len(g.dirs) != 0 || len(envs) != 1 || envs[0].Git != nil {
		t.Fatalf("git ran %v, envs %+v", g.dirs, envs)
	}
}

func TestRecordRealGitOutsideRepo(t *testing.T) {
	// The real runner on a directory that is not a repository: Git is nil
	// whether or not git is installed.
	home := t.TempDir()
	dir := t.TempDir()
	b, _ := json.Marshal(map[string]string{"cwd": dir})
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	record(t, home, "tool", "Start", string(b), Options{Now: at(fixedNow)})
	envs, _ := readAll(t, home)
	if len(envs) != 1 || envs[0].Git != nil {
		t.Fatalf("%+v", envs)
	}
}

func TestRecordConcurrent(t *testing.T) {
	home := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stdin := `{"n":` + string(rune('0'+i)) + `}`
			errs <- Record(home, "tool", "Ask", strings.NewReader(stdin), Options{Now: at(fixedNow)})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	envs, problems := readAll(t, home)
	if len(envs) != 2 || len(problems) != 0 || envs[0].File == envs[1].File {
		t.Fatalf("envs %+v problems %v", envs, problems)
	}
	got := []string{envs[0].Stdin, envs[1].Stdin}
	sort.Strings(got)
	if got[0] != `{"n":0}` || got[1] != `{"n":1}` {
		t.Errorf("stdin %v", got)
	}
}

func TestReadSpoolProblems(t *testing.T) {
	home := t.TempDir()
	record(t, home, "tool", "Ask", "{}", Options{Now: at(fixedNow)})
	dir := analyze.SpoolDir(home)
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("aaaa.json.tmp", `{"v":1`)
	write("bbbb.json", `garbage`)
	write("cccc.json", `{"v":99,"adapter_id":"tool","event":"Ask","recorded_at":"2026-09-01T00:00:00Z"}`)
	write("notes.txt", `ignored`)
	envs, problems := readAll(t, home)
	if len(envs) != 1 {
		t.Errorf("%d envelopes, want 1", len(envs))
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"aaaa.json.tmp", "bbbb.json", "cccc.json: unsupported envelope version 99"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems %q lack %q", joined, want)
		}
	}
	if strings.Contains(joined, "notes.txt") || strings.Contains(joined, dir) {
		t.Errorf("problems %q", joined)
	}
}

func TestReadSpoolMissingHome(t *testing.T) {
	envs, problems, err := ReadSpool(filepath.Join(t.TempDir(), "nope"))
	if err != nil || envs != nil || problems != nil {
		t.Fatal(envs, problems, err)
	}
}

// spoolNames lists the spool directory.
func spoolNames(t *testing.T, home string) []string {
	t.Helper()
	ents, err := os.ReadDir(analyze.SpoolDir(home))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func stdins(envs []analyze.Envelope) []string {
	var s []string
	for _, e := range envs {
		s = append(s, e.Stdin)
	}
	sort.Strings(s)
	return s
}

func TestCompact(t *testing.T) {
	home := t.TempDir()
	times := map[string]time.Time{
		"aug-a": time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		"aug-b": time.Date(2026, 8, 31, 23, 59, 59, 999, time.UTC),
		// 23:30 on Aug 31 in UTC-2 is Sep 1 in UTC: September.
		"sep-a": time.Date(2026, 8, 31, 23, 30, 0, 0, time.FixedZone("w", -2*3600)),
		"jul-a": time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
		"oct-a": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	for s, tm := range times {
		record(t, home, "tool", "Ask", s, Options{Now: at(tm)})
	}
	before, _ := readAll(t, home)

	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for i := range 2 {
		if err := Compact(home, now); err != nil {
			t.Fatalf("Compact run %d: %v", i, err)
		}
	}
	if names := spoolNames(t, home); len(names) != 1 {
		t.Errorf("spool after compaction: %v, want only October's file", names)
	}
	for _, m := range []string{"2026-07", "2026-08", "2026-09"} {
		if _, err := os.Stat(filepath.Join(analyze.RegistryDir(home), m+".jsonl")); err != nil {
			t.Errorf("month %s: %v", m, err)
		}
	}
	after, problems := readAll(t, home)
	if len(problems) != 0 {
		t.Errorf("problems %v", problems)
	}
	if got, want := stdins(after), stdins(before); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after compaction %v, before %v", got, want)
	}
	// Identity survives compaction.
	names := map[string]bool{}
	for _, e := range before {
		names[SpoolName(e)] = true
	}
	for _, e := range after {
		if !names[SpoolName(e)] {
			t.Errorf("envelope %q lost its spool name (%s)", e.Stdin, e.File)
		}
	}

	// A later run in a new month compacts October into its own file.
	if err := Compact(home, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	final, _ := readAll(t, home)
	if len(final) != len(before) {
		t.Errorf("%d envelopes after the second month, want %d", len(final), len(before))
	}
}

// An interrupted compaction (month written, spool files not yet deleted)
// neither duplicates on read nor on the next Compact.
func TestCompactInterrupted(t *testing.T) {
	home := t.TempDir()
	record(t, home, "tool", "Ask", "one", Options{Now: at(time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))})
	record(t, home, "tool", "Ask", "two", Options{Now: at(time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC))})
	spoolDir := analyze.SpoolDir(home)
	saved := map[string][]byte{}
	for _, n := range spoolNames(t, home) {
		b, err := os.ReadFile(filepath.Join(spoolDir, n))
		if err != nil {
			t.Fatal(err)
		}
		saved[n] = b
	}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := Compact(home, now); err != nil {
		t.Fatal(err)
	}
	for n, b := range saved { // simulate the crash before deletion
		if err := os.WriteFile(filepath.Join(spoolDir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if envs, _ := readAll(t, home); len(envs) != 2 {
		t.Fatalf("read %d envelopes, want 2", len(envs))
	}
	if err := Compact(home, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(analyze.RegistryDir(home), "2026-08.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Errorf("month file has %d lines, want 2", n)
	}
	if names := spoolNames(t, home); len(names) != 0 {
		t.Errorf("spool %v, want empty", names)
	}
}

func TestCompactLeavesInvalidFiles(t *testing.T) {
	home := t.TempDir()
	record(t, home, "tool", "Ask", "x", Options{Now: at(time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))})
	bad := filepath.Join(analyze.SpoolDir(home), "bad.json")
	if err := os.WriteFile(bad, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Compact(home, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Errorf("invalid spool file removed: %v", err)
	}
}

func TestCompactLocked(t *testing.T) {
	home := t.TempDir()
	record(t, home, "tool", "Ask", "x", Options{Now: at(time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))})
	lockPath := filepath.Join(analyze.RegistryDir(home), "compact.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Compact(home, fixedNow); err == nil {
		t.Fatal("Compact ran under another run's lock")
	}
	old := time.Now().Add(-2 * staleLock)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := Compact(home, fixedNow); err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock left behind: %v", err)
	}
}

func TestPrune(t *testing.T) {
	home := t.TempDir()
	for _, m := range []time.Month{6, 7, 8} {
		record(t, home, "tool", "Ask", m.String(), Options{Now: at(time.Date(2026, m, 10, 0, 0, 0, 0, time.UTC))})
	}
	if err := Compact(home, fixedNow); err != nil {
		t.Fatal(err)
	}
	// July ends Aug 1: pruned at Aug 1; August is not.
	if err := Prune(home, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	envs, _ := readAll(t, home)
	if got := stdins(envs); len(got) != 1 || got[0] != "August" {
		t.Errorf("after prune: %v", got)
	}
	if err := Prune(filepath.Join(home, "missing"), fixedNow); err != nil {
		t.Errorf("missing home: %v", err)
	}
}
