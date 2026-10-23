package state

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// ArchiveEntry is one session's metrics snapshot in archive/sessions-YYYY-MM.json.
// Frozen entries (transcript gone) are never replaced; no entry is deleted.
type ArchiveEntry struct {
	Metrics     analyze.SessionMetrics `json:"metrics"`
	LastUpdated time.Time              `json:"last_updated"`
	Frozen      bool                   `json:"frozen"`
}

type archiveFile struct {
	V        int            `json:"v"`
	Month    string         `json:"month"` // YYYY-MM
	Sessions []ArchiveEntry `json:"sessions"`
}

// LastEventMetric, when present in SessionMetrics.Values, is the session's
// last event time in the window as Unix seconds; it selects the archive
// month. Without it the month is that of the window's end (capped at now).
const LastEventMetric = "last_event_at"

const monthLayout = "2006-01"

func archivePath(home, month string) string {
	return filepath.Join(home, ArchiveDir, "sessions-"+month+".json")
}

// archiveMonth picks the month file for m.
func archiveMonth(m analyze.SessionMetrics, now time.Time) string {
	if v, ok := m.Values[LastEventMetric]; ok && v.N > 0 && !math.IsInf(v.N, 0) {
		return time.Unix(int64(v.N), 0).UTC().Format(monthLayout)
	}
	t := m.Window.Until
	if t.IsZero() || t.After(now) {
		t = now
	}
	return t.Add(-time.Nanosecond).UTC().Format(monthLayout) // Until is exclusive
}

// loadArchiveMonths reads every archive file, keyed by month.
func loadArchiveMonths(home string) (map[string]*archiveFile, error) {
	paths, err := filepath.Glob(filepath.Join(home, ArchiveDir, archiveGlob))
	if err != nil {
		return nil, err
	}
	out := map[string]*archiveFile{}
	for _, p := range paths {
		month := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "sessions-"), ".json")
		if _, err := time.Parse(monthLayout, month); err != nil {
			continue // not ours
		}
		b, err := readVersioned(p, kindArchive)
		if err != nil {
			return nil, err
		}
		if b == nil {
			continue
		}
		af := &archiveFile{}
		if err := json.Unmarshal(b, af); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		af.V, af.Month = current[kindArchive], month
		out[month] = af
	}
	return out, nil
}

// UpsertArchive merges this run's metrics into the archive:
//
//   - a session in ms that is present (present[key]) replaces its entry, which
//     moves to the month of its latest snapshot if that changed;
//   - a session in ms that is not present is added frozen when it has no
//     entry, and otherwise left alone;
//   - every unfrozen entry whose session is not present is frozen;
//   - frozen entries are never replaced and no entry is ever deleted.
//
// present must list every session whose transcript still exists, not only
// those with events in the window: an absent key means "transcript gone".
func UpsertArchive(home string, ms []analyze.SessionMetrics, present map[analyze.SessionKey]bool, now time.Time) error {
	months, err := loadArchiveMonths(home)
	if err != nil {
		return err
	}
	now = now.UTC()
	type loc struct {
		month string
		i     int
	}
	index := map[analyze.SessionKey]loc{}
	for month, af := range months {
		for i, e := range af.Sessions {
			index[e.Metrics.Session] = loc{month, i}
		}
	}
	dirty := map[string]bool{}
	removed := map[loc]bool{}
	get := func(month string) *archiveFile {
		af := months[month]
		if af == nil {
			af = &archiveFile{V: current[kindArchive], Month: month}
			months[month] = af
		}
		return af
	}

	// Freeze entries whose transcripts are gone.
	for month, af := range months {
		for i := range af.Sessions {
			e := &af.Sessions[i]
			if !e.Frozen && !present[e.Metrics.Session] {
				e.Frozen, e.LastUpdated = true, now
				dirty[month] = true
			}
		}
	}

	for _, m := range ms {
		key := m.Session
		at, exists := index[key]
		if exists && months[at.month].Sessions[at.i].Frozen {
			continue
		}
		if !exists && !present[key] {
			af := get(archiveMonth(m, now))
			af.Sessions = append(af.Sessions, ArchiveEntry{Metrics: m, LastUpdated: now, Frozen: true})
			index[key] = loc{af.Month, len(af.Sessions) - 1}
			dirty[af.Month] = true
			continue
		}
		if !present[key] {
			continue
		}
		month := archiveMonth(m, now)
		entry := ArchiveEntry{Metrics: m, LastUpdated: now}
		if exists && at.month == month {
			months[month].Sessions[at.i] = entry
			dirty[month] = true
			continue
		}
		if exists {
			removed[at] = true
			dirty[at.month] = true
		}
		af := get(month)
		af.Sessions = append(af.Sessions, entry)
		index[key] = loc{month, len(af.Sessions) - 1}
		dirty[month] = true
	}

	for month := range dirty {
		af := months[month]
		kept := af.Sessions[:0:0]
		for i, e := range af.Sessions {
			if !removed[loc{month, i}] {
				kept = append(kept, e)
			}
		}
		sort.SliceStable(kept, func(i, j int) bool {
			return kept[i].Metrics.Session.String() < kept[j].Metrics.Session.String()
		})
		af.Sessions = kept
		af.V = current[kindArchive]
		if err := WriteJSONAtomic(archivePath(home, month), af); err != nil {
			return err
		}
	}
	return nil
}

// LoadArchive returns the entries of every archive month that ends after
// since (a zero since loads everything), sorted by month, then session key.
func LoadArchive(home string, since time.Time) ([]ArchiveEntry, error) {
	months, err := loadArchiveMonths(home)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(months))
	for m := range months {
		names = append(names, m)
	}
	sort.Strings(names)
	var out []ArchiveEntry
	for _, m := range names {
		start, _ := time.Parse(monthLayout, m)
		if !since.IsZero() && !start.AddDate(0, 1, 0).After(since) {
			continue
		}
		out = append(out, months[m].Sessions...)
	}
	return out, nil
}

// Prune deletes out/<run-id>/ directories older than retentionDays (by the
// time in the run id, else the directory's modification time). The archive,
// state and everything outside out/ are never touched.
func Prune(home string, now time.Time, retentionDays int) error {
	if retentionDays <= 0 {
		return fmt.Errorf("retention must be positive, got %d days", retentionDays)
	}
	cutoff := now.AddDate(0, 0, -retentionDays)
	dir := filepath.Join(home, OutDir)
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		t, err := ParseRunID(e.Name())
		if err != nil {
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			t = info.ModTime()
		}
		if t.Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
