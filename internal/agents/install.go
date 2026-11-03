package agents

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Marker is the comment line every agent file cbrt installs carries. A target
// file with it is cbrt's own and is replaced; one without it is backed up.
const Marker = "Installed by cbrt agents install: reinstalling overwrites this file and uninstall removes it."

// InstallOptions control Install.
type InstallOptions struct {
	Tools    []Tool // empty = all
	Write    bool   // false: report only
	Home     string // the canonical folder, "" = Home()
	UserHome string // "" = os.UserHomeDir()
	Out      io.Writer
}

// codexName is the agent name written to a Codex agent file and its file
// name: unchanged, since Codex CLI 0.163.0 spawns agents with hyphenated
// names (probe 2026-11-02, docs/internal).
func codexName(name string) string { return name }

// TargetPath is where tool t reads the agent named name, under userHome.
func TargetPath(t Tool, userHome, name string) string {
	switch t {
	case Antigravity:
		return filepath.Join(userHome, ".gemini", "config", "agents", name, "agent.md")
	case Codex:
		return filepath.Join(userHome, ".codex", "agents", codexName(name)+".toml")
	}
	return filepath.Join(userHome, ".claude", "agents", name+".md")
}

// Render returns the native file for tool t.
func Render(a Agent, t Tool) string {
	switch t {
	case Antigravity:
		return RenderAntigravity(a)
	case Codex:
		return RenderCodex(a)
	}
	return RenderClaude(a)
}

// carried reports whether a Plan note's field is written to the tool's file.
func carried(n Note) bool { return n.Code == Kept || n.Code == Mapped }

// planFor returns the Plan notes of a's own fields, by key.
func planFor(a Agent, t Tool) map[string]Note {
	m := map[string]Note{}
	for _, n := range Plan(a, t)[3:] {
		m[n.Field] = n
	}
	return m
}

// RenderClaude writes a as a Claude Code agent: every field Plan keeps.
func RenderClaude(a Agent) string {
	plan := planFor(a, Claude)
	out := Agent{Name: a.Name, Description: a.Description, Body: a.Body}
	for _, f := range a.Fields {
		if carried(plan[f.Key]) {
			out.Fields = append(out.Fields, f)
		}
	}
	return Format(out, Marker)
}

// RenderAntigravity writes a as an Antigravity agent. Tools get Antigravity
// names; a tool list Plan warns about is still written with the names that
// map (an agent with fewer tools, never one with all of them). mcpServers
// becomes inheritMcp: true, and mainAgent and subagent default to true.
func RenderAntigravity(a Agent) string {
	plan := planFor(a, Antigravity)
	out := Agent{Name: a.Name, Description: a.Description, Body: a.Body}
	inherit := false
	for _, f := range a.Fields {
		switch {
		case f.Key == "tools":
			mapped, _ := mapTools(f.Value.List, toAntigravity)
			out.Fields = append(out.Fields, Field{"tools", Value{Kind: List, List: mapped}})
		case f.Key == "mcpServers":
			inherit = true
		case carried(plan[f.Key]):
			out.Fields = append(out.Fields, f)
		}
	}
	add := func(key string) {
		if _, ok := out.Get(key); !ok {
			out.Fields = append(out.Fields, Field{key, Value{Kind: Scalar, Str: "true"}})
		}
	}
	add("mainAgent")
	add("subagent")
	if inherit {
		add("inheritMcp")
	}
	return Format(out, Marker)
}

// RenderCodex writes a as a Codex agent: name, description, the body as
// developer_instructions, model_reasoning_effort, and features.shell_tool =
// false when Plan turns the shell off.
func RenderCodex(a Agent) string {
	var b strings.Builder
	b.WriteString("# " + Marker + "\n")
	b.WriteString("name = " + tomlBasic(codexName(a.Name)) + "\n")
	b.WriteString("description = " + tomlBasic(a.Description) + "\n")
	shellOff := false
	for _, n := range Plan(a, Codex)[3:] {
		if strings.Contains(n.Detail, "features.shell_tool = false") {
			shellOff = true
		}
		if n.Field == "effort" && n.Code == Mapped {
			v, _ := a.Get("effort")
			effort := v.Str
			if effort == "xhigh" || effort == "max" {
				effort = "high"
			}
			b.WriteString("model_reasoning_effort = " + tomlBasic(effort) + "\n")
		}
	}
	if shellOff {
		b.WriteString("features.shell_tool = false\n")
	}
	body := strings.TrimLeft(a.Body, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	b.WriteString("developer_instructions = " + quoteTOML(body) + "\n")
	return b.String()
}

// tomlBasic writes s as a one-line TOML basic string.
func tomlBasic(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Install renders the canonical agent name for each tool and, with Write,
// writes the files. A target file without Marker is backed up once to
// <file>.bak; when that backup already exists the tool is refused (a backup
// is never overwritten) and the others still run.
func Install(name string, o InstallOptions) error {
	o, err := o.resolved()
	if err != nil {
		return err
	}
	err = installAgent(name, o)
	o.dryRunNote()
	return err
}

// InstallAll installs every agent in the canonical folder, in name order. An
// agent that fails is reported and the others still run; the error names
// every agent that was not fully installed.
func InstallAll(o InstallOptions) error {
	o, err := o.resolved()
	if err != nil {
		return err
	}
	names, err := importedNames(o.Home)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no agents in %s: run cbrt agents import <file>", o.Home)
	}
	var failed []string
	for i, n := range names {
		if i > 0 {
			fmt.Fprintln(o.Out)
		}
		fmt.Fprintf(o.Out, "%s\n", n)
		if err := installAgent(n, o); err != nil {
			fmt.Fprintf(o.Out, "  error: %v\n", err)
			failed = append(failed, n)
		}
	}
	o.dryRunNote()
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d agents not fully installed: %s", len(failed), len(names), strings.Join(failed, ", "))
	}
	return nil
}

// resolved fills in the defaults: Out, Home and UserHome.
func (o InstallOptions) resolved() (InstallOptions, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Home == "" {
		o.Home = Home()
	}
	if o.UserHome == "" {
		var err error
		if o.UserHome, err = os.UserHomeDir(); err != nil {
			return o, err
		}
	}
	return o, nil
}

func (o InstallOptions) dryRunNote() {
	if !o.Write {
		fmt.Fprintln(o.Out, "dry run: nothing changed; add --write to apply")
	}
}

// installAgent installs one agent with resolved options.
func installAgent(name string, o InstallOptions) error {
	out, home, userHome := o.Out, o.Home, o.UserHome
	canon, err := CanonicalName(name)
	if err != nil {
		return err
	}
	src := filepath.Join(home, canon+".md")
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no agent %s in %s (%s)", canon, home, available(home))
	}
	a, err := Read(src)
	if err != nil {
		return err
	}
	// The name becomes a path in each tool's folder: it must be the one the
	// file name was checked as, not whatever an edit put in the frontmatter.
	if a.Name != canon {
		return fmt.Errorf("%s: its name is %q, want %q (the file name)", src, a.Name, canon)
	}
	tools := o.Tools
	if len(tools) == 0 {
		tools = Tools
	}
	var failed []string
	for _, t := range Tools {
		if !slices.Contains(tools, t) {
			continue
		}
		path := TargetPath(t, userHome, a.Name)
		action, err := installOne(path, Render(a, t), o.Write)
		if err != nil {
			fmt.Fprintf(out, "%-12s refused: %v\n", t, err)
			failed = append(failed, string(t))
		} else {
			fmt.Fprintf(out, "%-12s %s %s\n", t, action, path)
		}
		for _, n := range Plan(a, t) {
			if n.Code == Dropped || n.Code == Warned {
				fmt.Fprintf(out, "  %c %-22s %s\n", n.Code, n.Field, n.Detail)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("not installed for %s", strings.Join(failed, ", "))
	}
	return nil
}

// installOne writes text to path (when write is set) and says what it did.
func installOne(path, text string, write bool) (string, error) {
	would := func(s string) string {
		if write {
			return s
		}
		return "would " + s
	}
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if write {
			if err := writeAtomic(path, []byte(text)); err != nil {
				return "", err
			}
		}
		return would("write"), nil
	case err != nil:
		return "", err
	case strings.Contains(string(old), Marker):
		if write {
			if err := writeAtomic(path, []byte(text)); err != nil {
				return "", err
			}
		}
		return would("replace"), nil
	}
	bak := path + ".bak"
	if _, err := os.Stat(bak); err == nil {
		return "", fmt.Errorf("%s is not cbrt's and %s already exists; move one of them away", path, bak)
	}
	if write {
		f, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return "", err
		}
		_, err = f.Write(old)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(bak)
			return "", err
		}
		if err := writeAtomic(path, []byte(text)); err != nil {
			return "", err
		}
	}
	return would("back up to " + filepath.Base(bak) + ", then write"), nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".cbrt-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// available lists the agents in the canonical folder, for an error message.
func available(home string) string {
	names, _ := importedNames(home)
	if len(names) == 0 {
		return "none imported yet: run cbrt agents import <file>"
	}
	return "available: " + strings.Join(names, ", ")
}
