// Package tasks groups session ranges into candidate tasks (architecture
// doc §3.2.4) and keeps task ids stable across runs through TaskIdentity.
//
// Ranges: a main session is cut before a user prompt that carries a
// "#task:<id>" tag different from the current range's, or that follows at
// least gapHours with no event of any kind. A range is kept when it has an
// event in the window. Subagent sessions (Parent set) are not grouped: their
// work belongs to the parent's range.
//
// Linking (strongest first; a group's confidence is its weakest link):
//  1. explicit: the same #task tag (a tagged range alone is explicit too).
//  2. manual: Overrides (merge, split, rename, exclude), applied last.
//  3. strong: same project, same measured branch (not main/master, session
//     not flagged branch_drift/branch_partial), ranges overlapping or within
//     24 h.
//  4. weak: same project, both branches unknown, default or unreliable,
//     ranges within 4 h, and >= 2 shared files among edit/read_file calls.
//  5. single: anything else is its own task.
//
// Ranges with different tags never link by rules 3 and 4.
//
// Ids: a new task gets "t_" + the first 12 hex digits of sha1(founding key),
// the founding key being RangeKey of its earliest range. Every member's range
// key is recorded in TaskIdentity.ByFound (not only the founder's), so a task
// keeps its id after its founding range leaves the window. When one group
// holds several known ids, the id whose earliest member is earliest survives
// and the others become aliases.
//
// Overrides apply merges first, then splits. A split keeps the source id on
// the remaining part; the detached part gets SplitID(rule) with SplitFrom set
// and is not recorded in ByFound, so removing the rule merges it back under
// the source id without an alias. Excluded tasks keep their ids recorded.
package tasks

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// TagPattern finds an explicit task tag in a user prompt.
var TagPattern = regexp.MustCompile(`#task:([A-Za-z0-9][A-Za-z0-9._-]*)`)

// Link windows and thresholds (rules 3 and 4).
const (
	StrongWindow   = 24 * time.Hour
	WeakWindow     = 4 * time.Hour
	WeakSharedMin  = 2
	defaultGapHour = 4
)

// IdentityVersion is the current TaskIdentity.V.
const IdentityVersion = 1

// RangeKey is a range's key: "<adapter>/<store>/<native>#<from seq>". The
// founding key of a task is the RangeKey of its earliest range.
func RangeKey(s analyze.SessionKey, fromSeq string) string { return s.String() + "#" + fromSeq }

// NewID is the id of a task founded by the range with this key.
func NewID(foundingKey string) string {
	sum := sha1.Sum([]byte(foundingKey))
	return "t_" + hex.EncodeToString(sum[:])[:12]
}

type rng struct {
	key     string
	r       analyze.Range
	project string
	tag     string
	branch  string // most common measured branch, "" when none
	strong  bool   // branch usable for rule 3
	files   map[string]bool
}

var confRank = map[string]int{analyze.ConfWeak: 1, analyze.ConfStrong: 2, analyze.ConfManual: 3, analyze.ConfExplicit: 4}

const noLink = 99

func rankConf(r int) string {
	for c, n := range confRank {
		if n == r {
			return c
		}
	}
	return analyze.ConfSingle
}

func isDefault(b string) bool { return b == "main" || b == "master" }

// ranges cuts one session into ranges (full history), keeping those with an
// event in w.
func ranges(s *analyze.Session, w analyze.Window, gap time.Duration) []rng {
	var ev []analyze.Event
	for _, e := range s.Events {
		if e.ObservedBy == analyze.FromRegistry && s.LinkedTo[e.Seq] != "" {
			continue
		}
		ev = append(ev, e)
	}
	flagged := slices.Contains(s.Flags, "branch_drift") || slices.Contains(s.Flags, "branch_partial")
	var out []rng
	emit := func(part []analyze.Event, tag string) {
		if len(part) == 0 {
			return
		}
		in := false
		branches := map[string]int{}
		files := map[string]bool{}
		for _, e := range part {
			in = in || w.Contains(e.TS)
			if e.Branch != "" && (e.BranchCoverage == analyze.Measured || e.BranchCoverage == analyze.MeasuredAtStart) {
				branches[e.Branch]++
			}
			if e.Kind == analyze.KindToolCall && (e.ToolName == analyze.ToolEdit || e.ToolName == analyze.ToolReadFile) {
				for _, f := range e.Files {
					files[filepath.ToSlash(filepath.Clean(f))] = true
				}
			}
		}
		if !in {
			return
		}
		branch, best := "", 0
		for b, n := range branches {
			if n > best || (n == best && b < branch) {
				branch, best = b, n
			}
		}
		first, last := part[0], part[len(part)-1]
		out = append(out, rng{
			key:     RangeKey(s.Key, first.Seq),
			r:       analyze.Range{Session: s.Key, FromSeq: first.Seq, ToSeq: last.Seq, From: first.TS, To: last.TS},
			project: s.Project,
			tag:     tag,
			branch:  branch,
			strong:  branch != "" && !isDefault(branch) && !flagged,
			files:   files,
		})
	}
	start, tag, hasPrompt := 0, "", false
	for i, e := range ev {
		if e.Kind != analyze.KindUserPrompt {
			continue
		}
		t := ""
		if m := TagPattern.FindStringSubmatch(e.Text); m != nil {
			t = m[1]
		}
		long := i > 0 && e.TS.Sub(ev[i-1].TS) >= gap
		if hasPrompt && (long || (t != "" && t != tag)) {
			emit(ev[start:i], tag)
			start, tag = i, ""
		}
		if t != "" {
			tag = t
		}
		hasPrompt = true
	}
	emit(ev[start:], tag)
	return out
}

// distance is the time between two ranges, 0 when they overlap.
func distance(a, b analyze.Range) time.Duration {
	lo, hi := a, b
	if b.From.Before(a.From) {
		lo, hi = b, a
	}
	return max(0, hi.From.Sub(lo.To))
}

func shared(a, b map[string]bool) int {
	n := 0
	for f := range a {
		if b[f] {
			n++
		}
	}
	return n
}

// uf is a union-find whose roots carry the weakest link rank and the group's
// tag; groups with different tags are never joined.
type uf struct {
	parent, rank []int
	tag          []string
}

func (u *uf) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

func (u *uf) union(a, b, r int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb || (u.tag[ra] != "" && u.tag[rb] != "" && u.tag[ra] != u.tag[rb]) {
		return
	}
	lo, hi := min(ra, rb), max(ra, rb)
	u.parent[hi] = lo
	if u.tag[lo] == "" {
		u.tag[lo] = u.tag[hi]
	}
	u.rank[lo] = min(u.rank[lo], u.rank[hi], r)
}

type group struct {
	members   []int // indices into rs, sorted by From then key
	rank      int
	fixed     string // id forced by a split rule
	splitFrom string
	cands     map[string]bool // known ids of the members (resolved)
}

// Group builds the candidate tasks of the window from sessions (each with
// its full history), and returns them with the updated identity (ids is not
// modified). gapHours <= 0 means 4.
func Group(sessions []*analyze.Session, w analyze.Window, gapHours float64, overrides Overrides, ids analyze.TaskIdentity) ([]analyze.Task, analyze.TaskIdentity) {
	if gapHours <= 0 {
		gapHours = defaultGapHour
	}
	gap := time.Duration(gapHours * float64(time.Hour))
	next := analyze.TaskIdentity{V: IdentityVersion, ByFound: map[string]string{}, Aliases: map[string]string{}}
	for k, v := range ids.ByFound {
		next.ByFound[k] = v
	}
	for k, v := range ids.Aliases {
		next.Aliases[k] = v
	}
	resolve := func(id string) string {
		for range 64 {
			to, ok := next.Aliases[id]
			if !ok || to == id {
				break
			}
			id = to
		}
		return id
	}

	var rs []rng
	for _, s := range sessions {
		if s.Parent == nil {
			rs = append(rs, ranges(s, w, gap)...)
		}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if !rs[i].r.From.Equal(rs[j].r.From) {
			return rs[i].r.From.Before(rs[j].r.From)
		}
		return rs[i].key < rs[j].key
	})

	u := &uf{parent: make([]int, len(rs)), rank: make([]int, len(rs)), tag: make([]string, len(rs))}
	for i := range rs {
		u.parent[i], u.rank[i], u.tag[i] = i, noLink, rs[i].tag
		if rs[i].tag != "" {
			u.rank[i] = confRank[analyze.ConfExplicit]
		}
	}
	// Rule 1, then 3, then 4: a weaker rule never lowers a group an earlier
	// rule already joined.
	for i := range rs {
		for j := i + 1; j < len(rs); j++ {
			if rs[i].tag != "" && rs[i].tag == rs[j].tag {
				u.union(i, j, confRank[analyze.ConfExplicit])
			}
		}
	}
	linkable := func(a, b rng) bool {
		return a.project != "" && a.project == b.project && (a.tag == "" || b.tag == "" || a.tag == b.tag)
	}
	for i := range rs {
		for j := i + 1; j < len(rs); j++ {
			a, b := rs[i], rs[j]
			if linkable(a, b) && a.strong && b.strong && a.branch == b.branch && distance(a.r, b.r) <= StrongWindow {
				u.union(i, j, confRank[analyze.ConfStrong])
			}
		}
	}
	for i := range rs {
		for j := i + 1; j < len(rs); j++ {
			a, b := rs[i], rs[j]
			if linkable(a, b) && !a.strong && !b.strong && distance(a.r, b.r) <= WeakWindow && shared(a.files, b.files) >= WeakSharedMin {
				u.union(i, j, confRank[analyze.ConfWeak])
			}
		}
	}

	// Groups in order of their earliest range.
	byRoot := map[int]*group{}
	var groups []*group
	for i := range rs {
		r := u.find(i)
		g := byRoot[r]
		if g == nil {
			g = &group{rank: u.rank[r]}
			byRoot[r] = g
			groups = append(groups, g)
		}
		g.members = append(g.members, i)
	}
	cands := func(g *group) {
		g.cands = map[string]bool{}
		for _, m := range g.members {
			if id, ok := next.ByFound[rs[m].key]; ok {
				g.cands[resolve(id)] = true
			}
		}
		if len(g.cands) == 0 {
			g.cands[NewID(rs[g.members[0]].key)] = true
		}
	}
	for _, g := range groups {
		cands(g)
	}
	find := func(id string) int {
		id = resolve(id)
		for i, g := range groups {
			if g.cands[id] || g.fixed == id {
				return i
			}
		}
		return -1
	}

	// Manual merges.
	manual := confRank[analyze.ConfManual]
	for _, list := range overrides.Merge {
		target := -1
		for _, id := range list {
			gi := find(id)
			if gi < 0 {
				continue
			}
			if target < 0 {
				target = gi
				continue
			}
			if gi == target {
				continue
			}
			t, g := groups[target], groups[gi]
			t.members = append(t.members, g.members...)
			for c := range g.cands {
				t.cands[c] = true
			}
			t.rank = manual
			groups = slices.Delete(groups, gi, gi+1)
			if gi < target {
				target--
			}
		}
	}
	for _, g := range groups {
		sortMembers(g.members, rs)
	}

	// Manual splits.
	for _, rule := range overrides.Split {
		src := resolve(rule.Task)
		gi := find(src)
		if gi < 0 {
			continue
		}
		g := groups[gi]
		var keep, out []int
		for _, m := range g.members {
			if slices.Contains(rule.Members, rs[m].key) {
				out = append(out, m)
			} else {
				keep = append(keep, m)
			}
		}
		if len(out) == 0 || len(keep) == 0 {
			continue
		}
		g.members, g.rank, g.fixed = keep, manual, src
		groups = append(groups, &group{members: out, rank: manual, splitFrom: src, fixed: SplitID(rule)})
	}

	// Final ids.
	excluded := map[string]bool{}
	for _, id := range overrides.Exclude {
		excluded[resolve(id)] = true
	}
	var out []analyze.Task
	for _, g := range groups {
		id := g.fixed
		var aliases []string
		if g.splitFrom == "" {
			cands(g)
			// The oldest id: the one whose earliest member comes first.
			for _, m := range g.members {
				if c, ok := next.ByFound[rs[m].key]; ok && id == "" {
					id = resolve(c)
				}
			}
			if id == "" {
				id = NewID(rs[g.members[0]].key)
			}
			for c := range g.cands {
				if c != id {
					aliases = append(aliases, c)
				}
			}
			sort.Strings(aliases)
			for _, a := range aliases {
				next.Aliases[a] = id
			}
			// A split-off part is not recorded: its id comes from the rule, and
			// its ranges stay with the original task should the rule go away.
			for _, m := range g.members {
				next.ByFound[rs[m].key] = id
			}
		}
		if excluded[id] {
			continue
		}
		out = append(out, task(id, g, rs, aliases, overrides, resolve))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].First.Equal(out[j].First) {
			return out[i].First.Before(out[j].First)
		}
		return out[i].ID < out[j].ID
	})
	return out, next
}

// SplitID is the id of the part a split rule detaches: "t_" + 12 hex digits
// of sha1 over the source task and the rule's members (sorted), so it does
// not depend on the window.
func SplitID(rule SplitRule) string {
	ms := slices.Clone(rule.Members)
	sort.Strings(ms)
	sum := sha1.Sum([]byte(rule.Task + "|" + strings.Join(ms, "\n")))
	return "t_" + hex.EncodeToString(sum[:])[:12]
}

func sortMembers(ms []int, rs []rng) {
	slices.SortFunc(ms, func(a, b int) int {
		if c := rs[a].r.From.Compare(rs[b].r.From); c != 0 {
			return c
		}
		return strings.Compare(rs[a].key, rs[b].key)
	})
}

func task(id string, g *group, rs []rng, aliases []string, o Overrides, resolve func(string) string) analyze.Task {
	t := analyze.Task{ID: id, Confidence: rankConf(g.rank), SplitFrom: g.splitFrom, Aliases: aliases}
	if len(g.members) == 1 && g.rank == noLink {
		t.Confidence = analyze.ConfSingle
	}
	projects := map[string]int{}
	branch, sameBranch := rs[g.members[0]].branch, true
	for _, m := range g.members {
		r := rs[m]
		t.Members = append(t.Members, r.r)
		projects[r.project]++
		if r.branch != branch {
			sameBranch = false
		}
		if t.Last.Before(r.r.To) {
			t.Last = r.r.To
		}
	}
	t.First = t.Members[0].From
	best := 0
	for p, n := range projects {
		if n > best || (n == best && p < t.Project) {
			t.Project, best = p, n
		}
	}
	if sameBranch {
		t.Branch = branch
	}
	// An exact id wins over an alias; among aliases the first in sorted order.
	keys := make([]string, 0, len(o.Rename))
	for k := range o.Rename {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if resolve(k) == id && (t.Label == "" || k == id) {
			t.Label = o.Rename[k]
		}
	}
	if l, ok := o.Rename[id]; ok {
		t.Label = l
	}
	return t
}
