// Package jsonspan splits JSON objects and arrays into the raw byte spans of
// their members without decoding the values. It validates the full JSON grammar
// in the same single pass, so spans can be spliced into new documents verbatim.
package jsonspan

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrSyntax reports malformed JSON.
var ErrSyntax = errors.New("jsonspan: invalid JSON")

// Member is one object member. KeyRaw includes the quotes exactly as written;
// Value is the value's bytes with no surrounding whitespace.
type Member struct {
	Key    string
	KeyRaw []byte
	Value  []byte
}

// Object parses data, which must be exactly one JSON object (whitespace around
// it allowed), into its members in document order. Duplicate keys are kept.
func Object(data []byte) ([]Member, error) {
	i := skipWS(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil, syntaxErr(i, "expected object")
	}
	members, end, err := object(data, i)
	if err != nil {
		return nil, err
	}
	if j := skipWS(data, end); j != len(data) {
		return nil, syntaxErr(j, "trailing data after object")
	}
	return members, nil
}

// Array parses data, which must be exactly one JSON array, into its elements' spans.
func Array(data []byte) ([][]byte, error) {
	i := skipWS(data, 0)
	if i >= len(data) || data[i] != '[' {
		return nil, syntaxErr(i, "expected array")
	}
	var out [][]byte
	i = skipWS(data, i+1)
	if i < len(data) && data[i] == ']' {
		return out, checkEnd(data, i+1)
	}
	for {
		start := i
		end, err := value(data, i, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, data[start:end])
		i = skipWS(data, end)
		if i >= len(data) {
			return nil, syntaxErr(i, "unterminated array")
		}
		switch data[i] {
		case ',':
			i = skipWS(data, i+1)
		case ']':
			return out, checkEnd(data, i+1)
		default:
			return nil, syntaxErr(i, "expected , or ]")
		}
	}
}

// Valid reports whether data is exactly one valid JSON value.
func Valid(data []byte) bool {
	i := skipWS(data, 0)
	end, err := value(data, i, 0)
	return err == nil && skipWS(data, end) == len(data)
}

// Get returns the value of the last member named key (encoding/json semantics
// for duplicate keys), or nil.
func Get(members []Member, key string) []byte {
	for i := len(members) - 1; i >= 0; i-- {
		if members[i].Key == key {
			return members[i].Value
		}
	}
	return nil
}

const maxDepth = 10000

func checkEnd(data []byte, i int) error {
	if j := skipWS(data, i); j != len(data) {
		return syntaxErr(j, "trailing data after array")
	}
	return nil
}

func syntaxErr(i int, msg string) error { return fmt.Errorf("%w at offset %d: %s", ErrSyntax, i, msg) }

func skipWS(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// object parses the object starting at data[i] == '{' and returns its members.
func object(data []byte, i int) ([]Member, int, error) {
	var out []Member
	i = skipWS(data, i+1)
	if i < len(data) && data[i] == '}' {
		return out, i + 1, nil
	}
	for {
		if i >= len(data) || data[i] != '"' {
			return nil, 0, syntaxErr(i, "expected string key")
		}
		kStart := i
		kEnd, escaped, err := str(data, i)
		if err != nil {
			return nil, 0, err
		}
		keyRaw := data[kStart:kEnd]
		key := string(keyRaw[1 : len(keyRaw)-1])
		if escaped {
			if err := json.Unmarshal(keyRaw, &key); err != nil {
				return nil, 0, syntaxErr(kStart, "bad key")
			}
		}
		i = skipWS(data, kEnd)
		if i >= len(data) || data[i] != ':' {
			return nil, 0, syntaxErr(i, "expected :")
		}
		i = skipWS(data, i+1)
		vStart := i
		vEnd, err := value(data, i, 1)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, Member{Key: key, KeyRaw: keyRaw, Value: data[vStart:vEnd]})
		i = skipWS(data, vEnd)
		if i >= len(data) {
			return nil, 0, syntaxErr(i, "unterminated object")
		}
		switch data[i] {
		case ',':
			i = skipWS(data, i+1)
		case '}':
			return out, i + 1, nil
		default:
			return nil, 0, syntaxErr(i, "expected , or }")
		}
	}
}

// value validates the value starting at data[i] and returns the index just past it.
func value(data []byte, i, depth int) (int, error) {
	if depth > maxDepth {
		return 0, syntaxErr(i, "nesting too deep")
	}
	if i >= len(data) {
		return 0, syntaxErr(i, "unexpected end of input")
	}
	switch c := data[i]; {
	case c == '"':
		end, _, err := str(data, i)
		return end, err
	case c == '{':
		i = skipWS(data, i+1)
		if i < len(data) && data[i] == '}' {
			return i + 1, nil
		}
		for {
			if i >= len(data) || data[i] != '"' {
				return 0, syntaxErr(i, "expected string key")
			}
			end, _, err := str(data, i)
			if err != nil {
				return 0, err
			}
			i = skipWS(data, end)
			if i >= len(data) || data[i] != ':' {
				return 0, syntaxErr(i, "expected :")
			}
			if i, err = value(data, skipWS(data, i+1), depth+1); err != nil {
				return 0, err
			}
			i = skipWS(data, i)
			if i >= len(data) {
				return 0, syntaxErr(i, "unterminated object")
			}
			if data[i] == '}' {
				return i + 1, nil
			}
			if data[i] != ',' {
				return 0, syntaxErr(i, "expected , or }")
			}
			i = skipWS(data, i+1)
		}
	case c == '[':
		i = skipWS(data, i+1)
		if i < len(data) && data[i] == ']' {
			return i + 1, nil
		}
		for {
			var err error
			if i, err = value(data, i, depth+1); err != nil {
				return 0, err
			}
			i = skipWS(data, i)
			if i >= len(data) {
				return 0, syntaxErr(i, "unterminated array")
			}
			if data[i] == ']' {
				return i + 1, nil
			}
			if data[i] != ',' {
				return 0, syntaxErr(i, "expected , or ]")
			}
			i = skipWS(data, i+1)
		}
	case c == 't':
		return literal(data, i, "true")
	case c == 'f':
		return literal(data, i, "false")
	case c == 'n':
		return literal(data, i, "null")
	case c == '-' || (c >= '0' && c <= '9'):
		return number(data, i)
	default:
		return 0, syntaxErr(i, "unexpected character")
	}
}

func literal(data []byte, i int, lit string) (int, error) {
	if len(data)-i < len(lit) || string(data[i:i+len(lit)]) != lit {
		return 0, syntaxErr(i, "bad literal")
	}
	return i + len(lit), nil
}

// str validates the string starting at data[i] == '"'; escaped reports whether it contains escapes.
func str(data []byte, i int) (end int, escaped bool, err error) {
	i++
	for i < len(data) {
		c := data[i]
		switch {
		case c == '"':
			return i + 1, escaped, nil
		case c == '\\':
			escaped = true
			if i+1 >= len(data) {
				return 0, false, syntaxErr(i, "unterminated escape")
			}
			switch data[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if i+6 > len(data) || !isHex(data[i+2]) || !isHex(data[i+3]) || !isHex(data[i+4]) || !isHex(data[i+5]) {
					return 0, false, syntaxErr(i, "bad \\u escape")
				}
				i += 6
			default:
				return 0, false, syntaxErr(i, "bad escape")
			}
		case c < 0x20:
			return 0, false, syntaxErr(i, "control character in string")
		default:
			i++
		}
	}
	return 0, false, syntaxErr(i, "unterminated string")
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func number(data []byte, i int) (int, error) {
	start := i
	if data[i] == '-' {
		i++
	}
	switch {
	case i < len(data) && data[i] == '0':
		i++
	case i < len(data) && data[i] >= '1' && data[i] <= '9':
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	default:
		return 0, syntaxErr(start, "bad number")
	}
	if i < len(data) && data[i] == '.' {
		i++
		d := i
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
		if i == d {
			return 0, syntaxErr(start, "bad fraction")
		}
	}
	if i < len(data) && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < len(data) && (data[i] == '+' || data[i] == '-') {
			i++
		}
		d := i
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
		if i == d {
			return 0, syntaxErr(start, "bad exponent")
		}
	}
	return i, nil
}
