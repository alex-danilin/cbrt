package projects

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	inventory "github.com/alex-danilin/cbrt/internal/analyze/projects"
)

// SuggestWindow is how far back sessions count for a suggestion.
const SuggestWindow = StaleAfter

// MaxSuggestions caps the folders the page suggests.
const MaxSuggestions = 5

// RecentSession is one coding-agent session as the analyzer sees it: the
// tool, its working folder and its last event.
type RecentSession struct {
	Tool string
	Dir  string
	Last time.Time
}

// Suggestion is a folder with recent sessions that could be connected.
type Suggestion struct {
	Path     string // the repository's main folder, or the session folder outside git
	Name     string
	Sessions int
	Tools    []string // sorted
	Last     time.Time
}

// SuggestOptions are the machine facts Aggregate depends on; tests set them.
type SuggestOptions struct {
	Home     string   // "" = os.UserHomeDir
	TempDirs []string // nil = inventory.DefaultTempDirs
	// Root maps a session folder to the folder to connect; nil = gitRoot.
	Root func(ctx context.Context, dir string) string
}

// Aggregate groups sessions by the folder to connect: a repository's main
// folder (sessions in a linked worktree count for it), or the session folder
// itself outside git. Sessions without a folder, in the home folder, above
// it, in a filesystem root, a temp or scratch folder, or in a folder that no
// longer exists are left out. The result is sorted newest first.
func Aggregate(ctx context.Context, ss []RecentSession, o SuggestOptions) []Suggestion {
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	if o.TempDirs == nil {
		o.TempDirs = inventory.DefaultTempDirs()
	}
	if o.Root == nil {
		o.Root = gitRoot
	}
	roots := map[string]string{} // session folder -> folder to connect ("" = refused)
	byRoot := map[string]*Suggestion{}
	tools := map[string]map[string]bool{}
	for _, s := range ss {
		root, seen := roots[s.Dir]
		if !seen {
			root = ""
			if inventory.Refusal(s.Dir, o.Home, o.TempDirs) == "" && isDir(s.Dir) {
				if r := o.Root(ctx, s.Dir); inventory.Refusal(r, o.Home, o.TempDirs) == "" {
					root = r
				}
			}
			roots[s.Dir] = root
		}
		if root == "" {
			continue
		}
		key := rootKey(root)
		g := byRoot[key]
		if g == nil {
			g = &Suggestion{Path: root, Name: baseName(root)}
			byRoot[key] = g
			tools[key] = map[string]bool{}
		}
		g.Sessions++
		if s.Last.After(g.Last) {
			g.Last = s.Last
		}
		tools[key][s.Tool] = true
	}
	out := make([]Suggestion, 0, len(byRoot))
	for key, g := range byRoot {
		for t := range tools[key] {
			g.Tools = append(g.Tools, t)
		}
		sort.Strings(g.Tools)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Unconnected drops the suggestions that are connected projects, folders
// inside one, or dismissed, and keeps at most MaxSuggestions.
func Unconnected(sg []Suggestion, es []Entry, dismissed []string) []Suggestion {
	var out []Suggestion
	for _, s := range sg {
		if len(out) == MaxSuggestions {
			break
		}
		if covered(s.Path, es) || dismissedPath(s.Path, dismissed) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func covered(p string, es []Entry) bool {
	for _, e := range es {
		if within(p, e.Path) {
			return true
		}
	}
	return false
}

func dismissedPath(p string, dismissed []string) bool {
	for _, d := range dismissed {
		if inventory.SamePath(p, d) {
			return true
		}
	}
	return false
}

// within reports p being dir or a folder below it.
func within(p, dir string) bool {
	for {
		if inventory.SamePath(p, dir) {
			return true
		}
		up := filepath.Dir(p)
		if up == p {
			return false
		}
		p = up
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// rootKey groups roots that name the same folder (case-folded where paths
// are case-insensitive).
func rootKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}

// gitRoot is the main folder of the repository dir is in: the parent of the
// common git directory, so a linked worktree maps to its main repository.
// Outside git, or for a bare or submodule layout, it is the top level, else
// dir itself. On Windows the result has the folder names' real case.
func gitRoot(ctx context.Context, dir string) string {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := gitCtx(cctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel")
	root := dir
	if err == nil {
		common, top, _ := strings.Cut(out, "\n")
		common, top = filepath.FromSlash(strings.TrimSpace(common)), filepath.FromSlash(strings.TrimSpace(top))
		switch {
		case filepath.Base(common) == ".git":
			root = filepath.Dir(common)
		case top != "":
			root = top
		}
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return filepath.Clean(root)
}
