// Package anthropic translates between the Anthropic Messages API (as used by
// Claude Code) and the OpenAI Chat Completions API spoken by upstream targets.
package anthropic

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Error is a client-facing translation error (always HTTP 400
// invalid_request_error).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errorf(format string, a ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, a...)}
}

// ErrorJSON returns an Anthropic error body:
// {"type":"error","error":{"type":errType,"message":msg}}.
func ErrorJSON(errType, msg string) []byte {
	type detail struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	return marshal(struct {
		Type  string `json:"type"`
		Error detail `json:"error"`
	}{"error", detail{errType, msg}})
}

// ErrorType maps an HTTP status to the Anthropic error type.
func ErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// marshal encodes v as compact JSON without HTML escaping. Values built in
// this package always encode successfully.
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("anthropic: marshal: " + err.Error())
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// randomID returns prefix followed by 24 random hex characters.
func randomID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// isNull reports whether raw is absent or the JSON literal null.
func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}
