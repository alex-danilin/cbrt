// Package state is the only code that writes durable state under
// CBRT_ANALYZE_HOME (architecture doc §3.2.5): recommendations, task
// identity, the run log and the per-session metrics archive. Every file
// carries "v"; older versions are upgraded on load (migrate.go) and a newer,
// unknown version is an error. Whole-file writes go through WriteJSONAtomic,
// so a crash leaves the previous version intact.
//
// The package does no locking: one `cbrt analyze` process at a time is
// assumed to write state.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// File locations, relative to the analyzer home.
const (
	StateDir    = "state"
	ArchiveDir  = "archive"
	OutDir      = "out"
	RecsFile    = "recommendations.json"
	IdentFile   = "task_identity.json"
	RunsFile    = "runs.jsonl"
	ExportFile  = "recommendations.md"
	dirPerm     = 0o700
	archiveGlob = "sessions-*.json"
)

// rename is os.Rename; tests replace it to simulate a failed rename step.
var rename = os.Rename

// WriteJSONAtomic writes v as indented JSON to path: a temp file in the same
// directory (mode 0600), fsync, then rename over path. On any failure the
// temp file is removed and the previous content of path is left untouched.
func WriteJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeAtomic(path, b)
}

// WriteFileAtomic writes data to path the same way as WriteJSONAtomic.
func WriteFileAtomic(path string, data []byte) error { return writeAtomic(path, data) }

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp") // 0600
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
		err = rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes the rename durable where the platform allows it (best effort;
// directories cannot be fsynced on Windows).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// readVersioned reads path and upgrades it to the current version of kind.
// A missing file returns (nil, nil).
func readVersioned(path, kind string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, fmt.Errorf("%s: empty file", path)
	}
	out, err := upgrade(kind, b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

func recsPath(home string) string  { return filepath.Join(home, StateDir, RecsFile) }
func identPath(home string) string { return filepath.Join(home, StateDir, IdentFile) }
func runsPath(home string) string  { return filepath.Join(home, StateDir, RunsFile) }
