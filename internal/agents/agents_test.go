package agents

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fixtures = "../../testdata/agents"

func fixture(rel string) string { return filepath.Join(fixtures, filepath.FromSlash(rel)) }

func field(t *testing.T, a Agent, key string) Value {
	t.Helper()
	v, ok := a.Get(key)
	if !ok {
		t.Fatalf("%s: no field %s", a.Name, key)
	}
	return v
}

func noteFor(notes []Note, key string) (Note, bool) {
	for _, n := range notes {
		if n.Field == key {
			return n, true
		}
	}
	return Note{}, false
}

func TestParseFixtures(t *testing.T) {
	cases := []struct {
		file, name string
		source     Tool
		body       string // a substring of the body
	}{
		{"claude/reviewer.md", "reviewer", Claude, "Run `git diff main`"},
		{"claude/db-analyst.md", "db-analyst", Claude, "mcp__tasky_db__schema"},
		{"claude/release-helper.md", "release-helper", Claude, `v\d+\.\d+\.\d+`},
		{"claude/minimal.md", "minimal", Claude, "tasky --help only."},
		{"claude/cbrt-changelog.md", "cbrt-changelog", Claude, "Added, Fixed and Changed"},
		{"antigravity/triage/agent.md", "triage", Antigravity, "one urgency"},
		{"antigravity/notes-writer.md", "notes-writer", Antigravity, "for users, not developers"},
		{"codex/test_runner.toml", "test_runner", Codex, `from C:\Users\user\tasky and`},
		{"codex/doc-writer.toml", "doc-writer", Codex, `Headings match ^#{1,3}\s+\S`},
	}
	for _, c := range cases {
		a, _, err := Parse(fixture(c.file), "")
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		if a.Name != c.name || a.Source != c.source || a.Description == "" || !strings.Contains(a.Body, c.body) {
			t.Errorf("%s: got name %q source %s description %q body %q", c.file, a.Name, a.Source, a.Description, a.Body)
		}
		if strings.Contains(a.Body, "\r") || strings.Contains(a.Name, bom) {
			t.Errorf("%s: CRLF or BOM survived parsing", c.file)
		}
	}
}

func TestParseDetails(t *testing.T) {
	db, _, err := Parse(fixture("claude/db-analyst.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(db.Description, "questions.\n\n<example>\nuser: \"How many tasks are overdue?\"") {
		t.Errorf("escapes in the quoted description: %q", db.Description)
	}
	if got := field(t, db, "tools").List; !reflect.DeepEqual(got, []string{"Read", "Grep", "mcp__tasky_db__query", "mcp__tasky_db__schema", "WebSearch"}) {
		t.Errorf("comma tool list: %q", got)
	}

	rh, _, err := Parse(fixture("claude/release-helper.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := field(t, rh, "tools").List; len(got) != 8 || got[0] != "Bash" {
		t.Errorf("block tool list: %q", got)
	}
	if h := field(t, rh, "hooks"); h.Kind != Raw || !strings.Contains(h.Raw, `C:\Users\user\.claude\hooks\guard-release.ps1`) {
		t.Errorf("hooks kept verbatim: %+v", h)
	}
	for _, s := range []string{"'''", `"""`, `a\tb`, `C:\ paths`} {
		if !strings.Contains(rh.Body, s) {
			t.Errorf("release-helper body lost %q", s)
		}
	}

	nw, notes, err := Parse(fixture("antigravity/notes-writer.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := field(t, nw, "tools").List; !reflect.DeepEqual(got, []string{"Read", "Write", "WebFetch"}) {
		t.Errorf("flow tool list mapped to Claude names: %q", got)
	}
	if n, ok := noteFor(notes, "tools"); !ok || n.Code != Mapped {
		t.Errorf("tools mapping note: %+v", notes)
	}

	tr, notes, err := Parse(fixture("codex/test_runner.toml"), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.Body, `Quote failing assertions as "got X, want Y".`) {
		t.Errorf("basic-string escapes: %q", tr.Body)
	}
	if field(t, tr, "permissionMode").Str != "plan" || field(t, tr, "effort").Str != "high" {
		t.Errorf("read-only sandbox and effort: %+v", tr.Fields)
	}
	for key, code := range map[string]Code{"model": Dropped, "nickname_candidates": Dropped, "features.memories": Dropped,
		"skills": Dropped, "mcp_servers.tasky_db": Dropped, "sandbox_mode": Mapped, "model_reasoning_effort": Mapped} {
		if n, ok := noteFor(notes, key); !ok || n.Code != code {
			t.Errorf("test_runner note for %s: want %c, got %+v", key, code, n)
		}
	}
	if len(notes) != 8 { // model_reasoning_summary too; skills and the server once each
		t.Errorf("want 8 notes, got %d: %+v", len(notes), notes)
	}

	dw, _, err := Parse(fixture("codex/doc-writer.toml"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := field(t, dw, "disallowedTools").List; !reflect.DeepEqual(got, []string{"Bash", "PowerShell"}) {
		t.Errorf("features.shell_tool = false: %q", got)
	}
	if !strings.Contains(dw.Body, `Edit only files under docs\ and`) || dw.Description != `Writes and updates the docs in docs\ for tasky.` {
		t.Errorf("literal string or escaped description: %q / %q", dw.Body, dw.Description)
	}
}

func TestParseRejects(t *testing.T) {
	for file, want := range map[string]string{
		"invalid/no-frontmatter.md":    "no YAML frontmatter",
		"invalid/no-name.md":           "no name",
		"invalid/unclosed.md":          "never closed",
		"invalid/no-instructions.toml": "no developer_instructions",
		"invalid/bad-string.toml":      "unterminated multi-line string",
	} {
		_, _, err := Parse(fixture(file), "")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want an error with %q, got %v", file, want, err)
		}
	}
	if _, _, err := Parse(fixture("codex/doc-writer.toml"), Claude); err == nil {
		t.Error("a .toml file read as a Claude agent")
	}
	if _, _, err := Parse(fixture("claude/minimal.md"), Codex); err == nil {
		t.Error("a .md file read as a Codex agent")
	}
}

func TestCanonicalName(t *testing.T) {
	for in, want := range map[string]string{"reviewer": "cbrt-reviewer", "cbrt-changelog": "cbrt-changelog", "test_runner": "cbrt-test_runner", "CBRT-x": "CBRT-x"} {
		if got, err := CanonicalName(in); err != nil || got != want {
			t.Errorf("CanonicalName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "../x", `a\b`, "a b", "-x", strings.Repeat("a", 65)} {
		if _, err := CanonicalName(bad); err == nil {
			t.Errorf("CanonicalName(%q) accepted", bad)
		}
	}
}

// Every valid fixture imports under a cbrt- name and reads back unchanged.
func TestImportRoundTrip(t *testing.T) {
	home := t.TempDir()
	for _, file := range []string{"claude/reviewer.md", "claude/db-analyst.md", "claude/release-helper.md", "claude/minimal.md",
		"claude/cbrt-changelog.md", "antigravity/triage/agent.md", "antigravity/notes-writer.md", "codex/test_runner.toml", "codex/doc-writer.toml"} {
		r, err := Import(fixture(file), ImportOptions{Home: home})
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if !strings.HasPrefix(r.Agent.Name, Prefix) || strings.HasPrefix(r.Agent.Name, Prefix+Prefix) || filepath.Base(r.Path) != r.Agent.Name+".md" {
			t.Errorf("%s: name %q, path %s", file, r.Agent.Name, r.Path)
		}
		got, err := Read(r.Path)
		if err != nil {
			t.Errorf("%s: reading the canonical file: %v", file, err)
			continue
		}
		want := r.Agent
		if got.Name != want.Name || got.Description != want.Description || !reflect.DeepEqual(got.Fields, want.Fields) ||
			strings.TrimSpace(got.Body) != strings.TrimSpace(want.Body) {
			t.Errorf("%s: round trip changed the agent\n got %+v\nwant %+v", file, got, want)
		}
	}
}

func TestImportNeedsForce(t *testing.T) {
	home := t.TempDir()
	src := fixture("claude/reviewer.md")
	if r, err := Import(src, ImportOptions{Home: home, DryRun: true}); err != nil || r.Wrote {
		t.Fatalf("dry run: %v, wrote %v", err, r.Wrote)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Fatalf("dry run wrote %d files", len(ents))
	}
	if _, err := Import(src, ImportOptions{Home: home}); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(src, ImportOptions{Home: home}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("second import without --force: %v", err)
	}
	if _, err := Import(src, ImportOptions{Home: home, Force: true}); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

func TestPlan(t *testing.T) {
	db, _, err := Parse(fixture("claude/db-analyst.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[Tool]map[string]Code{
		Claude:      {"tools": Kept, "model": Kept, "color": Kept, "memory": Kept},
		Antigravity: {"tools": Warned, "disallowedTools": Warned, "model": Dropped, "effort": Dropped, "permissionMode": Kept, "mcpServers": Warned, "color": Dropped},
		Codex:       {"body": Mapped, "tools": Warned, "disallowedTools": Warned, "effort": Mapped, "permissionMode": Warned, "mcpServers": Dropped},
	}
	for tool, fields := range want {
		plan := Plan(db, tool)
		for key, code := range fields {
			if n, ok := noteFor(plan, key); !ok || n.Code != code {
				t.Errorf("%s %s: want %c, got %+v", tool, key, code, n)
			}
		}
	}
	if n, _ := noteFor(Plan(db, Antigravity), "tools"); !strings.Contains(n.Detail, "view_file, grep_search, search_web") || !strings.Contains(n.Detail, "mcp__tasky_db__query") {
		t.Errorf("antigravity tools detail: %q", n.Detail)
	}
	if n, _ := noteFor(Plan(db, Codex), "effort"); n.Detail != "model_reasoning_effort = high (Codex has no xhigh)" {
		t.Errorf("codex effort detail: %q", n.Detail)
	}

	rh, _, err := Parse(fixture("claude/release-helper.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []Tool{Antigravity, Codex} {
		for _, key := range []string{"permissionMode", "hooks"} {
			if n, _ := noteFor(Plan(rh, tool), key); n.Code != Warned {
				t.Errorf("%s %s: a lost safety setting must warn, got %+v", tool, key, n)
			}
		}
	}

	tr, _, err := Parse(fixture("antigravity/triage/agent.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	for tool, code := range map[Tool]Code{Claude: Dropped, Antigravity: Kept, Codex: Dropped} {
		if n, _ := noteFor(Plan(tr, tool), "mainAgent"); n.Code != code {
			t.Errorf("%s mainAgent: want %c, got %+v", tool, code, n)
		}
	}
	if n, _ := noteFor(Plan(tr, Codex), "commandExecutionPolicy"); n.Code != Warned {
		t.Errorf("codex commandExecutionPolicy: %+v", n)
	}
}

func TestWriteReport(t *testing.T) {
	r, err := Import(fixture("claude/db-analyst.md"), ImportOptions{Home: t.TempDir(), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	WriteReport(&b, r)
	for _, s := range []string{"would import", "as cbrt-db-analyst", "M name", "db-analyst -> cbrt-db-analyst", "install preview", "antigravity  tools"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("report lacks %q:\n%s", s, b.String())
		}
	}
}

// quoteTOML output parses back to the same text, for every fixture body.
func TestQuoteTOMLRoundTrip(t *testing.T) {
	bodies := []string{"plain\n", "ends with a quote '", "has ''' and \"\"\" and \\ and C:\\Users\\user\\x\n"}
	for _, file := range []string{"claude/release-helper.md", "claude/reviewer.md", "codex/doc-writer.toml"} {
		a, _, err := Parse(fixture(file), "")
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, a.Body)
	}
	for _, body := range bodies {
		fields, err := parseTOML("developer_instructions = " + quoteTOML(body) + "\n")
		if err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if fields[0].Value.Str != body {
			t.Errorf("round trip:\n got %q\nwant %q", fields[0].Value.Str, body)
		}
	}
}

func TestParseTOML(t *testing.T) {
	fields, err := parseTOML("a = \"x\\u00e9\" # c\n\"q.k\".b = 'lit\\'\narr = [\n  \"s\", # c\n  't',\n]\nn = [1, 2]\nt = { x = \"}\" }\n[[x.y]]\nz = true\n[[x.y]]\nz = 1979-05-27\n")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Value{}
	for _, f := range fields {
		got[f.Key] = f.Value
	}
	if got["a"].Str != "xé" || got["q.k.b"].Str != `lit\` || !reflect.DeepEqual(got["arr"].List, []string{"s", "t"}) ||
		got["n"].Kind != Raw || got["t"].Raw != `{ x = "}" }` || got["x.y[0].z"].Str != "true" || got["x.y[1].z"].Str != "1979-05-27" {
		t.Errorf("parsed: %+v", got)
	}
	for _, bad := range []string{"a = \"x", "a = 1\na = 2", "a = 1 b", "[t\nx = 1", "a = '''x", "a = \"\\q\""} {
		if _, err := parseTOML(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseYAML(t *testing.T) {
	front := strings.Split("a: plain # comment\nb: 'it''s'\nc: |\n  one\n  two\nd: >\n  folded\n  line\ne:\n- x\n- \"y\"\nf: [ ]\ng: plain\n  continued\nh:\n  k: v\n", "\n")
	fields, err := parseYAML(front, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Value{}
	for _, f := range fields {
		got[f.Key] = f.Value
	}
	if got["a"].Str != "plain" || got["b"].Str != "it's" || got["c"].Str != "one\ntwo" || got["d"].Str != "folded line" ||
		!reflect.DeepEqual(got["e"].List, []string{"x", "y"}) || got["f"].Kind != List || len(got["f"].List) != 0 ||
		got["g"].Str != "plain continued" || got["h"].Kind != Raw || got["h"].Raw != "  k: v" {
		t.Errorf("parsed: %+v", got)
	}
	for _, bad := range []string{"a: 1\na: 2", " a: 1", "no colon", `a: "open`, `a: "x" y`, `a: "\q"`} {
		if _, err := parseYAML(strings.Split(bad, "\n"), 2); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestQuoteYAML(t *testing.T) {
	for _, s := range []string{"plain", "has: colon", "- dash", "multi\nline", `back\slash`, "", " pad", "tab\there", "#hash", "it's \"q\"", "x #y"} {
		fields, err := parseYAML([]string{"k: " + quoteYAML(s)}, 2)
		if err != nil || fields[0].Value.Str != s {
			t.Errorf("quoteYAML(%q) = %s reads back %+v, %v", s, quoteYAML(s), fields, err)
		}
	}
}
