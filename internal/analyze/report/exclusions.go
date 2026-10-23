package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// ExclusionsFile is the optional scope file under the analyzer home.
const ExclusionsFile = "exclusions.json"

const exclusionsVersion = 1

// Exclusions are sessions that are never anchored or excerpted (they still
// count in totals). Projects match a session's project root exactly
// (normalized: cleaned, slashes unified, case-folded); PathGlobs are
// path.Match patterns over the slash form of the project root, its
// ancestors, and their trailing components, so "private" or "*/private"
// excludes every project inside a directory named private;
// Sessions match "<adapter>/<store>/<native id>" or the bare native id.
type Exclusions struct {
	Projects  []string `json:"projects"`
	PathGlobs []string `json:"path_globs"`
	Sessions  []string `json:"sessions"`
}

// LoadExclusions reads <home>/exclusions.json ({"v":1,"projects":[…],
// "path_globs":[…],"sessions":[…]}; missing file = nothing excluded).
// excludeHome adds the user's home directory as an excluded project (exact
// match: sessions started in the home directory itself, not below it).
func LoadExclusions(home string, excludeHome bool) (Exclusions, error) {
	var doc struct {
		V int `json:"v"`
		Exclusions
	}
	b, err := os.ReadFile(filepath.Join(home, ExclusionsFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return Exclusions{}, err
	default:
		if err := json.Unmarshal(b, &doc); err != nil {
			return Exclusions{}, fmt.Errorf("%s: %w", ExclusionsFile, err)
		}
		if doc.V > exclusionsVersion {
			return Exclusions{}, fmt.Errorf("%s: version %d is newer than this cbrt understands (%d)", ExclusionsFile, doc.V, exclusionsVersion)
		}
		for _, g := range doc.PathGlobs {
			if _, err := path.Match(normPath(g), ""); err != nil {
				return Exclusions{}, fmt.Errorf("%s: glob %q: %w", ExclusionsFile, g, err)
			}
		}
	}
	x := doc.Exclusions
	if excludeHome {
		if uh, err := os.UserHomeDir(); err == nil && uh != "" {
			x.Projects = append(x.Projects, uh)
		}
	}
	return x, nil
}

// normPath is the comparison form of a path: cleaned, forward slashes,
// lower case (project roots are case-folded on Windows by collect; folding
// everywhere only makes exclusions broader, never narrower).
func normPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = path.Clean(p)
	return strings.ToLower(p)
}

// Excluded reports whether s must stay out of anchors and excerpts.
func (x Exclusions) Excluded(s *analyze.Session) bool {
	if s == nil {
		return false
	}
	key := s.Key.String()
	for _, id := range x.Sessions {
		if id != "" && (id == key || id == s.Key.NativeID) {
			return true
		}
	}
	if s.Parent != nil {
		pk := s.Parent.String()
		for _, id := range x.Sessions {
			if id != "" && (id == pk || id == s.Parent.NativeID) {
				return true
			}
		}
	}
	if s.Project == "" {
		return false
	}
	proj := normPath(s.Project)
	for _, p := range x.Projects {
		if p != "" && normPath(analyze.ExpandRoot(p)) == proj {
			return true
		}
	}
	for _, g := range x.PathGlobs {
		if globMatch(normPath(g), proj) {
			return true
		}
	}
	return false
}

// globMatch matches g against p, every ancestor of p, and every trailing
// component sequence of those ("*/private" matches "/srv/private/x"), so a
// pattern covers a whole subtree wherever it sits.
func globMatch(g, p string) bool {
	for {
		for s := p; ; {
			if ok, _ := path.Match(g, s); ok {
				return true
			}
			i := strings.IndexByte(s, '/')
			if i < 0 {
				break
			}
			s = s[i+1:]
		}
		d := path.Dir(p)
		if d == p || d == "." {
			return false
		}
		p = d
	}
}
