package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// Recommendation categories (§3.3 step 5).
const (
	CatInstructions        = "instructions"
	CatSkillOrHook         = "skill_or_hook"
	CatPermissionAllowlist = "permission_allowlist"
	CatModelPolicy         = "model_policy"
	CatUserHabit           = "user_habit"
)

// Confidence levels.
const (
	ConfLow  = "low"
	ConfMed  = "med"
	ConfHigh = "high"
)

// impl_status: the user's decision.
const (
	ImplProposed = "proposed"
	ImplApplied  = "applied"
	ImplDeclined = "declined"
	ImplReverted = "reverted"
)

// eval_status: the reviewer's verdict on an applied recommendation.
const (
	EvalPending      = "pending"
	EvalImproved     = "associated_improvement"
	EvalNoChange     = "no_change"
	EvalWorsened     = "worsened"
	EvalInconclusive = "inconclusive"
)

// Scope limits a recommendation to tools (adapter ids) and projects; empty
// means all.
type Scope struct {
	Tools    []string `json:"tools,omitempty"`
	Projects []string `json:"projects,omitempty"`
}

// Baseline is the metric value captured when the recommendation was made.
// Value is a pointer so a real 0 is distinguishable from "not given".
type Baseline struct {
	Metric string         `json:"metric"`
	Value  *float64       `json:"value"`
	Window analyze.Window `json:"window"`
}

// Recommendation is one record of state/recommendations.json (§3.2.5).
type Recommendation struct {
	RecID        string    `json:"rec_id"`
	CreatedRunID string    `json:"created_run_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Title        string    `json:"title"`
	Category     string    `json:"category"`
	EvidenceRefs []string  `json:"evidence_refs"`
	Cause        string    `json:"cause"`
	Alternatives string    `json:"alternatives"`
	Change       string    `json:"change"`
	CostMinutes  int       `json:"cost_minutes"`
	Confidence   string    `json:"confidence"`
	Scope        Scope     `json:"scope"`
	// ContextChecked lists the project files the reviewer read before
	// recommending (paths relative to the project root), or is the single
	// entry "unavailable: <reason>". Required when Scope.Projects is set.
	ContextChecked []string   `json:"context_checked,omitempty"`
	Baseline       Baseline   `json:"baseline"`
	CheckMethod    string     `json:"check_method"`
	MinEvidence    string     `json:"min_evidence"`
	EvalWindowDays int        `json:"eval_window_days"`
	ImplStatus     string     `json:"impl_status"`
	ImplAt         *time.Time `json:"impl_at,omitempty"`
	ImplNotes      string     `json:"impl_notes,omitempty"`
	EvalStatus     string     `json:"eval_status"`
	EvalRunID      string     `json:"eval_run_id,omitempty"`
	EvalNotes      string     `json:"eval_notes,omitempty"`
}

// Recs is state/recommendations.json.
type Recs struct {
	V     int              `json:"v"`
	Items []Recommendation `json:"items"`
}

// ErrNotFound is returned for an unknown rec_id.
var ErrNotFound = errors.New("recommendation not found")

// LoadRecs reads state/recommendations.json, upgrading older versions. A
// missing file is an empty, current-version Recs.
func LoadRecs(home string) (Recs, error) {
	r := Recs{V: current[kindRecs]}
	b, err := readVersioned(recsPath(home), kindRecs)
	if err != nil || b == nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Recs{}, fmt.Errorf("%s: %w", RecsFile, err)
	}
	r.V = current[kindRecs]
	return r, nil
}

// SaveRecs writes state/recommendations.json atomically at the current version.
func SaveRecs(home string, r Recs) error {
	r.V = current[kindRecs]
	if r.Items == nil {
		r.Items = []Recommendation{}
	}
	return WriteJSONAtomic(recsPath(home), r)
}

// Validate checks the fields a reviewer must supply (§3.3 step 4).
func (r Recommendation) Validate() error {
	var miss []string
	need := func(ok bool, name string) {
		if !ok {
			miss = append(miss, name)
		}
	}
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	need(!blank(r.Title), "title")
	switch r.Category {
	case CatInstructions, CatSkillOrHook, CatPermissionAllowlist, CatModelPolicy, CatUserHabit:
	default:
		miss = append(miss, fmt.Sprintf("category (%q is not one of instructions, skill_or_hook, permission_allowlist, model_policy, user_habit)", r.Category))
	}
	refs := 0
	for _, e := range r.EvidenceRefs {
		if !blank(e) {
			refs++
		}
	}
	need(refs > 0, "evidence_refs")
	need(!blank(r.Cause), "cause")
	need(!blank(r.Alternatives), "alternatives")
	need(!blank(r.Change), "change")
	need(r.CostMinutes > 0, "cost_minutes (> 0)")
	switch r.Confidence {
	case ConfLow, ConfMed, ConfHigh:
	default:
		miss = append(miss, fmt.Sprintf("confidence (%q is not low, med or high)", r.Confidence))
	}
	need(!blank(r.Baseline.Metric), "baseline.metric")
	need(r.Baseline.Value != nil, "baseline.value")
	need(!blank(r.CheckMethod), "check_method")
	need(!blank(r.MinEvidence), "min_evidence")
	need(r.EvalWindowDays > 0, "eval_window_days (> 0)")
	if msg := checkContext(r.ContextChecked, len(r.Scope.Projects) > 0); msg != "" {
		miss = append(miss, msg)
	}
	if len(miss) > 0 {
		return fmt.Errorf("invalid recommendation: missing or invalid %s", strings.Join(miss, ", "))
	}
	return nil
}

// checkContext validates context_checked; required says scope.projects is
// set. It returns "" when valid, else what is wrong.
func checkContext(cc []string, required bool) string {
	const name = "context_checked"
	if len(cc) == 0 {
		if required {
			return name + " (required when scope.projects is set: the project files read, relative to the project root, or \"unavailable: <reason>\")"
		}
		return ""
	}
	if r, ok := strings.CutPrefix(cc[0], "unavailable:"); ok {
		if len(cc) != 1 || strings.TrimSpace(r) == "" {
			return name + ` ("unavailable: <reason>" must be the only entry and give a reason)`
		}
		return ""
	}
	for _, p := range cc {
		p = strings.TrimSpace(p)
		parts := strings.FieldsFunc(p, func(c rune) bool { return c == '/' || c == '\\' })
		abs := strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "~") || (len(p) >= 2 && p[1] == ':')
		up := false
		for _, e := range parts {
			up = up || e == ".."
		}
		if p == "" || abs || up || strings.HasPrefix(p, "unavailable:") {
			return fmt.Sprintf("%s (%q: want a path relative to the project root, or a single \"unavailable: <reason>\")", name, p)
		}
	}
	return ""
}

// AddRec validates r, assigns the next rec_id ("r_<n>"), sets impl_status
// proposed and eval_status pending, clears lifecycle fields, and saves.
func AddRec(home string, r Recommendation, now time.Time) (Recommendation, error) {
	if err := r.Validate(); err != nil {
		return Recommendation{}, err
	}
	all, err := LoadRecs(home)
	if err != nil {
		return Recommendation{}, err
	}
	n := 0
	for _, it := range all.Items {
		if k := recNum(it.RecID); k > n {
			n = k
		}
	}
	r.RecID = "r_" + strconv.Itoa(n+1)
	r.CreatedAt = now.UTC()
	r.ImplStatus, r.ImplAt, r.ImplNotes = ImplProposed, nil, ""
	r.EvalStatus, r.EvalRunID, r.EvalNotes = EvalPending, "", ""
	all.Items = append(all.Items, r)
	if err := SaveRecs(home, all); err != nil {
		return Recommendation{}, err
	}
	return r, nil
}

// recNum is n of "r_<n>", 0 if the id has another shape.
func recNum(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "r_"))
	if err != nil || !strings.HasPrefix(id, "r_") || n < 0 {
		return 0
	}
	return n
}

// SetImpl records the user's decision. Allowed transitions:
//
//	proposed -> applied | declined
//	declined -> applied
//	applied  -> reverted
//	reverted -> applied
//
// Applying (again) restarts the evaluation: eval_status goes back to pending.
func SetImpl(home, recID, status, notes string, now time.Time) error {
	all, err := LoadRecs(home)
	if err != nil {
		return err
	}
	i := find(all, recID)
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, recID)
	}
	r := &all.Items[i]
	ok := false
	switch status {
	case ImplApplied:
		ok = r.ImplStatus == ImplProposed || r.ImplStatus == ImplDeclined || r.ImplStatus == ImplReverted
	case ImplDeclined:
		ok = r.ImplStatus == ImplProposed
	case ImplReverted:
		ok = r.ImplStatus == ImplApplied
	default:
		return fmt.Errorf("impl status %q is not applied, declined or reverted", status)
	}
	if !ok {
		return fmt.Errorf("%s is %s; cannot mark it %s", recID, r.ImplStatus, status)
	}
	t := now.UTC()
	r.ImplStatus, r.ImplAt, r.ImplNotes = status, &t, notes
	if status == ImplApplied {
		r.EvalStatus, r.EvalRunID, r.EvalNotes = EvalPending, "", ""
	}
	return SaveRecs(home, all)
}

// SetEval records the reviewer's verdict on an applied recommendation.
func SetEval(home, recID, status, notes, runID string) error {
	switch status {
	case EvalImproved, EvalNoChange, EvalWorsened, EvalInconclusive:
	default:
		return fmt.Errorf("eval status %q is not associated_improvement, no_change, worsened or inconclusive", status)
	}
	if strings.TrimSpace(runID) == "" {
		return errors.New("eval needs the run id it was made in")
	}
	all, err := LoadRecs(home)
	if err != nil {
		return err
	}
	i := find(all, recID)
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, recID)
	}
	r := &all.Items[i]
	if r.ImplStatus != ImplApplied {
		return fmt.Errorf("%s is %s, not applied; only applied recommendations are evaluated", recID, r.ImplStatus)
	}
	r.EvalStatus, r.EvalNotes, r.EvalRunID = status, notes, runID
	return SaveRecs(home, all)
}

func find(r Recs, id string) int {
	for i, it := range r.Items {
		if it.RecID == id {
			return i
		}
	}
	return -1
}

// DueForEval returns the applied recommendations whose evaluation window has
// elapsed (impl_at + eval_window_days <= now) and that are still pending.
func DueForEval(r Recs, now time.Time) []Recommendation {
	var out []Recommendation
	for _, it := range r.Items {
		if it.ImplStatus != ImplApplied || it.EvalStatus != EvalPending || it.ImplAt == nil {
			continue
		}
		if !now.Before(it.ImplAt.AddDate(0, 0, it.EvalWindowDays)) {
			out = append(out, it)
		}
	}
	return out
}

// open reports whether a recommendation still needs the user or the reviewer.
func open(it Recommendation) bool {
	return it.ImplStatus == ImplProposed || (it.ImplStatus == ImplApplied && it.EvalStatus == EvalPending)
}

// recentClosed is how many closed items the export lists.
const recentClosed = 10

// ExportMarkdown renders recommendations.md: every open item in full, then
// the most recent closed items (declined, reverted, evaluated) in one line
// each, so the reviewer does not re-propose them without new evidence.
func ExportMarkdown(r Recs) string {
	var openItems, closed []Recommendation
	for _, it := range r.Items {
		if open(it) {
			openItems = append(openItems, it)
		} else {
			closed = append(closed, it)
		}
	}
	byNum := func(s []Recommendation, desc bool) {
		sort.SliceStable(s, func(i, j int) bool {
			if desc {
				return recNum(s[i].RecID) > recNum(s[j].RecID)
			}
			return recNum(s[i].RecID) < recNum(s[j].RecID)
		})
	}
	byNum(openItems, false)
	byNum(closed, true)
	if len(closed) > recentClosed {
		closed = closed[:recentClosed]
	}

	var b strings.Builder
	b.WriteString("# Recommendations\n\n")
	b.WriteString("Read-only export of state/recommendations.json, regenerated every run. ")
	b.WriteString("Record decisions with `cbrt analyze rec applied|declined|reverted <rec_id>`.\n\n")
	fmt.Fprintf(&b, "## Open (%d)\n\n", len(openItems))
	if len(openItems) == 0 {
		b.WriteString("None.\n\n")
	}
	for _, it := range openItems {
		fmt.Fprintf(&b, "### %s: %s\n\n", it.RecID, line(it.Title))
		status := it.ImplStatus
		if it.ImplStatus == ImplApplied {
			status += fmt.Sprintf(" %s, evaluation %s", day(it.ImplAt), it.EvalStatus)
			if it.ImplAt != nil {
				status += ", due " + it.ImplAt.AddDate(0, 0, it.EvalWindowDays).Format("2006-01-02")
			}
		}
		field := func(name, v string) {
			if strings.TrimSpace(v) != "" {
				fmt.Fprintf(&b, "- %s: %s\n", name, line(v))
			}
		}
		field("Status", status)
		field("Category", it.Category)
		field("Confidence", it.Confidence)
		field("Created", it.CreatedAt.Format("2006-01-02")+runSuffix(it.CreatedRunID))
		field("Change", it.Change)
		field("Cost", strconv.Itoa(it.CostMinutes)+" min")
		field("Cause", it.Cause)
		field("Alternatives", it.Alternatives)
		field("Evidence", strings.Join(it.EvidenceRefs, ", "))
		field("Scope", scopeText(it.Scope))
		field("Baseline", baselineText(it.Baseline))
		field("Check", it.CheckMethod)
		field("Minimum evidence", it.MinEvidence)
		field("Evaluation window", strconv.Itoa(it.EvalWindowDays)+" days")
		field("Notes", it.ImplNotes)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "## Recently closed (%d)\n\n", len(closed))
	if len(closed) == 0 {
		b.WriteString("None.\n")
	}
	for _, it := range closed {
		st := it.ImplStatus
		if it.ImplStatus == ImplApplied {
			st += ", " + it.EvalStatus
		}
		fmt.Fprintf(&b, "- %s: %s (%s, %s)", it.RecID, line(it.Title), it.Category, st)
		notes := it.EvalNotes
		if it.ImplStatus != ImplApplied {
			notes = it.ImplNotes
		}
		if strings.TrimSpace(notes) != "" {
			fmt.Fprintf(&b, ": %s", line(notes))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// line collapses whitespace so free text cannot break the Markdown structure.
func line(s string) string { return strings.Join(strings.Fields(s), " ") }

func day(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}

func runSuffix(id string) string {
	if id == "" {
		return ""
	}
	return " (run " + id + ")"
}

func scopeText(s Scope) string {
	tools, projects := "all tools", "all projects"
	if len(s.Tools) > 0 {
		tools = strings.Join(s.Tools, ", ")
	}
	if len(s.Projects) > 0 {
		projects = strings.Join(s.Projects, ", ")
	}
	return tools + "; " + projects
}

func baselineText(bl Baseline) string {
	s := bl.Metric
	if bl.Value != nil {
		s += " = " + strconv.FormatFloat(*bl.Value, 'g', -1, 64)
	}
	if !bl.Window.Since.IsZero() || !bl.Window.Until.IsZero() {
		s += " (" + bl.Window.Since.Format("2006-01-02") + " to " + bl.Window.Until.Format("2006-01-02") + ")"
	}
	return s
}

// WriteExport writes ExportMarkdown(r) to <home>/recommendations.md atomically.
func WriteExport(home string, r Recs) error {
	return writeAtomic(filepath.Join(home, ExportFile), []byte(ExportMarkdown(r)))
}
