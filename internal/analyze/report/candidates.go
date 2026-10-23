package report

import (
	"crypto/sha256"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// Candidate is one line of excerpt_candidates.jsonl: an event range the
// reviewer may read with `cbrt analyze excerpt`.
type Candidate struct {
	Session   string `json:"session"`
	FromSeq   string `json:"from_seq"`
	ToSeq     string `json:"to_seq"`
	Reason    string `json:"reason"`
	EstTokens int    `json:"est_tokens"`
	// Kind is hotspot, theme, sample_ordinary or sample_successful.
	Kind string `json:"kind"`
}

// hotspot is a session whose metrics stand out.
type hotspot struct {
	v       *view
	reasons []string
	score   float64
}

const (
	hotspotsPerMetric = 3
	hotspotMinPeers   = 3 // sessions with a value before a median means anything
)

// hotspots flags, for every metric name found in SessionMetrics.Values, the
// sessions whose value is at least twice the median of all sessions (top 3
// per metric). Metric names are read generically; metrics whose values all
// lie in [0,1] (fractions, flags) are skipped because "higher" is not
// necessarily worse for them. Unknown coverage never counts.
func hotspots(views []*view) []hotspot {
	type pt struct {
		v *view
		n float64
	}
	byMetric := map[string][]pt{}
	for _, v := range views {
		if v.metrics == nil {
			continue
		}
		for name, val := range v.metrics.Values {
			if val.Cov == analyze.Unknown || val.Cov == "" || math.IsNaN(val.N) {
				continue
			}
			byMetric[name] = append(byMetric[name], pt{v, val.N})
		}
	}
	names := make([]string, 0, len(byMetric))
	for n := range byMetric {
		names = append(names, n)
	}
	sort.Strings(names)
	found := map[*view]*hotspot{}
	for _, name := range names {
		pts := byMetric[name]
		if len(pts) < hotspotMinPeers {
			continue
		}
		unit := true
		vals := make([]float64, len(pts))
		for i, p := range pts {
			vals[i] = p.n
			if p.n < 0 || p.n > 1 {
				unit = false
			}
		}
		if unit {
			continue
		}
		med := median(vals)
		sort.Slice(pts, func(i, j int) bool {
			if pts[i].n != pts[j].n {
				return pts[i].n > pts[j].n
			}
			return pts[i].v.key < pts[j].v.key
		})
		n := 0
		for _, p := range pts {
			if n == hotspotsPerMetric || p.n <= 0 || p.n < 2*med || p.n <= med {
				break
			}
			n++
			if p.v.excluded {
				continue
			}
			score := p.n / math.Max(med, 1)
			h := found[p.v]
			if h == nil {
				h = &hotspot{v: p.v}
				found[p.v] = h
			}
			h.reasons = append(h.reasons, fmt.Sprintf("%s = %s (median %s)", name, fmtNum(p.n), fmtNum(med)))
			h.score = math.Max(h.score, score)
		}
	}
	out := make([]hotspot, 0, len(found))
	for _, h := range found {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].v.key < out[j].v.key
	})
	return out
}

func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func fmtNum(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// span is a contiguous range of excerptable events (positions in view.ex)
// with the estimated size of its excerpt at DefaultContext.
type span struct {
	lo, hi int
	est    int
}

// evEst is estimateEvent for event i of v, cached.
func (v *view) evEst(i int) int {
	if v.estCache == nil {
		v.estCache = make([]int, len(v.s.Events))
		for j := range v.estCache {
			v.estCache[j] = -1
		}
	}
	if v.estCache[i] < 0 {
		v.estCache[i] = estimateEvent(&v.s.Events[i], v.red)
	}
	return v.estCache[i]
}

// spanEst is the estimated size of Excerpt's output for positions lo..hi
// at DefaultContext: the frame plus estimateEvent of every event shown.
func (v *view) spanEst(lo, hi int) int {
	from, to := widen(v.s, v.ex[lo], v.ex[hi], DefaultContext)
	est := (utf8.RuneCountInString(frame(v.s, v.s.Events[from].Seq, v.s.Events[to].Seq)+frameEnd) + 3) / 4
	for i := from; i <= to; i++ {
		est += v.evEst(i)
	}
	return est
}

// grow builds a span from position c, adding events after it (and before
// it, unless forward) while the estimate stays within limit. Position c is
// always included, even when it alone exceeds limit.
func (v *view) grow(c, limit int, forward bool) span {
	sp := span{lo: c, hi: c, est: v.spanEst(c, c)}
	for grown := true; grown; {
		grown = false
		if sp.hi+1 < len(v.ex) {
			if e := v.spanEst(sp.lo, sp.hi+1); e <= limit {
				sp.hi, sp.est, grown = sp.hi+1, e, true
			}
		}
		if !forward && sp.lo > 0 {
			if e := v.spanEst(sp.lo-1, sp.hi); e <= limit {
				sp.lo, sp.est, grown = sp.lo-1, e, true
			}
		}
	}
	return sp
}

// hasText reports a span with at least one shown body: a range of injected
// context, empty errors and bookkeeping events is not worth the budget.
func (v *view) hasText(sp span) bool {
	for p := sp.lo; p <= sp.hi; p++ {
		e := &v.s.Events[v.ex[p]]
		if renderable(e) && bodyCap(e.Kind) > 0 && e.TextVolumeUnits > 0 {
			return true
		}
	}
	return false
}

func (v *view) candidate(sp span, kind, reason string) Candidate {
	return Candidate{
		Session: v.key, FromSeq: v.s.Events[v.ex[sp.lo]].Seq, ToSeq: v.s.Events[v.ex[sp.hi]].Seq,
		Reason: reason, EstTokens: sp.est, Kind: kind,
	}
}

// position is the v.ex position of event index i (the next one after it
// when i itself is not excerptable).
func (v *view) position(i int) int {
	c := sort.SearchInts(v.ex, i)
	if c == len(v.ex) {
		c = len(v.ex) - 1
	}
	return c
}

// focus is the event a hotspot excerpt centers on: the first correction or
// error anchor, else the bulkiest in-window event.
func (v *view) focus() int {
	for _, a := range v.anchors {
		if a.Kind == AnchorCorrection || a.Kind == AnchorError {
			return a.idx
		}
	}
	return v.bulkiest()
}

// bulkiest is the in-window excerptable event with the most text, or -1.
func (v *view) bulkiest() int {
	best, bestVol := -1, -1
	for _, i := range v.ex {
		if u := v.s.Events[i].TextVolumeUnits; u > bestVol {
			best, bestVol = i, u
		}
	}
	return best
}

// sampleStart is the position of the first in-window user prompt of a
// top-level session, or -1: a sample shows a prompt and the work it caused,
// so subagent sessions (whose prompts come from the parent agent) and
// sessions without a prompt are never sampled.
func (v *view) sampleStart() int {
	if v.s.Parent != nil {
		return -1
	}
	for p, i := range v.ex {
		if e := &v.s.Events[i]; e.Kind == analyze.KindUserPrompt && e.TextVolumeUnits > 0 {
			return p
		}
	}
	return -1
}

// blocked says why v cannot have a candidate at all, or "".
func (v *view) blocked() string {
	switch {
	case v.excluded:
		return "excluded"
	case v.registryOnly:
		return "registry-only session, no transcript"
	case len(v.ex) == 0:
		return "no transcript events in the window"
	}
	if _, loaded := analyze.Get(v.s.Key.AdapterID); !loaded {
		return "its adapter is not loaded"
	}
	return ""
}

var (
	reWord    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]{3,}`)
	stopWords = map[string]bool{}
)

func init() {
	for _, w := range strings.Fields(`about actually after again also because been before being could does doing done from have here into just like make more most must need only other over please really redacted should some still such than that their them then there these they this those through very want was were what when where which while will with would your that's it's don't revert wrong meant undo broke`) {
		stopWords[w] = true
	}
}

type theme struct {
	term string
	hits []Anchor // one per session, sessions sorted
}

// themes are words that recur in correction and error anchors of at least
// two sessions, most widespread first.
func themes(views []*view) []theme {
	bySession := map[string]map[string]Anchor{} // term -> session -> first anchor
	for _, v := range views {
		if v.excluded || len(v.ex) == 0 {
			continue
		}
		for _, a := range v.anchors {
			if a.Kind != AnchorCorrection && a.Kind != AnchorError {
				continue
			}
			for _, w := range reWord.FindAllString(a.Text, -1) {
				w = strings.ToLower(w)
				if stopWords[w] {
					continue
				}
				m := bySession[w]
				if m == nil {
					m = map[string]Anchor{}
					bySession[w] = m
				}
				if _, ok := m[v.key]; !ok {
					m[v.key] = a
				}
			}
		}
	}
	var out []theme
	for term, m := range bySession {
		if len(m) < 2 {
			continue
		}
		t := theme{term: term}
		for _, a := range m {
			t.hits = append(t.hits, a)
		}
		sort.Slice(t.hits, func(i, j int) bool { return t.hits[i].Session < t.hits[j].Session })
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].hits) != len(out[j].hits) {
			return len(out[i].hits) > len(out[j].hits)
		}
		return out[i].term < out[j].term
	})
	return out
}

const (
	maxThemes        = 3
	sessionsPerTheme = 2
	sampleOrdinary   = 2
	sampleSuccessful = 1
)

// candidates ranks excerpt ranges: numeric hotspots in rank order, recurring
// anchor themes, then a rotating sample (seeded by runID) of ordinary and
// successful sessions. The sample's budget is reserved first, so hotspots can
// never crowd it out; the total est_tokens never exceeds budget. est_tokens
// is the estimated size of `cbrt analyze excerpt` for the range at its
// default context. Excluded and registry-only sessions are never candidates.
// notes says, for every hotspot session, which range it got or why none.
func candidates(views []*view, hot []hotspot, runID string, budget int) (cands []Candidate, notes map[string]string) {
	limit := max(budget/6, 1) // one candidate's share
	left := budget
	notes = map[string]string{}
	used := map[string]bool{}
	hotSet := map[*view]bool{}
	for _, h := range hot {
		hotSet[h.v] = true
	}
	eligible := func(v *view) bool { return v.blocked() == "" }
	byKey := map[string]*view{}
	for _, v := range views {
		byKey[v.key] = v
	}
	ths := themes(views)
	if len(ths) > maxThemes {
		ths = ths[:maxThemes]
	}
	// Sessions the themes will want, so the sample does not take them.
	themeSet := map[*view]bool{}
	for _, t := range ths {
		taken := 0
		for _, a := range t.hits {
			if v := byKey[a.Session]; v != nil && taken < sessionsPerTheme && eligible(v) && !hotSet[v] {
				themeSet[v] = true
				taken++
			}
		}
	}

	// Sample first (reserved), in a per-run order.
	var succ, ord []*view
	for _, v := range views {
		if !eligible(v) || hotSet[v] || themeSet[v] || v.sampleStart() < 0 {
			continue
		}
		if successful(v.anchors) {
			succ = append(succ, v)
		} else {
			ord = append(ord, v)
		}
	}
	rotate(succ, runID)
	rotate(ord, runID)
	var sample []Candidate
	pick := func(pool []*view, n int, kind, reason string) {
		for _, v := range pool {
			if n == 0 {
				return
			}
			sp := v.grow(v.sampleStart(), min(limit, left), true)
			if sp.est > left || !v.hasText(sp) {
				continue
			}
			sample = append(sample, v.candidate(sp, kind, reason))
			left -= sp.est
			used[v.key] = true
			n--
		}
	}
	pick(succ, sampleSuccessful, "sample_successful", "rotating sample: a session with passing tests and no corrections")
	pick(ord, sampleOrdinary, "sample_ordinary", "rotating sample: an ordinary session")

	var ranked []Candidate
	for _, h := range hot {
		v := h.v
		if why := v.blocked(); why != "" {
			notes[v.key] = "no excerpt: " + why
			continue
		}
		f := v.focus()
		if f < 0 {
			notes[v.key] = "no excerpt: no transcript events in the window"
			continue
		}
		sp := v.grow(v.position(f), min(limit, left), false)
		if !v.hasText(sp) {
			// The anchor sits among events without text (an empty error):
			// show the session's bulkiest event instead.
			if b := v.bulkiest(); b >= 0 && b != f {
				sp = v.grow(v.position(b), min(limit, left), false)
			}
		}
		switch {
		case !v.hasText(sp):
			notes[v.key] = "no excerpt: no text near its focus or its bulkiest event"
			continue
		case sp.est > left:
			notes[v.key] = fmt.Sprintf("no excerpt: over the excerpt budget (needs ~%d tokens, %d left)", sp.est, left)
			continue
		}
		c := v.candidate(sp, "hotspot", "numeric hotspot: "+strings.Join(h.reasons, "; "))
		ranked = append(ranked, c)
		notes[v.key] = fmt.Sprintf("excerpt %s .. %s (~%d tokens)", c.FromSeq, c.ToSeq, c.EstTokens)
		left -= sp.est
		used[v.key] = true
	}
	for _, t := range ths {
		taken := 0
		for _, a := range t.hits {
			v := byKey[a.Session]
			if taken == sessionsPerTheme || v == nil || !eligible(v) || used[v.key] {
				continue
			}
			sp := v.grow(v.position(a.idx), min(limit, left), false)
			if sp.est > left || !v.hasText(sp) {
				continue
			}
			ranked = append(ranked, v.candidate(sp, "theme",
				fmt.Sprintf("recurring theme %q in correction/error anchors of %d sessions", t.term, len(t.hits))))
			left -= sp.est
			used[v.key] = true
			taken++
		}
	}
	return append(ranked, sample...), notes
}

// rotate orders vs by sha256(runID, session key): stable for one run id,
// different from run to run.
func rotate(vs []*view, runID string) {
	h := func(v *view) string {
		s := sha256.Sum256([]byte(runID + "\x00" + v.key))
		return string(s[:])
	}
	sort.Slice(vs, func(i, j int) bool {
		hi, hj := h(vs[i]), h(vs[j])
		if hi != hj {
			return hi < hj
		}
		return vs[i].key < vs[j].key
	})
}
