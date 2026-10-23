package privacy

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestFixturePrivacy fails if any file in the repository looks like it
// identifies a person, machine or account. Run it before every push:
//
//	go test ./scripts/privacy
//
// The built-in checks always run (also in CI). Personal terms (user name,
// employer, connected apps, project names) come from a local, git-ignored
// .privacy-terms file in the repo root, one term per line; '#' starts a comment.

const repoRoot = "../.."

// skipDirs are not part of the repository's content: VCS data, git-ignored
// local folders, and this package, whose self-test plants fake leaks.
var skipDirs = map[string]bool{
	".git": true, ".gotmp": true, ".claude": true, "captures": true, "data": true, "dist": true,
	"docs/internal": true, "scripts/privacy": true, ".cbrt": true,
}

// skipFiles are git-ignored local files that hold personal values by design,
// and .git, which is a file (with an absolute gitdir path) in a worktree.
var skipFiles = map[string]bool{".git": true, ".env": true, ".privacy-terms": true, "cbrt.json": true, "AGENTS.md": true, "CLAUDE.md": true, "PLAN.md": true}

var (
	// UUIDs other than the sanitizer's placeholders. Version 7 (time-ordered
	// message IDs that servers generate) is allowed; v1/v4 are typical account,
	// session and installation IDs.
	auditUUID   = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-([0-9a-f])[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}`)
	auditEmail  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	auditKey    = regexp.MustCompile(`nvapi-[A-Za-z0-9_-]{8,}|sk-(ant-)?[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|eyJ[A-Za-z0-9_-]{20,}\.eyJ`)
	auditHome   = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]+users[\\/]+|/home/|/users/)([^\\/"\s]+)`)
	auditTemp   = regexp.MustCompile(`(?i)appdata[\\/]+local[\\/]+temp|users-[a-z0-9._]+-desktop`)
	auditApp    = regexp.MustCompile(`mcp__codex_apps__([A-Za-z0-9]+)`)
	auditAppGen = regexp.MustCompile(`^(app([0-9]{2}x*|NN)?|[A-Za-z0-9]*acmecorp[A-Za-z0-9]*)$`)
)

func TestFixturePrivacy(t *testing.T) {
	terms := loadPrivacyTerms(t)
	module := modulePath(t)
	if len(terms) == 0 {
		t.Log("no .privacy-terms in the repo root: personal-term check skipped (built-in checks still run)")
	}
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFiles[rel] || strings.HasSuffix(rel, ".exe") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The repository's own module path names its owner; that is the one
		// sanctioned occurrence. Fixtures get every check; code and docs, which
		// hold deliberately fake keys and UUIDs in tests, get the rest.
		text := strings.ReplaceAll(string(raw), module, "")
		for _, p := range auditFile(text, terms, strings.HasPrefix(rel, "testdata/")) {
			t.Errorf("%s: %s", filepath.ToSlash(rel), p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// auditFile returns the problems found in one file's text. strict adds the
// UUID and key checks, which only make sense for fixtures.
func auditFile(s string, terms []string, strict bool) []string {
	var out []string
	add := func(format string, v string) {
		if len(v) > 80 {
			v = v[:80] + "…"
		}
		out = append(out, strings.Replace(format, "%s", v, 1))
	}
	for _, m := range auditUUID.FindAllStringSubmatch(s, -1) {
		if strict && m[1] != "7" && !strings.HasPrefix(m[0], "00000000-0000-") {
			add("real-looking UUID %s", m[0])
		}
	}
	for _, m := range auditEmail.FindAllString(s, -1) {
		if !strings.HasSuffix(strings.ToLower(m), "@example.com") {
			add("e-mail address %s", m)
		}
	}
	for _, m := range auditKey.FindAllString(s, -1) {
		if !strict {
			break
		}
		add("API key or token %s", m)
	}
	for _, m := range auditHome.FindAllStringSubmatch(s, -1) {
		if name := strings.ToLower(m[1]); name != "user" && name != "you" && name != "public" {
			add("user path %s", m[0])
		}
	}
	for _, m := range auditTemp.FindAllString(s, -1) {
		add("temp or scratch path %s", m)
	}
	for _, m := range auditApp.FindAllStringSubmatch(s, -1) {
		if !auditAppGen.MatchString(m[1]) {
			add("app namespace not anonymized %s", m[0])
		}
	}
	if len(terms) > 0 {
		lower := strings.ToLower(s)
		joined := strings.ToLower(streamText(s)) // catches a term split across stream deltas
		for _, term := range terms {
			if strings.Contains(lower, term) || strings.Contains(joined, term) {
				add("personal term %s", strconv.Quote(term))
			}
		}
	}
	return out
}

// streamText concatenates the "delta" strings of the SSE events in a fixture
// (a recorded response body, or a raw .sse/.txt file), so a word split across
// stream fragments can be found.
func streamText(s string) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if strings.Contains(t, "data: ") {
				b.WriteString(streamText(t))
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				if d, ok := e.(string); ok && k == "delta" {
					b.WriteString(d)
				} else {
					walk(e)
				}
			}
		}
	}
	var doc any
	if json.Unmarshal([]byte(s), &doc) == nil && !strings.HasPrefix(strings.TrimSpace(s), `"`) {
		walk(doc)
		return b.String()
	}
	for _, line := range strings.Split(s, "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var ev any
		if json.Unmarshal([]byte(data), &ev) == nil {
			walk(ev)
		}
	}
	return b.String()
}

func loadPrivacyTerms(t *testing.T) []string {
	f, err := os.Open("../../.privacy-terms")
	if err != nil {
		return nil
	}
	defer f.Close()
	var terms []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line != "" {
			terms = append(terms, strings.ToLower(line))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return terms
}

// modulePath reads the module path from go.mod.
func modulePath(t *testing.T) string {
	b, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(m)
		}
	}
	t.Fatal("no module line in go.mod")
	return ""
}

func TestAuditCatches(t *testing.T) {
	cases := map[string]string{
		"uuid":     `{"id":"3f2b8c1e-9a4d-4e7b-8c2a-1b2c3d4e5f60"}`,
		"email":    `{"x":"mail jane.doe@corp.io"}`,
		"key":      `{"k":"nvapi-AbCdEf0123456789"}`,
		"home":     `{"p":"C:\\Users\\jane\\proj"}`,
		"temp":     `{"p":"C:/Users/user/AppData/Local/Temp/x"}`,
		"app":      `{"n":"mcp__codex_apps__mailbox"}`,
		"split":    "data: {\"delta\":\"exa\"}\n\ndata: {\"delta\":\"mplecorp seo\"}\n",
		"jsonsse":  `{"response_body":"event: x\ndata: {\"delta\":\"exa\"}\n\ndata: {\"delta\":\"mplecorp\"}\n"}`,
		"rawterms": `{"x":"made at examplecorp"}`,
	}
	for name, s := range cases {
		if len(auditFile(s, []string{"examplecorp"}, true)) == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
	clean := `{"id":"00000000-0000-4000-8000-000000000001","m":"msg_01a0ed27-bae6-7dd1-a3f8-ed9257ef465e","e":"user@example.com","p":"C:\\Users\\user\\x","n":"mcp__codex_apps__app01xx","t":"hello"}`
	if p := auditFile(clean, []string{"examplecorp"}, true); len(p) != 0 {
		t.Errorf("false positives: %v", p)
	}
}
