package projects

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServerSuggestions(t *testing.T) {
	base := t.TempDir()
	conn, fresh, other := filepath.Join(base, "conn"), filepath.Join(base, "fresh"), filepath.Join(base, "other")
	mkdirs(t, filepath.Join(conn, "sub"), fresh, other)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	var (
		mu        sync.Mutex
		dismissed []string
		recentErr error
	)
	s := NewServer(ServerOptions{
		Load:  func() ([]Entry, error) { return []Entry{{Name: "conn", Path: conn}}, nil },
		Now:   func() time.Time { return now },
		Token: "tok",
		Recent: func(context.Context) ([]RecentSession, error) {
			return []RecentSession{
				{Tool: "claude", Dir: filepath.Join(conn, "sub"), Last: now.Add(-time.Hour)},
				{Tool: "claude", Dir: fresh, Last: now.Add(-2 * time.Hour)},
				{Tool: "codex", Dir: fresh, Last: now.Add(-3 * time.Hour)},
				{Tool: "claude", Dir: other, Last: now.Add(-50 * time.Hour)},
			}, recentErr
		},
		Suggest: SuggestOptions{Home: filepath.Join(base, "home"), TempDirs: []string{},
			Root: func(_ context.Context, dir string) string {
				if dir == filepath.Join(conn, "sub") {
					return conn
				}
				return dir
			}},
		Dismissed: func() ([]string, error) { mu.Lock(); defer mu.Unlock(); return dismissed, nil },
		Dismiss:   func(dir string) error { mu.Lock(); defer mu.Unlock(); dismissed = append(dismissed, dir); return nil },
	})
	page := func() string {
		t.Helper()
		code, body, _ := get(t, s, "GET", "127.0.0.1:8091", "/")
		if code != 200 {
			t.Fatalf("page: %d %s", code, body)
		}
		return body
	}

	if b := page(); !strings.Contains(b, "Looking for recent sessions") {
		t.Fatal("before the first refresh the page should say it is looking")
	}
	s.RefreshSuggestions(context.Background())
	b := page()
	for _, want := range []string{"cbrt projects connect " + fresh, "2 sessions (claude, codex), last 2h ago", "cbrt projects connect " + other, "1 session (claude), last 2d ago"} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(b, "cbrt projects connect "+conn) {
		t.Error("a connected project (via a session in its subfolder) is suggested")
	}
	if strings.Index(b, fresh) > strings.Index(b, other) {
		t.Error("suggestions are not newest first")
	}

	form := url.Values{"token": {"tok"}, "path": {fresh}}
	if code, body, _ := post(t, s, "/api/dismiss", "127.0.0.1:8091", "http://127.0.0.1:8091", "", form); code != 303 {
		t.Fatalf("dismiss: %d %s", code, body)
	}
	if b := page(); strings.Contains(b, "connect "+fresh) || !strings.Contains(b, "connect "+other) {
		t.Error("after dismissing, the dismissed folder should be gone and the other one stay")
	}
	for _, f := range []url.Values{
		{"token": {"tok"}, "path": {filepath.Join(base, "never-suggested")}},
		{"token": {"tok"}, "path": {""}},
	} {
		if code, _, _ := post(t, s, "/api/dismiss", "127.0.0.1:8091", "http://127.0.0.1:8091", "", f); code != 404 {
			t.Errorf("dismissing %q: %d, want 404", f.Get("path"), code)
		}
	}
	if code, _, _ := post(t, s, "/api/dismiss", "127.0.0.1:8091", "http://evil.example", "", url.Values{"token": {"tok"}, "path": {other}}); code != 403 {
		t.Errorf("cross-origin dismiss: %d, want 403", code)
	}
	if len(dismissed) != 1 {
		t.Errorf("dismissed = %v, want only %s", dismissed, fresh)
	}

	recentErr = errors.New("transcripts unreadable")
	s.RefreshSuggestions(context.Background())
	if b := page(); !strings.Contains(b, "Could not read recent sessions: transcripts unreadable") {
		t.Error("a failed refresh is not shown")
	}
}

func TestServerNoSuggestions(t *testing.T) {
	s, _ := serverEnv(t, "# Plan: inkwell\n")
	s.RefreshSuggestions(context.Background()) // no Recent: a no-op
	if _, b, _ := get(t, s, "GET", "127.0.0.1:8091", "/"); strings.Contains(b, "Not connected yet") {
		t.Error("the section shows without a session source")
	}
}

func TestConnectCommand(t *testing.T) {
	for in, want := range map[string]string{
		`/srv/work/inkwell`:       `cbrt projects connect /srv/work/inkwell`,
		`D:\Work\My Projects\ink`: `cbrt projects connect "D:\\Work\\My Projects\\ink"`,
	} {
		if got := connectCommand(in); got != want {
			t.Errorf("connectCommand(%q) = %q, want %q", in, got, want)
		}
	}
}
