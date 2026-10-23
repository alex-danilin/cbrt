package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

const (
	topTasks      = 5
	topHotspots   = 8
	topTrend      = 12
	maxListed     = 5
	maxLabelRunes = 60
)

// tok accumulates token usage over model_request events.
type tok struct {
	reqs, measured, partial int
	consumed, peak          int64
	volume                  int64
}

func (t *tok) add(e *analyze.Event) {
	t.volume += int64(e.TextVolumeUnits)
	if e.Kind != analyze.KindModelRequest {
		return
	}
	t.reqs++
	if e.Tokens == nil {
		return
	}
	t.measured++
	if e.TokensCoverage == analyze.Partial {
		t.partial++
	}
	var ctx, out int64
	for _, p := range []*int64{e.Tokens.InUncached, e.Tokens.InCacheRead, e.Tokens.InCacheWrite} {
		if p != nil {
			ctx += *p
		}
	}
	for _, p := range []*int64{e.Tokens.OutVisible, e.Tokens.OutThinking} {
		if p != nil {
			out += *p
		}
	}
	t.consumed += ctx + out
	t.peak = max(t.peak, ctx)
}

func (t *tok) merge(o tok) {
	t.reqs += o.reqs
	t.measured += o.measured
	t.partial += o.partial
	t.consumed += o.consumed
	t.peak = max(t.peak, o.peak)
	t.volume += o.volume
}

// consumption renders consumed tokens with their coverage fraction; n/a when
// the tool records no tokens or nothing was measured (never 0 for missing).
func (t tok) consumption(c analyze.Capabilities) string {
	switch {
	case c.Tokens == "unknown":
		return "n/a (tool records no tokens)"
	case t.reqs == 0:
		return "n/a (no model requests)"
	case t.measured == 0:
		return fmt.Sprintf("n/a (0 of %d requests measured)", t.reqs)
	}
	s := fmt.Sprintf("%d (coverage %.2f = %d/%d requests", t.consumed, float64(t.measured)/float64(t.reqs), t.measured, t.reqs)
	if t.partial > 0 {
		s += fmt.Sprintf("; %d with partial buckets", t.partial)
	}
	return s + ")"
}

func (t tok) peakContext(c analyze.Capabilities) string {
	if c.Tokens == "unknown" || t.measured == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d (coverage %.2f)", t.peak, float64(t.measured)/float64(t.reqs))
}

// rangeIdx returns the in-window event indices of a task member range (the
// whole window part when its seqs are not found).
func rangeIdx(v *view, r analyze.Range) []int {
	from, okF := v.pos[r.FromSeq]
	to, okT := v.pos[r.ToSeq]
	if !okF || !okT {
		return v.win
	}
	var out []int
	for _, i := range v.win {
		if i >= from && i <= to {
			out = append(out, i)
		}
	}
	return out
}

type taskRow struct {
	t           *analyze.Task
	tok         tok
	caps        analyze.Capabilities // "unknown" tokens if every member's tool lacks them
	tools       []string
	corrections int
	retries     float64
	hasRetries  bool
	label       string
	excluded    bool // every member excluded
}

func taskRows(in Input, byKey map[string]*view, red *Redactor) []taskRow {
	var rows []taskRow
	for ti := range in.Tasks {
		t := &in.Tasks[ti]
		row := taskRow{t: t, caps: analyze.Capabilities{Tokens: "unknown"}, excluded: true}
		tools := map[string]bool{}
		sessions := map[*view]bool{}
		firstPrompt := ""
		for _, m := range t.Members {
			tools[m.Session.AdapterID] = true
			v := byKey[m.Session.String()]
			if v == nil {
				continue
			}
			sessions[v] = true
			if !v.excluded {
				row.excluded = false
			}
			if v.caps.Tokens != "unknown" {
				row.caps.Tokens = v.caps.Tokens
			}
			idx := rangeIdx(v, m)
			in := map[int]bool{}
			for _, i := range idx {
				in[i] = true
				row.tok.add(&v.s.Events[i])
			}
			for _, a := range v.anchors {
				if !in[a.idx] {
					continue
				}
				switch a.Kind {
				case AnchorCorrection:
					row.corrections++
				case AnchorFirstPrompt:
					if firstPrompt == "" {
						firstPrompt = v.s.Events[a.idx].Text
					}
				}
			}
			if firstPrompt == "" && !v.excluded {
				for _, i := range idx {
					if e := &v.s.Events[i]; e.Kind == analyze.KindUserPrompt && strings.TrimSpace(e.Text) != "" {
						firstPrompt = e.Text
						break
					}
				}
			}
		}
		for v := range sessions {
			if v.metrics == nil {
				continue
			}
			for name, val := range v.metrics.Values {
				if strings.Contains(strings.ToLower(name), "retr") && val.Cov != analyze.Unknown && val.Cov != "" {
					row.retries += val.N
					row.hasRetries = true
				}
			}
		}
		for tl := range tools {
			row.tools = append(row.tools, tl)
		}
		sort.Strings(row.tools)
		switch {
		case row.excluded:
			row.label = "(excluded)"
		case t.Label != "":
			row.label = trimRunes(oneLine(red.Apply(t.Label)), maxLabelRunes)
		default:
			row.label = trimRunes(oneLine(red.Apply(firstPrompt)), maxLabelRunes)
		}
		rows = append(rows, row)
	}
	return rows
}

// oneLine collapses whitespace and neutralizes Markdown table pipes.
func oneLine(s string) string {
	return neutralize(strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", "/"))
}

func (r taskRow) project(red *Redactor) string {
	if r.excluded {
		return "(excluded)"
	}
	return projectName(r.t.Project, red)
}

func projectName(p string, red *Redactor) string {
	if p == "" {
		return "(unknown)"
	}
	return oneLine(red.Apply(p))
}

func renderSummary(in Input, views []*view, byKey map[string]*view, hot []hotspot, notes map[string]string, red *Redactor) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	ds := in.Dataset
	excluded := 0
	for _, v := range views {
		if v.excluded {
			excluded++
		}
	}
	p("# Agent session analysis, run %s\n\n", oneLine(in.RunID))
	p("Window: %s to %s (UTC). Sessions: %d (%d excluded from anchors and excerpts; counted in totals). ",
		ds.Window.Since.UTC().Format(time.RFC3339), ds.Window.Until.UTC().Format(time.RFC3339), len(views), excluded)
	p("Collection problems: %d. Redaction rules v%s.\n\n", len(ds.Problems), RedactionVersion)
	budget := in.ExcerptTokens
	if budget <= 0 {
		budget = DefaultExcerptTokens
	}
	p("Excerpt budget: %d tokens of transcript text (excerpt_candidates.jsonl fits within it).\n\n", budget)

	// Totals by tool.
	type toolAgg struct {
		sessions int
		caps     analyze.Capabilities
		tok      tok
	}
	agg := map[string]*toolAgg{}
	for _, v := range views {
		a := agg[v.s.Key.AdapterID]
		if a == nil {
			a = &toolAgg{caps: v.caps}
			agg[v.s.Key.AdapterID] = a
		}
		a.sessions++
		for _, i := range v.win {
			a.tok.add(&v.s.Events[i])
		}
	}
	p("## Totals by tool\n\n")
	p("| Tool | Sessions | Model requests | Consumption, tokens (coverage) | Peak request context, tokens | Text volume, not tokens |\n|---|---|---|---|---|---|\n")
	for _, id := range sortedKeys(agg) {
		a := agg[id]
		p("| %s | %d | %d | %s | %s | %d |\n", oneLine(id), a.sessions, a.tok.reqs, a.tok.consumption(a.caps), a.tok.peakContext(a.caps), a.tok.volume)
	}
	p("\nConsumption sums every request's input and output (context re-sent on each call counts each time); coverage is measured requests / all requests. n/a means not recorded, never zero. Text volume is ceil(chars/4) of persisted text, a cross-tool volume proxy.\n\n")

	// Served by.
	type servedAgg struct{ reqs, fallbacks int }
	served := map[string]*servedAgg{}
	var unverified []string
	for _, v := range views {
		for seq, sv := range v.s.Served {
			if i, ok := v.pos[seq]; ok && !ds.Window.Contains(v.s.Events[i].TS) {
				continue
			}
			k := oneLine(red.Apply(sv.Provider + " / " + sv.Model))
			a := served[k]
			if a == nil {
				a = &servedAgg{}
				served[k] = a
			}
			a.reqs++
			if sv.Fallback {
				a.fallbacks++
			}
		}
		for _, f := range v.s.Flags {
			if f == "model_unverified" {
				if v.excluded {
					unverified = append(unverified, "(excluded)")
				} else {
					unverified = append(unverified, v.key)
				}
			}
		}
	}
	p("## Served by (cbrt records)\n\n")
	if len(served) == 0 {
		p("No model requests were joined to cbrt records.\n")
	} else {
		p("| Provider / model | Requests | Fallbacks |\n|---|---|---|\n")
		for _, k := range sortedKeys(served) {
			p("| %s | %d | %d |\n", k, served[k].reqs, served[k].fallbacks)
		}
	}
	p("\nSessions flagged model_unverified: %d%s\n\n", len(unverified), listSome(unverified))

	// Tasks.
	rows := taskRows(in, byKey, red)
	p("## Top candidate tasks by cost\n\n")
	if len(rows) == 0 {
		p("No candidate tasks.\n\n")
	} else {
		byCost := append([]taskRow(nil), rows...)
		sort.SliceStable(byCost, func(i, j int) bool {
			a, c := byCost[i], byCost[j]
			am, cm := a.tok.measured > 0, c.tok.measured > 0
			if am != cm {
				return am
			}
			if a.tok.consumed != c.tok.consumed {
				return a.tok.consumed > c.tok.consumed
			}
			if a.tok.volume != c.tok.volume {
				return a.tok.volume > c.tok.volume
			}
			return a.t.ID < c.t.ID
		})
		p("| Candidate task | Project | Confidence | Members (tools) | Consumption, tokens (coverage) | Text volume, not tokens | Label |\n|---|---|---|---|---|---|---|\n")
		for _, r := range byCost[:min(topTasks, len(byCost))] {
			p("| %s | %s | %s | %d (%s) | %s | %d | %s |\n", oneLine(r.t.ID), r.project(red), oneLine(r.t.Confidence), len(r.t.Members),
				strings.Join(r.tools, ", "), r.tok.consumption(r.caps), r.tok.volume, r.label)
		}
		p("\n## Top candidate tasks by rework\n\n")
		var rework []taskRow
		for _, r := range rows {
			if r.corrections > 0 || r.retries > 0 {
				rework = append(rework, r)
			}
		}
		sort.SliceStable(rework, func(i, j int) bool {
			a, c := float64(rework[i].corrections)+rework[i].retries, float64(rework[j].corrections)+rework[j].retries
			if a != c {
				return a > c
			}
			return rework[i].t.ID < rework[j].t.ID
		})
		if len(rework) == 0 {
			p("No corrections or retries detected.\n")
		} else {
			p("| Candidate task | Project | Confidence | Members | Corrections | Retries (session-level) | Label |\n|---|---|---|---|---|---|---|\n")
			for _, r := range rework[:min(topTasks, len(rework))] {
				retries := "n/a"
				if r.hasRetries {
					retries = fmtNum(r.retries)
				}
				p("| %s | %s | %s | %d | %d | %s | %s |\n", oneLine(r.t.ID), r.project(red), oneLine(r.t.Confidence), len(r.t.Members), r.corrections, retries, r.label)
			}
		}
		p("\nGroups are candidate tasks: correct them in task_overrides.json.\n\n")
	}

	// Hotspots.
	p("## Hotspots\n\n")
	if len(hot) == 0 {
		p("No session stands out (a hotspot is at least twice the median of a metric over at least %d sessions).\n\n", hotspotMinPeers)
	} else {
		for _, h := range hot[:min(topHotspots, len(hot))] {
			p("- %s (%s): %s. %s.\n", h.v.key, projectName(h.v.s.Project, red), strings.Join(h.reasons, "; "), oneLine(notes[h.v.key]))
		}
		if rest := hot[min(topHotspots, len(hot)):]; len(rest) > 0 {
			with := 0
			for _, h := range rest {
				if strings.HasPrefix(notes[h.v.key], "excerpt ") {
					with++
				}
			}
			p("- … %d more hotspots, %d of them with a range in excerpt_candidates.jsonl\n", len(rest), with)
		}
		p("\n")
	}

	renderProjects(&b, in, red)

	// Collection problems, each with its reason. Problems name sources by
	// adapter/store/session id or file base name; redact them all the same.
	p("## Collection problems\n\n")
	if len(ds.Problems) == 0 {
		p("None.\n\n")
	} else {
		for _, pr := range ds.Problems {
			p("- %s\n", oneLine(red.Apply(pr)))
		}
		p("\nWhat could not be read is missing from the numbers above.\n\n")
	}

	renderTrend(&b, in, views, rows, red)
	return b.String()
}

func renderTrend(b *strings.Builder, in Input, views []*view, rows []taskRow, red *Redactor) {
	p := func(format string, a ...any) { fmt.Fprintf(b, format, a...) }
	p("## Trend versus the previous run\n\n")
	if len(in.Previous) == 0 {
		p("No previous run to compare with.\n\n")
	} else {
		type acc struct{ sum, n float64 }
		sum := func(ms []analyze.SessionMetrics) map[string]*acc {
			out := map[string]*acc{}
			for _, m := range ms {
				for name, v := range m.Values {
					if v.Cov == analyze.Unknown || v.Cov == "" {
						continue
					}
					a := out[name]
					if a == nil {
						a = &acc{}
						out[name] = a
					}
					a.sum += v.N
					a.n++
				}
			}
			return out
		}
		cur, prev := sum(in.Metrics), sum(in.Previous)
		p("Sessions with metrics: %d now, %d previously.\n\n", len(in.Metrics), len(in.Previous))
		var names []string
		for name := range cur {
			if prev[name] != nil {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		if len(names) == 0 {
			p("No metric is present in both runs.\n\n")
		} else {
			p("| Metric | Total now | Total before | Per session now | Per session before | Change per session |\n|---|---|---|---|---|---|\n")
			for _, name := range names[:min(topTrend, len(names))] {
				c, q := cur[name], prev[name]
				cm, qm := c.sum/c.n, q.sum/q.n
				change := "n/a"
				if qm != 0 {
					change = fmt.Sprintf("%+.0f%%", (cm-qm)/qm*100)
				}
				p("| %s | %s | %s | %s | %s | %s |\n", oneLine(name), fmtNum(c.sum), fmtNum(q.sum), fmtNum(cm), fmtNum(qm), change)
			}
			if len(names) > topTrend {
				p("\n%d more metrics omitted.\n", len(names)-topTrend)
			}
			p("\nUnknown-coverage values are left out of both sides.\n\n")
		}
	}

	// Task mix, so a lighter week is not read as an improvement.
	p("### Task mix\n\n")
	type mix struct {
		tasks             int
		members, volumes  []float64
		sessNow, sessPrev int
	}
	mx := map[string]*mix{}
	get := func(proj string) *mix {
		m := mx[proj]
		if m == nil {
			m = &mix{}
			mx[proj] = m
		}
		return m
	}
	for _, r := range rows {
		m := get(r.project(red))
		m.tasks++
		m.members = append(m.members, float64(len(r.t.Members)))
		m.volumes = append(m.volumes, float64(r.tok.volume))
	}
	excl := map[string]bool{}
	for _, v := range views {
		if v.excluded && v.s.Project != "" {
			excl[normPath(v.s.Project)] = true
		}
	}
	name := func(proj string) string {
		if excl[normPath(proj)] {
			return "(excluded)"
		}
		return projectName(proj, red)
	}
	for _, m := range in.Metrics {
		get(name(m.Project)).sessNow++
	}
	for _, m := range in.Previous {
		get(name(m.Project)).sessPrev++
	}
	if len(mx) == 0 {
		p("No tasks or sessions.\n")
		return
	}
	p("| Project | Candidate tasks | Median members per task | Median task text volume | Sessions now | Sessions before |\n|---|---|---|---|---|---|\n")
	for _, proj := range sortedKeys(mx) {
		m := mx[proj]
		med := func(xs []float64) string {
			if len(xs) == 0 {
				return "n/a"
			}
			return fmtNum(median(xs))
		}
		p("| %s | %d | %s | %s | %d | %d |\n", proj, m.tasks, med(m.members), med(m.volumes), m.sessNow, m.sessPrev)
	}
	p("\nThe previous run's task grouping is not available here, so its task mix is shown as sessions per project. Fewer or smaller tasks lower totals without any change in efficiency.\n")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func listSome(ss []string) string {
	if len(ss) == 0 {
		return "."
	}
	ss = uniqSorted(ss)
	s := ": " + strings.Join(ss[:min(maxListed, len(ss))], ", ")
	if len(ss) > maxListed {
		s += fmt.Sprintf(", … %d more", len(ss)-maxListed)
	}
	return s + "."
}
