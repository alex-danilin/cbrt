package agents

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// UninstallOptions control Uninstall; the fields mean what they mean for
// InstallOptions.
type UninstallOptions struct {
	Tools    []Tool
	Write    bool
	UserHome string
	Out      io.Writer
}

// Uninstall removes the agent's files that carry Marker from each tool and
// restores a <file>.bak that install made. A file without Marker is not
// cbrt's and stays. The canonical copy stays too: import it again or delete
// it by hand.
func Uninstall(name string, o UninstallOptions) error {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	canon, err := CanonicalName(name)
	if err != nil {
		return err
	}
	userHome := o.UserHome
	if userHome == "" {
		if userHome, err = os.UserHomeDir(); err != nil {
			return err
		}
	}
	var failed []string
	for _, t := range Tools {
		if len(o.Tools) > 0 && !slices.Contains(o.Tools, t) {
			continue
		}
		path := TargetPath(t, userHome, canon)
		action, err := uninstallOne(t, path, o.Write)
		if err != nil {
			fmt.Fprintf(out, "%-12s refused: %v\n", t, err)
			failed = append(failed, string(t))
			continue
		}
		fmt.Fprintf(out, "%-12s %s\n", t, action)
	}
	if !o.Write {
		fmt.Fprintln(out, "dry run: nothing changed; add --write to apply")
	}
	if len(failed) > 0 {
		return fmt.Errorf("not uninstalled for %s", strings.Join(failed, ", "))
	}
	return nil
}

func uninstallOne(t Tool, path string, write bool) (string, error) {
	would := func(s string) string {
		if write {
			return s
		}
		return "would " + s
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "not installed: " + path, nil
	}
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(raw), Marker) {
		return "", fmt.Errorf("%s is not cbrt's: left alone", path)
	}
	bak := path + ".bak"
	_, berr := os.Stat(bak)
	restore := berr == nil
	if write {
		if restore {
			if err := os.Rename(bak, path); err != nil { // replaces cbrt's file
				return "", err
			}
		} else {
			if err := os.Remove(path); err != nil {
				return "", err
			}
			if t == Antigravity { // the <name>/ folder install created
				os.Remove(filepath.Dir(path)) // fails, harmlessly, when not empty
			}
		}
	}
	if restore {
		return would("restore " + filepath.Base(bak) + " to " + path), nil
	}
	return would("remove " + path), nil
}

// State is an agent's state in one tool.
type State string

const (
	NotInstalled State = "-"
	Installed    State = "installed"
	NotCbrts     State = "not cbrt's" // a file of the user's has the name
)

// Entry is one row of ListAgents.
type Entry struct {
	Name      string
	Canonical bool // the canonical copy exists
	States    map[Tool]State
}

// ListAgents reports every canonical agent and, per tool, whether it is installed.
// Agents installed by cbrt whose canonical copy is gone are listed too.
func ListAgents(home, userHome string) ([]Entry, error) {
	if home == "" {
		home = Home()
	}
	if userHome == "" {
		var err error
		if userHome, err = os.UserHomeDir(); err != nil {
			return nil, err
		}
	}
	names := map[string]bool{}
	ents, err := os.ReadDir(home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".md"); ok && !e.IsDir() && nameRe.MatchString(n) {
			names[n] = true
		}
	}
	installed := map[string]bool{}
	for _, t := range Tools {
		for _, n := range markedIn(t, userHome) {
			installed[n] = true
		}
	}
	all := map[string]bool{}
	for n := range names {
		all[n] = true
	}
	for n := range installed {
		all[n] = true
	}
	var out []Entry
	for n := range all {
		e := Entry{Name: n, Canonical: names[n], States: map[Tool]State{}}
		for _, t := range Tools {
			e.States[t] = stateOf(TargetPath(t, userHome, n))
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func stateOf(path string) State {
	raw, err := os.ReadFile(path)
	switch {
	case err != nil:
		return NotInstalled
	case strings.Contains(string(raw), Marker):
		return Installed
	}
	return NotCbrts
}

// markedIn returns the names of cbrt- agents in tool t's folder that carry
// Marker.
func markedIn(t Tool, userHome string) []string {
	dir := filepath.Dir(TargetPath(t, userHome, "x"))
	if t == Antigravity {
		dir = filepath.Dir(dir) // agents/<name>/agent.md
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		n := e.Name()
		if t != Antigravity {
			n = strings.TrimSuffix(strings.TrimSuffix(n, ".md"), ".toml")
		}
		if !strings.HasPrefix(n, Prefix) || !nameRe.MatchString(n) {
			continue
		}
		if stateOf(TargetPath(t, userHome, n)) == Installed {
			names = append(names, n)
		}
	}
	return names
}

// WriteList prints the rows of ListAgents.
func WriteList(w io.Writer, es []Entry) {
	if len(es) == 0 {
		fmt.Fprintln(w, "no agents imported yet: run cbrt agents import <file>")
		return
	}
	fmt.Fprintf(w, "%-34s %-11s %-12s %s\n", "name", "claude", "antigravity", "codex")
	for _, e := range es {
		name := e.Name
		if !e.Canonical {
			name += " (no canonical copy)"
		}
		fmt.Fprintf(w, "%-34s %-11s %-12s %s\n", name, e.States[Claude], e.States[Antigravity], e.States[Codex])
	}
}
