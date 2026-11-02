package agents

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// parseTOML reads the TOML a Codex agent file uses into flat fields: keys are
// dotted paths ("features.shell_tool"), with an index for array tables
// ("skills.config[0].path"). Strings and arrays of strings are decoded;
// other arrays and inline tables are kept as Raw text, other values (numbers,
// booleans, dates) as their literal text.
func parseTOML(text string) ([]Field, error) {
	p := &tomlParser{s: strings.ReplaceAll(strings.TrimPrefix(text, bom), "\r\n", "\n")}
	fields, err := p.parse()
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", strings.Count(p.s[:min(p.i, len(p.s))], "\n")+1, err)
	}
	return fields, nil
}

type tomlParser struct {
	s string
	i int
}

func (p *tomlParser) parse() ([]Field, error) {
	var out []Field
	seen := map[string]bool{}
	arrays := map[string]int{}
	prefix := ""
	for {
		p.skipBlank()
		if p.i >= len(p.s) {
			return out, nil
		}
		if p.s[p.i] == '[' {
			n := 1
			if strings.HasPrefix(p.s[p.i:], "[[") {
				n = 2
			}
			end := strings.Index(p.s[p.i:], strings.Repeat("]", n))
			if end < 0 || strings.Contains(p.s[p.i:p.i+end], "\n") {
				return nil, errors.New("table header without a closing ]")
			}
			hp := &tomlParser{s: strings.TrimSpace(p.s[p.i+n : p.i+end])}
			name, err := hp.key()
			if err != nil || hp.i != len(hp.s) {
				return nil, errors.New("bad table name")
			}
			p.i += end + n
			if err := p.endOfLine(); err != nil {
				return nil, err
			}
			if n == 2 {
				prefix = fmt.Sprintf("%s[%d].", name, arrays[name])
				arrays[name]++
			} else {
				prefix = name + "."
			}
			continue
		}
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.i >= len(p.s) || p.s[p.i] != '=' {
			return nil, fmt.Errorf("expected = after %s", key)
		}
		p.i++
		p.skipSpace()
		v, err := p.value()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if err := p.endOfLine(); err != nil {
			return nil, err
		}
		full := prefix + key
		if seen[full] {
			return nil, fmt.Errorf("%s appears twice", full)
		}
		seen[full] = true
		out = append(out, Field{Key: full, Value: v})
	}
}

// skipBlank skips spaces, newlines and comments.
func (p *tomlParser) skipBlank() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n':
			p.i++
		case '#':
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
		default:
			return
		}
	}
}

func (p *tomlParser) skipSpace() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *tomlParser) endOfLine() error {
	p.skipSpace()
	if p.i < len(p.s) && p.s[p.i] == '#' {
		for p.i < len(p.s) && p.s[p.i] != '\n' {
			p.i++
		}
	}
	if p.i < len(p.s) && p.s[p.i] != '\n' {
		return fmt.Errorf("unexpected %q after the value", p.s[p.i])
	}
	return nil
}

// key reads a bare, quoted or dotted key and returns it joined with dots.
func (p *tomlParser) key() (string, error) {
	var parts []string
	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return "", errors.New("missing key")
		}
		switch c := p.s[p.i]; {
		case c == '"':
			s, err := p.basic()
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		case c == '\'':
			s, err := p.literal()
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		default:
			start := p.i
			for p.i < len(p.s) && isBareKey(p.s[p.i]) {
				p.i++
			}
			if p.i == start {
				return "", fmt.Errorf("unexpected %q where a key should be", c)
			}
			parts = append(parts, p.s[start:p.i])
		}
		p.skipSpace()
		if p.i < len(p.s) && p.s[p.i] == '.' {
			p.i++
			continue
		}
		return strings.Join(parts, "."), nil
	}
}

func isBareKey(c byte) bool {
	return c == '_' || c == '-' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func (p *tomlParser) value() (Value, error) {
	if p.i >= len(p.s) {
		return Value{}, errors.New("missing value")
	}
	rest := p.s[p.i:]
	switch {
	case strings.HasPrefix(rest, `"""`):
		s, err := p.multi(`"""`, true)
		return Value{Kind: Scalar, Str: s}, err
	case strings.HasPrefix(rest, "'''"):
		s, err := p.multi("'''", false)
		return Value{Kind: Scalar, Str: s}, err
	case rest[0] == '"':
		s, err := p.basic()
		return Value{Kind: Scalar, Str: s}, err
	case rest[0] == '\'':
		s, err := p.literal()
		return Value{Kind: Scalar, Str: s}, err
	case rest[0] == '[':
		return p.array()
	case rest[0] == '{':
		start := p.i
		if err := p.skipInlineTable(); err != nil {
			return Value{}, err
		}
		return Value{Kind: Raw, Raw: p.s[start:p.i]}, nil
	}
	start := p.i
	for p.i < len(p.s) && !strings.ContainsRune(" \t\n,]}#", rune(p.s[p.i])) {
		p.i++
	}
	if p.i == start {
		return Value{}, errors.New("missing value")
	}
	return Value{Kind: Scalar, Str: p.s[start:p.i]}, nil
}

func (p *tomlParser) array() (Value, error) {
	start := p.i
	p.i++ // [
	var items []string
	strs := true
	for {
		p.skipBlank()
		if p.i >= len(p.s) {
			return Value{}, errors.New("array without a closing ]")
		}
		if p.s[p.i] == ']' {
			p.i++
			break
		}
		v, err := p.value()
		if err != nil {
			return Value{}, err
		}
		if v.Kind != Scalar || (p.s[start] == '[' && !isQuotedAt(p.s, p.i, v)) {
			strs = false
		}
		items = append(items, v.Str)
		p.skipBlank()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		p.skipBlank()
		if p.i >= len(p.s) || p.s[p.i] != ']' {
			return Value{}, errors.New("expected , or ] in an array")
		}
		p.i++
		break
	}
	if !strs {
		return Value{Kind: Raw, Raw: p.s[start:p.i]}, nil
	}
	if items == nil {
		items = []string{}
	}
	return Value{Kind: List, List: items}, nil
}

// isQuotedAt reports whether the value that ends at end was a string: the
// character before end is its closing quote.
func isQuotedAt(s string, end int, _ Value) bool {
	return end > 0 && (s[end-1] == '"' || s[end-1] == '\'')
}

func (p *tomlParser) skipInlineTable() error {
	depth := 0
	for p.i < len(p.s) {
		switch c := p.s[p.i]; c {
		case '"', '\'':
			var err error
			if c == '"' {
				_, err = p.basic()
			} else {
				_, err = p.literal()
			}
			if err != nil {
				return err
			}
			continue
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				p.i++
				return nil
			}
		case '\n':
			return errors.New("inline table across lines")
		}
		p.i++
	}
	return errors.New("inline table without a closing }")
}

func (p *tomlParser) literal() (string, error) {
	end := strings.IndexAny(p.s[p.i+1:], "'\n")
	if end < 0 || p.s[p.i+1+end] != '\'' {
		return "", errors.New("unterminated literal string")
	}
	s := p.s[p.i+1 : p.i+1+end]
	p.i += end + 2
	return s, nil
}

func (p *tomlParser) basic() (string, error) {
	var b strings.Builder
	p.i++
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch c {
		case '"':
			p.i++
			return b.String(), nil
		case '\n':
			return "", errors.New("unterminated string")
		case '\\':
			if err := p.escape(&b); err != nil {
				return "", err
			}
			continue
		}
		b.WriteByte(c)
		p.i++
	}
	return "", errors.New("unterminated string")
}

// multi reads a multi-line basic or literal string (delim is three double or
// three single quotes); a newline right after the opening delimiter is
// dropped, and in a basic string a backslash at a line end joins lines.
func (p *tomlParser) multi(delim string, escapes bool) (string, error) {
	p.i += 3
	if p.i < len(p.s) && p.s[p.i] == '\n' {
		p.i++
	}
	var b strings.Builder
	for p.i < len(p.s) {
		if strings.HasPrefix(p.s[p.i:], delim) {
			// Up to two quotes may sit right before the closing delimiter.
			extra := 0
			for extra < 2 && strings.HasPrefix(p.s[p.i+extra+1:], delim) {
				extra++
			}
			b.WriteString(p.s[p.i : p.i+extra])
			p.i += extra + 3
			return b.String(), nil
		}
		c := p.s[p.i]
		if escapes && c == '\\' {
			j := p.i + 1
			for j < len(p.s) && (p.s[j] == ' ' || p.s[j] == '\t') {
				j++
			}
			if j < len(p.s) && p.s[j] == '\n' {
				for j < len(p.s) && strings.ContainsRune(" \t\n", rune(p.s[j])) {
					j++
				}
				p.i = j
				continue
			}
			if err := p.escape(&b); err != nil {
				return "", err
			}
			continue
		}
		b.WriteByte(c)
		p.i++
	}
	return "", fmt.Errorf("unterminated multi-line string (no closing %s)", delim)
}

func (p *tomlParser) escape(b *strings.Builder) error {
	if p.i+1 >= len(p.s) {
		return errors.New("unterminated string")
	}
	e := p.s[p.i+1]
	p.i += 2
	switch e {
	case 'b':
		b.WriteByte('\b')
	case 't':
		b.WriteByte('\t')
	case 'n':
		b.WriteByte('\n')
	case 'f':
		b.WriteByte('\f')
	case 'r':
		b.WriteByte('\r')
	case 'e':
		b.WriteByte(0x1b)
	case '"', '\\':
		b.WriteByte(e)
	case 'u', 'U':
		n := 4
		if e == 'U' {
			n = 8
		}
		if p.i+n > len(p.s) {
			return fmt.Errorf("short \\%c escape", e)
		}
		r, err := strconv.ParseUint(p.s[p.i:p.i+n], 16, 32)
		if err != nil || !utf8.ValidRune(rune(r)) {
			return fmt.Errorf("bad \\%c escape", e)
		}
		b.WriteRune(rune(r))
		p.i += n
	default:
		return fmt.Errorf("unknown escape \\%c", e)
	}
	return nil
}

// quoteTOML writes s as a multi-line TOML string for developer_instructions:
// a literal string (nothing to escape) unless s holds three single quotes in
// a row or ends with one; then a basic string with backslashes and double
// quotes escaped.
func quoteTOML(s string) string {
	if !strings.Contains(s, "'''") && !strings.HasSuffix(s, "'") {
		return "'''\n" + s + "'''"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return "\"\"\"\n" + r.Replace(s) + "\"\"\""
}
