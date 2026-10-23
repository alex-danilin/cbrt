package report

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// Anchor kinds.
const (
	AnchorFirstPrompt = "first_prompt"
	AnchorCorrection  = "correction"
	AnchorError       = "error"
	AnchorClaim       = "completion_claim"
	AnchorTest        = "test_result"
	AnchorHandoff     = "handoff"
)

// maxAnchorRunes caps an anchor's text.
const maxAnchorRunes = 300

// Anchor is one line of anchors.jsonl.
type Anchor struct {
	Session string `json:"session"`
	Kind    string `json:"kind"`
	Seq     string `json:"seq"`
	Text    string `json:"text"`

	idx int // event index in the session (for excerpt spans)
}

var (
	// §3.2.3 correction patterns: a leading "no"/"wrong", or one of the
	// phrases anywhere.
	reCorrection = regexp.MustCompile(`(?i)^\W*(?:no|nope|wrong)\b|\b(?:not that|revert|actually|i meant|that's wrong|that is wrong|undo that|you broke)\b`)
	reCompletion = regexp.MustCompile(`(?i)\b(?:done|fixed|implemented|completed?|all tests pass(?:ed)?|tests pass(?:ed)?)\b`)
	reTestCmd    = regexp.MustCompile(`(?i)\b(?:go test|cargo test|npm (?:run )?test|pnpm (?:run )?test|yarn test|pytest|vitest|jest|playwright test|mvn test|gradle test|dotnet test|make test|rspec)\b`)
)

// trimRunes cuts s to n runes, appending "…" when cut.
func trimRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// sessionAnchors extracts the anchors of one session's in-window events.
// Handoff anchors are added by the caller (they need the tasks).
func sessionAnchors(v *view, red *Redactor) []Anchor {
	s := v.s
	key := s.Key.String()
	var out []Anchor
	add := func(kind string, i int, text string) {
		e := &s.Events[i]
		out = append(out, Anchor{
			Session: key, Kind: kind, Seq: e.Seq,
			// Redact before trimming, so a cut cannot split a secret into a
			// fragment the patterns no longer recognize.
			Text: neutralize(trimRunes(red.Apply(text), maxAnchorRunes)), idx: i,
		})
	}
	seenPrompt := false
	lastAssistant := -1
	calls := map[string]int{} // CorrelationID -> index of a test-command call
	for _, i := range v.win {
		e := &s.Events[i]
		if e.ObservedBy == analyze.FromRegistry && s.LinkedTo[e.Seq] != "" {
			continue // the transcript event it links to is anchored instead
		}
		switch e.Kind {
		case analyze.KindUserPrompt:
			if !seenPrompt {
				seenPrompt = true
				add(AnchorFirstPrompt, i, e.Text)
			} else if reCorrection.MatchString(e.Text) {
				add(AnchorCorrection, i, e.Text)
			}
		case analyze.KindError:
			add(AnchorError, i, e.Text)
		case analyze.KindClaimComplete:
			add(AnchorClaim, i, e.Text)
		case analyze.KindTestResult:
			add(AnchorTest, i, testText(red, e.Text, e.ToolSuccess))
		case analyze.KindAssistantText:
			if strings.TrimSpace(e.Text) != "" {
				lastAssistant = i
			}
		case analyze.KindToolCall:
			if e.ToolName == analyze.ToolShell && reTestCmd.MatchString(e.Text) && e.CorrelationID != "" {
				calls[e.CorrelationID] = i
			}
		case analyze.KindToolResult:
			if ci, ok := calls[e.CorrelationID]; ok {
				delete(calls, e.CorrelationID)
				add(AnchorTest, ci, testText(red, s.Events[ci].Text, e.ToolSuccess))
			}
		}
	}
	// The completion claim is the last assistant answer when it claims so
	// (adapters do not emit task_claimed_complete themselves).
	if lastAssistant >= 0 && reCompletion.MatchString(s.Events[lastAssistant].Text) && !hasKind(out, AnchorClaim) {
		add(AnchorClaim, lastAssistant, s.Events[lastAssistant].Text)
	}
	sortAnchors(out)
	return out
}

func outcome(ok *bool) string {
	switch {
	case ok == nil:
		return " → outcome unknown"
	case *ok:
		return " → pass"
	}
	return " → fail"
}

func hasKind(as []Anchor, kind string) bool {
	for _, a := range as {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

// sortAnchors orders by event index, then kind (stable for equal events).
func sortAnchors(as []Anchor) {
	sort.SliceStable(as, func(i, j int) bool {
		if as[i].idx != as[j].idx {
			return as[i].idx < as[j].idx
		}
		return as[i].Kind < as[j].Kind
	})
}

// testText is a test command (redacted, trimmed so the outcome survives the
// anchor cap) and its outcome.
func testText(red *Redactor, cmd string, ok *bool) string {
	return trimRunes(red.Apply(cmd), maxAnchorRunes-30) + outcome(ok)
}

// successful reports a session with a passing test and no corrections or
// failing tests after it: the "successful" sample class.
func successful(as []Anchor) bool {
	pass := false
	for _, a := range as {
		switch a.Kind {
		case AnchorCorrection:
			return false
		case AnchorTest:
			pass = strings.HasSuffix(a.Text, "→ pass")
		}
	}
	return pass
}
