package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
)

// EnvFile loads a KEY=VALUE file into the process environment, repeatably.
// Variables present in the original process environment (when NewEnvFile ran) always win;
// every other key follows the file, so edits and removals take effect on the next Load.
type EnvFile struct {
	path   string
	mu     sync.Mutex
	orig   map[string]bool // keys set in the process environment at creation
	loaded map[string]bool // keys this EnvFile set on the previous Load
}

// envKey is the identity of a variable name: Windows environment names are
// case-insensitive, so a file's "Path" is the process's "PATH" there.
func envKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

// NewEnvFile snapshots which variables the process environment sets; it does not read the file.
func NewEnvFile(path string) *EnvFile {
	e := &EnvFile{path: path, orig: map[string]bool{}, loaded: map[string]bool{}}
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "" {
			e.orig[envKey(k)] = true
		}
	}
	return e
}

// Load (re)reads the file: it sets every key not in the original process environment,
// overwriting values from earlier loads, and unsets keys an earlier load set that the
// file no longer has. A missing file is not an error (it unsets all previously loaded keys).
// On a read error the environment is left unchanged.
func (e *EnvFile) Load() error {
	vars, err := readEnvFile(e.path)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	next := map[string]bool{}
	var errs []error
	for k, v := range vars {
		if e.orig[envKey(k)] {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			errs = append(errs, fmt.Errorf("env file: %s: %w", k, err))
			continue
		}
		next[envKey(k)] = true
	}
	for k := range e.loaded {
		if !next[k] {
			os.Unsetenv(k)
		}
	}
	e.loaded = next
	return errors.Join(errs...)
}

// LoadEnvFile sets variables from a KEY=VALUE file without overriding ones already set.
// A missing file is not an error. Use EnvFile when the file is loaded more than once.
func LoadEnvFile(path string) error { return NewEnvFile(path).Load() }

// readEnvFile parses a KEY=VALUE file; a missing file yields no variables.
// Blank lines and # comments are skipped, an "export " prefix is allowed, surrounding
// quotes are trimmed from values, and the first occurrence of a key wins.
func readEnvFile(path string) (map[string]string, error) {
	vars := map[string]string{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return vars, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, dup := vars[k]; !dup && k != "" {
			vars[k] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return vars, nil
}
