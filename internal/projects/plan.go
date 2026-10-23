// Package projects is `cbrt projects`: a cross-project progress tracker.
// Each connected repository keeps its plan in PLAN.md and its agents learn
// the format from a rule block in AGENTS.md; cbrt keeps the list of
// connected folders and serves a loopback-only overview page.
//
// The PLAN.md parser never writes. The only write cbrt makes to a plan is
// the park toggle (Toggle), which changes one marker character on one line.
package projects

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"
)

// Task states, from the marker between the brackets of "- [?] text".
const (
	Open    = "open"    // "[ ]"
	Active  = "active"  // "[>]"
	Done    = "done"    // "[x]" or "[X]"
	Parked  = "parked"  // "[~]": switched off; not counted in progress
	Unknown = "unknown" // any other marker: shown as-is, counted as open
)

// bom is the UTF-8 byte order mark some editors write first.
const bom = "\xef\xbb\xbf"

// MaxPlanBytes is the largest PLAN.md that is parsed.
const MaxPlanBytes = 1 << 20

// Plan is a parsed PLAN.md.
type Plan struct {
	Title      string // the text after "# " on the title line
	Goal       string
	Now        string
	Milestones []Milestone // tasks before the first "##" go in one with an empty Name
}

// Milestone is a "##" section and its top-level tasks.
type Milestone struct {
	Name  string
	Tasks []Task
}

// Task is one top-level "- [?] text" line.
type Task struct {
	Marker string // the marker as written, e.g. " ", "x", ">", "~", "?"
	State  string
	Text   string
	Notes  []string // indented lines below the task, trimmed
	Line   int      // 1-based
}

// Progress is done / (all − parked) for a milestone or a plan.
type Progress struct {
	Done, Total int
}

// Percent is the share done, 0..100 (0 when there is nothing to do).
func (p Progress) Percent() int {
	if p.Total == 0 {
		return 0
	}
	return p.Done * 100 / p.Total
}

// Progress of the milestone: parked tasks are left out.
func (m Milestone) Progress() Progress {
	var p Progress
	for _, t := range m.Tasks {
		switch t.State {
		case Parked:
		case Done:
			p.Done++
			p.Total++
		default:
			p.Total++
		}
	}
	return p
}

// Progress of the whole plan.
func (p *Plan) Progress() Progress {
	var out Progress
	for _, m := range p.Milestones {
		mp := m.Progress()
		out.Done += mp.Done
		out.Total += mp.Total
	}
	return out
}

// Next returns up to n tasks still to do (active first, then open and
// unknown markers), in plan order.
func (p *Plan) Next(n int) []Task {
	var active, rest []Task
	for _, m := range p.Milestones {
		for _, t := range m.Tasks {
			switch t.State {
			case Active:
				active = append(active, t)
			case Open, Unknown:
				rest = append(rest, t)
			}
		}
	}
	out := append(active, rest...)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Parse errors: the file is not a plan cbrt can read.
var (
	ErrTooLarge = errors.New("PLAN.md is larger than 1 MiB")
	ErrBinary   = errors.New("PLAN.md is not UTF-8 text")
	ErrNoTitle  = errors.New(`PLAN.md has no "# Plan" title line`)
)

// Parse reads a PLAN.md. Free text is allowed anywhere and ignored; a plan
// needs a "# Plan…" title line. Both LF and CRLF line endings are read.
func Parse(b []byte) (*Plan, error) {
	if len(b) > MaxPlanBytes {
		return nil, ErrTooLarge
	}
	b = bytes.TrimPrefix(b, []byte(bom))
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return nil, ErrBinary
	}
	p := &Plan{}
	titled := false
	var cur *Milestone // nil until a "##" or a task before any
	var task *Task     // the task indented notes attach to
	var field *string  // Goal or Now, for continuation lines
	fence := false
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = !fence
			task, field = nil, nil
			continue
		}
		if fence {
			continue
		}
		indented := line != "" && (line[0] == ' ' || line[0] == '\t')
		switch {
		case trimmed == "":
			field = nil // a blank line ends Goal/Now; notes may continue after it
			continue
		case indented && task != nil:
			task.Notes = append(task.Notes, trimmed)
			continue
		case indented && field != nil:
			*field += " " + trimmed
			continue
		}
		task, field = nil, nil
		switch {
		case !titled && heading(line, 1) != "" && isPlanTitle(heading(line, 1)):
			titled = true
			p.Title = heading(line, 1)
		case heading(line, 2) != "":
			p.Milestones = append(p.Milestones, Milestone{Name: heading(line, 2)})
			cur = &p.Milestones[len(p.Milestones)-1]
		case cur == nil && hasField(line, "Goal:"):
			p.Goal = strings.TrimSpace(line[len("Goal:"):])
			field = &p.Goal
		case cur == nil && hasField(line, "Now:"):
			p.Now = strings.TrimSpace(line[len("Now:"):])
			field = &p.Now
		default:
			t, ok := parseTask(line)
			if !ok {
				continue // free text
			}
			t.Line = i + 1
			if cur == nil {
				p.Milestones = append(p.Milestones, Milestone{})
				cur = &p.Milestones[len(p.Milestones)-1]
			}
			cur.Tasks = append(cur.Tasks, t)
			task = &cur.Tasks[len(cur.Tasks)-1]
		}
	}
	if !titled {
		return nil, ErrNoTitle
	}
	return p, nil
}

// heading returns the text of a level-n ATX heading line, or "".
func heading(line string, n int) string {
	prefix := strings.Repeat("#", n) + " "
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line[len(prefix):]), "#"))
}

func isPlanTitle(t string) bool {
	return len(t) >= 4 && strings.EqualFold(t[:4], "plan") && (len(t) == 4 || t[4] == ':' || t[4] == ' ')
}

func hasField(line, name string) bool {
	return len(line) >= len(name) && strings.EqualFold(line[:len(name)], name)
}

// markerAt returns the byte offset and length of the marker m in a task
// line "- [m] text" (the line without its "\r"); ok is false for any other
// line. Parse and Toggle both use it, so the marker the page shows is the
// byte the toggle rewrites.
func markerAt(line string) (off, size int, ok bool) {
	if len(line) < 2 || (line[0] != '-' && line[0] != '*') || line[1] != ' ' {
		return 0, 0, false
	}
	rest := strings.TrimLeft(line[2:], " ")
	if !strings.HasPrefix(rest, "[") {
		return 0, 0, false
	}
	r, size := utf8.DecodeRuneInString(rest[1:])
	if r == utf8.RuneError || !strings.HasPrefix(rest[1+size:], "]") {
		return 0, 0, false
	}
	return len(line) - len(rest) + 1, size, true
}

// parseTask reads "- [m] text" (also "* [m] text") at the start of a line;
// m is exactly one character.
func parseTask(line string) (Task, bool) {
	off, size, ok := markerAt(line)
	if !ok {
		return Task{}, false
	}
	m := line[off : off+size]
	t := Task{Marker: m, Text: strings.TrimSpace(line[off+size+1:])}
	switch m {
	case " ":
		t.State = Open
	case "x", "X":
		t.State = Done
	case ">":
		t.State = Active
	case "~":
		t.State = Parked
	default:
		t.State = Unknown
	}
	return t, true
}
