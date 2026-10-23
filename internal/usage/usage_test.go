package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

var baseNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func testOpts(t *testing.T, c *clock) Options {
	t.Helper()
	return Options{
		Dir:           t.TempDir(),
		Now:           c.Now,
		FlushInterval: time.Hour,
		SnapshotEvery: time.Hour,
	}
}

func mustOpen(t *testing.T, o Options) *Store {
	t.Helper()
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rec(ts time.Time, provider, model string, o Outcome) Record {
	return Record{
		TS: ts, RequestID: "req", AttemptNo: 1, ClientAPI: "chat", Route: "coder",
		Provider: provider, Location: "cloud", Model: model, Outcome: o,
		PromptTokens: 10, CompletionTokens: 5, UsageSource: SourceReported, LatencyMS: 400,
	}
}

func readLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if sc.Text() != "" {
			out = append(out, sc.Text())
		}
	}
	return out
}

func findProvider(t *testing.T, st Stats, p string) ProviderStats {
	t.Helper()
	for _, ps := range st.Providers {
		if ps.Provider == p {
			return ps
		}
	}
	t.Fatalf("provider %q not in stats", p)
	return ProviderStats{}
}

func findModel(t *testing.T, st Stats, p, m string) ModelStats {
	t.Helper()
	for _, ms := range st.Models {
		if ms.Provider == p && ms.Model == m {
			return ms
		}
	}
	t.Fatalf("model %s/%s not in stats", p, m)
	return ModelStats{}
}

func TestAggregatesUpdateWhenQueueFull(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	o.QueueSize = 1
	s, err := open(o) // writer not started: the queue cannot drain
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.Add(rec(baseNow, "nvidia", "m", OutcomeCompleted))
	}
	s.AddEvent(map[string]string{"kind": "switch"})
	st := s.Stats()
	if got := findProvider(t, st, "nvidia").Today.Completed; got != 5 {
		t.Fatalf("completed = %d, want 5", got)
	}
	if st.Health.Dropped != 5 { // 4 records + 1 event
		t.Fatalf("dropped = %d, want 5", st.Health.Dropped)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(readLines(t, filepath.Join(o.Dir, "attempts-2026-09.jsonl"))); n != 1 {
		t.Fatalf("persisted lines = %d, want 1", n)
	}
}

func TestAddNeverBlocks(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	o.QueueSize = 1
	s := mustOpen(t, o)
	defer s.Close()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 20000; i++ {
			s.Add(rec(baseNow, "p", "m", OutcomeCompleted))
			s.AddEvent(i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Add blocked")
	}
	if got := findProvider(t, s.Stats(), "p").Today.Completed; got != 20000 {
		t.Fatalf("completed = %d", got)
	}
}

func TestAddAfterCloseIsDropped(t *testing.T) {
	c := newClock(baseNow)
	s := mustOpen(t, testOpts(t, c))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	s.Add(rec(baseNow, "p", "m", OutcomeCompleted))
	if d := s.Stats().Health.Dropped; d != 1 {
		t.Fatalf("dropped = %d", d)
	}
}

func TestOutcomeAndTokenAccounting(t *testing.T) {
	tests := []struct {
		name   string
		recs   []Record
		counts Counts
		tokens Tokens
	}{
		{
			name:   "outcomes",
			recs:   []Record{{Outcome: OutcomeCompleted}, {Outcome: OutcomeCompleted}, {Outcome: OutcomePartial}, {Outcome: OutcomeCancelled}, {Outcome: OutcomeFailed}},
			counts: Counts{Completed: 2, Partial: 1, Cancelled: 1, Failed: 1},
			tokens: Tokens{UnknownCount: 5},
		},
		{
			name: "sources",
			recs: []Record{
				{Outcome: OutcomeCompleted, UsageSource: SourceReported, PromptTokens: 100, CompletionTokens: 20, ReasoningTokens: 7, CachedTokens: 50},
				{Outcome: OutcomeCompleted, UsageSource: SourceEstimated, PromptTokens: 30, CompletionTokens: 4, ReasoningTokens: 1},
				{Outcome: OutcomeFailed, UsageSource: SourceUnknown, PromptTokens: 999, CompletionTokens: 999, CachedTokens: 2},
			},
			counts: Counts{Completed: 2, Failed: 1},
			tokens: Tokens{PromptReported: 100, CompletionReported: 20, PromptEstimated: 30, CompletionEstimated: 4, Reasoning: 8, Cached: 52, UnknownCount: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newClock(baseNow)
			s := mustOpen(t, testOpts(t, c))
			defer s.Close()
			for _, r := range tc.recs {
				r.TS, r.Provider, r.Model = baseNow, "p", "m"
				s.Add(r)
			}
			st := s.Stats()
			ps := findProvider(t, st, "p")
			if ps.Today != tc.counts || ps.Month != tc.counts {
				t.Errorf("counts today=%+v month=%+v, want %+v", ps.Today, ps.Month, tc.counts)
			}
			if ps.TokensToday != tc.tokens || ps.TokensMonth != tc.tokens {
				t.Errorf("tokens today=%+v month=%+v, want %+v", ps.TokensToday, ps.TokensMonth, tc.tokens)
			}
			if got := ps.Today.Invocations(); got != int64(len(tc.recs)) {
				t.Errorf("invocations = %d", got)
			}
		})
	}
}

func TestTokensTotal(t *testing.T) {
	tk := Tokens{PromptReported: 1, PromptEstimated: 2, CompletionReported: 4, CompletionEstimated: 8, Reasoning: 100, Cached: 100, UnknownCount: 100}
	if tk.Total() != 15 {
		t.Fatalf("Total = %d", tk.Total())
	}
}

func TestTimeWindows(t *testing.T) {
	c := newClock(baseNow)
	s := mustOpen(t, testOpts(t, c))
	defer s.Close()
	days := []int{0, 0, 1, 13, 14, 29, 30, 61, 62, 100}
	for _, d := range days {
		s.Add(rec(baseNow.AddDate(0, 0, -d), "nvidia", "m", OutcomeCompleted))
	}
	s.Add(Record{TS: baseNow, Provider: "ollama", Location: "local", Model: "q", Outcome: OutcomeFailed, UsageSource: SourceEstimated, PromptTokens: 3})

	st := s.Stats()
	nv := findProvider(t, st, "nvidia")
	if nv.Today.Completed != 2 {
		t.Errorf("today = %d, want 2", nv.Today.Completed)
	}
	if nv.Month.Completed != 6 { // days 0,0,1,13,14,29
		t.Errorf("month = %d, want 6", nv.Month.Completed)
	}
	if nv.TokensMonth.Total() != 6*15 || nv.TokensToday.Total() != 2*15 {
		t.Errorf("tokens today=%d month=%d", nv.TokensToday.Total(), nv.TokensMonth.Total())
	}
	if nv.Location != "cloud" || findProvider(t, st, "ollama").Location != "local" {
		t.Errorf("locations wrong: %+v", st.Providers)
	}

	if len(st.Daily) != 14 {
		t.Fatalf("daily len = %d", len(st.Daily))
	}
	if st.Daily[0].Day != "2026-09-16" || st.Daily[13].Day != "2026-09-29" {
		t.Errorf("daily range %s..%s", st.Daily[0].Day, st.Daily[13].Day)
	}
	wantInv := map[string]int64{"2026-09-29": 2, "2026-09-28": 1, "2026-09-16": 1}
	for _, d := range st.Daily {
		if d.TokensByProvider == nil || d.InvocationsByProvider == nil {
			t.Fatalf("nil maps on %s", d.Day)
		}
		if got := d.InvocationsByProvider["nvidia"]; got != wantInv[d.Day] {
			t.Errorf("%s: nvidia invocations = %d, want %d", d.Day, got, wantInv[d.Day])
		}
	}
	if got := st.Daily[13].InvocationsByProvider["ollama"]; got != 1 {
		t.Errorf("ollama today invocations = %d", got)
	}
	if got := st.Daily[13].TokensByProvider["nvidia"]; got != 30 {
		t.Errorf("nvidia today tokens = %d", got)
	}

	// Only days within the 62-day window are kept (0..61).
	s.mu.Lock()
	n := 0
	for k, b := range s.aggs {
		if k.provider == "nvidia" {
			n += int(b.Counts.Completed)
		}
	}
	s.mu.Unlock()
	if n != 8 {
		t.Errorf("kept nvidia records = %d, want 8", n)
	}

	// Moving the clock forward shifts the windows and prunes old days.
	c.Set(baseNow.AddDate(0, 0, 1))
	s.Add(rec(baseNow.AddDate(0, 0, 1), "nvidia", "m", OutcomeCompleted))
	st = s.Stats()
	nv = findProvider(t, st, "nvidia")
	if nv.Today.Completed != 1 || nv.Month.Completed != 6 { // day 29 fell out, new day in
		t.Errorf("after advance today=%d month=%d", nv.Today.Completed, nv.Month.Completed)
	}
	s.mu.Lock()
	for k := range s.aggs {
		if k.day < "2026-08-01" {
			t.Errorf("stale day %s kept", k.day)
		}
	}
	s.mu.Unlock()
}

func TestP50Latency(t *testing.T) {
	tests := []struct {
		name string
		lat  []int64
		want int64
	}{
		{"none", nil, 0},
		{"single low", []int64{5}, 100},
		{"exact bound", []int64{100}, 100},
		{"just above", []int64{101}, 250},
		{"two buckets", []int64{50, 300}, 100},
		{"majority high", []int64{50, 1500, 1800}, 2000},
		{"inf bucket", []int64{400000}, 300000},
		{"mixed", []int64{90, 200, 400, 900, 9000}, 500},
		{"zero", []int64{0}, 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var h histogram
			for _, l := range tc.lat {
				h.observe(l)
			}
			if got := h.p50(); got != tc.want {
				t.Fatalf("p50 = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestModelStats(t *testing.T) {
	c := newClock(baseNow)
	s := mustOpen(t, testOpts(t, c))
	defer s.Close()
	add := func(p, m string, o Outcome, lat int64, n int) {
		for i := 0; i < n; i++ {
			r := rec(baseNow, p, m, o)
			r.LatencyMS = lat
			s.Add(r)
		}
	}
	add("nvidia", "big", OutcomeCompleted, 3000, 3)
	add("nvidia", "big", OutcomeFailed, 10, 5) // failures do not affect latency
	add("ollama", "a", OutcomePartial, 200, 2)
	add("groq", "z", OutcomeCompleted, 50, 2)
	add("groq", "b", OutcomeCancelled, 50, 1)
	add("groq", "old", OutcomeCompleted, 50, 0)
	s.Add(rec(baseNow.AddDate(0, 0, -40), "groq", "old", OutcomeCompleted)) // outside month

	st := s.Stats()
	var order []string
	for _, m := range st.Models {
		order = append(order, m.Provider+"/"+m.Model)
	}
	want := []string{"nvidia/big", "groq/z", "ollama/a", "groq/b"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	big := findModel(t, st, "nvidia", "big")
	if big.P50LatencyMS != 5000 {
		t.Errorf("p50 = %d, want 5000", big.P50LatencyMS)
	}
	if big.FailureRate != 5.0/8.0 {
		t.Errorf("failure rate = %v", big.FailureRate)
	}
	if big.Today.Failed != 5 || big.Month.Completed != 3 || big.TokensMonth.Total() != 8*15 {
		t.Errorf("big stats = %+v", big)
	}
	if a := findModel(t, st, "ollama", "a"); a.P50LatencyMS != 250 || a.FailureRate != 0 {
		t.Errorf("ollama/a = %+v", a)
	}
	if b := findModel(t, st, "groq", "b"); b.P50LatencyMS != 0 {
		t.Errorf("cancelled-only p50 = %d", b.P50LatencyMS)
	}
	var provs []string
	for _, p := range st.Providers {
		provs = append(provs, p.Provider)
	}
	if !sort.StringsAreSorted(provs) || len(provs) != 3 {
		t.Errorf("providers = %v", provs)
	}
}

func TestCloseFlushesFilesAndSnapshot(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	s := mustOpen(t, o)
	r1 := rec(baseNow, "nvidia", "m", OutcomeCompleted)
	r1.Session = "sess-1"
	s.Add(r1)
	s.Add(rec(baseNow.Add(time.Second), "nvidia", "m", OutcomeFailed))
	aug := rec(time.Date(2026, 8, 31, 23, 59, 0, 0, time.UTC), "nvidia", "m", OutcomeCompleted)
	s.Add(aug)
	s.AddEvent(map[string]string{"kind": "switch", "route": "coder"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	lines := readLines(t, filepath.Join(o.Dir, "attempts-2026-09.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("september lines = %d", len(lines))
	}
	var got Record
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	r1.Seq = 1 // assigned by Add
	if !reflect.DeepEqual(got, r1) {
		t.Errorf("record round trip:\n got %+v\nwant %+v", got, r1)
	}
	if n := len(readLines(t, filepath.Join(o.Dir, "attempts-2026-08.jsonl"))); n != 1 {
		t.Errorf("august lines = %d", n)
	}
	ev := readLines(t, filepath.Join(o.Dir, "events-2026-09.jsonl"))
	if len(ev) != 1 || !strings.Contains(ev[0], `"route":"coder"`) {
		t.Errorf("events = %v", ev)
	}

	var snap snapshotFile
	data, err := os.ReadFile(filepath.Join(o.Dir, snapshotName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.LastTS.Equal(baseNow.Add(time.Second)) {
		t.Errorf("last_ts = %v", snap.LastTS)
	}
	if snap.Version != snapshotVer || snap.LastSeq != 3 {
		t.Errorf("version = %d, last_seq = %d, want %d, 3", snap.Version, snap.LastSeq, snapshotVer)
	}
	if !strings.Contains(string(data), `"last_ts"`) {
		t.Errorf("snapshot lacks last_ts: %s", data)
	}
	if tmp, _ := filepath.Glob(filepath.Join(o.Dir, "*.tmp")); len(tmp) != 0 {
		t.Errorf("temp files left: %v", tmp)
	}
}

func TestPeriodicFlushAndSnapshot(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	o.FlushInterval = 5 * time.Millisecond
	o.SnapshotEvery = 5 * time.Millisecond
	s := mustOpen(t, o)
	defer s.Close()
	s.Add(rec(baseNow, "p", "m", OutcomeCompleted))
	name := filepath.Join(o.Dir, "attempts-2026-09.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(name)
		_, serr := os.Stat(filepath.Join(o.Dir, snapshotName))
		if bytes.Count(data, []byte("\n")) == 1 && serr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not flushed in time: %q, snapshot err %v", data, serr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFlushEvery100Records(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c) // flush interval is one hour
	s := mustOpen(t, o)
	defer s.Close()
	for i := 0; i < flushEvery; i++ {
		s.Add(rec(baseNow, "p", "m", OutcomeCompleted))
	}
	name := filepath.Join(o.Dir, "attempts-2026-09.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(name)
		if bytes.Count(data, []byte("\n")) == flushEvery {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d lines before Close", bytes.Count(data, []byte("\n")))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func appendLines(t *testing.T, name string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l); err != nil {
			t.Fatal(err)
		}
	}
}

func recLine(t *testing.T, r Record) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func TestSnapshotReopenReplaysOnlyNewer(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	s := mustOpen(t, o)
	for i := 0; i < 3; i++ {
		s.Add(rec(baseNow.Add(time.Duration(i)*time.Minute), "nvidia", "m", OutcomeCompleted))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate records flushed after the snapshot, then a crash mid-line.
	name := filepath.Join(o.Dir, "attempts-2026-09.jsonl")
	appendLines(t, name,
		recLine(t, rec(baseNow.Add(10*time.Minute), "nvidia", "m", OutcomeFailed)),
		"\n",
		recLine(t, rec(baseNow.Add(11*time.Minute), "ollama", "q", OutcomeCompleted)),
		`{"ts":"2026-09-29T12:12:00Z","provider":"nvi`,
	)

	c.Set(baseNow.Add(time.Hour))
	s2 := mustOpen(t, o)
	st := s2.Stats()
	nv := findProvider(t, st, "nvidia")
	if nv.Today.Completed != 3 || nv.Today.Failed != 1 {
		t.Errorf("nvidia today = %+v, want 3 completed 1 failed", nv.Today)
	}
	if findProvider(t, st, "ollama").Today.Completed != 1 {
		t.Errorf("ollama not replayed")
	}
	if st.Health.CorruptLines != 1 {
		t.Errorf("corrupt lines = %d, want 1", st.Health.CorruptLines)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	// A third open must not double count anything.
	s3 := mustOpen(t, o)
	defer s3.Close()
	st = s3.Stats()
	if got := findProvider(t, st, "nvidia").Today.Invocations(); got != 4 {
		t.Errorf("third open nvidia invocations = %d, want 4", got)
	}
	if got := findProvider(t, st, "ollama").Today.Invocations(); got != 1 {
		t.Errorf("third open ollama invocations = %d, want 1", got)
	}
}

// persistQueued writes everything queued on an unstarted store to disk,
// simulating the writer having flushed before a crash (no final snapshot).
func persistQueued(t *testing.T, s *Store) {
	t.Helper()
	w := newWriter(s)
	w.drain(s.q)
	if err := w.closeAll(); err != nil {
		t.Fatal(err)
	}
}

func fileSeqs(t *testing.T, name string) []uint64 {
	t.Helper()
	var out []uint64
	for _, l := range readLines(t, name) {
		var r Record
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r.Seq)
	}
	return out
}

func TestReplayOutOfOrderCompletionBySeq(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	s, err := open(o) // no writer goroutine: the test drives persistence
	if err != nil {
		t.Fatal(err)
	}
	// A short request started at T2 completes first and is snapshotted.
	s.Add(rec(baseNow.Add(time.Minute), "nvidia", "m", OutcomeCompleted))
	if err := s.writeSnapshot(); err != nil {
		t.Fatal(err)
	}
	// A long request started earlier (T1 < T2) completes after the snapshot.
	s.Add(rec(baseNow, "nvidia", "m", OutcomeFailed))
	persistQueued(t, s) // both lines on disk, then "crash" without a snapshot

	name := filepath.Join(o.Dir, "attempts-2026-09.jsonl")
	if got := fileSeqs(t, name); !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Fatalf("file seqs = %v, want [1 2]", got)
	}

	check := func(s *Store, when string) {
		t.Helper()
		nv := findProvider(t, s.Stats(), "nvidia")
		if nv.Today.Completed != 1 || nv.Today.Failed != 1 {
			t.Errorf("%s: nvidia today = %+v, want 1 completed 1 failed", when, nv.Today)
		}
	}
	c.Set(baseNow.Add(time.Hour))
	s2 := mustOpen(t, o)
	check(s2, "reopen after crash")
	s2.Add(rec(baseNow.Add(time.Hour), "ollama", "q", OutcomeCompleted))
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fileSeqs(t, name); !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Errorf("seq did not continue after replay: %v", got)
	}

	// The snapshot now holds all three: nothing may be counted twice.
	s3 := mustOpen(t, o)
	defer s3.Close()
	check(s3, "third open")
	if got := findProvider(t, s3.Stats(), "ollama").Today.Invocations(); got != 1 {
		t.Errorf("ollama invocations = %d, want 1", got)
	}
}

func TestReplayOldFormatFallsBackToTS(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	// Version 1 snapshot (no last_seq) holding one record at baseNow.
	v1 := fmt.Sprintf(`{"version":1,"last_ts":%q,"locations":{"p":"cloud"},"buckets":[`+
		`{"day":"2026-09-29","provider":"p","model":"m","counts":{"completed":1},"tokens":{}}]}`,
		baseNow.Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(o.Dir, snapshotName), []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(o.Dir, "attempts-2026-09.jsonl")
	appendLines(t, name,
		recLine(t, rec(baseNow, "p", "m", OutcomeCompleted)),                // in snapshot
		recLine(t, rec(baseNow.Add(-time.Minute), "p", "m", OutcomeFailed)), // older than last_ts: skipped
		recLine(t, rec(baseNow.Add(time.Minute), "p", "m", OutcomeCompleted)))
	s := mustOpen(t, o)
	ps := findProvider(t, s.Stats(), "p")
	if ps.Today.Completed != 2 || ps.Today.Failed != 0 {
		t.Errorf("today = %+v, want 2 completed 0 failed", ps.Today)
	}
	s.Add(rec(baseNow.Add(2*time.Minute), "p", "m", OutcomeCompleted))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fileSeqs(t, name); !reflect.DeepEqual(got, []uint64{0, 0, 0, 1}) {
		t.Errorf("file seqs = %v, want [0 0 0 1]", got)
	}
	// Reopen under the new (v2) snapshot: mixed old and new lines, no double count.
	s2 := mustOpen(t, o)
	defer s2.Close()
	if got := findProvider(t, s2.Stats(), "p").Today.Completed; got != 3 {
		t.Errorf("completed after v2 reopen = %d, want 3", got)
	}
}

func TestReplayWithoutSnapshotIncludesPreviousMonth(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	c := newClock(now)
	o := testOpts(t, c)
	appendLines(t, filepath.Join(o.Dir, "attempts-2026-09.jsonl"),
		recLine(t, rec(time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC), "p", "m", OutcomeCompleted)))
	appendLines(t, filepath.Join(o.Dir, "attempts-2026-10.jsonl"),
		recLine(t, rec(now.Add(-time.Hour), "p", "m", OutcomeCompleted)),
		"not json\n")
	appendLines(t, filepath.Join(o.Dir, "attempts-2026-08.jsonl"), // not replayed
		recLine(t, rec(time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC), "p", "m", OutcomeCompleted)))
	s := mustOpen(t, o)
	defer s.Close()
	st := s.Stats()
	ps := findProvider(t, st, "p")
	if ps.Today.Completed != 1 || ps.Month.Completed != 2 {
		t.Errorf("today=%d month=%d", ps.Today.Completed, ps.Month.Completed)
	}
	if st.Health.CorruptLines != 1 {
		t.Errorf("corrupt = %d", st.Health.CorruptLines)
	}
}

func TestCorruptSnapshotStartsEmpty(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	var logs bytes.Buffer
	o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := os.WriteFile(filepath.Join(o.Dir, snapshotName), []byte(`{"version":1,"last_ts":`), 0o644); err != nil {
		t.Fatal(err)
	}
	appendLines(t, filepath.Join(o.Dir, "attempts-2026-09.jsonl"), recLine(t, rec(baseNow, "p", "m", OutcomeCompleted)))
	s := mustOpen(t, o)
	defer s.Close()
	if got := findProvider(t, s.Stats(), "p").Today.Completed; got != 1 {
		t.Errorf("completed = %d, want 1 (from replay)", got)
	}
	if !strings.Contains(logs.String(), "snapshot") {
		t.Errorf("corrupt snapshot not logged: %q", logs.String())
	}
}

func TestRetention(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	o.RetentionMonths = 6
	o.FlushInterval = 5 * time.Millisecond
	files := []string{
		"attempts-2026-01.jsonl", "attempts-2026-02.jsonl", "attempts-2026-03.jsonl",
		"attempts-2026-08.jsonl", "attempts-2026-09.jsonl",
		"events-2025-12.jsonl", "events-2026-04.jsonl",
		"notes-2020-01.jsonl", "attempts-bogus.jsonl", "attempts-2020-01.json",
	}
	for _, f := range files {
		appendLines(t, filepath.Join(o.Dir, f), "")
	}
	s := mustOpen(t, o)
	defer s.Close()

	exists := func(f string) bool {
		_, err := os.Stat(filepath.Join(o.Dir, f))
		return err == nil
	}
	want := map[string]bool{
		"attempts-2026-01.jsonl": false, "attempts-2026-02.jsonl": false, "attempts-2026-03.jsonl": true,
		"attempts-2026-08.jsonl": true, "attempts-2026-09.jsonl": true,
		"events-2025-12.jsonl": false, "events-2026-04.jsonl": true,
		"notes-2020-01.jsonl": true, "attempts-bogus.jsonl": true, "attempts-2020-01.json": true,
	}
	for f, w := range want {
		if exists(f) != w {
			t.Errorf("%s exists=%v, want %v", f, !w, w)
		}
	}

	// The background loop reapplies retention when the day changes.
	c.Set(time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC))
	deadline := time.Now().Add(5 * time.Second)
	for exists("attempts-2026-03.jsonl") {
		if time.Now().After(deadline) {
			t.Fatal("daily retention did not run")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !exists("attempts-2026-04.jsonl") && !exists("events-2026-04.jsonl") {
		t.Error("april files removed too early")
	}
}

func TestLogMonth(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
		want string
	}{
		{"attempts-2026-09.jsonl", true, "2026-09"},
		{"events-2025-12.jsonl", true, "2025-12"},
		{"attempts-2026-13.jsonl", false, ""},
		{"aggregates.json", false, ""},
		{"attempts-2026-09.jsonl.bak", false, ""},
	}
	for _, tc := range tests {
		m, ok := logMonth(tc.name)
		if ok != tc.ok || (ok && m.Format(monthFormat) != tc.want) {
			t.Errorf("logMonth(%q) = %v, %v", tc.name, m, ok)
		}
	}
}

func TestPersistErrorsCounted(t *testing.T) {
	c := newClock(baseNow)
	o := testOpts(t, c)
	// A directory where the attempts file should be makes opening it fail.
	if err := os.Mkdir(filepath.Join(o.Dir, "attempts-2026-09.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t, o)
	s.Add(rec(baseNow, "p", "m", OutcomeCompleted))
	s.Add(rec(baseNow, "p", "m", OutcomeCompleted))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Health.PersistErrors != 2 {
		t.Errorf("persist errors = %d, want 2", st.Health.PersistErrors)
	}
	if findProvider(t, st, "p").Today.Completed != 2 {
		t.Error("aggregates lost on persist error")
	}
}

func TestStatsJSONShape(t *testing.T) {
	c := newClock(baseNow)
	s := mustOpen(t, testOpts(t, c))
	defer s.Close()
	s.Add(rec(baseNow, "p", "m", OutcomeCompleted))
	b, err := json.Marshal(s.Stats())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	checkKeys := func(what string, v any, keys ...string) {
		t.Helper()
		obj, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s is %T", what, v)
		}
		var got []string
		for k := range obj {
			got = append(got, k)
		}
		sort.Strings(got)
		sort.Strings(keys)
		if !reflect.DeepEqual(got, keys) {
			t.Errorf("%s keys = %v, want %v", what, got, keys)
		}
	}
	counts := []string{"completed", "partial", "cancelled", "failed"}
	tokens := []string{"prompt_reported", "prompt_estimated", "completion_reported", "completion_estimated", "reasoning", "cached", "unknown_count"}
	checkKeys("stats", m, "generated_at", "providers", "models", "daily", "health")
	checkKeys("health", m["health"], "dropped", "persist_errors", "corrupt_lines")
	prov := m["providers"].([]any)[0]
	checkKeys("provider", prov, "provider", "location", "today", "month", "tokens_today", "tokens_month")
	checkKeys("counts", prov.(map[string]any)["today"], counts...)
	checkKeys("tokens", prov.(map[string]any)["tokens_month"], tokens...)
	model := m["models"].([]any)[0]
	checkKeys("model", model, "provider", "model", "today", "month", "tokens_month", "p50_latency_ms", "failure_rate")
	daily := m["daily"].([]any)
	if len(daily) != 14 {
		t.Fatalf("daily len %d", len(daily))
	}
	checkKeys("day", daily[0], "day", "tokens_by_provider", "invocations_by_provider")
	if !strings.Contains(string(b), `"tokens_by_provider":{}`) {
		t.Errorf("empty day maps should marshal as {}: %s", b)
	}
}

func TestEmptyStats(t *testing.T) {
	c := newClock(baseNow)
	s := mustOpen(t, testOpts(t, c))
	defer s.Close()
	st := s.Stats()
	if st.Providers == nil || st.Models == nil || len(st.Daily) != 14 {
		t.Fatalf("empty stats = %+v", st)
	}
	if !st.GeneratedAt.Equal(baseNow) {
		t.Errorf("generated_at = %v", st.GeneratedAt)
	}
}

func TestConcurrentAddAndStats(t *testing.T) {
	c := newClock(baseNow)
	s := mustOpen(t, testOpts(t, c))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 250; i++ {
				s.Add(rec(baseNow, fmt.Sprint("p", g%3), "m", OutcomeCompleted))
				if i%50 == 0 {
					s.Stats()
				}
			}
		}(g)
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range s.Stats().Providers {
		total += p.Today.Completed
	}
	if total != 2000 {
		t.Fatalf("total = %d", total)
	}
}
