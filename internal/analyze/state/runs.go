package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// LoadIdentity reads state/task_identity.json, upgrading older versions. A
// missing file is an empty identity; the maps are never nil.
func LoadIdentity(home string) (analyze.TaskIdentity, error) {
	id := analyze.TaskIdentity{V: current[kindIdentity]}
	b, err := readVersioned(identPath(home), kindIdentity)
	if err == nil && b != nil {
		if err = json.Unmarshal(b, &id); err != nil {
			err = fmt.Errorf("%s: %w", IdentFile, err)
		}
	}
	id.V = current[kindIdentity]
	if id.ByFound == nil {
		id.ByFound = map[string]string{}
	}
	if id.Aliases == nil {
		id.Aliases = map[string]string{}
	}
	return id, err
}

// SaveIdentity writes state/task_identity.json atomically.
func SaveIdentity(home string, id analyze.TaskIdentity) error {
	id.V = current[kindIdentity]
	if id.ByFound == nil {
		id.ByFound = map[string]string{}
	}
	if id.Aliases == nil {
		id.Aliases = map[string]string{}
	}
	return WriteJSONAtomic(identPath(home), id)
}

// Run is one line of state/runs.jsonl.
type Run struct {
	RunID             string            `json:"run_id"`
	RanAt             time.Time         `json:"ran_at"`
	Window            analyze.Window    `json:"window"`
	Sources           int               `json:"sources"`
	Sessions          int               `json:"sessions"`
	Events            int               `json:"events"`
	AdapterVersions   map[string]string `json:"adapter_versions,omitempty"`
	MetricsVersion    string            `json:"metrics_version"`
	RubricVersion     string            `json:"rubric_version,omitempty"`
	RedactionVersion  string            `json:"redaction_version,omitempty"`
	ReviewTokensSpent int               `json:"review_tokens_spent,omitempty"`
	Notes             string            `json:"notes,omitempty"`
	// The run's scope: the --tools and --project filters it ran with. Empty
	// means all; lines written before the scope was stored have none.
	Tools   []string `json:"tools,omitempty"`
	Project string   `json:"project,omitempty"`
}

// Cursor reports whether the next default run may start where this one
// ended: an analyzer run (non-zero window) over every tool and project. A
// --tools or --project run saw only part of the sessions, and a review note
// line (run-note) records spend for a run, not a window of its own. Lines
// without a scope (older versions) count as full runs.
func (r Run) Cursor() bool {
	return len(r.Tools) == 0 && r.Project == "" && !r.Window.Until.IsZero()
}

type runLine struct {
	V int `json:"v"`
	Run
}

// runIDLayout is NewRunID's format: UTC, lexically sortable.
const runIDLayout = "20060102T150405Z"

// NewRunID returns a sortable run id for now, e.g. 20261008T121500Z.
func NewRunID(now time.Time) string { return now.UTC().Format(runIDLayout) }

// ParseRunID is the inverse of NewRunID.
func ParseRunID(id string) (time.Time, error) { return time.Parse(runIDLayout, id) }

// AppendRun appends one line to state/runs.jsonl and fsyncs it. The review
// skill appends a second line with the same run id for its token spend;
// readers combine lines by run id as they need.
func AppendRun(home string, r Run) error {
	if r.RunID == "" {
		return errors.New("run without run_id")
	}
	b, err := json.Marshal(runLine{V: current[kindRun], Run: r})
	if err != nil {
		return err
	}
	p := runsPath(home)
	if err := os.MkdirAll(filepath.Dir(p), dirPerm); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	// A previous crash may have left a partial last line; start on a fresh one.
	prefix := []byte{}
	if last, err := lastByte(p); err == nil && last != '\n' {
		prefix = []byte{'\n'}
	}
	_, err = f.Write(append(append(prefix, b...), '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func lastByte(p string) (byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(-1, io.SeekEnd); err != nil {
		return 0, err
	}
	var b [1]byte
	_, err = io.ReadFull(f, b[:])
	return b[0], err
}

// LoadRuns reads state/runs.jsonl in order. A line that is not valid JSON
// (cut short by a crash; AppendRun starts the next line fresh after one) is
// skipped; a valid line with a bad version or shape is an error.
func LoadRuns(home string) ([]Run, error) {
	data, err := os.ReadFile(runsPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Run
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	n := 0
	for sc.Scan() {
		n++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		b, err := upgrade(kindRun, line)
		var rl runLine
		if err == nil {
			err = json.Unmarshal(b, &rl)
		}
		if err != nil {
			var syn *json.SyntaxError
			if errors.As(err, &syn) || errors.Is(err, io.ErrUnexpectedEOF) {
				continue // torn write
			}
			return out, fmt.Errorf("%s line %d: %w", RunsFile, n, err)
		}
		out = append(out, rl.Run)
	}
	return out, sc.Err()
}
