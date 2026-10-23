package events

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxDetailRunes caps redacted diagnostic text.
const maxDetailRunes = 200

var (
	reBearer   = regexp.MustCompile(`(?i)\bbearer [^ ]+`)
	reAPIKey   = regexp.MustCompile(`\b(?:nvapi-|sk-ant-|sk-)[A-Za-z0-9_\-]{8,}`)
	reAccount  = regexp.MustCompile(`(?i)\baccount '[^']*'`)
	reFunction = regexp.MustCompile(`(?i)\bfunction '[^']*'`)
	reUUID     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reLongTok  = regexp.MustCompile(`[A-Za-z0-9+/_\-]{24,}={0,2}`)
	reDigit    = regexp.MustCompile(`[0-9]`)
)

// Redact turns an upstream diagnostic message into loggable text: it
// collapses whitespace, masks API keys, bearer tokens, UUIDs, account and
// function identifiers and long opaque tokens, and caps the result to 200
// runes (appending "…" when cut).
func Redact(s string) string {
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	s = reBearer.ReplaceAllString(s, "Bearer [redacted]")
	s = reAPIKey.ReplaceAllString(s, "[redacted]")
	s = reAccount.ReplaceAllString(s, "account '<account>'")
	s = reFunction.ReplaceAllString(s, "Function '<id>'")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reLongTok.ReplaceAllStringFunc(s, func(tok string) string {
		// Long runs of letters only are usually words or identifiers;
		// mask them only when very long.
		if reDigit.MatchString(tok) || len(tok) >= 32 {
			return "<id>"
		}
		return tok
	})
	if utf8.RuneCountInString(s) > maxDetailRunes {
		r := []rune(s)
		s = string(r[:maxDetailRunes]) + "…"
	}
	return s
}
