package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// snapshotFile is the on-disk form of aggregates.json.
type snapshotFile struct {
	Version   int               `json:"version"`
	LastTS    time.Time         `json:"last_ts"`
	LastSeq   uint64            `json:"last_seq"` // version >= 2: records with Seq <= LastSeq are included
	Locations map[string]string `json:"locations"`
	Buckets   []snapshotBucket  `json:"buckets"`
}

type snapshotBucket struct {
	Day      string `json:"day"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	bucket
}

// writeSnapshot writes aggregates.json atomically (temp file + rename).
func (s *Store) writeSnapshot() error {
	s.mu.Lock()
	snap := snapshotFile{
		Version:   snapshotVer,
		LastTS:    s.lastTS,
		LastSeq:   s.seq,
		Locations: make(map[string]string, len(s.locations)),
		Buckets:   make([]snapshotBucket, 0, len(s.aggs)),
	}
	for p, l := range s.locations {
		snap.Locations[p] = l
	}
	for k, b := range s.aggs {
		snap.Buckets = append(snap.Buckets, snapshotBucket{Day: k.day, Provider: k.provider, Model: k.model, bucket: *b})
	}
	s.mu.Unlock()

	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, snapshotName+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpName, filepath.Join(s.dir, snapshotName))
	}
	if err != nil {
		os.Remove(tmpName)
	}
	return err
}

// loadSnapshot restores aggregates from aggregates.json. A missing file is
// fine; a corrupt one is logged and ignored.
func (s *Store) loadSnapshot() {
	data, err := os.ReadFile(filepath.Join(s.dir, snapshotName))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var snap snapshotFile
	if err == nil {
		err = json.Unmarshal(data, &snap)
	}
	if err != nil {
		if s.log != nil {
			s.log.Warn("usage: ignoring unreadable snapshot", "err", err)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastTS = snap.LastTS
	s.snapSeq = snap.Version >= 2
	s.seq = snap.LastSeq
	for p, l := range snap.Locations {
		s.locations[p] = l
	}
	for _, sb := range snap.Buckets {
		b := sb.bucket
		s.aggs[aggKey{sb.Day, sb.Provider, sb.Model}] = &b
	}
}

// replayCut decides which persisted records the snapshot does not contain.
type replayCut struct {
	useSeq  bool      // snapshot has last_seq (version >= 2)
	lastSeq uint64    // records with Seq > lastSeq are replayed
	lastTS  time.Time // fallback for Seq == 0 records or old snapshots
}

// include reports whether r must be aggregated on replay. Records carrying
// a Seq are judged by sequence, which is exact regardless of completion
// order. Records without one (older files), or any record under an old
// snapshot, fall back to the start-timestamp rule.
func (c replayCut) include(r *Record) bool {
	if c.useSeq && r.Seq > 0 {
		return r.Seq > c.lastSeq
	}
	return r.TS.After(c.lastTS)
}

// replay adds records not in the snapshot from the current and previous
// month attempts files. Malformed lines are skipped and counted. Afterwards
// the sequence counter continues after the highest Seq seen or snapshotted.
func (s *Store) replay() {
	now := s.now().UTC()
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	s.mu.Lock()
	cut := replayCut{useSeq: s.snapSeq, lastSeq: s.seq, lastTS: s.lastTS}
	s.mu.Unlock()
	maxSeq := cut.lastSeq
	for _, m := range []time.Time{cur.AddDate(0, -1, 0), cur} {
		if seq := s.replayFile(s.fileName(attemptsKind, m.Format(monthFormat)), cut); seq > maxSeq {
			maxSeq = seq
		}
	}
	s.mu.Lock()
	if maxSeq > s.seq {
		s.seq = maxSeq
	}
	s.mu.Unlock()
}

// replayFile aggregates the records of name that cut includes and returns
// the highest Seq seen in the file.
func (s *Store) replayFile(name string, cut replayCut) (maxSeq uint64) {
	f, err := os.Open(name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && s.log != nil {
			s.log.Warn("usage: cannot replay", "file", name, "err", err)
		}
		return 0
	}
	defer f.Close()
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var r Record
			if jerr := json.Unmarshal(line, &r); jerr != nil || r.TS.IsZero() {
				s.corruptLines.Add(1)
			} else {
				if r.Seq > maxSeq {
					maxSeq = r.Seq
				}
				if cut.include(&r) {
					s.mu.Lock()
					s.aggregate(&r)
					s.mu.Unlock()
				}
			}
		}
		if err != nil {
			if err != io.EOF && s.log != nil {
				s.log.Warn("usage: replay read error", "file", name, "err", err)
			}
			return maxSeq
		}
	}
}

// applyRetention deletes attempts-/events- files whose month is more than
// RetentionMonths before the current month.
func (s *Store) applyRetention() {
	if s.retention <= 0 {
		return
	}
	now := s.now().UTC()
	cutoff := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -s.retention, 0)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		s.persistError("read dir for retention", err)
		return
	}
	for _, e := range entries {
		month, ok := logMonth(e.Name())
		if !ok || !month.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil {
			s.persistError("retention remove", err)
		} else if s.log != nil {
			s.log.Info("usage: retention removed file", "file", e.Name())
		}
	}
}

// logMonth parses "attempts-YYYY-MM.jsonl" or "events-YYYY-MM.jsonl".
func logMonth(name string) (time.Time, bool) {
	rest, ok := strings.CutSuffix(name, ".jsonl")
	if !ok {
		return time.Time{}, false
	}
	for _, p := range []string{attemptsKind + "-", eventsKind + "-"} {
		if m, ok := strings.CutPrefix(rest, p); ok {
			t, err := time.Parse(monthFormat, m)
			return t, err == nil
		}
	}
	return time.Time{}, false
}
