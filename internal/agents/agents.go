// Package agents converts custom agent definitions between Claude Code,
// Antigravity CLI and Codex CLI. Import reads one tool's agent file into a
// canonical Markdown file in cbrt's agents folder, named cbrt-<name>, and
// reports what each tool would keep, map or drop when the agent is installed.
//
// The canonical file uses Claude Code's frontmatter (the widest schema) and
// Claude tool names; Antigravity-only fields are kept beside them. Nothing a
// source file holds is dropped silently: every field ends in a Note.
package agents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HomeEnv overrides the agents folder.
const HomeEnv = "CBRT_AGENTS_HOME"

// Prefix starts the name of every agent cbrt writes, as for cbrt's skills.
const Prefix = "cbrt-"

const bom = string(rune(0xFEFF))

// Home is $CBRT_AGENTS_HOME, else <UserConfigDir>/cbrt/agents, else
// ~/.cbrt/agents when the platform has no config directory.
func Home() string {
	if h := os.Getenv(HomeEnv); h != "" {
		return filepath.Clean(h)
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "cbrt", "agents")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cbrt", "agents")
	}
	return filepath.Join(".cbrt", "agents")
}

// Tool is a CLI that reads agent files.
type Tool string

const (
	Claude      Tool = "claude"
	Antigravity Tool = "antigravity"
	Codex       Tool = "codex"
)

// Tools lists every target, in report order.
var Tools = []Tool{Claude, Antigravity, Codex}

// ParseTool accepts a tool name ("agy" for Antigravity).
func ParseTool(s string) (Tool, error) {
	switch s {
	case "claude":
		return Claude, nil
	case "antigravity", "agy":
		return Antigravity, nil
	case "codex":
		return Codex, nil
	}
	return "", fmt.Errorf("unknown tool %q: claude, antigravity or codex", s)
}

// Kind tells which member of a Value is set.
type Kind int

const (
	Scalar Kind = iota // Str
	List               // List
	Raw                // Raw: a nested YAML block or TOML text, kept verbatim
)

// Value is one frontmatter value.
type Value struct {
	Kind Kind
	Str  string
	List []string
	Raw  string
}

// Field is one frontmatter key and its value.
type Field struct {
	Key   string
	Value Value
}

// Agent is an agent in canonical form.
type Agent struct {
	Name, Description, Body string
	Fields                  []Field // every other frontmatter field, in order
	Source                  Tool
}

// Get returns a field's value.
func (a *Agent) Get(key string) (Value, bool) {
	for _, f := range a.Fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return Value{}, false
}

func (a *Agent) set(key string, v Value) {
	for i := range a.Fields {
		if a.Fields[i].Key == key {
			a.Fields[i].Value = v
			return
		}
	}
	a.Fields = append(a.Fields, Field{Key: key, Value: v})
}

// Code is what happens to a field.
type Code byte

const (
	Kept    Code = 'K'
	Mapped  Code = 'M'
	Dropped Code = 'D'
	Warned  Code = 'W' // dropped, and a restriction or safety setting is lost
)

// Note records what happened to one field.
type Note struct {
	Field  string
	Code   Code
	Detail string
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// CanonicalName adds the cbrt- prefix unless the name has it. Names are
// letters, digits, - and _ only: they become file names.
func CanonicalName(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("agent name %q: use letters, digits, - and _ only (at most 64)", name)
	}
	if strings.HasPrefix(strings.ToLower(name), Prefix) {
		return name, nil
	}
	return Prefix + name, nil
}

// Parse reads an agent file. from is the source tool, or "" to detect it:
// .toml is Codex; a .md file is Antigravity when it uses Antigravity-only
// fields or sits under .gemini or .agents, else Claude Code.
func Parse(path string, from Tool) (Agent, []Note, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Agent{}, nil, err
	}
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".toml":
		if from != "" && from != Codex {
			return Agent{}, nil, fmt.Errorf("%s: a .toml file is a Codex agent, not %s", path, from)
		}
		a, notes, err := fromCodex(string(raw))
		if err != nil {
			return Agent{}, nil, fmt.Errorf("%s: %w", path, err)
		}
		return a, notes, nil
	case ".md":
		if from == Codex {
			return Agent{}, nil, fmt.Errorf("%s: a Codex agent is a .toml file", path)
		}
		a, notes, err := fromMarkdown(string(raw), from, path)
		if err != nil {
			return Agent{}, nil, fmt.Errorf("%s: %w", path, err)
		}
		return a, notes, nil
	default:
		return Agent{}, nil, fmt.Errorf("%s: not an agent file (want .md or .toml, got %q)", path, ext)
	}
}

// Fields of each source format that cbrt understands; anything else is kept
// in the canonical file and dropped at install, with a note.
var (
	claudeFields = set("tools", "disallowedTools", "model", "permissionMode", "maxTurns", "skills", "memory",
		"color", "hooks", "mcpServers", "background", "effort", "isolation", "initialPrompt")
	antigravityOnly = set("mainAgent", "subagent", "hidden", "inheritMcp", "commandExecutionPolicy")
	listFields      = set("tools", "disallowedTools", "skills")
)

func set(keys ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func fromMarkdown(text string, from Tool, path string) (Agent, []Note, error) {
	front, body, err := splitFrontmatter(text)
	if err != nil {
		return Agent{}, nil, err
	}
	fields, err := parseYAML(front, 2)
	if err != nil {
		return Agent{}, nil, err
	}
	a := Agent{Body: body, Source: from}
	if a.Source == "" {
		a.Source = Claude
		for _, seg := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
			if seg == ".gemini" || seg == ".agents" {
				a.Source = Antigravity
			}
		}
		for _, f := range fields {
			if antigravityOnly[f.Key] {
				a.Source = Antigravity
			}
		}
	}
	var notes []Note
	for _, f := range fields {
		v := f.Value
		if listFields[f.Key] && v.Kind == Scalar { // Claude writes "tools: Read, Grep"
			v = Value{Kind: List, List: splitList(v.Str)}
		}
		switch {
		case f.Key == "name" || f.Key == "description":
			if v.Kind != Scalar {
				return Agent{}, nil, fmt.Errorf("%s must be a single value", f.Key)
			}
			if f.Key == "name" {
				a.Name = v.Str
			} else {
				a.Description = v.Str
			}
			continue
		case f.Key == "tools" && a.Source == Antigravity:
			mapped, lost := mapTools(v.List, fromAntigravity)
			if len(lost) > 0 {
				notes = append(notes, Note{"tools", Warned, strings.Join(lost, ", ") + " have no Claude Code equivalent: dropped"})
			}
			if len(mapped) > 0 {
				notes = append(notes, Note{"tools", Mapped, "as " + strings.Join(mapped, ", ")})
			}
			v = Value{Kind: List, List: mapped}
		case !claudeFields[f.Key] && !antigravityOnly[f.Key]:
			notes = append(notes, Note{f.Key, Kept, "not a field cbrt knows: kept here, dropped at install"})
		}
		a.set(f.Key, v)
	}
	if a.Name == "" {
		return Agent{}, nil, errors.New("the frontmatter has no name")
	}
	if a.Description == "" {
		return Agent{}, nil, errors.New("the frontmatter has no description")
	}
	return a, notes, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fromCodex(text string) (Agent, []Note, error) {
	fields, err := parseTOML(text)
	if err != nil {
		return Agent{}, nil, err
	}
	a := Agent{Source: Codex}
	var notes []Note
	grouped := map[string]bool{}
	drop := func(group, detail string) {
		if !grouped[group] {
			grouped[group] = true
			notes = append(notes, Note{group, Dropped, detail})
		}
	}
	for _, f := range fields {
		k, v := f.Key, f.Value
		str := func() (string, error) {
			if v.Kind != Scalar {
				return "", fmt.Errorf("%s must be a string", k)
			}
			return v.Str, nil
		}
		switch {
		case k == "name":
			if a.Name, err = str(); err != nil {
				return Agent{}, nil, err
			}
		case k == "description":
			if a.Description, err = str(); err != nil {
				return Agent{}, nil, err
			}
		case k == "developer_instructions":
			if a.Body, err = str(); err != nil {
				return Agent{}, nil, err
			}
		case k == "model_reasoning_effort":
			a.set("effort", v)
			notes = append(notes, Note{k, Mapped, "as effort"})
		case k == "sandbox_mode" && v.Str == "read-only":
			a.set("permissionMode", Value{Kind: Scalar, Str: "plan"})
			notes = append(notes, Note{k, Mapped, "as permissionMode: plan (Claude Code and Antigravity enforce it; Codex did not)"})
		case k == "sandbox_mode":
			drop(k, fmt.Sprintf("%q: the session's own sandbox decides", v.Str))
		case k == "features.shell_tool" && v.Str == "false":
			a.set("disallowedTools", Value{Kind: List, List: []string{"Bash", "PowerShell"}})
			notes = append(notes, Note{k, Mapped, "as disallowedTools: Bash, PowerShell"})
		case k == "model", k == "model_reasoning_summary", k == "model_verbosity":
			drop(k, "model settings don't port")
		case k == "nickname_candidates":
			drop(k, "Codex only")
		case strings.HasPrefix(k, "features."):
			drop(k, "a Codex capability switch")
		case strings.HasPrefix(k, "skills."):
			drop("skills", "Codex skill settings")
		case strings.HasPrefix(k, "mcp_servers."):
			server, _, _ := strings.Cut(strings.TrimPrefix(k, "mcp_servers."), ".")
			drop("mcp_servers."+server, "MCP server definitions are not copied: set the server up in each tool")
		default:
			drop(k, "not a Codex field cbrt knows")
		}
	}
	switch {
	case a.Name == "":
		return Agent{}, nil, errors.New("no name")
	case a.Description == "":
		return Agent{}, nil, errors.New("no description")
	case strings.TrimSpace(a.Body) == "":
		return Agent{}, nil, errors.New("no developer_instructions")
	}
	return a, notes, nil
}

// Format writes the canonical file.
func Format(a Agent, comment string) string {
	var b strings.Builder
	b.WriteString("---\n")
	for _, l := range strings.Split(comment, "\n") {
		if l != "" {
			b.WriteString("# " + l + "\n")
		}
	}
	b.WriteString("name: " + quoteYAML(a.Name) + "\n")
	b.WriteString("description: " + quoteYAML(a.Description) + "\n")
	for _, f := range a.Fields {
		switch v := f.Value; v.Kind {
		case Scalar:
			b.WriteString(f.Key + ": " + quoteYAML(v.Str) + "\n")
		case List:
			if len(v.List) == 0 {
				b.WriteString(f.Key + ": []\n")
				continue
			}
			b.WriteString(f.Key + ":\n")
			for _, item := range v.List {
				b.WriteString("  - " + quoteYAML(item) + "\n")
			}
		case Raw:
			if !strings.Contains(v.Raw, "\n") && !strings.HasPrefix(v.Raw, " ") {
				b.WriteString(f.Key + ": " + v.Raw + "\n")
				continue
			}
			b.WriteString(f.Key + ":\n" + v.Raw + "\n")
		}
	}
	b.WriteString("---\n")
	body := strings.TrimLeft(a.Body, "\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// Read loads a canonical file.
func Read(path string) (Agent, error) {
	a, _, err := Parse(path, Claude)
	return a, err
}

// ImportOptions control Import.
type ImportOptions struct {
	From   Tool // "" detects it
	Force  bool // replace an earlier import of the same name
	DryRun bool // report only
	Home   string
}

// Result is what Import did.
type Result struct {
	Agent Agent
	From  string // the source file
	Path  string // the canonical file
	Notes []Note // what the import changed
	Wrote bool
}

// Import reads src and writes it as <home>/cbrt-<name>.md. It never changes src.
func Import(src string, o ImportOptions) (Result, error) {
	a, notes, err := Parse(src, o.From)
	if err != nil {
		return Result{}, err
	}
	name, err := CanonicalName(a.Name)
	if err != nil {
		return Result{}, err
	}
	if name != a.Name {
		notes = append([]Note{{"name", Mapped, a.Name + " -> " + name}}, notes...)
		a.Name = name
	}
	home := o.Home
	if home == "" {
		home = Home()
	}
	r := Result{Agent: a, From: src, Path: filepath.Join(home, name+".md"), Notes: notes}
	if _, err := os.Stat(r.Path); err == nil && !o.Force {
		return r, fmt.Errorf("%s is already imported (%s); --force replaces it", name, r.Path)
	}
	if o.DryRun {
		return r, nil
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		abs = src
	}
	text := Format(a, fmt.Sprintf("Imported by cbrt agents import from %s (%s).", abs, a.Source))
	if err := os.MkdirAll(home, 0o755); err != nil {
		return r, err
	}
	tmp := r.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return r, err
	}
	if err := os.Rename(tmp, r.Path); err != nil {
		os.Remove(tmp)
		return r, err
	}
	r.Wrote = true
	return r, nil
}
