package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTranscript writes a synthetic Claude Code transcript: one user line,
// then one assistant line per context size, the last at last.
func writeTranscript(t *testing.T, dir string, last time.Time, sizes ...int64) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"type":"user","timestamp":"2026-09-14T08:00:00Z","message":{"role":"user","content":"hello"}}` + "\n")
	for i, n := range sizes {
		ts := last.Add(time.Duration(i-len(sizes)+1) * time.Minute).Format(time.RFC3339Nano)
		// Spread the size over the buckets that count toward the context.
		fmt.Fprintf(&b, `{"type":"assistant","timestamp":%q,"message":{"model":"claude-x","usage":{"input_tokens":10,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":90}}}`+"\n",
			ts, n/2, n-n/2-100)
	}
	p := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func warnInput(session, path string) *strings.Reader {
	b, _ := json.Marshal(map[string]string{"session_id": session, "transcript_path": path, "prompt": "next"})
	return strings.NewReader(string(b))
}

func TestWarnSize(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	now := fixedNow
	opts := WarnOptions{Now: at(now), Threshold: 300_000}

	steps := []struct {
		size int64
		want string // substring, or "" for no warning
	}{
		{250_000, ""},
		{300_000, "context is 300k (warning at 300k)"},
		{350_000, ""}, // already warned; repeats only 100k later
		{400_000, "context is 400k"},
		{120_000, ""}, // compacted: below the threshold, state reset
		{310_000, "context is 310k"},
	}
	for i, s := range steps {
		p := writeTranscript(t, dir, now.Add(-2*time.Minute), 1000, s.size)
		msg, err := Warn(home, warnInput("s-1", p), opts)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if (s.want == "") != (msg == "") || !strings.Contains(msg, s.want) {
			t.Fatalf("step %d (%d tokens): got %q, want %q", i, s.size, msg, s.want)
		}
	}
}

func TestWarnIdle(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	cases := []struct {
		idle time.Duration
		size int64
		want string
	}{
		{30 * time.Minute, 150_000, ""},
		{90 * time.Minute, 50_000, ""},
		{90 * time.Minute, 150_000, "idle 90 min"},
		{5 * time.Hour, 150_000, "idle 5h"},
	}
	for i, c := range cases {
		p := writeTranscript(t, dir, fixedNow.Add(-c.idle), c.size)
		msg, err := Warn(home, warnInput(fmt.Sprintf("idle-%d", i), p), WarnOptions{Now: at(fixedNow)})
		if err != nil {
			t.Fatal(err)
		}
		if (c.want == "") != (msg == "") || !strings.Contains(msg, c.want) {
			t.Errorf("case %d: got %q, want %q", i, msg, c.want)
		}
	}
}

func TestWarnIgnoresSidechainAndSynthetic(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	p := writeTranscript(t, dir, fixedNow.Add(-time.Minute), 100_000)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	ts := fixedNow.Format(time.RFC3339)
	fmt.Fprintf(f, `{"type":"assistant","isSidechain":true,"timestamp":%q,"message":{"model":"claude-x","usage":{"input_tokens":900000}}}`+"\n", ts)
	fmt.Fprintf(f, `{"type":"assistant","timestamp":%q,"message":{"model":"<synthetic>","usage":{"input_tokens":900000}}}`+"\n", ts)
	f.Close()
	if msg, err := Warn(home, warnInput("s", p), WarnOptions{Now: at(fixedNow)}); err != nil || msg != "" {
		t.Fatalf("got %q, %v; want no warning from sidechain or synthetic lines", msg, err)
	}
}

func TestWarnLongTranscriptTail(t *testing.T) {
	// The last usage sits after more than warnTail bytes: only the tail is read.
	home, dir := t.TempDir(), t.TempDir()
	p := filepath.Join(dir, "long.jsonl")
	pad := `{"type":"user","message":{"content":"` + strings.Repeat("x", 4000) + `"}}` + "\n"
	body := strings.Repeat(pad, warnTail/len(pad)+10) +
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"model":"m","usage":{"input_tokens":400000}}}`+"\n", fixedNow.Format(time.RFC3339))
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := Warn(home, warnInput("s", p), WarnOptions{Now: at(fixedNow)}); err != nil || !strings.Contains(msg, "400k") {
		t.Fatalf("got %q, %v", msg, err)
	}
}

func TestWarnBadInput(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	if _, err := Warn(home, strings.NewReader("not json"), WarnOptions{}); err == nil {
		t.Error("bad stdin: want an error")
	}
	if _, err := Warn(home, warnInput("s", filepath.Join(dir, "missing.jsonl")), WarnOptions{}); err == nil {
		t.Error("missing transcript: want an error")
	}
	// An unsafe session id still warns, but keeps no state file.
	p := writeTranscript(t, dir, fixedNow, 500_000)
	if msg, err := Warn(home, warnInput("../evil", p), WarnOptions{Now: at(fixedNow)}); err != nil || msg == "" {
		t.Fatalf("got %q, %v", msg, err)
	}
	if es, _ := os.ReadDir(filepath.Join(home, "warn")); len(es) != 0 {
		t.Errorf("state written for an unsafe session id: %v", es)
	}
}

func TestWarnStatePruned(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	stale := filepath.Join(home, "warn", "old.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(`{"warned":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := fixedNow.Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	p := writeTranscript(t, dir, fixedNow, 500_000)
	if msg, err := Warn(home, warnInput("new", p), WarnOptions{Now: at(fixedNow)}); err != nil || msg == "" {
		t.Fatalf("got %q, %v", msg, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale state file kept")
	}
}
