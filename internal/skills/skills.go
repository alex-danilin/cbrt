// Package skills holds the Claude Code skills cbrt installs, one folder per
// skill (<name>/SKILL.md, the layout of ~/.claude/skills), and installs and
// removes them. Adding a skill means adding a folder; skills not listed in
// Required are optional and installed with `analyze install claude --with`.
package skills

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze/state"
)

//go:embed */SKILL.md
var files embed.FS

// Required are the skills every install writes.
var Required = []string{"cbrt-multi-session-review"}

// Marker is the line every installed skill carries; uninstall removes only
// files that have it, and install never overwrites a skill without it, so a
// hand-made skill of the same name is never touched.
const Marker = "<!-- Installed by cbrt analyze install claude: reinstalling overwrites this file and uninstall removes it. -->"

// Names lists every skill, sorted.
func Names() []string {
	ents, _ := files.ReadDir(".")
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names // ReadDir sorts by name
}

// Optional lists the skills not in Required, sorted.
func Optional() []string {
	var names []string
	for _, n := range Names() {
		if !slices.Contains(Required, n) {
			names = append(names, n)
		}
	}
	return names
}

// Render fills skill name's placeholders for the cbrt binary at exe and the
// rubric at rubricPath; ok is false for an unknown name. Paths use forward
// slashes so the skill text is the same on every OS.
func Render(name, exe, rubricPath string) (text string, ok bool) {
	b, err := files.ReadFile(name + "/SKILL.md")
	if err != nil {
		return "", false
	}
	r := strings.NewReplacer(
		"{marker}", Marker,
		"{cbrt}", filepath.ToSlash(exe),
		"{rubric}", filepath.ToSlash(rubricPath),
		"{args}", "$ARGUMENTS",
	)
	return r.Replace(string(b)), true
}

// Dir is the folder of skill name in the Claude Code config dir.
func Dir(claudeDir, name string) string { return filepath.Join(claudeDir, "skills", name) }

// Owner says who made a skill's SKILL.md.
type Owner int

const (
	Absent Owner = iota // no SKILL.md
	Ours                // carries Marker
	Other               // made by someone else
)

// OwnerOf reports who made <claudeDir>/skills/<name>/SKILL.md.
func OwnerOf(claudeDir, name string) (Owner, error) {
	b, err := os.ReadFile(filepath.Join(Dir(claudeDir, name), "SKILL.md"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Absent, nil
	case err != nil:
		return Absent, err
	case strings.Contains(string(b), Marker):
		return Ours, nil
	}
	return Other, nil
}

// Install writes skill name unless a skill of that name not made by cbrt is
// there: that one is kept, and kept is true.
func Install(claudeDir, name, exe, rubricPath string) (path string, kept bool, err error) {
	text, ok := Render(name, exe, rubricPath)
	if !ok {
		return "", false, fmt.Errorf("unknown skill %q", name)
	}
	path = filepath.Join(Dir(claudeDir, name), "SKILL.md")
	switch o, err := OwnerOf(claudeDir, name); {
	case err != nil:
		return "", false, err
	case o == Other:
		return path, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	return path, false, state.WriteFileAtomic(path, []byte(text))
}

// Installed lists the skills cbrt made in claudeDir, sorted: every skill
// folder whose SKILL.md carries Marker, including skills this cbrt no longer
// ships (Stale).
func Installed(claudeDir string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(claudeDir, "skills"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		o, err := OwnerOf(claudeDir, e.Name())
		if err != nil {
			return nil, err
		}
		if o == Ours {
			names = append(names, e.Name())
		}
	}
	return names, nil // ReadDir sorts by name
}

// Stale lists the skills cbrt made in claudeDir that this cbrt no longer
// ships (a renamed or removed skill); install removes them.
func Stale(claudeDir string) ([]string, error) {
	installed, err := Installed(claudeDir)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, n := range installed {
		if !slices.Contains(Names(), n) {
			stale = append(stale, n)
		}
	}
	return stale, nil
}
