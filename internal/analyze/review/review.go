// Package review holds the review rubric the session-review skills follow
// (the skills themselves are in internal/skills) and installs it into the
// analyzer home.
package review

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed RUBRIC.md
var rubric string

// RubricVersion is the "Rubric version" line of the embedded rubric.
var RubricVersion = rubricVersion(rubric)

// rubricVersion is the "Rubric version: " value of a rubric text, "unknown"
// when there is none.
func rubricVersion(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Rubric version: "); ok {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

// Rubric returns the embedded rubric.
func Rubric() string { return rubric }

// RubricPath is where the installed rubric lives.
func RubricPath(home string) string { return filepath.Join(home, "RUBRIC.md") }

// InstallRubric writes the embedded rubric to the analyzer home (local
// additions belong in RUBRIC.local.md). A missing copy is written. An older
// version is saved next to it as RUBRIC.md.v<old>-backup-<UTC time> (an
// existing backup is never overwritten) and then replaced. A newer version,
// or the same version with other text (edited by hand), is kept. note is one
// line for the user whenever the installed copy was kept or replaced.
func InstallRubric(home string, now time.Time) (path, note string, err error) {
	p := RubricPath(home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", "", err
	}
	old, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return p, "", writeAtomic(p, []byte(rubric))
	case err != nil:
		return "", "", err
	case string(old) == rubric:
		return p, "", nil
	}
	oldV := rubricVersion(string(old))
	have, want := versionNum(oldV), versionNum(RubricVersion)
	switch {
	case have > want:
		return p, fmt.Sprintf("kept %s: its version %s is newer than this cbrt's %s", p, oldV, RubricVersion), nil
	case have == want:
		return p, fmt.Sprintf("kept %s: it differs from the built-in version %s (edited by hand?); move local additions to RUBRIC.local.md and delete RUBRIC.md to get the built-in one", p, RubricVersion), nil
	}
	backup, err := saveBackup(p, oldV, old, now)
	if err != nil {
		return "", "", fmt.Errorf("backing up the installed rubric: %w", err)
	}
	if err := writeAtomic(p, []byte(rubric)); err != nil {
		return "", "", err
	}
	return p, fmt.Sprintf("replaced %s (version %s) with version %s; the old copy is in %s", p, oldV, RubricVersion, backup), nil
}

// saveBackup writes b to <p>.v<version>-backup-<UTC time>, adding -2, -3, …
// rather than overwriting an existing file.
func saveBackup(p, version string, b []byte, now time.Time) (string, error) {
	base := fmt.Sprintf("%s.v%s-backup-%s", p, version, now.UTC().Format("20060102T150405Z"))
	for i := 1; i < 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return name, err
	}
	return "", errors.New("too many backups with the same name")
}

// versionNum parses a rubric version; a missing or malformed one is 0.
func versionNum(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeAtomic writes b to a temp file next to path and renames it over path.
func writeAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
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
