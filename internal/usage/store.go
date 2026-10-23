// Package usage accounts invocations and tokens per provider, model and day.
// Aggregates live in memory and are updated synchronously; attempt records
// and events are persisted best-effort as monthly JSONL files by a single
// background writer, with a periodic atomic snapshot of the aggregates.
package usage

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Outcome is the result of one invocation.
type Outcome string

const (
	OutcomeCompleted Outcome = "completed"
	OutcomePartial   Outcome = "partial"
	OutcomeCancelled Outcome = "cancelled"
	OutcomeFailed    Outcome = "failed"
)

// Source says where token counts came from.
type Source string

const (
	SourceReported  Source = "reported"
	SourceEstimated Source = "estimated"
	SourceUnknown   Source = "unknown"
)

// Record is one invocation (an attempt that reached an upstream).
type Record struct {
	// Seq is the ingestion sequence number assigned by Store.Add when the
	// record is aggregated; the snapshot's last_seq says exactly which
	// records it contains. 0 in files written before seq existed.
	Seq              uint64    `json:"seq,omitempty"`
	TS               time.Time `json:"ts"` // attempt start; descriptive only (records complete out of order)
	RequestID        string    `json:"request_id"`
	AttemptNo        int       `json:"attempt_no"`
	ClientAPI        string    `json:"client_api"` // "chat" | "messages" | "responses"
	Route            string    `json:"route"`
	Provider         string    `json:"provider"`
	Location         string    `json:"location"` // "local" | "cloud"
	Model            string    `json:"model"`
	Outcome          Outcome   `json:"outcome"`
	Class            string    `json:"class,omitempty"` // failure class when failed/partial
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	ReasoningTokens  int64     `json:"reasoning_tokens,omitempty"`
	CachedTokens     int64     `json:"cached_tokens,omitempty"`
	UsageSource      Source    `json:"usage_source"`
	LatencyMS        int64     `json:"latency_ms"`
	TTFBMS           int64     `json:"ttfb_ms"`
	Session          string    `json:"session,omitempty"`
	// ResponseID is the id of the answer the client received (Anthropic
	// message id, Responses id, Chat Completions id), so a client transcript
	// can be matched to this attempt. Empty when no answer reached the client.
	ResponseID string `json:"response_id,omitempty"`
}

// Options configures a Store.
type Options struct {
	Dir             string
	RetentionMonths int              // delete attempts-/events- files older than this many months; <=0 keeps all
	Now             func() time.Time // default time.Now
	Logger          *slog.Logger     // may be nil
	FlushInterval   time.Duration    // default 1s
	SnapshotEvery   time.Duration    // default 60s
	QueueSize       int              // default 10000
}

const (
	keepDays     = 62  // aggregate days kept
	flushEvery   = 100 // records per forced flush
	snapshotName = "aggregates.json"
	monthFormat  = "2006-01"
	attemptsKind = "attempts"
	eventsKind   = "events"
	snapshotVer  = 2 // 2 adds last_seq; 1 is still read
)

type aggKey struct{ day, provider, model string }

type bucket struct {
	Counts  Counts    `json:"counts"`
	Tokens  Tokens    `json:"tokens"`
	Latency histogram `json:"latency_hist"`
}

// queued is one line waiting to be persisted.
type queued struct {
	kind  string // attemptsKind or eventsKind
	month string // "2006-01"
	rec   Record // for attempts
	line  []byte // for events, already marshalled
}

// Store holds the aggregates and owns the persistence goroutine.
type Store struct {
	dir       string
	retention int
	now       func() time.Time
	log       *slog.Logger
	flushIvl  time.Duration
	snapIvl   time.Duration

	mu        sync.Mutex
	aggs      map[aggKey]*bucket
	locations map[string]string
	lastTS    time.Time // max TS aggregated; replay watermark for records without Seq
	seq       uint64    // last ingestion sequence number assigned
	snapSeq   bool      // the loaded snapshot carries last_seq
	pruneDay  string

	q        chan queued
	stop     chan struct{}
	done     chan struct{}
	started  bool
	closed   atomic.Bool
	once     sync.Once
	closeErr error

	dropped       atomic.Int64
	persistErrors atomic.Int64
	corruptLines  atomic.Int64
}

// Open creates Dir, applies retention, loads the snapshot, replays newer
// attempt records and starts the background writer.
func Open(o Options) (*Store, error) {
	s, err := open(o)
	if err != nil {
		return nil, err
	}
	s.start()
	return s, nil
}

// open prepares a store without starting the writer goroutine.
func open(o Options) (*Store, error) {
	if o.Dir == "" {
		return nil, fmt.Errorf("usage: empty Dir")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = time.Second
	}
	if o.SnapshotEvery <= 0 {
		o.SnapshotEvery = 60 * time.Second
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 10000
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("usage: create dir: %w", err)
	}
	s := &Store{
		dir:       o.Dir,
		retention: o.RetentionMonths,
		now:       o.Now,
		log:       o.Logger,
		flushIvl:  o.FlushInterval,
		snapIvl:   o.SnapshotEvery,
		aggs:      map[aggKey]*bucket{},
		locations: map[string]string{},
		q:         make(chan queued, o.QueueSize),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	s.applyRetention()
	s.loadSnapshot()
	s.replay()
	s.mu.Lock()
	s.prune(dayOffset(s.now(), 0))
	s.mu.Unlock()
	return s, nil
}

func (s *Store) start() {
	if s.started {
		return
	}
	s.started = true
	go s.loop(dayOffset(s.now(), 0))
}

// Add updates the aggregates and queues r for persistence without blocking.
// When the queue is full the record is not persisted and Dropped grows.
func (s *Store) Add(r Record) {
	if r.TS.IsZero() {
		r.TS = s.now()
	}
	// Seq is assigned in the same critical section as aggregation, so a
	// snapshot with last_seq N holds exactly the records with Seq <= N.
	// The queued copy carries that Seq into the JSONL line.
	s.mu.Lock()
	s.seq++
	r.Seq = s.seq
	s.aggregate(&r)
	s.mu.Unlock()
	s.enqueue(queued{kind: attemptsKind, month: r.TS.UTC().Format(monthFormat), rec: r})
}

// AddEvent queues v as one JSON line of the current month's events file.
func (s *Store) AddEvent(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.persistError("marshal event", err)
		return
	}
	s.enqueue(queued{kind: eventsKind, month: s.now().UTC().Format(monthFormat), line: b})
}

func (s *Store) enqueue(it queued) {
	if s.closed.Load() {
		s.dropped.Add(1)
		return
	}
	select {
	case s.q <- it:
	default:
		s.dropped.Add(1)
	}
}

// aggregate adds r to the in-memory aggregates. Caller holds s.mu.
func (s *Store) aggregate(r *Record) {
	if r.TS.After(s.lastTS) {
		s.lastTS = r.TS
	}
	if r.Location != "" {
		s.locations[r.Provider] = r.Location
	} else if _, ok := s.locations[r.Provider]; !ok {
		s.locations[r.Provider] = ""
	}
	today := dayOffset(s.now(), 0)
	if today != s.pruneDay {
		s.prune(today)
	}
	day := r.TS.UTC().Format(dayFormat)
	if day < dayOffset(s.now(), keepDays-1) {
		return
	}
	k := aggKey{day, r.Provider, r.Model}
	b := s.aggs[k]
	if b == nil {
		b = &bucket{}
		s.aggs[k] = b
	}
	b.Counts.inc(r.Outcome)
	b.Tokens.addRecord(r)
	if r.Outcome == OutcomeCompleted || r.Outcome == OutcomePartial {
		b.Latency.observe(r.LatencyMS)
	}
}

// prune drops aggregate days older than keepDays. Caller holds s.mu.
func (s *Store) prune(today string) {
	t, err := time.Parse(dayFormat, today)
	if err != nil {
		return
	}
	cutoff := dayOffset(t, keepDays-1)
	for k := range s.aggs {
		if k.day < cutoff {
			delete(s.aggs, k)
		}
	}
	s.pruneDay = today
}

// Close stops the writer, flushes all queued lines and writes a snapshot.
func (s *Store) Close() error {
	s.once.Do(func() {
		s.closed.Store(true)
		s.start()
		close(s.stop)
		<-s.done
	})
	return s.closeErr
}

func (s *Store) persistError(msg string, err error) {
	s.persistErrors.Add(1)
	if s.log != nil {
		s.log.Error("usage: "+msg, "err", err)
	}
}

// loop owns all file I/O. retentionDay is captured by Open so a clock change right after Open is not missed.
func (s *Store) loop(retentionDay string) {
	defer close(s.done)
	w := newWriter(s)
	flushT := time.NewTicker(s.flushIvl)
	snapT := time.NewTicker(s.snapIvl)
	defer flushT.Stop()
	defer snapT.Stop()

	for {
		select {
		case it := <-s.q:
			w.write(it)
		case <-flushT.C:
			w.flush()
			w.closeIdle()
			if d := dayOffset(s.now(), 0); d != retentionDay {
				retentionDay = d
				w.closeAll() // Windows cannot delete open files
				s.applyRetention()
			}
		case <-snapT.C:
			if err := s.writeSnapshot(); err != nil {
				s.persistError("write snapshot", err)
			}
		case <-s.stop:
			w.drain(s.q)
			w.flush()
			if err := w.closeAll(); err != nil {
				s.closeErr = err
			}
			if err := s.writeSnapshot(); err != nil {
				s.persistError("write snapshot", err)
				if s.closeErr == nil {
					s.closeErr = err
				}
			}
			return
		}
	}
}

func (s *Store) fileName(kind, month string) string {
	return filepath.Join(s.dir, kind+"-"+month+".jsonl")
}
