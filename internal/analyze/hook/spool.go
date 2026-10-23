package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// compactLine is one line of registry/YYYY-MM.jsonl: the envelope plus the
// name of the spool file it came from, which keeps compaction idempotent and
// gives the envelope the same identity before and after compaction.
type compactLine struct {
	analyze.Envelope
	Spool string `json:"spool"`
}

const monthLayout = "2006-01"

// staleLock is how old a compaction lock must be before a new run takes it
// over (the run that held it crashed).
const staleLock = 10 * time.Minute

// SpoolName is the base name of the spool file env was recorded to, whether
// ReadSpool found it in the spool or in a compacted month. Adapters derive a
// registry event's Seq from it.
func SpoolName(env analyze.Envelope) string {
	b := filepath.Base(env.File)
	if i := strings.Index(b, "#"); i >= 0 {
		return b[i+1:]
	}
	return b
}

// ReadSpool returns every envelope under home: the compacted months first
// (oldest first), then the spool files sorted by name. Envelope.File is the
// spool file's path, or "<month file>#<spool name>" for a compacted one.
// Leftover .tmp files and files or lines that are not valid envelopes are
// reported in problems (by base name) and skipped.
func ReadSpool(home string) (envs []analyze.Envelope, problems []string, err error) {
	regDir := analyze.RegistryDir(home)
	entries, err := os.ReadDir(regDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	compacted := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir():
		case isMonthFile(name):
			path := filepath.Join(regDir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, nil, err
			}
			for i, line := range bytes.Split(data, []byte("\n")) {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				var cl compactLine
				if json.Unmarshal(line, &cl) != nil || cl.Spool == "" {
					problems = append(problems, fmt.Sprintf("%s:%d: not a valid envelope", name, i+1))
					continue
				}
				if p := checkEnvelope(cl.Envelope); p != "" {
					problems = append(problems, fmt.Sprintf("%s:%d: %s", name, i+1, p))
					continue
				}
				cl.Envelope.File = path + "#" + cl.Spool
				compacted[cl.Spool] = true
				envs = append(envs, cl.Envelope)
			}
		case strings.HasSuffix(name, ".tmp"):
			problems = append(problems, name+": incomplete write")
		}
	}

	spool, err := readSpoolDir(home)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range spool {
		switch {
		case s.problem != "":
			problems = append(problems, s.name+": "+s.problem)
		case compacted[s.name]: // compacted, its deletion was interrupted
		default:
			envs = append(envs, s.env)
		}
	}
	return envs, problems, nil
}

type spoolFile struct {
	name    string
	env     analyze.Envelope
	problem string
}

// readSpoolDir reads the spool directory sorted by name. A missing spool is
// empty.
func readSpoolDir(home string) ([]spoolFile, error) {
	dir := analyze.SpoolDir(home)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []spoolFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".tmp") {
			out = append(out, spoolFile{name: name, problem: "incomplete write (an interrupted hook)"})
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // compacted meanwhile
		}
		if err != nil {
			return nil, err
		}
		var env analyze.Envelope
		if json.Unmarshal(data, &env) != nil {
			out = append(out, spoolFile{name: name, problem: "not a valid envelope"})
			continue
		}
		if p := checkEnvelope(env); p != "" {
			out = append(out, spoolFile{name: name, problem: p})
			continue
		}
		env.File = path
		out = append(out, spoolFile{name: name, env: env})
	}
	return out, nil
}

func checkEnvelope(env analyze.Envelope) string {
	switch {
	case env.V != analyze.EnvelopeVersion:
		return fmt.Sprintf("unsupported envelope version %d", env.V)
	case env.AdapterID == "" || env.Event == "":
		return "envelope without adapter_id or event"
	case env.RecordedAt.IsZero():
		return "envelope without recorded_at"
	}
	return ""
}

func isMonthFile(name string) bool {
	m, ok := strings.CutSuffix(name, ".jsonl")
	if !ok || len(m) != len(monthLayout) {
		return false
	}
	_, err := time.Parse(monthLayout, m)
	return err == nil
}

// Compact moves the spool files recorded before now's month (UTC) into
// registry/YYYY-MM.jsonl: each month file is rewritten through a temp file and
// rename with the new envelopes appended, and only then are the spool files
// deleted. An envelope already in the month file (an earlier run stopped
// before deleting) is not added again, so Compact is safe to rerun. Invalid
// spool files are left where they are. Concurrent Compact calls exclude each
// other through registry/compact.lock.
func Compact(home string, now time.Time) error {
	now = now.UTC()
	cutoff := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	spool, err := readSpoolDir(home)
	if err != nil || len(spool) == 0 {
		return err
	}
	byMonth := map[string][]spoolFile{}
	for _, s := range spool {
		if s.problem == "" && s.env.RecordedAt.Before(cutoff) {
			m := s.env.RecordedAt.UTC().Format(monthLayout)
			byMonth[m] = append(byMonth[m], s)
		}
	}
	if len(byMonth) == 0 {
		return nil
	}
	unlock, err := lock(analyze.RegistryDir(home))
	if err != nil {
		return err
	}
	defer unlock()

	months := make([]string, 0, len(byMonth))
	for m := range byMonth {
		months = append(months, m)
	}
	slices.Sort(months)
	for _, m := range months {
		if err := compactMonth(home, m, byMonth[m]); err != nil {
			return err
		}
	}
	return nil
}

func compactMonth(home, month string, files []spoolFile) error {
	regDir := analyze.RegistryDir(home)
	path := filepath.Join(regDir, month+".jsonl")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	present := map[string]bool{}
	for _, line := range bytes.Split(old, []byte("\n")) {
		var cl struct {
			Spool string `json:"spool"`
		}
		if json.Unmarshal(line, &cl) == nil && cl.Spool != "" {
			present[cl.Spool] = true
		}
	}
	buf := bytes.Clone(old)
	if len(buf) > 0 && buf[len(buf)-1] != '\n' {
		buf = append(buf, '\n')
	}
	added := false
	for _, s := range files {
		if present[s.name] {
			continue
		}
		env := s.env
		env.File = ""
		line, err := json.Marshal(compactLine{Envelope: env, Spool: s.name})
		if err != nil {
			return err
		}
		buf = append(append(buf, line...), '\n')
		present[s.name] = true
		added = true
	}
	if added {
		if err := writeAtomic(regDir, path, buf); err != nil {
			return err
		}
	}
	spoolDir := analyze.SpoolDir(home)
	for _, s := range files {
		if err := os.Remove(filepath.Join(spoolDir, s.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// writeAtomic replaces path with data through a temp file in dir.
func writeAtomic(dir, path string, data []byte) error {
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// lock takes registry/compact.lock, taking over a lock older than staleLock.
func lock(regDir string) (unlock func(), err error) {
	path := filepath.Join(regDir, "compact.lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > staleLock {
			os.Remove(path)
			continue
		}
		break
	}
	return nil, errors.New("registry compaction already in progress (" + path + ")")
}

// Prune deletes the compacted months that ended before before.
func Prune(home string, before time.Time) error {
	regDir := analyze.RegistryDir(home)
	entries, err := os.ReadDir(regDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !isMonthFile(name) {
			continue
		}
		start, _ := time.Parse(monthLayout, strings.TrimSuffix(name, ".jsonl"))
		if !start.AddDate(0, 1, 0).After(before) {
			if err := os.Remove(filepath.Join(regDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
