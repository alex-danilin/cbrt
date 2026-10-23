package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// Synthetic credential values; none of them may survive redaction.
const (
	pemBody = "MIIEowIBAAKCAQEAsynthetic0fixture"
	pemRSA  = "-----BEGIN RSA PRIVATE KEY-----\n" + pemBody + "\nAbCdEf123\n-----END RSA PRIVATE KEY-----"
	pemJSON = `{"content":"-----BEGIN PRIVATE KEY-----\n` + pemBody + `\n-----END PRIVATE KEY-----\n"}`
)

// credentialCases maps an input to its exact redacted form.
var credentialCases = map[string]string{
	// JSON, YAML, escaped nested JSON.
	`{"password":"hunter2"}`:                 `{"password":"[REDACTED:secret]"}`,
	`"api_key": "abc123"`:                    `"api_key": "[REDACTED:secret]"`,
	"token: xyz789":                          "token: [REDACTED:secret]",
	"password: hunter2":                      "password: [REDACTED:secret]",
	`{"cfg":"{\"secret\":\"s3cr3t\"}"}`:      `{"cfg":"{\"secret\":\"[REDACTED:secret]\"}"}`,
	`"x-api-key": "abc123"`:                  `"x-api-key": "[REDACTED:secret]"`,
	`<password>hunter2</password>`:           `<password>[REDACTED:secret]</password>`,
	`<db:api_key id="1">abc123</db:api_key>`: `<db:api_key id="1">[REDACTED:secret]</db:api_key>`,
	// Key names.
	"passwd: hunter2":              "passwd: [REDACTED:secret]",
	"pwd=hunter2":                  "pwd=[REDACTED:secret]",
	"pass: hunter2":                "pass: [REDACTED:secret]",
	"SMTP_PASS: hunter2":           "SMTP_PASS: [REDACTED:secret]",
	"credentials: abc123":          "credentials: [REDACTED:secret]",
	"auth: abc123":                 "auth: [REDACTED:secret]",
	"private_key: abc123":          "private_key: [REDACTED:secret]",
	"Client-Secret: abc123":        "Client-Secret: [REDACTED:secret]",
	`$cfg = @{ token = 'xyz789' }`: `$cfg = @{ token = '[REDACTED:secret]' }`,
	// Separators.
	"api_key => 'abc123'":    "api_key => '[REDACTED:secret]'",
	`password := "hunter2"`:  `password := "[REDACTED:secret]"`,
	"GITHUB_TOKEN_RW=xyz789": "GITHUB_TOKEN_RW=[REDACTED:secret]",
	// Command-line flags.
	"mysql -u bob --password hunter2 db": "mysql -u bob --password [REDACTED:secret] db",
	"cli --api-key abc123 --verbose":     "cli --api-key [REDACTED:secret] --verbose",
	`cli --token "xyz 789"`:              `cli --token "[REDACTED:secret]"`,
	"mysql --password=hunter2":           "mysql --password=[REDACTED:secret]",
	// Authorization headers and Basic credentials (dXNlcjpwYXNz is "user:pass").
	"Authorization: Basic dXNlcjpwYXNz":                 "Authorization: Basic [REDACTED:auth]",
	`{"authorization": "Basic dXNlcjpwYXNz"}`:           `{"authorization": "Basic [REDACTED:auth]"}`,
	"Authorization: Token xyz789abc":                    "Authorization: Token [REDACTED:auth]",
	`curl -H "X-Proxy: basic dXNlcjpwYXNz" example.org`: `curl -H "X-Proxy: basic [REDACTED:auth]" example.org`,
	"Authorization: rawsecret99":                        "Authorization: [REDACTED:secret]",
	// PEM blocks: whole, JSON-escaped, and cut before the END line.
	"key file:\n" + pemRSA + "\nend": "key file:\n[REDACTED:private_key]\nend",
	pemJSON:                          `{"content":"[REDACTED:private_key]\n"}`,
	"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk": "[REDACTED:private_key]",
	// Already caught before; still caught.
	"PASSWORD=hunter2":                   "PASSWORD=[REDACTED:secret]",
	`password = "hunter2"`:               `password = "[REDACTED:secret]"`,
	"export API_KEY=abc123":              "export API_KEY=[REDACTED:secret]",
	`$env:API_KEY = "abc123"`:            `$env:API_KEY = "[REDACTED:secret]"`,
	"Authorization: Bearer xyz789abcdef": "Authorization: Bearer [REDACTED:bearer]",
}

// plainCases stay unchanged: prose, config flags, type names, test output.
var plainCases = []string{
	"the token: we need a new one",
	"auth: true",
	`{"auth": false, "token": null}`,
	"def login(user: str, password: str):",
	"--- PASS: TestRedact (0.00s)",
	"PASS",
	"max_tokens: 5, input_tokens: 1200",
	`if token == "" {`,
	"primary_key: id",
	"Use Basic authentication for the proxy",
	"the secret is out",
	"--token-file creds.txt",
}

func TestRedactCredentials(t *testing.T) {
	red, err := LoadRedactor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range credentialCases {
		got := red.Apply(in)
		if got != want {
			t.Errorf("Apply(%q)\n got %q\nwant %q", in, got, want)
		}
		if again := red.Apply(got); again != got {
			t.Errorf("Apply is not idempotent on %q: %q", got, again)
		}
	}
	for _, in := range plainCases {
		if got := red.Apply(in); got != in {
			t.Errorf("Apply(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestReportHasNoCredentials builds a run from synthetic events carrying
// every credential form above and checks that neither the written report
// files (anchors included) nor an excerpt hold any credential value.
func TestReportHasNoCredentials(t *testing.T) {
	values := []string{"hunter2", "abc123", "xyz789", "s3cr3t", "dXNlcjpwYXNz", "rawsecret99", pemBody, "AbCdEf123", "b3BlbnNzaC1rZXk"}
	inputs := make([]string, 0, len(credentialCases))
	for in := range credentialCases {
		inputs = append(inputs, in)
	}
	b := newSession(stubID, "cred", "/work/proj").ev(analyze.KindUserPrompt, "Set up the deploy. "+inputs[0], nil).
		ev(analyze.KindModelRequest, "", tokens(100, 10))
	for _, in := range inputs[1:] {
		b.ev(analyze.KindError, "config error near "+in, nil)
	}
	b.ev(analyze.KindAssistantText, "Done, fixed: "+pemJSON, nil)
	s := b.s
	in := Input{
		Dataset:       &analyze.Dataset{Window: analyze.Window{Since: t0, Until: t0.Add(24 * time.Hour)}, Sessions: []*analyze.Session{s}},
		Metrics:       []analyze.SessionMetrics{{Session: s.Key, Project: s.Project, Values: map[string]analyze.Value{"tool_calls": metric(1)}}},
		RunID:         "run-cred",
		ExcerptTokens: 100000,
	}
	red, err := LoadRedactor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Write(dir, in, red, Exclusions{}); err != nil {
		t.Fatal(err)
	}
	ex, err := Excerpt(s, s.Events[0].Seq, s.Events[len(s.Events)-1].Seq, red, 0)
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{"excerpt": ex}
	for _, f := range []string{SummaryFile, AnchorsFile, CandidatesFile} {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		outputs[f] = string(raw)
	}
	anchors := readJSONL[Anchor](t, filepath.Join(dir, AnchorsFile))
	if len(anchors) < len(inputs) {
		t.Fatalf("%d anchors for %d credential events", len(anchors), len(inputs))
	}
	if !strings.Contains(ex, "[REDACTED:") || !strings.Contains(outputs[AnchorsFile], "[REDACTED:") {
		t.Error("no redaction markers in the excerpt or the anchors")
	}
	for where, text := range outputs {
		for _, v := range values {
			if strings.Contains(text, v) {
				t.Errorf("%s holds credential value %q", where, v)
			}
		}
	}
}
