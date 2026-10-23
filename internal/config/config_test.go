package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const valid = `{
  "providers": [
    {"id": "nvidia", "location": "cloud", "base_url": "https://integrate.api.nvidia.com/v1/", "api_key": "${TEST_NV_KEY}"},
    {"id": "ollama", "type": "ollama", "location": "local"}
  ],
  "routes": {
    "default": [
      {"provider": "nvidia", "model": "deepseek-ai/deepseek-v4.1-flash", "first_byte_timeout": "180s", "tool_choice_required": false},
      "ollama:gemma-4-e2b-4:latest"
    ],
    "claude-*haiku*": ["ollama:minicpm-v4.6"]
  }
}`

func TestParseValid(t *testing.T) {
	t.Setenv("TEST_NV_KEY", "nvapi-test")
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	nv, _ := c.Provider("nvidia")
	if nv.APIKey != "nvapi-test" || nv.BaseURL != "https://integrate.api.nvidia.com/v1" || nv.Type != "openai" {
		t.Errorf("provider not expanded/normalized: %+v", nv)
	}
	ol, _ := c.Provider("ollama")
	if ol.BaseURL != "http://127.0.0.1:11434/v1" {
		t.Errorf("ollama default base_url: %q", ol.BaseURL)
	}
	d := c.Routes["default"]
	if d[0].FirstByteTimeout.D() != 180*time.Second || d[0].SupportsToolChoiceRequired() {
		t.Errorf("target profile not parsed: %+v", d[0])
	}
	if d[1].Provider != "ollama" || d[1].Model != "gemma-4-e2b-4:latest" {
		t.Errorf("shorthand must split on the first colon only: %+v", d[1])
	}
	if !d[1].SupportsToolChoiceRequired() || !d[1].WantsStreamUsage() || !d[1].HasCapability("vision") {
		t.Errorf("profile defaults wrong: %+v", d[1])
	}
	if c.Listen != "127.0.0.1:8080" || c.Limits.MaxInflightBytes != 64<<20 || c.Timeouts.FirstByteCloud.D() != time.Minute {
		t.Errorf("defaults not applied: %+v", c)
	}
	if got := strings.Join(c.RouteNames(), ","); got != "default" {
		t.Errorf("RouteNames = %q (globs must be excluded)", got)
	}
}

func TestParseInvalid(t *testing.T) {
	t.Setenv("TEST_NV_KEY", "")
	cases := map[string]struct{ doc, want string }{
		"missing key":      {valid, `"nvidia": api_key is empty`},
		"unknown field":    {`{"providerz": []}`, "unknown field"},
		"unknown provider": {`{"providers":[{"id":"a","location":"local","base_url":"http://x"}],"routes":{"r":["b:m"]}}`, `unknown provider "b"`},
		"duplicate id":     {`{"providers":[{"id":"a","location":"local","base_url":"http://x"},{"id":"a","location":"local","base_url":"http://x"}],"routes":{"r":["a:m"]}}`, "duplicate id"},
		"bad location":     {`{"providers":[{"id":"a","location":"moon","base_url":"http://x"}],"routes":{"r":["a:m"]}}`, "location must be"},
		"bad url":          {`{"providers":[{"id":"a","location":"local","base_url":"ftp://x"}],"routes":{"r":["a:m"]}}`, "not an http(s) URL"},
		"no routes":        {`{"providers":[{"id":"a","location":"local","base_url":"http://x"}]}`, "at least one route"},
		"bad shorthand":    {`{"providers":[{"id":"a","location":"local","base_url":"http://x"}],"routes":{"r":["a"]}}`, "provider:model"},
		"bad capability":   {`{"providers":[{"id":"a","location":"local","base_url":"http://x"}],"routes":{"r":[{"provider":"a","model":"m","capabilities":["fly"]}]}}`, "unknown capability"},
		"exposed no key":   {`{"listen":"0.0.0.0:8080","providers":[{"id":"a","location":"local","base_url":"http://x"}],"routes":{"r":["a:m"]}}`, "api_key is required"},
		"exposed no tls":   {`{"listen":"0.0.0.0:8080","api_key":"k","providers":[{"id":"a","location":"local","base_url":"http://x"}],"routes":{"r":["a:m"]}}`, "tls_cert"},
		"bad pattern":      {`{"providers":[{"id":"a","location":"local","base_url":"http://x","quota_patterns":["("]}],"routes":{"r":["a:m"]}}`, "bad pattern"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestOfflineAllowsMissingCloudKey(t *testing.T) {
	t.Setenv("TEST_NV_KEY", "")
	doc := strings.Replace(valid, `"providers"`, `"offline": true, "providers"`, 1)
	if _, err := Parse([]byte(doc)); err != nil {
		t.Fatalf("offline config should not require cloud keys: %v", err)
	}
}

func TestUnsetBaseURLVariableIsAnError(t *testing.T) {
	os.Unsetenv("TEST_OLLAMA_URL")
	doc := `{"providers":[{"id":"o","type":"ollama","location":"local","base_url":"${TEST_OLLAMA_URL}"}],"routes":{"r":["o:m"]}}`
	if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), "${TEST_OLLAMA_URL}") {
		t.Fatalf("want error about the unset variable, got %v", err)
	}
	t.Setenv("TEST_OLLAMA_URL", "http://ollama:11434/v1")
	c, err := Parse([]byte(doc))
	if err != nil || c.Providers[0].BaseURL != "http://ollama:11434/v1" {
		t.Fatalf("set variable: %v %+v", err, c)
	}
}

func TestOfflineFromEnv(t *testing.T) {
	t.Setenv("CBRT_OFFLINE", "1")
	t.Setenv("TEST_NV_KEY", "")
	c, err := Parse([]byte(valid))
	if err != nil || !c.Offline {
		t.Fatalf("CBRT_OFFLINE=1 must force offline (and waive cloud keys): %v %v", err, c)
	}
}

func TestExposedWithProxyAllowed(t *testing.T) {
	doc := `{"listen":"0.0.0.0:8080","api_key":"k","trust_proxy":true,"providers":[{"id":"a","location":"local","base_url":"http://x"}],"routes":{"r":["a:m"]}}`
	if _, err := Parse([]byte(doc)); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedProxies(t *testing.T) {
	parse := func(extra string) (*Config, error) {
		return Parse([]byte(`{"providers":[{"id":"a","location":"local","base_url":"http://x"}],"routes":{"r":["a:m"]}` + extra + `}`))
	}
	if _, err := parse(`,"trust_proxy":true,"trusted_proxies":["10.0.0.0/8","not-an-ip"]`); err == nil {
		t.Error("an invalid trusted_proxies entry must be rejected")
	}
	if _, err := parse(`,"trusted_proxies":["10.0.0.0/8"]`); err == nil {
		t.Error("trusted_proxies without trust_proxy must be rejected")
	}
	def, err := parse(`,"trust_proxy":true`)
	if err != nil {
		t.Fatal(err)
	}
	list, err := parse(`,"trust_proxy":true,"trusted_proxies":["172.16.0.0/12","2001:db8::/32","192.0.2.1","::ffff:10.0.0.0/104"]`)
	if err != nil {
		t.Fatal(err)
	}
	off, _ := parse(``)
	for _, c := range []struct {
		cfg  *Config
		ip   string
		want bool
	}{
		{def, "127.0.0.1", true}, {def, "::1", true}, {def, "::ffff:127.0.0.1", true}, {def, "172.18.0.2", false},
		{list, "172.18.0.2", true}, {list, "127.0.0.1", false}, {list, "2001:db8::7", true},
		{list, "192.0.2.1", true}, {list, "192.0.2.2", false}, {list, "10.1.2.3", true},
		{off, "127.0.0.1", false},
	} {
		if got := c.cfg.ProxyTrusted(netip.MustParseAddr(c.ip)); got != c.want {
			t.Errorf("ProxyTrusted(%s) = %v (trusted_proxies %v)", c.ip, got, c.cfg.TrustedProxies)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "localhost:1": true, "[::1]:80": true,
		"0.0.0.0:8080": false, ":8080": false, "192.168.1.5:80": false, "garbage": false,
	} {
		if IsLoopback(addr) != want {
			t.Errorf("IsLoopback(%q) != %v", addr, want)
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comment\nTEST_A=\"one\"\nexport TEST_B=two\nTEST_C=from-file\n"), 0o600)
	t.Setenv("TEST_C", "preset")
	os.Unsetenv("TEST_A")
	os.Unsetenv("TEST_B")
	t.Cleanup(func() { os.Unsetenv("TEST_A"); os.Unsetenv("TEST_B") })
	if err := LoadEnvFile(p); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TEST_A") != "one" || os.Getenv("TEST_B") != "two" || os.Getenv("TEST_C") != "preset" {
		t.Errorf("got A=%q B=%q C=%q", os.Getenv("TEST_A"), os.Getenv("TEST_B"), os.Getenv("TEST_C"))
	}
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("missing file must be ignored: %v", err)
	}
}

func TestEnvFileReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	t.Setenv("TEST_ENV_REAL", "process")
	for _, k := range []string{"TEST_ENV_A", "TEST_ENV_B"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	e := NewEnvFile(p)
	get := func(k string) string {
		v, ok := os.LookupEnv(k)
		if !ok {
			return "<unset>"
		}
		return v
	}

	os.WriteFile(p, []byte("TEST_ENV_A=one\nTEST_ENV_B='b1'\nTEST_ENV_REAL=file\n"), 0o600)
	if err := e.Load(); err != nil {
		t.Fatal(err)
	}
	if get("TEST_ENV_A") != "one" || get("TEST_ENV_B") != "b1" || get("TEST_ENV_REAL") != "process" {
		t.Fatalf("first load: A=%q B=%q REAL=%q", get("TEST_ENV_A"), get("TEST_ENV_B"), get("TEST_ENV_REAL"))
	}

	// Edit A, drop B: the new value wins over the earlier load, and B is unset again.
	os.WriteFile(p, []byte("TEST_ENV_A=two\nTEST_ENV_REAL=file2\n"), 0o600)
	if err := e.Load(); err != nil {
		t.Fatal(err)
	}
	if get("TEST_ENV_A") != "two" || get("TEST_ENV_B") != "<unset>" || get("TEST_ENV_REAL") != "process" {
		t.Fatalf("reload: A=%q B=%q REAL=%q", get("TEST_ENV_A"), get("TEST_ENV_B"), get("TEST_ENV_REAL"))
	}

	// Removing the file removes what it had set, but never the real process env.
	os.Remove(p)
	if err := e.Load(); err != nil {
		t.Fatalf("missing file must not be an error: %v", err)
	}
	if get("TEST_ENV_A") != "<unset>" || get("TEST_ENV_REAL") != "process" {
		t.Fatalf("after removal: A=%q REAL=%q", get("TEST_ENV_A"), get("TEST_ENV_REAL"))
	}
}

func TestNumericValidation(t *testing.T) {
	base := `{"providers":[{"id":"a","location":"local","base_url":"http://x"%s}],"routes":{"r":[{"provider":"a","model":"m"%s}]}%s}`
	doc := func(prov, tgt, top string) string { return fmt.Sprintf(base, prov, tgt, top) }
	cases := map[string]struct{ doc, want string }{
		"neg max_request_bytes":  {doc("", "", `,"limits":{"max_request_bytes":-1}`), "limits.max_request_bytes must be > 0"},
		"neg max_inflight_bytes": {doc("", "", `,"limits":{"max_inflight_bytes":-5}`), "limits.max_inflight_bytes must be > 0"},
		"neg concurrency":        {doc("", "", `,"limits":{"max_concurrent_requests":-1}`), "limits.max_concurrent_requests must be > 0"},
		"neg streams":            {doc("", "", `,"limits":{"max_concurrent_streams":-1}`), "limits.max_concurrent_streams must be > 0"},
		"neg rpm":                {doc("", "", `,"limits":{"requests_per_minute":-1}`), "limits.requests_per_minute must be > 0"},
		"inflight < request":     {doc("", "", `,"limits":{"max_request_bytes":2048,"max_inflight_bytes":1024}`), "limits.max_inflight_bytes (1024) must be >= limits.max_request_bytes (2048)"},
		"neg idle":               {doc("", "", `,"timeouts":{"idle":"-1s"}`), "timeouts.idle must be >= 0"},
		"neg write":              {doc("", "", `,"timeouts":{"write":"-1s"}`), "timeouts.write must be >= 0"},
		"neg first_byte_cloud":   {doc("", "", `,"timeouts":{"first_byte_cloud":"-1s"}`), "timeouts.first_byte_cloud must be >= 0"},
		"neg first_byte_local":   {doc("", "", `,"timeouts":{"first_byte_local":"-1s"}`), "timeouts.first_byte_local must be >= 0"},
		"neg read":               {doc("", "", `,"timeouts":{"read":"-1s"}`), "timeouts.read must be > 0"},
		"neg quota cooldown":     {doc("", "", `,"cooldowns":{"quota":"-1m"}`), "cooldowns.quota must be >= 0"},
		"neg retention":          {doc("", "", `,"retention_months":-2`), "retention_months must be >= 1, or -1"},
		"neg provider timeout":   {doc(`,"first_byte_timeout":"-2s"`, "", ""), `provider "a": first_byte_timeout must be >= 0`},
		"neg target timeout":     {doc("", `,"first_byte_timeout":"-2s"`, ""), "target a:m: first_byte_timeout must be >= 0"},
		"neg max_context":        {doc("", `,"max_context":-1`, ""), "target a:m: max_context must be >= 0"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	c, err := Parse([]byte(doc(`,"first_byte_timeout":"0s"`, `,"max_context":0`, `,"limits":{"max_request_bytes":1024,"max_inflight_bytes":1024}`)))
	if err != nil {
		t.Fatalf("zero/equal values must be accepted: %v", err)
	}
	if c.Timeouts.Read.D() != 60*time.Second {
		t.Errorf("timeouts.read default = %v", c.Timeouts.Read.D())
	}
	// Only max_request_bytes set above the inflight default: inflight follows it.
	c, err = Parse([]byte(doc("", "", `,"limits":{"max_request_bytes":104857600}`)))
	if err != nil || c.Limits.MaxInflightBytes != 100<<20 {
		t.Fatalf("inflight default should follow max_request_bytes: %v %+v", err, c)
	}
	// -1 keeps all usage files.
	c, err = Parse([]byte(doc("", "", `,"retention_months":-1`)))
	if err != nil || c.RetentionMonths != -1 {
		t.Fatalf("retention_months -1 must be accepted: %v", err)
	}
}

func TestRestartRequired(t *testing.T) {
	doc := `{"providers":[{"id":"a","location":"local","base_url":"http://x"}],"routes":{"r":["a:m"]}}`
	old, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	same := *old
	same.APIKey, same.Limits.MaxRequestBytes, same.Timeouts.Idle = "new", 1, Duration(time.Second)
	if got := RestartRequired(old, &same); len(got) != 0 {
		t.Errorf("live-reloadable fields reported as restart-only: %v", got)
	}
	n := *old
	n.Listen, n.TLSCert, n.TLSKey, n.DataDir, n.RetentionMonths = "0.0.0.0:1", "c", "k", "d", 1
	n.Limits.MaxConcurrentRequests, n.Limits.MaxConcurrentStreams, n.Limits.RequestsPerMinute = 1, 1, 1
	want := "listen,tls_cert,tls_key,data_dir,retention_months,limits.max_concurrent_requests,limits.max_concurrent_streams,limits.requests_per_minute"
	if got := strings.Join(RestartRequired(old, &n), ","); got != want {
		t.Errorf("RestartRequired = %q, want %q", got, want)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(valid))
	f.Add([]byte(`{"routes":{"[":["a:b"]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		Parse(data) // must never panic
	})
}

// On Windows, environment names are case-insensitive: a file key differing only
// in case from a real variable must not override (or later unset) it.
func TestEnvFileCaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive env names are Windows-only")
	}
	p := filepath.Join(t.TempDir(), ".env")
	t.Setenv("TEST_ENV_CASE", "process")
	e := NewEnvFile(p)
	os.WriteFile(p, []byte("test_env_case=file\n"), 0o600)
	if err := e.Load(); err != nil {
		t.Fatal(err)
	}
	if v := os.Getenv("TEST_ENV_CASE"); v != "process" {
		t.Fatalf("file overrode real variable: %q", v)
	}
	os.Remove(p)
	e.Load()
	if v := os.Getenv("TEST_ENV_CASE"); v != "process" {
		t.Fatalf("removal unset real variable: %q", v)
	}
}

func TestAnthropicPassthroughProvider(t *testing.T) {
	ok := `{"providers":[{"id":"an","type":"anthropic"},{"id":"o","type":"ollama","location":"local"}],"routes":{"claude-*":["an:*","o:m"]}}`
	c, err := Parse([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Provider("an")
	if !p.Passthrough() || p.BaseURL != "https://api.anthropic.com" || p.Location != "cloud" {
		t.Fatalf("defaults: %+v", p)
	}

	cases := map[string]struct{ doc, want string }{
		"own key":     {`{"providers":[{"id":"an","type":"anthropic","api_key":"sk"}],"routes":{"r":["an:*"]}}`, "remove api_key"},
		"gateway key": {`{"api_key":"k","providers":[{"id":"an","type":"anthropic"}],"routes":{"r":["an:*"]}}`, "loopback listen address and no api_key"},
		"exposed":     {`{"listen":"0.0.0.0:8080","api_key":"k","tls_cert":"c","tls_key":"k","providers":[{"id":"an","type":"anthropic"}],"routes":{"r":["an:*"]}}`, "loopback listen address"},
		"model":       {`{"providers":[{"id":"an","type":"anthropic"}],"routes":{"r":["an:claude-opus-4-8"]}}`, `use model "*"`},
		"type":        {`{"providers":[{"id":"x","type":"gemini","location":"local","base_url":"http://x"}],"routes":{"r":["x:m"]}}`, "openai, ollama or anthropic"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}
