// Package jsonscan is a small allocation-free JSON toolkit for the API
// request decoders.
//
// NewIndex validates a whole body once (exactly as json.Valid would); after
// that the walkers below slice values out of the body without copying or
// re-validating them, so large opaque values (tool schemas, message texts)
// are passed through to the output as the original bytes. Struct-like
// objects are matched field by field with the same rules as encoding/json
// (case-insensitive key folding, null leaves a field unchanged, last
// duplicate wins, type mismatches are errors).
package jsonscan

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// maxNestingDepth matches encoding/json's scanner limit.
const maxNestingDepth = 10000

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipWS(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

// TrimSpace trims JSON whitespace from both ends.
func TrimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

// strSpecial marks bytes that end the fast path inside a string.
var strSpecial = func() (t [256]bool) {
	for c := 0; c < 0x20; c++ {
		t[c] = true
	}
	t['"'], t['\\'] = true, true
	return t
}()

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// indexDepth is the nesting depth up to which containers are indexed: deep
// enough for every object the decoders look into.
const indexDepth = 8

// Index speeds up walking a validated body: it records where each
// container at depth <= indexDepth ends, so that skipping a large value
// (a tool list, a schema) is a lookup instead of a scan, and whether the
// body has whitespace outside strings at all, so that compacting a value is
// usually a plain copy.
type Index struct {
	body    []byte
	starts  []int32 // offsets of the indexed containers, ascending
	ends    []int32 // offsets just past them
	spacing bool    // whitespace outside strings somewhere in body
}

// NewIndex validates body like json.Valid and indexes it; it returns nil if
// body is not valid JSON. Bodies too large for int32 offsets are validated
// but not indexed.
func NewIndex(body []byte) *Index {
	x := &Index{body: body}
	if len(body) > 1<<31-1 {
		x.body = nil
	} else {
		n := len(body)/128 + 16 // about right for API requests
		x.starts, x.ends = make([]int32, 0, n), make([]int32, 0, n)
	}
	if !x.scan(body) {
		return nil
	}
	return x
}

// validJSON reports whether b is one valid JSON value (with optional
// surrounding whitespace). It accepts and rejects exactly what json.Valid
// does, including the nesting limit, but is several times faster.
func validJSON(b []byte) bool { return (&Index{}).scan(b) }

// scan validates b; if x.body is set, it also fills in the index.
func (x *Index) scan(b []byte) bool {
	var stackBuf [64]byte
	stack := stackBuf[:0] // '{' or '['
	var open [indexDepth]int32
	record := x.body != nil
	n := len(b)
	i := skipWS(b, 0)
	x.spacing = i > 0
value:
	if i >= n {
		return false
	}
	switch c := b[i]; {
	case c == '{', c == '[':
		stack = append(stack, c)
		if len(stack) > maxNestingDepth {
			return false
		}
		if record && len(stack) <= indexDepth {
			open[len(stack)-1] = int32(len(x.starts))
			x.starts = append(x.starts, int32(i))
			x.ends = append(x.ends, 0)
		}
		j := skipWS(b, i+1)
		x.spacing = x.spacing || j > i+1
		i = j
		if c == '{' {
			if i < n && b[i] == '}' {
				goto pop
			}
			goto key
		}
		if i < n && b[i] == ']' {
			goto pop
		}
		goto value
	case c == '"':
		if i = validString(b, i); i < 0 {
			return false
		}
	case c == '-' || c >= '0' && c <= '9':
		if i = validNumber(b, i); i < 0 {
			return false
		}
	case c == 't':
		if n-i < 4 || string(b[i:i+4]) != "true" {
			return false
		}
		i += 4
	case c == 'f':
		if n-i < 5 || string(b[i:i+5]) != "false" {
			return false
		}
		i += 5
	case c == 'n':
		if n-i < 4 || string(b[i:i+4]) != "null" {
			return false
		}
		i += 4
	default:
		return false
	}
end:
	if j := skipWS(b, i); j != i {
		x.spacing, i = true, j
	}
	if len(stack) == 0 {
		return i == n
	}
	if i >= n {
		return false
	}
	switch b[i] {
	case ',':
		j := skipWS(b, i+1)
		x.spacing = x.spacing || j > i+1
		i = j
		if stack[len(stack)-1] == '{' {
			goto key
		}
		goto value
	case '}':
		if stack[len(stack)-1] != '{' {
			return false
		}
	case ']':
		if stack[len(stack)-1] != '[' {
			return false
		}
	default:
		return false
	}
pop: // b[i] closes the innermost container
	if record && len(stack) <= indexDepth {
		x.ends[open[len(stack)-1]] = int32(i + 1)
	}
	stack = stack[:len(stack)-1]
	i++
	goto end
key:
	if i >= n || b[i] != '"' {
		return false
	}
	if i = validString(b, i); i < 0 {
		return false
	}
	if j := skipWS(b, i); j != i {
		x.spacing, i = true, j
	}
	if i >= n || b[i] != ':' {
		return false
	}
	if j := skipWS(b, i+1); j != i+1 {
		x.spacing, i = true, j
	} else {
		i = j
	}
	goto value
}

// end returns the index after the value starting at v[i], where v is a
// slice of the indexed body (else it falls back to scanning).
func (x *Index) end(v []byte, i int) int {
	if c := v[i]; (c == '{' || c == '[') && x != nil && len(x.starts) > 0 {
		// v's offset in the body, from the capacities; the pointer
		// comparison makes sure v really is a slice of the body.
		off := cap(x.body) - cap(v)
		if abs := off + i; abs >= 0 && abs < len(x.body) && &x.body[abs] == &v[i] {
			if k, ok := slices.BinarySearch(x.starts, int32(abs)); ok {
				return int(x.ends[k]) - off
			}
		}
	}
	return valueEnd(v, i)
}

// AppendCompact appends the value v (a slice of the indexed body) without
// insignificant whitespace.
func (x *Index) AppendCompact(dst, v []byte) []byte {
	if x != nil && !x.spacing && x.body != nil {
		return append(dst, v...)
	}
	return appendCompact(dst, v)
}

// CompactLen is len(x.AppendCompact(nil, v)).
func (x *Index) CompactLen(v []byte) int {
	if x != nil && !x.spacing && x.body != nil {
		return len(v)
	}
	return compactLen(v)
}

// validString validates the string starting at b[i] == '"' and returns the
// index after it, or -1.
func validString(b []byte, i int) int {
	const (
		ones   = 0x0101010101010101
		highs  = 0x8080808080808080
		quotes = ones * '"'
		slashs = ones * '\\'
		ctrls  = ones * 0x20
	)
	i++
	for i < len(b) {
		// Skip 8 ordinary bytes at a time: no '"', '\\' or control byte.
		for i+8 <= len(b) {
			w := binary.LittleEndian.Uint64(b[i:])
			q, s := w^quotes, w^slashs
			if ((q-ones)&^q|(s-ones)&^s|(w-ctrls)&^w)&highs != 0 {
				break
			}
			i += 8
		}
		for i < len(b) && !strSpecial[b[i]] {
			i++
		}
		if i >= len(b) {
			return -1
		}
		c := b[i]
		switch {
		case c == '"':
			return i + 1
		case c < 0x20:
			return -1
		}
		// backslash
		if i+1 >= len(b) {
			return -1
		}
		switch b[i+1] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			i += 2
		case 'u':
			if i+6 > len(b) || !isHex(b[i+2]) || !isHex(b[i+3]) || !isHex(b[i+4]) || !isHex(b[i+5]) {
				return -1
			}
			i += 6
		default:
			return -1
		}
	}
	return -1
}

// validNumber validates the number starting at b[i] and returns the index
// after it, or -1.
func validNumber(b []byte, i int) int {
	n := len(b)
	if b[i] == '-' {
		i++
		if i >= n {
			return -1
		}
	}
	switch {
	case b[i] == '0':
		i++
	case b[i] >= '1' && b[i] <= '9':
		for i++; i < n && b[i] >= '0' && b[i] <= '9'; i++ {
		}
	default:
		return -1
	}
	if i < n && b[i] == '.' {
		i++
		if i >= n || b[i] < '0' || b[i] > '9' {
			return -1
		}
		for i++; i < n && b[i] >= '0' && b[i] <= '9'; i++ {
		}
	}
	if i < n && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < n && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if i >= n || b[i] < '0' || b[i] > '9' {
			return -1
		}
		for i++; i < n && b[i] >= '0' && b[i] <= '9'; i++ {
		}
	}
	return i
}

// The functions below require valid JSON.

// stringEnd returns the index after the string starting at b[i] == '"'.
func stringEnd(b []byte, i int) int {
	i++
	for {
		k := bytes.IndexByte(b[i:], '"')
		if k < 0 {
			return len(b)
		}
		k += i
		bs := 0
		for p := k - 1; p >= i && b[p] == '\\'; p-- {
			bs++
		}
		if bs%2 == 0 {
			return k + 1
		}
		i = k + 1
	}
}

// valueEnd returns the index after the value starting at b[i].
func valueEnd(b []byte, i int) int {
	switch b[i] {
	case '"':
		return stringEnd(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = stringEnd(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for i < len(b) {
		switch b[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return i
		}
		i++
	}
	return i
}

// Members iterates over the members of the object v; key is the raw quoted
// key and val the value without surrounding whitespace.
type Members struct {
	x *Index
	b []byte
	i int
}

// ObjectMembers returns an iterator over the members of the object v (a
// slice of x's body, or of any valid JSON if x is nil).
func ObjectMembers(x *Index, v []byte) Members { return Members{x, v, 1} }

// Next returns the next member; ok is false after the last one.
func (m *Members) Next() (key, val []byte, ok bool) {
	b := m.b
	i := skipWS(b, m.i)
	if i < len(b) && b[i] == ',' {
		i = skipWS(b, i+1)
	}
	if i >= len(b) || b[i] != '"' {
		return nil, nil, false
	}
	ke := stringEnd(b, i)
	key = b[i:ke]
	i = skipWS(b, ke) + 1 // ':'
	i = skipWS(b, i)
	ve := m.x.end(b, i)
	m.i = ve
	return key, b[i:ve], true
}

// Elements iterates over the elements of the array v.
type Elements struct {
	x *Index
	b []byte
	i int
}

// ArrayElements returns an iterator over the elements of the array v (a
// slice of x's body, or of any valid JSON if x is nil).
func ArrayElements(x *Index, v []byte) Elements { return Elements{x, v, 1} }

// Next returns the next element; ok is false after the last one.
func (e *Elements) Next() (val []byte, ok bool) {
	b := e.b
	i := skipWS(b, e.i)
	if i < len(b) && b[i] == ',' {
		i = skipWS(b, i+1)
	}
	if i >= len(b) || b[i] == ']' {
		return nil, false
	}
	ve := e.x.end(b, i)
	e.i = ve
	return b[i:ve], true
}

// IsNull reports whether the raw value v is the literal null.
func IsNull(v []byte) bool { return len(v) == 4 && string(v) == "null" }

// Unquote decodes the JSON string literal s exactly like encoding/json
// (invalid UTF-8 and unpaired surrogates become U+FFFD).
func Unquote(s []byte) string {
	s = s[1 : len(s)-1]
	r := 0
	for r < len(s) {
		c := s[r]
		if c == '\\' || c < ' ' {
			break
		}
		if c < utf8.RuneSelf {
			r++
			continue
		}
		rr, size := utf8.DecodeRune(s[r:])
		if rr == utf8.RuneError && size == 1 {
			break
		}
		r += size
	}
	if r == len(s) {
		return string(s)
	}
	b := make([]byte, 0, len(s)+2*utf8.UTFMax)
	b = append(b, s[:r]...)
	for r < len(s) {
		switch c := s[r]; {
		case c == '\\':
			r++
			switch s[r] {
			case 'b':
				b = append(b, '\b')
			case 'f':
				b = append(b, '\f')
			case 'n':
				b = append(b, '\n')
			case 'r':
				b = append(b, '\r')
			case 't':
				b = append(b, '\t')
			case 'u':
				rr := getu4(s[r-1:])
				r += 5
				if utf16.IsSurrogate(rr) {
					if dec := utf16.DecodeRune(rr, getu4(s[r:])); dec != unicode.ReplacementChar {
						r += 6
						b = utf8.AppendRune(b, dec)
						continue
					}
					rr = unicode.ReplacementChar
				}
				b = utf8.AppendRune(b, rr)
				continue
			default: // '"', '\\', '/'
				b = append(b, s[r])
			}
			r++
		case c < utf8.RuneSelf:
			b = append(b, c)
			r++
		default:
			rr, size := utf8.DecodeRune(s[r:])
			r += size
			b = utf8.AppendRune(b, rr)
		}
	}
	return string(b)
}

// getu4 decodes \uXXXX from the beginning of s, returning -1 if absent.
func getu4(s []byte) rune {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return -1
	}
	var r rune
	for _, c := range s[2:6] {
		switch {
		case '0' <= c && c <= '9':
			c = c - '0'
		case 'a' <= c && c <= 'f':
			c = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			c = c - 'A' + 10
		default:
			return -1
		}
		r = r*16 + rune(c)
	}
	return r
}

// verbatimOK reports whether the JSON string literal s can be copied to the
// output as is: it is valid UTF-8 and has no unpaired surrogate escapes, so
// every JSON parser decodes it to the same text encoding/json does.
func verbatimOK(s []byte) bool {
	if !utf8.Valid(s) {
		return false
	}
	c := s[1 : len(s)-1]
	for {
		k := bytes.IndexByte(c, '\\')
		if k < 0 {
			return true
		}
		if c[k+1] != 'u' {
			c = c[k+2:]
			continue
		}
		r := getu4(c[k:])
		switch {
		case r >= 0xD800 && r < 0xDC00:
			r2 := getu4(c[k+6:])
			if r2 < 0xDC00 || r2 >= 0xE000 {
				return false
			}
			c = c[k+12:]
		case r >= 0xDC00 && r < 0xE000:
			return false
		default:
			c = c[k+6:]
		}
	}
}

// AppendStrBody appends the contents (without quotes) of the JSON string
// literal s; s == nil is the empty string.
func AppendStrBody(dst, s []byte) []byte {
	if len(s) < 2 {
		return dst
	}
	if verbatimOK(s) {
		return append(dst, s[1:len(s)-1]...)
	}
	return appendEscaped(dst, Unquote(s))
}

// AppendStrLit appends the JSON string literal s (nil is "").
func AppendStrLit(dst, s []byte) []byte {
	dst = append(dst, '"')
	dst = AppendStrBody(dst, s)
	return append(dst, '"')
}

// AppendJoined appends one JSON string: the string literals parts joined by
// sep (which must not need escaping in JSON other than '\n').
func AppendJoined(dst []byte, parts [][]byte, sep string) []byte {
	dst = append(dst, '"')
	for i, p := range parts {
		if i > 0 {
			for j := 0; j < len(sep); j++ {
				if sep[j] == '\n' {
					dst = append(dst, '\\', 'n')
				} else {
					dst = append(dst, sep[j])
				}
			}
		}
		dst = AppendStrBody(dst, p)
	}
	return append(dst, '"')
}

const hexDigits = "0123456789abcdef"

// AppendQuoted appends s as a JSON string like encoding/json with HTML
// escaping off (invalid UTF-8 becomes \ufffd).
func AppendQuoted(dst []byte, s string) []byte {
	dst = append(dst, '"')
	dst = appendEscaped(dst, s)
	return append(dst, '"')
}

func appendEscaped(dst []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `\ufffd`...)
			i += size
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	return append(dst, s[start:]...)
}

// appendCompact appends the valid JSON value v without insignificant
// whitespace (byte-identical to json.Compact).
func appendCompact(dst, v []byte) []byte {
	start := 0
	for i := 0; i < len(v); {
		switch v[i] {
		case '"':
			i = stringEnd(v, i)
		case ' ', '\t', '\n', '\r':
			dst = append(dst, v[start:i]...)
			i = skipWS(v, i)
			start = i
		default:
			i++
		}
	}
	return append(dst, v[start:]...)
}

// compactLen is len(appendCompact(nil, v)).
func compactLen(v []byte) int {
	n := 0
	for i := 0; i < len(v); {
		switch v[i] {
		case '"':
			e := stringEnd(v, i)
			n += e - i
			i = e
		case ' ', '\t', '\n', '\r':
			i++
		default:
			n++
			i++
		}
	}
	return n
}

// StrLen is the byte length of the decoded JSON string literal s.
func StrLen(s []byte) int {
	c := s[1 : len(s)-1]
	if bytes.IndexByte(c, '\\') < 0 && utf8.Valid(c) {
		return len(c)
	}
	// As Unquote, without building the string.
	n := 0
	for r := 0; r < len(c); {
		switch ch := c[r]; {
		case ch == '\\':
			if c[r+1] != 'u' {
				n++
				r += 2
				continue
			}
			rr := getu4(c[r:])
			r += 6
			if utf16.IsSurrogate(rr) {
				if dec := utf16.DecodeRune(rr, getu4(c[r:])); dec != unicode.ReplacementChar {
					r += 6
					n += utf8.RuneLen(dec)
					continue
				}
				rr = unicode.ReplacementChar
			}
			n += utf8.RuneLen(rr)
		case ch < utf8.RuneSelf:
			n++
			r++
		default:
			rr, size := utf8.DecodeRune(c[r:])
			r += size
			n += utf8.RuneLen(rr) // invalid bytes become U+FFFD
		}
	}
	return n
}

// Struct-field decoding with encoding/json semantics. Each returns false on
// a type mismatch; null leaves the destination unchanged.

// FieldStr decodes a string field.
func FieldStr(v []byte, dst *string) bool {
	switch v[0] {
	case '"':
		*dst = Unquote(v)
	case 'n':
	default:
		return false
	}
	return true
}

// FieldRawStr keeps a string field as its raw literal.
func FieldRawStr(v []byte, dst *[]byte) bool {
	switch v[0] {
	case '"':
		*dst = v
	case 'n':
	default:
		return false
	}
	return true
}

// FieldBool decodes a bool field.
func FieldBool(v []byte, dst *bool) bool {
	switch v[0] {
	case 't':
		*dst = true
	case 'f':
		*dst = false
	case 'n':
	default:
		return false
	}
	return true
}

// FieldPtrStr decodes a *string field kept raw: null clears it.
func FieldPtrStr(v []byte, dst *[]byte) bool {
	switch v[0] {
	case '"':
		*dst = v
	case 'n':
		*dst = nil
	default:
		return false
	}
	return true
}

// DecodeSlice decodes the array v into s the way encoding/json decodes into
// an existing slice: elements are decoded into (and merged with) existing
// ones, null elements leave them unchanged, and the length is set to the
// number of elements. elem decodes one non-null element. With fresh, s is
// scratch space only and decoding behaves as into a nil slice.
func DecodeSlice[T any](x *Index, s []T, v []byte, fresh bool, elem func(*Index, *T, []byte) bool) ([]T, bool) {
	if v[0] != '[' {
		return s, false
	}
	if fresh {
		s = s[:0]
	}
	if cap(s) == 0 {
		s = make([]T, 0, countElements(x, v))
	}
	it := ArrayElements(x, v)
	i := 0
	for {
		ev, ok := it.Next()
		if !ok {
			break
		}
		if i >= len(s) {
			var zero T
			if i < cap(s) {
				s = s[:i+1]
				if fresh {
					s[i] = zero
				}
			} else {
				s = append(s, zero)
			}
		}
		if ev[0] != 'n' && !elem(x, &s[i], ev) {
			return s, false
		}
		i++
	}
	if i == 0 {
		return []T{}, true
	}
	return s[:i], true
}

// FoldKey returns the raw quoted object key folded the way encoding/json
// matches struct fields (ASCII lower case, Unicode simple folding), written
// into buf. Keys longer than buf are returned as "" (they match no field).
func FoldKey(buf *[32]byte, key []byte) []byte {
	k := key[1 : len(key)-1]
	out := buf[:0]
	simple := len(k) <= len(buf)
	if simple {
		for _, c := range k {
			if c == '\\' || c >= utf8.RuneSelf {
				simple = false
				break
			}
		}
	}
	if simple {
		for _, c := range k {
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			out = append(out, c)
		}
		return out
	}
	s := Unquote(key)
	for _, r := range s {
		if r >= utf8.RuneSelf {
			r = foldRune(r)
		}
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		if len(out)+utf8.RuneLen(r) > len(buf) {
			return nil
		}
		out = utf8.AppendRune(out, r)
	}
	return out
}

// foldRune returns the smallest rune in r's case folding set (as in
// encoding/json).
func foldRune(r rune) rune {
	for {
		r2 := unicode.SimpleFold(r)
		if r2 <= r {
			return r2
		}
		r = r2
	}
}

// ParseFloat decodes a JSON number into a float64 like encoding/json.
func ParseFloat(v []byte) (float64, bool) {
	if len(v) == 0 || v[0] != '-' && (v[0] < '0' || v[0] > '9') {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(v), 64)
	return f, err == nil
}

// countElements returns the number of elements of the array v.
func countElements(x *Index, v []byte) int {
	it := ArrayElements(x, v)
	n := 0
	for {
		if _, ok := it.Next(); !ok {
			return n
		}
		n++
	}
}
