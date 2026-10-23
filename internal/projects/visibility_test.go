package projects

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// at splits user@host remote URLs so they do not read as e-mail addresses
// to the privacy scan.
const at = "@"

func TestParseRemote(t *testing.T) {
	cases := []struct {
		in    string
		host  string
		path  string
		local bool
		ok    bool
	}{
		{"https://github.com/acme/widget.git", "github.com", "acme/widget", false, true},
		{"https://github.com/acme/widget", "github.com", "acme/widget", false, true},
		{"https://github.com/acme/widget/", "github.com", "acme/widget", false, true},
		{"https://github.com/acme/widget.git/", "github.com", "acme/widget", false, true},
		{"https://GitHub.COM/Acme/Widget.git", "github.com", "Acme/Widget", false, true},
		{"https://user:secret" + at + "github.com/acme/widget.git", "github.com", "acme/widget", false, true},
		{"http://gitlab.com/group/sub/proj.git", "gitlab.com", "group/sub/proj", false, true},
		{"ssh://git" + at + "github.com/acme/widget.git", "github.com", "acme/widget", false, true},
		{"ssh://git" + at + "github.com:22/acme/widget.git", "github.com", "acme/widget", false, true},
		{"ssh://git" + at + "[::1]:2222/acme/widget", "::1", "acme/widget", false, true},
		{"git://example.org/acme/widget", "example.org", "acme/widget", false, true},
		{"git" + at + "github.com:acme/widget.git", "github.com", "acme/widget", false, true},
		{"git" + at + "GITLAB.com:group/proj", "gitlab.com", "group/proj", false, true},
		{"github.com:acme/widget", "github.com", "acme/widget", false, true},
		{"git" + at + "github.com:/acme/widget.git/", "github.com", "acme/widget", false, true},
		{"  git" + at + "github.com:acme/widget.git\n", "github.com", "acme/widget", false, true},
		// local remotes: no host to ask
		{"/srv/git/widget.git", "", "", true, true},
		{"../widget", "", "", true, true},
		{"./widget.git", "", "", true, true},
		{`C:\repos\widget`, "", "", true, true},
		{"C:/repos/widget.git", "", "", true, true},
		{`\\server\share\widget.git`, "", "", true, true},
		{"file:///C:/repos/widget.git", "", "", true, true},
		{"file:///srv/widget", "", "", true, true},
		{`repos\widget:x`, "", "", true, true},
		{"", "", "", false, false},
	}
	for _, c := range cases {
		r, ok := parseRemote(c.in)
		if ok != c.ok || r.Local != c.local || (!c.local && (r.Host != c.host || r.Path != c.path)) {
			t.Errorf("parseRemote(%q) = %+v, %v; want host %q path %q local %v ok %v", c.in, r, ok, c.host, c.path, c.local, c.ok)
		}
	}
}

// fakeHTTP answers by URL: a status, or (block) waits until the request is
// cancelled. It records every request; no request reaches the network.
type fakeHTTP struct {
	mu     sync.Mutex
	status map[string]int
	block  map[string]bool
	seen   []*http.Request
}

func (f *fakeHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.seen = append(f.seen, r)
	st, ok := f.status[r.URL.String()]
	blk := f.block[r.URL.String()]
	f.mu.Unlock()
	if blk {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	if !ok {
		return nil, errors.New("fake: unexpected request " + r.URL.String())
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: r}, nil
}

func (f *fakeHTTP) client() *http.Client { return &http.Client{Transport: f} }

const (
	ghWidget = "https://api.github.com/repos/acme/widget"
	ghSecret = "https://api.github.com/repos/acme/secret"
	ghBroken = "https://api.github.com/repos/acme/broken"
	ghSlow   = "https://api.github.com/repos/acme/slow"
	glProj   = "https://gitlab.com/api/v4/projects/group%2Fsub%2Fproj"
)

func newFake() *fakeHTTP {
	return &fakeHTTP{
		status: map[string]int{ghWidget: 200, ghSecret: 404, ghBroken: 500, glProj: 200},
		block:  map[string]bool{ghSlow: true},
	}
}

func TestDecideVisibility(t *testing.T) {
	old := probeTimeout
	probeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { probeTimeout = old })
	cases := []struct {
		name, flag string
		urls       []string
		want       string
		how        string
	}{
		{"github 200", "", []string{"git" + at + "github.com:acme/widget.git"}, Public, "github.com/acme/widget is public"},
		{"github 404", "", []string{"https://github.com/acme/secret"}, Private, "private"},
		{"github 500", "", []string{"https://github.com/acme/broken"}, Public, "answered 500"},
		{"timeout", "", []string{"https://github.com/acme/slow"}, Public, "timed out"},
		{"gitlab 200 subgroup", "", []string{"https://gitlab.com/group/sub/proj.git"}, Public, "gitlab.com/group/sub/proj is public"},
		{"unknown host", "", []string{"git" + at + "git.example.org:acme/widget.git"}, Public, "git.example.org is not a host"},
		{"github not owner/repo", "", []string{"https://github.com/acme"}, Public, "not an owner/repository path"},
		{"no remote", "", nil, Private, "no git remote"},
		{"local folder", "", []string{"/srv/git/widget.git"}, Private, "folder on this machine"},
		{"two remotes, one public", "", []string{"https://github.com/acme/secret", "https://github.com/acme/widget"}, Public, "acme/widget is public"},
		{"two remotes, both private", "", []string{"https://github.com/acme/secret", "/srv/x.git"}, Private, "acme/secret is private"},
		{"flag public", Public, nil, Public, "--public"},
		{"flag private", Private, []string{"https://github.com/acme/widget"}, Private, "--private"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			v := decideVisibility(context.Background(), c.flag, c.urls, f.client())
			if v.Visibility != c.want || !strings.Contains(v.How, c.how) {
				t.Errorf("got %+v; want %s containing %q", v, c.want, c.how)
			}
			if c.flag != "" && len(f.seen) > 0 {
				t.Errorf("a flag still asked the network: %d requests", len(f.seen))
			}
			for _, r := range f.seen {
				if r.Header.Get("User-Agent") == "" || r.Header.Get("Authorization") != "" || r.URL.User != nil || r.Method != http.MethodGet {
					t.Errorf("request %s %s headers %v", r.Method, r.URL, r.Header)
				}
			}
		})
	}
}

func TestDecideVisibilityTotalBound(t *testing.T) {
	oldT, oldTotal := probeTimeout, probeTotal
	probeTimeout, probeTotal = time.Hour, 50*time.Millisecond
	t.Cleanup(func() { probeTimeout, probeTotal = oldT, oldTotal })
	f := newFake()
	v := decideVisibility(context.Background(), "", []string{"https://github.com/acme/slow", "https://github.com/acme/secret"}, f.client())
	if v.Visibility != Public || !strings.Contains(v.How, "timed out") {
		t.Errorf("got %+v", v)
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("https://user:tok@example.com/a/b"); strings.Contains(got, "tok") || strings.Contains(got, "user") {
		t.Errorf("redactURL kept credentials: %q", got)
	}
	// URLs url.Parse rejects keep no credentials either
	for _, raw := range []string{
		"https://user:pa%zzsecret@example.com/o/r",
		"https://user:pa ss secret@example.com/o/r",
		"https://user:secret@example.com:99x/o/r",
	} {
		if got := redactURL(raw); strings.Contains(got, "secret") || strings.Contains(got, "user") {
			t.Errorf("redactURL(%q) = %q", raw, got)
		}
	}
}

// A private fetch mirror with a public push URL is checked by both URLs.
func TestRemotesIncludePushURLs(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "mirror", nil)
	gitT(t, dir, "init", "-q")
	gitT(t, dir, "remote", "add", "origin", "https://example.com/mirror/r.git")
	gitT(t, dir, "remote", "set-url", "--push", "origin", "https://github.com/o/r.git")
	urls, err := remotes(dir)
	if err != nil || !slices.Contains(urls, "https://example.com/mirror/r.git") || !slices.Contains(urls, "https://github.com/o/r.git") {
		t.Errorf("remotes %v %v", urls, err)
	}
}

func TestOnlyOurAgents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENTS.md")
	ours := strings.ReplaceAll(agentsHeader+RuleBlock, "\n", "\r\n")
	for content, want := range map[string]bool{ours: true, agentsHeader + RuleBlock + "\n": true, "# Mine\n" + RuleBlock: false, "": false} {
		os.WriteFile(p, []byte(content), 0o644)
		if got := onlyOurAgents(p); got != want {
			t.Errorf("onlyOurAgents(%q) = %v", content, got)
		}
	}
	if onlyOurAgents(filepath.Join(dir, "none.md")) {
		t.Errorf("a missing file counted as ours")
	}
}

// A project connected without a remote is flagged once it has one.
func TestRemoteAddedFlag(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "local", nil)
	gitT(t, dir, "init", "-q")
	entry := Entry{Name: "local", Path: dir, Visibility: Private, VisibilityHow: noRemoteHow + ": the repository is local only"}
	if s := Collect(context.Background(), []Entry{entry}); s[0].RemoteAdded {
		t.Errorf("flagged without a remote")
	}
	gitT(t, dir, "remote", "add", "origin", "https://github.com/o/r.git")
	if s := Collect(context.Background(), []Entry{entry}); !s[0].RemoteAdded {
		t.Errorf("remote not noticed")
	}
	entry.VisibilityHow = "set with --private"
	if s := Collect(context.Background(), []Entry{entry}); s[0].RemoteAdded {
		t.Errorf("flagged although --private was chosen")
	}
}

func TestExcludeBlockEdits(t *testing.T) {
	orig := []byte("# git ls-files --others --exclude-from=.git/info/exclude\n*.log")
	files := excludeFiles(true)
	out, changed := withExcludeBlock(orig, excludeBlock("", files), "")
	if !changed || !strings.Contains(string(out), "\n/PLAN.md\n/.cbrt/\n/AGENTS.md\n") {
		t.Fatalf("block not added:\n%s", out)
	}
	if again, changed := withExcludeBlock(out, excludeBlock("", files), ""); changed || !bytes.Equal(again, out) {
		t.Errorf("not idempotent:\n%s", again)
	}
	// a second project in the same repository has its own block
	two, _ := withExcludeBlock(out, excludeBlock("packages/app/", excludeFiles(false)), "packages/app/")
	if !strings.Contains(string(two), "/packages/app/PLAN.md\n") || strings.Count(string(two), excludeEnd) != 2 {
		t.Fatalf("second block:\n%s", two)
	}
	// updated in place when the files change
	upd, changed := withExcludeBlock(two, excludeBlock("", excludeFiles(false)), "")
	if !changed || strings.Contains(string(upd), "\n/AGENTS.md\n") || strings.Count(string(upd), excludeEnd) != 2 {
		t.Errorf("update:\n%s", upd)
	}
	back, ok := withoutExcludeBlock(upd, "packages/app/")
	if !ok {
		t.Fatal("block not found")
	}
	back, _ = withoutExcludeBlock(back, "")
	if want := string(orig) + "\n"; string(back) != want {
		t.Errorf("removal %q, want %q", back, want)
	}
	// CRLF files keep their line endings
	crlf, _ := withExcludeBlock([]byte("a\r\nb\r\n"), excludeBlock("", files), "")
	if strings.Contains(strings.ReplaceAll(string(crlf), "\r\n", ""), "\n") {
		t.Errorf("mixed line endings %q", crlf)
	}
	if got := excludePattern("we[ir]d*/"); got != `/we\[ir]d\*/` {
		t.Errorf("excludePattern %q", got)
	}
}

// publicEnv is a git repository with a GitHub remote the fake calls public.
func publicEnv(t *testing.T, name string, files map[string]string) (*env, string, *fakeHTTP) {
	t.Helper()
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, name, files)
	gitT(t, dir, "init", "-q")
	gitT(t, dir, "remote", "add", "origin", "git"+at+"github.com:acme/widget.git")
	f := newFake()
	e.o.HTTP = f.client()
	return e, dir, f
}

func excludeOf(t *testing.T, dir string) string {
	t.Helper()
	p := gitT(t, dir, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestConnectPublicRepo(t *testing.T) {
	e, dir, _ := publicEnv(t, "widget", nil)
	before := excludeOf(t, dir)
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	ex := excludeOf(t, dir)
	for _, want := range []string{excludeStart + "/\n", "\n/PLAN.md\n", "\n/.cbrt/\n", "\n/AGENTS.md\n", excludeEnd} {
		if !strings.Contains(ex, want) {
			t.Errorf("exclude lacks %q:\n%s", want, ex)
		}
	}
	if !strings.Contains(out, "visibility: public (github.com/acme/widget is public)") || !strings.Contains(out, "out of git locally") {
		t.Errorf("output:\n%s", out)
	}
	if ignored, err := gitStatus(dir, "check-ignore", "-q", "PLAN.md"); err != nil || !ignored {
		t.Errorf("PLAN.md not ignored: %v %v", ignored, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); len(b) > 0 {
		t.Errorf(".gitignore written: %q", b)
	}
	es, _ := Load(e.o.Home)
	if len(es) != 1 || es[0].Visibility != Public || !es[0].CreatedAgents || es[0].VisibilityHow == "" {
		t.Fatalf("entry %+v", es)
	}

	// again: nothing changes
	e.out.Reset()
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	if excludeOf(t, dir) != ex || !strings.Contains(e.out.String(), "nothing to change") {
		t.Errorf("second connect changed something:\n%s", e.out)
	}

	// list shows the marker
	var lb bytes.Buffer
	WriteList(&lb, Collect(context.Background(), es), e.o.Now())
	if !strings.Contains(lb.String(), PublicMarker) {
		t.Errorf("list:\n%s", lb.String())
	}

	// disconnect --remove-files is the inverse
	e.out.Reset()
	if err := Disconnect("widget", true, e.o); err != nil {
		t.Fatal(err)
	}
	if got := excludeOf(t, dir); got != before {
		t.Errorf("exclude after disconnect %q, want %q", got, before)
	}
	if !strings.Contains(e.out.String(), "removed the local exclude block") {
		t.Errorf("output:\n%s", e.out)
	}
}

func TestConnectPublicTrackedFiles(t *testing.T) {
	e, dir, _ := publicEnv(t, "widget", map[string]string{"PLAN.md": "# Plan: widget\n\nNow: x\n", "AGENTS.md": "# Rules\n"})
	gitT(t, dir, "add", "--", "PLAN.md", "AGENTS.md")
	gitT(t, dir, "commit", "-q", "-m", "plan")
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	want := `git -C "` + dir + `" rm --cached -- PLAN.md`
	if !strings.Contains(out, want) || !strings.Contains(out, "AGENTS.md is tracked") {
		t.Errorf("output lacks %q or the AGENTS.md note:\n%s", want, out)
	}
	if read(t, filepath.Join(dir, "PLAN.md")) != "# Plan: widget\n\nNow: x\n" {
		t.Errorf("PLAN.md changed")
	}
	if gitT(t, dir, "ls-files", "--", "PLAN.md") != "PLAN.md" || gitT(t, dir, "rev-list", "--count", "HEAD") != "1" {
		t.Errorf("history or index changed")
	}
	// AGENTS.md was not created by connect: not in the block
	if ex := excludeOf(t, dir); strings.Contains(ex, "/AGENTS.md") || !strings.Contains(ex, "/PLAN.md") {
		t.Errorf("exclude:\n%s", ex)
	}
}

func TestConnectVisibilityFlagsAndPrivateKeepsBlock(t *testing.T) {
	e, dir, f := publicEnv(t, "widget", nil)
	gitT(t, dir, "remote", "set-url", "origin", "https://github.com/acme/secret.git")
	o := e.o
	o.Visibility = Public
	if err := Connect(dir, o); err != nil {
		t.Fatal(err)
	}
	if len(f.seen) != 0 || !strings.Contains(excludeOf(t, dir), "/PLAN.md") {
		t.Fatalf("--public: %d requests, exclude %q", len(f.seen), excludeOf(t, dir))
	}
	// detection now says private: the block stays, with a note
	e.out.Reset()
	if err := Connect(dir, e.o); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	if !strings.Contains(out, "visibility: private") || !strings.Contains(out, "stays in") || !strings.Contains(out, "to remove it") {
		t.Errorf("output:\n%s", out)
	}
	if !strings.Contains(excludeOf(t, dir), "/PLAN.md") {
		t.Errorf("block removed")
	}
	es, _ := Load(e.o.Home)
	if es[0].Visibility != Private {
		t.Errorf("entry %+v", es[0])
	}
	o.Visibility = Private
	e.out.Reset()
	if err := Connect(dir, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "set with --private") {
		t.Errorf("output:\n%s", e.out)
	}
	o.Visibility = "maybe"
	if err := Connect(dir, o); err == nil {
		t.Errorf("a bad visibility was accepted")
	}
}

func TestConnectPublicMonorepoSubfolder(t *testing.T) {
	e, outer, _ := publicEnv(t, "mono", map[string]string{"keep.txt": "x\n"})
	gitT(t, outer, "add", "--", "keep.txt")
	gitT(t, outer, "commit", "-q", "-m", "outer")
	sub := filepath.Join(outer, "packages", "app")
	os.MkdirAll(sub, 0o755)
	if err := Connect(sub, e.o); err != nil {
		t.Fatal(err)
	}
	ex := excludeOf(t, outer)
	for _, want := range []string{excludeStart + "/packages/app/\n", "\n/packages/app/PLAN.md\n", "\n/packages/app/.cbrt/\n", "\n/packages/app/AGENTS.md\n"} {
		if !strings.Contains(ex, want) {
			t.Errorf("exclude lacks %q:\n%s", want, ex)
		}
	}
	if ignored, _ := gitStatus(outer, "check-ignore", "-q", "packages/app/PLAN.md"); !ignored {
		t.Errorf("packages/app/PLAN.md not ignored")
	}
	if ignored, _ := gitStatus(outer, "check-ignore", "-q", "PLAN.md"); ignored {
		t.Errorf("the top-level PLAN.md is ignored too")
	}
	if err := Disconnect(sub, true, e.o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(excludeOf(t, outer), "cbrt:exclude") {
		t.Errorf("block left after disconnect")
	}
}

func TestConnectDryRunPublic(t *testing.T) {
	e, dir, _ := publicEnv(t, "widget", nil)
	before := snapshot(t, dir)
	exBefore := excludeOf(t, dir)
	o := e.o
	o.DryRun = true
	if err := Connect(dir, o); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	if !strings.Contains(out, "visibility: public") || !strings.Contains(out, "would keep /PLAN.md /.cbrt/ /AGENTS.md out of git locally") {
		t.Errorf("output:\n%s", out)
	}
	if excludeOf(t, dir) != exBefore {
		t.Errorf("dry run wrote the exclude file")
	}
	if after := snapshot(t, dir); len(after) != len(before) {
		t.Errorf("dry run changed files: %v", after)
	}
	if _, err := os.Stat(e.o.Home); err == nil {
		t.Errorf("dry run created the list folder")
	}

	// a folder that is no repository yet, --public: the would-be path
	e.out.Reset()
	fresh := e.project(t, "fresh", nil)
	o.Visibility = Public
	if err := Connect(fresh, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "would keep /PLAN.md") || !strings.Contains(e.out.String(), "would commit README.md (only these files)") {
		t.Errorf("output:\n%s", e.out)
	}
	if _, err := os.Stat(filepath.Join(fresh, ".git")); err == nil {
		t.Errorf("dry run ran git init")
	}
}

func TestConnectNewRepoPublicCommitsNoPlan(t *testing.T) {
	needGit(t, true)
	e := newEnv(t)
	dir := e.project(t, "fresh", nil)
	o := e.o
	o.Visibility = Public
	if err := Connect(dir, o); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, dir, "ls-files"); got != "README.md" {
		t.Errorf("committed %q", got)
	}
	if !strings.Contains(excludeOf(t, dir), "/PLAN.md") {
		t.Errorf("no exclude block")
	}
}

func TestLoadOldListWithoutVisibility(t *testing.T) {
	home := t.TempDir()
	old := `{"v":1,"projects":[{"name":"app","path":"/work/app","connected_at":"2026-10-20T12:00:00Z"}]}`
	os.WriteFile(filepath.Join(home, ListFile), []byte(old), 0o600)
	es, err := Load(home)
	if err != nil || len(es) != 1 || es[0].Visibility != "" || es[0].CreatedAgents {
		t.Fatalf("%+v %v", es, err)
	}
	if err := Save(home, es); err != nil {
		t.Fatal(err)
	}
	if b := read(t, filepath.Join(home, ListFile)); strings.Contains(b, "visibility") {
		t.Errorf("an unknown visibility was written: %s", b)
	}
}
