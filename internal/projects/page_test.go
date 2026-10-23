package projects

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pageEnv connects a healthy project, then lists it with a missing folder,
// a folder without PLAN.md and one whose PLAN.md is unparsable.
func pageEnv(t *testing.T) []Entry {
	t.Helper()
	needGit(t, true)
	e := newEnv(t)
	good := e.project(t, "good", map[string]string{
		"README.md": "# good\n\nFast <b>notes</b>.\n",
		".cbrt/checks/20261001T000000Z-claude.md": "Verdict: off track\n",
		".cbrt/checks/20261019T000000Z-codex.md":  "Verdict: drifting\nreasons\n",
	})
	if err := Connect(good, e.o); err != nil {
		t.Fatal(err)
	}
	plan := "# Plan: good\n\nGoal: Fast <b>notes</b>.\n\nNow: sync\n\n## Core\n- [x] a — evidence: test\n- [>] sync\n- [ ] export\n- [~] parked\n## Later\n- [?] maybe\n- [ ] share\n"
	os.WriteFile(filepath.Join(good, "PLAN.md"), []byte(plan), 0o644)
	gitT(t, good, "add", "PLAN.md")
	gitT(t, good, "commit", "-q", "-m", "Plan the core milestone")

	broken := e.project(t, "broken", map[string]string{"PLAN.md": "no title here\n- [ ] x\n"})
	old := time.Now().Add(-48 * time.Hour) // last activity: older than good's commit
	os.Chtimes(filepath.Join(broken, "PLAN.md"), old, old)
	noplan := e.project(t, "noplan", nil)
	return []Entry{
		{Name: "good", Path: good},
		{Name: "gone", Path: filepath.Join(e.base, "work", "gone")},
		{Name: "broken", Path: broken, Visibility: Public},
		{Name: "noplan", Path: noplan},
	}
}

func get(t *testing.T, h http.Handler, method, host, path string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest(method, "http://"+host+path, nil)
	r.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b), w.Result().Header
}

func TestHandler(t *testing.T) {
	es := pageEnv(t)
	h := Handler(func() ([]Entry, error) { return es, nil }, nil, nil)

	code, body, hdr := get(t, h, "GET", "127.0.0.1:8091", "/")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type %q", ct)
	}
	if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP %q", csp)
	}
	for _, want := range []string{
		"<h2>good</h2>", "<h2>gone</h2>", "<h2>broken</h2>", `<span class="chip public" title="public repository: PLAN.md stays local (.git/info/exclude)">public (plan local)</span>`, "<h2>noplan</h2>",
		"folder missing", "no PLAN.md", "PLAN.md unparsable", `has no &#34;# Plan&#34; title line`,
		"Fast &lt;b&gt;notes&lt;/b&gt;.", // escaped, never markup
		`<span class="name" title="Core">Core</span><span class="count">1/3</span>`,
		`<span class="name" title="Later">Later</span><span class="count">0/2</span>`,
		`<li class="task"><span>▶ sync</span><form method="post" action="/api/park">`, `<span>export</span>`, `<span>[?] maybe</span></li>`,
		`<summary>1 more to do</summary><ul class="tasks"><li class="task"><span>share</span>`,
		`<summary>1 parked</summary><ul class="tasks"><li class="task parked"><span>parked</span>`, `<button class="ghost" title="mark it [ ] in PLAN.md again">unpark</button>`,
		`<select name="agent" aria-label="agent"><option>claude</option><option>codex</option><option>agy</option></select><button class="primary">Run check</button>`,
		"Plan the core milestone",
		`<span class="verdict drifting">drifting</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if i, j := strings.Index(body, "<span>share</span>"), strings.Index(body, "more to do"); i < 0 || i < j {
		t.Errorf("page shows more than 3 next tasks")
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") || strings.Contains(body, "<script") {
		t.Errorf("page makes external requests or runs script")
	}
	// The project with commits sorts first.
	if strings.Index(body, "<h2>good</h2>") > strings.Index(body, "<h2>broken") {
		t.Errorf("good is not first")
	}

	for _, host := range []string{"localhost:8091", "[::1]:8091"} {
		if code, _, _ := get(t, h, "GET", host, "/"); code != 200 {
			t.Errorf("%s: %d", host, code)
		}
	}
	for _, host := range []string{"evil.example", "evil.example:8091", "192.168.1.5:8091", "localhost.evil.example"} {
		if code, _, _ := get(t, h, "GET", host, "/"); code != http.StatusMisdirectedRequest {
			t.Errorf("foreign host %s: %d", host, code)
		}
	}
	if code, _, _ := get(t, h, "POST", "127.0.0.1", "/"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", code)
	}
	if code, _, _ := get(t, h, "GET", "127.0.0.1", "/x"); code != http.StatusNotFound {
		t.Errorf("/x: %d", code)
	}
}

func TestHandlerEmptyAndStale(t *testing.T) {
	h := Handler(func() ([]Entry, error) { return nil, nil }, nil, nil)
	if code, body, _ := get(t, h, "GET", "localhost", "/"); code != 200 || !strings.Contains(body, "No connected projects yet") {
		t.Errorf("%d %s", code, body)
	}
	es := pageEnv(t)
	later := func() time.Time { return time.Now().Add(30 * 24 * time.Hour) }
	h = Handler(func() ([]Entry, error) { return es, nil }, later, nil)
	if _, body, _ := get(t, h, "GET", "localhost", "/"); !strings.Contains(body, "stale: last commit") {
		t.Errorf("no stale flag")
	}
}

func TestWriteList(t *testing.T) {
	es := pageEnv(t)
	var buf bytes.Buffer
	if err := WriteList(&buf, Collect(context.Background(), es), time.Now()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "NAME") || !strings.HasPrefix(lines[1], "good ") {
		t.Fatalf("list:\n%s", buf.String())
	}
	if !strings.Contains(lines[1], "sync") || !strings.Contains(lines[1], "1/5 (20%)") || !strings.Contains(lines[1], "just now") {
		t.Errorf("good line %q", lines[1])
	}
	if !strings.Contains(buf.String(), "folder missing") {
		t.Errorf("missing flag absent:\n%s", buf.String())
	}
	buf.Reset()
	WriteList(&buf, nil, time.Now())
	if !strings.Contains(buf.String(), "no connected projects") {
		t.Errorf("empty list %q", buf.String())
	}
}

func TestWriteListCleansControlCharacters(t *testing.T) {
	ss := []Status{{Entry: Entry{Name: "x\x1b]0;title\x07"}, Plan: &Plan{Now: "a\tb\x1b[2Jc\nd"}}}
	var buf bytes.Buffer
	WriteList(&buf, ss, time.Now())
	if strings.ContainsAny(buf.String(), "\x1b\x07\t") || strings.Count(buf.String(), "\n") != 2 {
		t.Errorf("control characters reached the terminal: %q", buf.String())
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 5 * time.Minute: "5m ago", 3 * time.Hour: "3h ago",
		3 * 24 * time.Hour: "3d ago", 30 * 24 * time.Hour: "4w ago",
	} {
		if got := Age(now, now.Add(-d)); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestBucketWeeks(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	stamp := func(d time.Duration) string { return strconv.FormatInt(now.Add(-d).Unix(), 10) }
	got := bucketWeeks([]string{stamp(time.Hour), stamp(2 * day), stamp(8 * day), stamp(83 * day), stamp(90 * day), "x"}, now)
	want := make([]int, ActivityWeeks)
	want[ActivityWeeks-1], want[ActivityWeeks-2], want[0] = 2, 1, 1
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bucketWeeks = %v, want %v", got, want)
	}
}

func TestLineChart(t *testing.T) {
	c := lineChart([]int{0, 2, 4}, 100, 50, 5)
	if !c.Has || c.Line != "M5.0 45.0 L50.0 25.0 L95.0 5.0" || c.EndX != 95 || c.EndY != 5 {
		t.Errorf("chart %+v", c)
	}
	if !strings.HasPrefix(c.Area, c.Line) || !strings.HasSuffix(c.Area, "L95.0 50.0 L5.0 50.0 Z") {
		t.Errorf("area %q", c.Area)
	}
	if flat := lineChart([]int{0, 0}, 100, 50, 5); flat.Has {
		t.Errorf("an all-zero series has points")
	}
	if one := lineChart([]int{3}, 100, 50, 5); one.Line != "" {
		t.Errorf("one point drew a line: %+v", one)
	}
}

func TestSummarize(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	weeks := func(prev, last int) []int {
		w := make([]int, ActivityWeeks)
		w[ActivityWeeks-2], w[ActivityWeeks-1] = prev, last
		return w
	}
	cs := []card{{Done: 3, Total: 4, Active: true, Verdict: OnTrack}, {Done: 1, Total: 6, Verdict: Drifting}}
	ss := []Status{{Weekly: weeks(4, 6), CommitTime: now}, {Weekly: weeks(1, 0), CommitTime: now.Add(-30 * 24 * time.Hour)}}
	sm := summarize(cs, ss, now)
	if sm.Projects != 2 || sm.Done != 4 || sm.Total != 10 || sm.Percent != 40 || sm.Active != 1 || sm.Stale != 1 ||
		sm.Checked != 2 || sm.OnTrack != 1 || sm.Commits != 11 || sm.LastWeek != 6 || sm.Trend != "+20%" || !sm.Activity.Has {
		t.Errorf("summary %+v", sm)
	}
	if sm := summarize(nil, nil, now); sm.Trend != "" || sm.Activity.Has || len(sm.Activity.Labels) == 0 {
		t.Errorf("empty summary %+v", sm)
	}
}
