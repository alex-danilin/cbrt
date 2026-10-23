// Package responses translates between the OpenAI Responses API (the only
// API Codex CLI speaks) and the OpenAI Chat Completions API spoken by
// upstream targets.
package responses

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Error is a client-facing translation error (always HTTP 400
// invalid_request_error).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errorf(format string, a ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, a...)}
}

// ErrorJSON returns an OpenAI error body:
// {"error":{"type":errType,"code":code,"message":msg}}. An empty code is
// encoded as null.
func ErrorJSON(errType, code, msg string) []byte {
	type detail struct {
		Type    string  `json:"type"`
		Code    *string `json:"code"`
		Message string  `json:"message"`
	}
	d := detail{Type: errType, Message: msg}
	if code != "" {
		d.Code = &code
	}
	return marshal(struct {
		Error detail `json:"error"`
	}{d})
}

// marshal encodes v as compact JSON without HTML escaping. Values built in
// this package always encode successfully.
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("responses: marshal: " + err.Error())
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

// compactJSON compacts valid JSON; invalid input is returned unchanged.
func compactJSON(raw []byte) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return json.RawMessage(raw)
	}
	return json.RawMessage(buf.Bytes())
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
