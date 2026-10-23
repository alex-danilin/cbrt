package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	inventory "github.com/alex-danilin/cbrt/internal/analyze/projects"
	"github.com/alex-danilin/cbrt/internal/analyze/state"
)

// HomeEnv overrides the folder that holds projects.json.
const HomeEnv = "CBRT_PROJECTS_HOME"

// ListFile is the project list's file name.
const ListFile = "projects.json"

const listVersion = 1

// Home is $CBRT_PROJECTS_HOME, else <UserConfigDir>/cbrt, else
// ~/.cbrt when the platform has no config directory.
func Home() string {
	if h := os.Getenv(HomeEnv); h != "" {
		return filepath.Clean(h)
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "cbrt")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cbrt")
	}
	return ".cbrt"
}

// Entry is one connected project.
type Entry struct {
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	ConnectedAt time.Time `json:"connected_at"`
	// PendingCommit: files connect created after a git init but could not
	// commit (no git identity, a failing hook); the next connect retries
	// while the repository still has no commits.
	PendingCommit []string `json:"pending_commit,omitempty"`
	// Visibility is Public or Private ("" in lists written before it
	// existed: unknown until the next connect); VisibilityHow says how it
	// was decided. A public project's plan files are kept out of git in
	// .git/info/exclude.
	Visibility    string `json:"visibility,omitempty"`
	VisibilityHow string `json:"visibility_how,omitempty"`
	// CreatedAgents: connect created AGENTS.md, so a public repository's
	// exclude block also lists it.
	CreatedAgents bool `json:"created_agents,omitempty"`
}

type listFile struct {
	V        int     `json:"v"`
	Projects []Entry `json:"projects"`
}

// Load reads the project list in home; a missing file is an empty list.
func Load(home string) ([]Entry, error) {
	b, err := os.ReadFile(filepath.Join(home, ListFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f listFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", ListFile, err)
	}
	if f.V > listVersion {
		return nil, fmt.Errorf("%s: version %d is newer than this cbrt understands (%d)", ListFile, f.V, listVersion)
	}
	return f.Projects, nil
}

// Save writes the project list atomically.
func Save(home string, es []Entry) error {
	if es == nil {
		es = []Entry{}
	}
	return state.WriteJSONAtomic(filepath.Join(home, ListFile), listFile{V: listVersion, Projects: es})
}

// Find returns the index of the entry named key, else of the entry whose
// path is key; -1 when none matches.
func Find(es []Entry, key string) int {
	for i, e := range es {
		if e.Name == key {
			return i
		}
	}
	for i, e := range es {
		if inventory.SamePath(e.Path, key) {
			return i
		}
	}
	return -1
}

// findPath returns the index of the entry for dir, or -1.
func findPath(es []Entry, dir string) int {
	for i, e := range es {
		if inventory.SamePath(e.Path, dir) {
			return i
		}
	}
	return -1
}

// baseName is the last element of p (either separator).
func baseName(p string) string {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return p
	}
	return parts[len(parts)-1]
}

// uniqueName is the folder's name, with "-2", "-3"… when another project
// already uses it.
func uniqueName(es []Entry, dir string) string {
	base := baseName(dir)
	taken := map[string]bool{}
	for _, e := range es {
		taken[e.Name] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		if n := base + "-" + strconv.Itoa(i); !taken[n] {
			return n
		}
	}
}
