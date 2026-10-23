package metrics

import (
	"regexp"
	"strings"
)

// The English patterns behind the pattern-based facts. They are the only
// place wording is interpreted; every fact that uses them has coverage
// Inferred. Changing any of them changes a formula: bump Version.
var (
	// CompletionPattern: the assistant says it is done (claimed_complete).
	CompletionPattern = regexp.MustCompile(`(?i)\b(done|fixed|implemented|completed?|finished|resolved|all (?:the )?tests (?:now )?pass(?:ed|ing)?|tests (?:now )?pass(?:ed|ing)?|ready (?:for review|to merge))\b`)

	// TestRunPattern: a shell command line that runs tests (evidence_complete).
	TestRunPattern = regexp.MustCompile(`(?i)(\bgo (?:test|vet)\b|\b(?:npm|pnpm|yarn|bun)(?: run)? test\b|\b(?:vitest|jest|playwright test|pytest|rspec|phpunit|ctest)\b|\bpython3? -m (?:pytest|unittest)\b|\bcargo (?:test|nextest)\b|\bmvn (?:\S+ )*test\b|\bgradlew? test\b|\bdotnet test\b|\bmake (?:test|check)\b)`)

	// CommitPattern: a shell command line that commits (evidence_complete).
	CommitPattern = regexp.MustCompile(`(?i)\bgit\s+(?:-c\s+\S+\s+)*commit\b`)

	// CorrectionPattern: the user corrects the agent (corrections). "no" counts
	// only at the start of the prompt, so "no need to" in the middle does not.
	CorrectionPattern = regexp.MustCompile(`(?i)(^\W*(?:no|nope)\b|\bwrong\b|\bnot that\b|\brevert\b|\bundo\b|\bactually\b|\bi meant\b|\bthat'?s not what\b|\bthat is not what\b|\bdon'?t do that\b|\bstill (?:broken|failing|fails|wrong)\b)`)

	// termPattern splits a prompt into candidate key terms.
	termPattern = regexp.MustCompile(`[a-z0-9_./-]{4,}`)
)

// Repetition: a prompt repeats an earlier one when it has at least
// repeatMinTerms key terms and at least two thirds of them occur in one
// earlier prompt of the same session.
const (
	repeatMinTerms = 3
	repeatShareNum = 2 // repeatShareNum/repeatShareDen of the terms
	repeatShareDen = 3
)

var stopwords = map[string]bool{
	"this": true, "that": true, "with": true, "from": true, "have": true, "what": true,
	"when": true, "where": true, "which": true, "will": true, "would": true, "could": true,
	"should": true, "there": true, "their": true, "them": true, "then": true, "than": true,
	"please": true, "into": true, "your": true, "just": true, "also": true, "make": true,
	"sure": true, "does": true, "about": true, "only": true, "some": true, "more": true,
	"like": true, "these": true, "those": true, "been": true, "were": true, "they": true,
	"here": true, "very": true, "want": true, "need": true, "okay": true, "thanks": true,
}

// keyTerms returns the distinct key terms of a prompt.
func keyTerms(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range termPattern.FindAllString(strings.ToLower(text), -1) {
		w = strings.Trim(w, "./-")
		if len(w) >= 4 && !stopwords[w] {
			out[w] = true
		}
	}
	return out
}

// repeats reports whether terms repeat any of the earlier term sets.
func repeats(terms map[string]bool, earlier []map[string]bool) bool {
	if len(terms) < repeatMinTerms {
		return false
	}
	for _, prev := range earlier {
		shared := 0
		for t := range terms {
			if prev[t] {
				shared++
			}
		}
		if shared*repeatShareDen >= repeatShareNum*len(terms) {
			return true
		}
	}
	return false
}
