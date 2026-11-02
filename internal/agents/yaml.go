package agents

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The YAML here is the subset agent frontmatter uses: one level of keys with
// plain, quoted and block (| >) scalars, flow ([a, b]) and block (- a)
// lists of scalars. Anything nested deeper (hooks, a map of MCP servers) is
// kept verbatim as Raw, so it survives a round trip without being understood.

// splitFrontmatter returns the frontmatter lines and the body. The text may
// start with a UTF-8 BOM and use CRLF line endings.
func splitFrontmatter(text string) (front []string, body string, err error) {
	text = strings.ReplaceAll(strings.TrimPrefix(text, bom), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if strings.TrimRight(lines[0], " \t") != "---" {
		return nil, "", errors.New("no YAML frontmatter: the file must start with a --- line")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			return lines[1:i], strings.Join(lines[i+1:], "\n"), nil
		}
	}
	return nil, "", errors.New("the frontmatter is never closed: no second --- line")
}

// parseYAML reads frontmatter lines; first is the file line number of lines[0].
func parseYAML(lines []string, first int) ([]Field, error) {
	var fields []Field
	seen := map[string]bool{}
	for i := 0; i < len(lines); {
		line := lines[i]
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			i++
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, fmt.Errorf("line %d: unexpected indentation", first+i)
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok || key == "" || key != strings.TrimSpace(key) || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("line %d: expected key: value", first+i)
		}
		if seen[key] {
			return nil, fmt.Errorf("line %d: %s appears twice", first+i, key)
		}
		seen[key] = true
		// The value's block: the indented lines (and "- " items) that follow.
		j := i + 1
		for j < len(lines) && (strings.TrimSpace(lines[j]) == "" || lines[j][0] == ' ' || lines[j][0] == '\t' || lines[j] == "-" || strings.HasPrefix(lines[j], "- ")) {
			j++
		}
		for j > i+1 && strings.TrimSpace(lines[j-1]) == "" {
			j--
		}
		v, err := yamlValue(strings.TrimSpace(rest), lines[i+1:j])
		if err != nil {
			return nil, fmt.Errorf("line %d: %s: %w", first+i, key, err)
		}
		fields = append(fields, Field{Key: key, Value: v})
		i = j
	}
	return fields, nil
}

func yamlValue(rest string, block []string) (Value, error) {
	switch {
	case rest == "" && len(block) == 0:
		return Value{Kind: Scalar}, nil
	case rest == "":
		if items, ok := blockList(block); ok {
			return Value{Kind: List, List: items}, nil
		}
		return Value{Kind: Raw, Raw: strings.Join(block, "\n")}, nil
	case rest[0] == '|' || rest[0] == '>':
		return Value{Kind: Scalar, Str: blockScalar(rest[0] == '>', block)}, nil
	}
	if len(block) > 0 { // a scalar continued on indented lines folds with spaces
		parts := []string{rest}
		for _, l := range block {
			if t := strings.TrimSpace(l); t != "" {
				parts = append(parts, t)
			}
		}
		rest = strings.Join(parts, " ")
	}
	switch rest[0] {
	case '[':
		items, err := flowList(rest)
		return Value{Kind: List, List: items}, err
	case '{':
		return Value{Kind: Raw, Raw: rest}, nil
	}
	s, err := yamlScalar(rest)
	return Value{Kind: Scalar, Str: s}, err
}

// blockList reads "- item" lines that hold plain or quoted scalars only.
func blockList(block []string) ([]string, bool) {
	var items []string
	for _, l := range block {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		item, ok := strings.CutPrefix(t, "-")
		if !ok || (item != "" && item[0] != ' ') {
			return nil, false
		}
		item = strings.TrimSpace(item)
		if item == "" || item[0] == '-' || item[0] == '[' || item[0] == '{' || strings.HasSuffix(item, ":") || (item[0] != '"' && item[0] != '\'' && strings.Contains(item, ": ")) {
			return nil, false
		}
		s, err := yamlScalar(item)
		if err != nil {
			return nil, false
		}
		items = append(items, s)
	}
	// Items must all sit at one indentation; deeper lines mean nesting.
	ind := -1
	for _, l := range block {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if ind >= 0 && n != ind {
			return nil, false
		}
		ind = n
	}
	return items, true
}

func blockScalar(fold bool, block []string) string {
	ind := -1
	for _, l := range block {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " \t")); ind < 0 || n < ind {
			ind = n
		}
	}
	out := make([]string, len(block))
	for i, l := range block {
		if len(l) >= ind && ind > 0 {
			l = l[ind:]
		}
		out[i] = strings.TrimRight(l, " \t")
	}
	if !fold {
		return strings.Join(out, "\n")
	}
	var b strings.Builder
	for i, l := range out {
		switch {
		case i == 0:
		case l == "" || out[i-1] == "":
			b.WriteByte('\n')
		default:
			b.WriteByte(' ')
		}
		b.WriteString(l)
	}
	return b.String()
}

func flowList(s string) ([]string, error) {
	if !strings.HasSuffix(s, "]") {
		return nil, errors.New("flow list without a closing ]")
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []string{}, nil
	}
	var items []string
	var quote byte
	start := 0
	for i := 0; i <= len(inner); i++ {
		if i < len(inner) {
			c := inner[i]
			switch {
			case quote != 0:
				if c == '\\' && quote == '"' {
					i++
				} else if c == quote {
					quote = 0
				}
				continue
			case c == '"' || c == '\'':
				quote = c
				continue
			case c != ',':
				continue
			}
		}
		item, err := yamlScalar(strings.TrimSpace(inner[start:i]))
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		start = i + 1
	}
	return items, nil
}

// yamlScalar decodes a plain, single- or double-quoted scalar.
func yamlScalar(s string) (string, error) {
	switch {
	case s == "":
		return "", nil
	case s[0] == '\'':
		end := -1
		for i := 1; i < len(s); i++ {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				end = i
				break
			}
		}
		if end < 0 {
			return "", errors.New("unterminated single-quoted string")
		}
		if err := onlyComment(s[end+1:]); err != nil {
			return "", err
		}
		return strings.ReplaceAll(s[1:end], "''", "'"), nil
	case s[0] == '"':
		return doubleQuoted(s)
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s), nil
}

func doubleQuoted(s string) (string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			if err := onlyComment(s[i+1:]); err != nil {
				return "", err
			}
			return b.String(), nil
		case '\\':
			if i+1 >= len(s) {
				return "", errors.New("unterminated double-quoted string")
			}
			i++
			switch e := s[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '0':
				b.WriteByte(0)
			case '"', '\\', '/', ' ':
				b.WriteByte(e)
			case 'x', 'u', 'U':
				n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
				if i+n >= len(s) {
					return "", fmt.Errorf("short \\%c escape", e)
				}
				r, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
				if err != nil || !utf8.ValidRune(rune(r)) {
					return "", fmt.Errorf("bad \\%c escape", e)
				}
				b.WriteRune(rune(r))
				i += n
			default:
				return "", fmt.Errorf("unknown escape \\%c", e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", errors.New("unterminated double-quoted string")
}

func onlyComment(rest string) error {
	if t := strings.TrimSpace(rest); t != "" && !strings.HasPrefix(t, "#") {
		return fmt.Errorf("unexpected text after the closing quote: %q", t)
	}
	return nil
}

// quoteYAML writes s as a plain scalar when it reads back unchanged, else as
// a double-quoted one.
func quoteYAML(s string) string {
	plain := s != "" && s == strings.TrimSpace(s) &&
		!strings.ContainsAny(s, "\n\r\t\\") && !strings.Contains(s, ": ") && !strings.Contains(s, " #") &&
		!strings.HasSuffix(s, ":") && !strings.ContainsRune("-?:,[]{}#&*!|>'\"%@`", rune(s[0]))
	if plain {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
