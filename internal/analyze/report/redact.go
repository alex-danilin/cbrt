// Package report writes what a `cbrt analyze` run hands to the reviewer
// model: summary.md, anchors.jsonl and excerpt_candidates.jsonl, plus
// transcript excerpts on demand. Everything that leaves the machine goes
// through here, and every piece of transcript text in it is redacted
// (Redactor) and scoped (Exclusions) first. See the architecture doc §3.2.6.
package report

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RedactionVersion identifies the built-in rule set; it is recorded on every
// run. Bump it whenever a built-in rule changes.
const RedactionVersion = "2"

// RulesFile is the optional per-machine rule file under the analyzer home.
const RulesFile = "redaction_rules.json"

// rulesVersion is the current RulesFile "v".
const rulesVersion = 1

type rule struct {
	name string
	re   *regexp.Regexp
	// repl, when set, replaces a match instead of "[REDACTED:<name>]".
	repl func(m string, sub []string) string
	// homePath marks the rule that turns the home folder path into "~".
	homePath bool
}

// Redactor masks secrets and personal identifiers in transcript text. The
// zero value is not usable; build one with LoadRedactor.
type Redactor struct {
	rules    []rule
	ownEmail string // lower-cased; "" when none
}

func marker(name string) string { return "[REDACTED:" + name + "]" }

var (
	// From events.Redact, widened to the key shapes the analyzer meets in
	// transcripts (GitHub tokens, AWS access key ids, JWTs).
	reBearer = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/\-]{8,}=*`)
	reAPIKey = regexp.MustCompile(`\b(?:nvapi-|sk-ant-|sk-|gh[pousr]_|github_pat_)[A-Za-z0-9_\-]{8,}`)
	reAWS    = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	reJWT    = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]*`)
	// scheme://user:password@host
	reURLPass = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.\-]*://[^\s:/@"'\\]+):([^\s@/"'\\]+)@`)
	// PEM private key blocks, whole, also with line breaks escaped as a
	// literal "\n" inside a JSON string. A block cut before its END line is
	// masked to the end of the text.
	rePEM = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
	// "Authorization: Basic|Digest|Token|… <credentials>" (Bearer has its own
	// rule). Group 1 is the header up to the scheme, 2 the scheme.
	reAuthHeader = regexp.MustCompile(`(?i)\b(authorization\\*["']?[ \t]*[:=][ \t]*\\*["']?)(basic|digest|negotiate|ntlm|token)[ \t]+([^\s"'\\,;]+)`)
	// A free-standing "Basic <base64>"; masked only when it decodes to
	// "user:password" (see basicCreds), so prose such as "Basic setup" stays.
	reBasic = regexp.MustCompile(`(?i)\bbasic[ \t]+([A-Za-z0-9+/]{4,}={0,2})`)

	// secretValue is the value of a secret assignment: an escaped-quoted
	// string (\"…\", inside a JSON string), a quoted string, or a bare word
	// that stops at whitespace, quotes, a backslash (so a JSON-escaped "\n"
	// ends it), ",", ";" and a closing "}" or "]". A bare value never starts
	// with an opening bracket, "<", ">" or "=" ("=>", "==").
	secretValue = `\\+"[^"\n]*?\\+"|"[^"\n]*"|'[^'\n]*'|[^\s"'\\,;{}\[\]<>=][^\s"'\\,;}\]]*`
	// secretKey is a key name that holds a secret: optional "_"/"-"
	// separated segments ending in one of the words. "key" alone and "pass"
	// alone (any case but lower) are too common to count: "key" needs a
	// telling prefix (api_key, x-api-key, private_key, …), and only lowercase
	// "pass" counts bare, so test output like "--- PASS: TestX" stays.
	secretKey = `(?:[A-Za-z0-9]+[_-])*(?:password|passwd|passphrase|pwd|secret|token|credentials?|auth|authorization|apikey|` +
		`(?:api|private|secret|access|signing|encryption|master|client|session|ssh|app|license|service|account|auth|admin)[_-]?key)` +
		`|(?:[A-Za-z0-9]+[_-])+pass|(?-i:pass)`
	// reKV is a secret key/value pair in any common syntax: NAME=value,
	// "name": "value", name: value (YAML), name => 'value', name := "value",
	// and the escaped form inside a JSON string (\"name\":\"value\"). Group 1
	// is the key, 2 its closing quote (possibly escaped), 3 the separator, 4
	// the value. Only the value is masked.
	reKV = regexp.MustCompile(`(?i)\b(` + secretKey + `)(\\*["']?)([ \t]*(?:=>|:=|=|:)[ \t]*)(` + secretValue + `)`)
	// reEnv is the older, broader .env rule: NAME=value for any name that
	// merely contains KEY, SECRET, TOKEN or PASSWORD (GITHUB_TOKEN_RW=…).
	reEnv = regexp.MustCompile(`(?i)\b([A-Za-z0-9_\-]*(?:KEY|SECRET|TOKEN|PASSWORD|PASSWD)[A-Za-z0-9_\-]*)(\\*["']?)([ \t]*=[ \t]*)(` + secretValue + `)`)
	// reFlag is a command-line flag followed by its value: --password v,
	// --api-key "v", -token v. (--password=v is reKV's.)
	reFlag = regexp.MustCompile(`(?i)(^|[\s"'(])(--?(?:[a-z0-9]+-)*(?:password|passwd|passphrase|pwd|pass|secret|token|credentials?|auth|` +
		`(?:api|private|secret|access|client|auth)-?key))([ \t]+)("[^"\n]*"|'[^'\n]*'|[^\s"'\\\-][^\s"'\\]*)`)
	// reTag is an XML-ish element holding a secret: <password>v</password>.
	reTag = regexp.MustCompile(`(?i)(<(?:[A-Za-z0-9]+[_:-])*(?:password|passwd|passphrase|pwd|pass|secret|token|credentials?|apikey|` +
		`(?:api|private|secret|access|client)[_-]?key|(?:auth|access|refresh|api)[_-]?token)(?:\s[^<>]*)?>)([^<]+)(</)`)

	reEmail    = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}\b`)
	rePrivIP   = regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})\b`)
	ruleNameRe = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// rulesDoc is RulesFile.
type rulesDoc struct {
	V int `json:"v"`
	// OwnEmail is the user's own address, left visible (the analyzer config
	// has no field for it, so it lives with the redaction rules).
	OwnEmail string `json:"own_email"`
	Rules    []struct {
		Name   string `json:"name"`
		Regexp string `json:"regexp"`
	} `json:"rules"`
}

// LoadRedactor builds the built-in rules plus the optional
// <home>/redaction_rules.json ({"v":1,"own_email":"…","rules":[{"name","regexp"}]}).
// Custom matches become "[REDACTED:<name>]". The user's home directory
// (os.UserHomeDir) is replaced with "~" in any separator form, including
// JSON-escaped backslashes.
func LoadRedactor(home string) (*Redactor, error) {
	r := &Redactor{}
	var doc rulesDoc
	b, err := os.ReadFile(filepath.Join(home, RulesFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", RulesFile, err)
		}
		if doc.V > rulesVersion {
			return nil, fmt.Errorf("%s: version %d is newer than this cbrt understands (%d)", RulesFile, doc.V, rulesVersion)
		}
	}
	r.ownEmail = strings.ToLower(strings.TrimSpace(doc.OwnEmail))

	keepFirst := func(name string) func(string, []string) string {
		return func(_ string, sub []string) string { return sub[1] + ":" + marker(name) + "@" }
	}
	// kv masks group 4 (the value) and keeps groups 1-3 (key, closing
	// quote, separator).
	kv := func(m string, sub []string) string {
		v, ok := maskValue(sub[4])
		if !ok {
			return m
		}
		return sub[1] + sub[2] + sub[3] + v
	}
	r.rules = []rule{
		{name: "private_key", re: rePEM},
		{name: "password", re: reURLPass, repl: keepFirst("password")},
		{name: "jwt", re: reJWT},
		{name: "bearer", re: reBearer, repl: func(string, []string) string { return "Bearer " + marker("bearer") }},
		{name: "api_key", re: reAPIKey},
		{name: "api_key", re: reAWS},
		{name: "auth", re: reAuthHeader, repl: func(m string, sub []string) string {
			if strings.HasPrefix(sub[3], "[REDACTED:") {
				return m
			}
			return sub[1] + sub[2] + " " + marker("auth")
		}},
		{name: "auth", re: reBasic, repl: func(m string, sub []string) string {
			if !basicCreds(sub[1]) {
				return m
			}
			return m[:len(m)-len(sub[1])] + marker("auth")
		}},
		{name: "secret", re: reTag, repl: func(m string, sub []string) string {
			if v := strings.TrimSpace(sub[2]); v == "" || strings.HasPrefix(v, "[REDACTED:") {
				return m
			}
			return sub[1] + marker("secret") + sub[3]
		}},
		{name: "secret", re: reFlag, repl: func(m string, sub []string) string {
			v, ok := maskValue(sub[4])
			if !ok {
				return m
			}
			return sub[1] + sub[2] + sub[3] + v
		}},
		{name: "secret", re: reKV, repl: kv},
		{name: "secret", re: reEnv, repl: kv},
		{name: "email", re: reEmail, repl: func(m string, _ []string) string {
			if r.ownEmail != "" && strings.ToLower(m) == r.ownEmail {
				return m
			}
			return marker("email")
		}},
		{name: "private_ip", re: rePrivIP},
	}
	for i, cr := range doc.Rules {
		if !ruleNameRe.MatchString(cr.Name) {
			return nil, fmt.Errorf("%s: rule %d: name %q: want [a-z0-9_]+", RulesFile, i, cr.Name)
		}
		re, err := regexp.Compile(cr.Regexp)
		if err != nil {
			return nil, fmt.Errorf("%s: rule %q: %w", RulesFile, cr.Name, err)
		}
		r.rules = append(r.rules, rule{name: cr.Name, re: re})
	}
	if uh, err := os.UserHomeDir(); err == nil {
		r.addHomeRules(uh)
	}
	return r, nil
}

// addHomeRules masks the home directory (any separator form), its "slug"
// form with every separator turned into "-" (Claude Code names project
// folders like that: "C--Users-<name>-Desktop-…"), and the bare user name as
// a word, unless it is a generic account name.
func (r *Redactor) addHomeRules(uh string) {
	tilde := func(_ string, sub []string) string { return "~" + sub[1] }
	if re := homeRegexp(uh); re != nil {
		r.rules = append(r.rules, rule{name: "home", re: re, repl: tilde, homePath: true})
	}
	clean := filepath.Clean(uh)
	slug := strings.Map(func(c rune) rune {
		if c == ':' || c == '\\' || c == '/' {
			return '-'
		}
		return c
	}, clean)
	if strings.Trim(slug, "-") != "" {
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(slug) + `(-|[^A-Za-z0-9_.]|$)`)
		r.rules = append(r.rules, rule{name: "home", re: re, repl: tilde})
	}
	parts := strings.FieldsFunc(clean, func(c rune) bool { return c == '/' || c == '\\' })
	user := ""
	if len(parts) > 0 {
		user = parts[len(parts)-1]
	}
	switch strings.ToLower(user) {
	case "", ".", "user", "users", "admin", "administrator", "root", "owner", "dev", "default", "public", "home":
		return
	}
	if len(user) >= 3 {
		r.rules = append(r.rules, rule{name: "user", re: regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(user) + `\b`)})
	}
}

// homeRegexp matches the home directory path with any run of slashes or
// backslashes between components (so "C:\\Users\\user" in JSON matches), the
// MSYS form "/c/Users/user" for a Windows drive, case-insensitively, and only
// as a whole path component (group 1 is the following character, if any).
func homeRegexp(home string) *regexp.Regexp {
	home = filepath.Clean(home)
	parts := strings.FieldsFunc(home, func(c rune) bool { return c == '/' || c == '\\' })
	if len(parts) == 0 {
		return nil // "/" or empty: nothing meaningful to mask
	}
	sep := `[\\/]+`
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = regexp.QuoteMeta(p)
	}
	body := strings.Join(quoted, sep)
	var alts []string
	if len(parts[0]) == 2 && parts[0][1] == ':' && len(parts) > 1 {
		drive := regexp.QuoteMeta(parts[0][:1])
		rest := strings.Join(quoted[1:], sep)
		alts = append(alts, body, `/`+drive+sep+rest)
	} else {
		alts = append(alts, sep+body)
	}
	return regexp.MustCompile(`(?i)(?:` + strings.Join(alts, "|") + `)([^A-Za-z0-9_.\-]|$)`)
}

// plainWords are bare values after a secret key that are left visible: they
// are configuration flags, type names, auth scheme names or prose ("the
// token: we need …", "auth: true", "password: str"), not secrets.
var plainWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`true false null nil none yes no on off required optional enabled disabled
		empty unset set missing invalid valid expired hidden masked
		str string int bool bytes object dict list map any type
		basic bearer digest negotiate ntlm token
		the a an we i you he she they it is are was were be been this that these those not and or but so
		to in of for on at by with from as all my your our its their his her if then when should will
		can could must may might need needs here there`) {
		plainWords[w] = true
	}
}

// maskValue masks a secret value matched by secretValue, keeping its quotes
// (plain, or escaped as \" inside a JSON string). ok is false when v is to
// stay: empty, already redacted, or a bare plain word.
func maskValue(v string) (string, bool) {
	open, inner, close := "", v, ""
	switch {
	case v == "":
		return v, false
	case v[0] == '\\':
		i := strings.IndexByte(v, '"')
		k := len(v) - 1
		for k > i+1 && v[k-1] == '\\' {
			k--
		}
		open, inner, close = v[:i+1], v[i+1:k], v[k:]
	case v[0] == '"' || v[0] == '\'':
		open, inner, close = v[:1], v[1:len(v)-1], v[len(v)-1:]
	default:
		if plainWords[strings.ToLower(strings.TrimRight(v, "):.!?"))] {
			return v, false
		}
	}
	// "Basic [REDACTED:auth]": an auth scheme before a masked value.
	rest := strings.TrimSpace(inner)
	if f := strings.Fields(rest); len(f) > 1 && plainWords[strings.ToLower(f[0])] {
		rest = strings.TrimSpace(rest[len(f[0]):])
	}
	if strings.TrimSpace(inner) == "" || strings.HasPrefix(rest, "[REDACTED:") {
		return v, false
	}
	return open + marker("secret") + close, true
}

// basicCreds reports base64 text that decodes to printable "user:password".
func basicCreds(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err != nil {
			return false
		}
	}
	d := string(b)
	if !utf8.ValidString(d) || !strings.Contains(d, ":") {
		return false
	}
	for _, c := range d {
		if !unicode.IsPrint(c) {
			return false
		}
	}
	return true
}

// Apply returns s with every rule applied, in order: private key blocks, URL
// passwords, JWTs, bearer tokens, API keys, Authorization credentials,
// secret XML elements, secret command-line flags, secret key/value pairs,
// e-mail addresses (except the configured own address), private IPv4
// addresses, custom rules, the home directory.
func (r *Redactor) Apply(s string) string {
	if s == "" || r == nil {
		return s
	}
	for _, ru := range r.rules {
		if ru.repl == nil {
			s = ru.re.ReplaceAllLiteralString(s, marker(ru.name))
			continue
		}
		re, repl := ru.re, ru.repl
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			return repl(m, re.FindStringSubmatch(m))
		})
	}
	return s
}

// ApplyPath redacts a file path so the reader can still open it: ok when the
// only change Apply makes is the home folder becoming "~". When any other
// rule fires (the user name, the home folder's slug form, a secret, a custom
// rule), the path is not usable and ok is false; p is then "".
func (r *Redactor) ApplyPath(path string) (p string, ok bool) {
	if path == "" || r == nil {
		return path, true
	}
	home := path
	for _, ru := range r.rules {
		if ru.homePath {
			re, repl := ru.re, ru.repl
			home = re.ReplaceAllStringFunc(home, func(m string) string { return repl(m, re.FindStringSubmatch(m)) })
		}
	}
	if r.Apply(path) != home {
		return "", false
	}
	return home, true
}

// applyJSON redacts every string inside a JSON value (keys included) and
// returns it re-encoded; invalid JSON is redacted as text and returned as a
// JSON string.
func (r *Redactor) applyJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		b, _ := json.Marshal(r.Apply(string(raw)))
		return b
	}
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			return r.Apply(t)
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case map[string]any:
			m := make(map[string]any, len(t))
			for k, e := range t {
				m[r.Apply(k)] = walk(e)
			}
			return m
		}
		return v
	}
	b, err := json.Marshal(walk(v))
	if err != nil {
		return nil
	}
	return b
}
