// Package projects builds the per-run project context inventory
// (projects.jsonl): for every project with sessions in the window, where its
// root is and which instruction files, docs, build entry points and tool
// context exist there, by name and size only. The reviewer reads the files
// it needs from these paths before recommending anything for a project.
//
// Reads are bounded: a fixed list of names at the root, one level of docs/
// and of the folders adapters name, and only the script or target names of
// package.json and Makefiles. Nothing else is opened.
package projects

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// Status values.
const (
	Known   = "known"
	Unknown = "unknown"
)

// Caps on listed names.
const (
	maxNames   = 30
	maxDepth   = 64      // ancestors walked looking for a git top level
	maxParsed  = 1 << 20 // package.json and Makefiles larger than this are not parsed
	maxCounted = 1000    // files counted in one folder
)

// File is a file that exists, relative to the project root.
type File struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// Build is one build entry point: a manifest and the names it defines
// (package.json scripts, Makefile targets).
type Build struct {
	File  string   `json:"file"`
	Names []string `json:"names,omitempty"`
	More  int      `json:"more,omitempty"` // names past the cap
}

// ToolContext is one place a tool reads project context from that exists.
type ToolContext struct {
	Tool string `json:"tool"`
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"` // relative to the root, or absolute outside it
	// PathWithheld: redaction would make the path unusable (it names the user
	// or a home-folder slug), so only the counts are reported.
	PathWithheld bool     `json:"path_withheld,omitempty"`
	Bytes        int64    `json:"bytes,omitempty"` // files
	Files        int      `json:"files,omitempty"` // folders: files directly inside
	Names        []string `json:"names,omitempty"` // folders: entry names (capped)
	More         int      `json:"more,omitempty"`
}

// Project is one line of projects.jsonl (before redaction).
type Project struct {
	Project      string        `json:"project"` // the session project key
	Status       string        `json:"status"`
	Reason       string        `json:"reason,omitempty"`
	Root         string        `json:"root,omitempty"`
	Git          bool          `json:"git"`
	Sessions     int           `json:"sessions"`
	Instructions []File        `json:"instructions,omitempty"`
	Readme       []File        `json:"readme,omitempty"`
	Docs         []string      `json:"docs,omitempty"` // top-level docs/ entries, folders end in "/"
	DocsMore     int           `json:"docs_more,omitempty"`
	Build        []Build       `json:"build,omitempty"`
	ToolContext  []ToolContext `json:"tool_context,omitempty"`
}

// Options are the machine facts the inventory depends on; tests set them.
type Options struct {
	Home     string   // the user's home folder; "" = os.UserHomeDir
	TempDirs []string // nil = os.TempDir and the usual temp roots
	// Adapters contribute tool-specific context (analyze.ProjectContexter),
	// with Roots giving each one's store roots.
	Adapters []analyze.Adapter
	Roots    func(analyze.Adapter) []string
}

// Inventory returns one Project per key of sessions (project key -> number
// of sessions), sorted by key. The empty key is the sessions without a
// recorded working directory.
func Inventory(sessions map[string]int, o Options) []Project {
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	if o.TempDirs == nil {
		o.TempDirs = DefaultTempDirs()
	}
	keys := make([]string, 0, len(sessions))
	for k := range sessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Project, 0, len(keys))
	for _, k := range keys {
		p := inspect(k, o)
		p.Sessions = sessions[k]
		out = append(out, p)
	}
	return out
}

// DefaultTempDirs are the usual temp roots on this machine.
func DefaultTempDirs() []string {
	ds := []string{os.TempDir(), "/tmp", "/var/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}
	if v := os.Getenv("TMPDIR"); v != "" {
		ds = append(ds, v)
	}
	return ds
}

// Kinds of folder that are never a project (Refusal).
const (
	RefuseEmpty   = "no folder"
	RefuseHome    = "the home folder"
	RefuseAbove   = "a folder above the home folder"
	RefuseRoot    = "a filesystem root"
	RefuseTemp    = "a temporary folder"
	RefuseScratch = "a scratch folder"
)

// Refusal says why dir cannot be a project folder: it is empty, the home
// folder or a folder above it, a filesystem root, inside one of tempDirs or
// a scratch folder. It returns "" for a folder that may be a project. The
// check is lexical (paths may use either separator); callers that can
// resolve links check the resolved paths too.
func Refusal(dir, home string, tempDirs []string) string {
	switch {
	case dir == "":
		return RefuseEmpty
	case home != "" && same(dir, home):
		return RefuseHome
	case isRoot(dir):
		return RefuseRoot
	case home != "" && under(home, dir):
		return RefuseAbove
	}
	for _, t := range tempDirs {
		if t != "" && under(dir, t) {
			return RefuseTemp
		}
	}
	if elem(dir, "scratchpad") {
		return RefuseScratch
	}
	return ""
}

// SamePath reports a and b naming the same folder (either separator, a
// trailing separator, and case where paths are case-insensitive ignored).
func SamePath(a, b string) bool { return same(a, b) }

func inspect(key string, o Options) Project {
	p := Project{Project: key, Status: Unknown}
	switch Refusal(key, o.Home, o.TempDirs) {
	case "":
	case RefuseEmpty:
		p.Reason = "no working directory recorded"
		return p
	default:
		p.Reason = Refusal(key, o.Home, o.TempDirs) + " is the working directory"
		return p
	}
	st, err := os.Stat(key)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.Reason = "the folder no longer exists"
		return p
	case err != nil:
		p.Reason = "the folder is unreadable: " + reason(err)
		return p
	case !st.IsDir():
		p.Reason = "not a folder"
		return p
	}
	p.Root, p.Git = gitRoot(key, o.Home)
	if _, err := os.ReadDir(p.Root); err != nil {
		p.Root = ""
		p.Reason = "the folder is unreadable: " + reason(err)
		return p
	}
	p.Status = Known
	fill(&p, o)
	return p
}

// reason is an error's message without the path it names.
func reason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// gitRoot is the nearest folder from dir upward that holds .git, stopping
// below home and the filesystem root (a dotfiles repository in the home
// folder does not make every project one repository); dir itself when none.
func gitRoot(dir, home string) (string, bool) {
	d := dir
	for range maxDepth {
		if home != "" && same(d, home) {
			break
		}
		if _, err := os.Stat(join(d, ".git")); err == nil {
			return d, true
		}
		up, ok := parent(d)
		if !ok || isRoot(up) {
			break
		}
		d = up
	}
	return dir, false
}

// fill records what exists under p.Root.
func fill(p *Project, o Options) {
	root := p.Root
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), "readme") {
			if f, ok := file(root, name); ok {
				p.Readme = append(p.Readme, f)
			}
		}
	}
	if f, ok := file(root, "AGENTS.md"); ok {
		p.Instructions = append(p.Instructions, f)
	}
	for _, dir := range []string{".agents", ".agents/rules"} {
		for _, n := range names(join(root, dir), true) {
			if f, ok := file(root, dir+"/"+n); ok {
				p.Instructions = append(p.Instructions, f)
			}
		}
	}
	docs := names(join(root, "docs"), false)
	if len(docs) > maxNames {
		p.DocsMore = len(docs) - maxNames
		docs = docs[:maxNames]
	}
	p.Docs = docs
	p.Build = builds(root)

	for _, a := range o.Adapters {
		pc, ok := a.(analyze.ProjectContexter)
		if !ok {
			continue
		}
		var roots []string
		if o.Roots != nil {
			roots = o.Roots(a)
		}
		for _, c := range pc.ProjectContext(root, roots) {
			if t, ok := toolContext(a.ID(), root, c); ok {
				p.ToolContext = append(p.ToolContext, t)
			}
		}
	}
	sort.SliceStable(p.ToolContext, func(i, j int) bool {
		a, b := p.ToolContext[i], p.ToolContext[j]
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		return a.Path < b.Path
	})
}

func toolContext(tool, root string, c analyze.ContextPath) (ToolContext, bool) {
	full := c.Path
	if !isAbs(c.Path) {
		full = join(root, c.Path)
	}
	t := ToolContext{Tool: tool, Kind: c.Kind, Path: c.Path}
	st, err := os.Stat(full)
	if err != nil {
		return t, false
	}
	if !c.Dir {
		if st.IsDir() {
			return t, false
		}
		t.Bytes = st.Size()
		return t, true
	}
	if !st.IsDir() {
		return t, false
	}
	ents, err := os.ReadDir(full)
	if err != nil {
		return t, false
	}
	var ns []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		} else if t.Files < maxCounted {
			t.Files++
		}
		ns = append(ns, n)
	}
	if len(ns) > maxNames {
		t.More = len(ns) - maxNames
		ns = ns[:maxNames]
	}
	t.Names = ns
	return t, true
}

// file stats rel (slash-separated) under root.
func file(root, rel string) (File, bool) {
	st, err := os.Stat(join(root, rel))
	if err != nil || st.IsDir() {
		return File{}, false
	}
	return File{Path: rel, Bytes: st.Size()}, true
}

// names lists a folder's entries, sorted (folders end in "/"); with
// filesOnly, only its files.
func names(dir string, filesOnly bool) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		switch {
		case e.IsDir() && filesOnly:
		case e.IsDir():
			out = append(out, e.Name()+"/")
		default:
			out = append(out, e.Name())
		}
	}
	return out
}

// reTarget matches a rule line "name: deps" (or "name:: deps"); a rest that
// starts with "=" is a variable assignment (":=", "::=").
var reTarget = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_./-]*)\s*::?(.*)$`)

// Builds lists the build entry points at root: package.json scripts, the
// targets of the first Makefile, and go.mod, Cargo.toml, pyproject.toml.
func Builds(root string) []Build { return builds(root) }

// builds lists the build entry points at root.
func builds(root string) []Build {
	var out []Build
	if b, ok := readSmall(join(root, "package.json")); ok {
		var pj struct {
			Scripts map[string]json.RawMessage `json:"scripts"`
		}
		bd := Build{File: "package.json"}
		if json.Unmarshal(b, &pj) == nil {
			for n := range pj.Scripts {
				bd.Names = append(bd.Names, n)
			}
		}
		out = append(out, capNames(bd))
	}
	for _, mk := range []string{"Makefile", "makefile", "GNUmakefile"} {
		b, ok := readSmall(join(root, mk))
		if !ok {
			continue
		}
		bd := Build{File: mk}
		seen := map[string]bool{}
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		sc.Buffer(nil, maxParsed)
		for sc.Scan() {
			if m := reTarget.FindStringSubmatch(sc.Text()); m != nil && !strings.HasPrefix(m[2], "=") && !seen[m[1]] {
				seen[m[1]] = true
				bd.Names = append(bd.Names, m[1])
			}
		}
		out = append(out, capNames(bd))
		break // one Makefile: make reads the first it finds
	}
	for _, f := range []string{"go.mod", "Cargo.toml", "pyproject.toml"} {
		if st, err := os.Stat(join(root, f)); err == nil && !st.IsDir() {
			out = append(out, Build{File: f})
		}
	}
	return out
}

func capNames(b Build) Build {
	sort.Strings(b.Names)
	if len(b.Names) > maxNames {
		b.More = len(b.Names) - maxNames
		b.Names = b.Names[:maxNames]
	}
	return b
}

// readSmall reads a regular file of at most maxParsed bytes.
func readSmall(p string) ([]byte, bool) {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() || st.Size() > maxParsed {
		return nil, false
	}
	b, err := os.ReadFile(p)
	return b, err == nil
}

// --- paths that may use either separator ---

// sep is the separator p is written with.
func sep(p string) string {
	if strings.Contains(p, `\`) && !strings.Contains(p, "/") {
		return `\`
	}
	if runtime.GOOS == "windows" && !strings.Contains(p, "/") {
		return `\`
	}
	return "/"
}

// join appends rel (slash-separated) to dir in dir's separator style.
func join(dir, rel string) string {
	s := sep(dir)
	rel = strings.ReplaceAll(rel, "/", s)
	return strings.TrimRight(dir, `/\`) + s + rel
}

func isSep(r rune) bool { return r == '/' || r == '\\' }

// parent is p without its last element.
func parent(p string) (string, bool) {
	t := strings.TrimRight(p, `/\`)
	i := strings.LastIndexAny(t, `/\`)
	if i < 0 {
		return "", false
	}
	up := t[:i]
	if up == "" || (len(up) == 2 && up[1] == ':') { // "/x" -> "/", "C:\x" -> "C:\"
		up = t[:i+1]
	}
	return up, true
}

// norm is p with forward slashes, no trailing slash, and lower case where
// paths are case-insensitive (Windows, or a drive-letter path).
func norm(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if runtime.GOOS == "windows" || (len(p) >= 2 && p[1] == ':') {
		p = strings.ToLower(p)
	}
	return p
}

func same(a, b string) bool { return norm(a) == norm(b) }

// under reports p inside dir (or equal to it).
func under(p, dir string) bool {
	np, nd := norm(p), norm(dir)
	return np == nd || strings.HasPrefix(np, strings.TrimRight(nd, "/")+"/")
}

func isRoot(p string) bool {
	n := norm(p)
	return n == "/" || n == "" || (len(n) <= 3 && len(n) >= 2 && n[1] == ':')
}

func isAbs(p string) bool {
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || (len(p) >= 3 && p[1] == ':' && isSep(rune(p[2])))
}

// elem reports a path element equal to name (case-insensitive).
func elem(p, name string) bool {
	for _, e := range strings.FieldsFunc(p, isSep) {
		if strings.EqualFold(e, name) {
			return true
		}
	}
	return false
}
