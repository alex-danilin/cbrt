package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func authReq(t *testing.T, method, url, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Editing .env and calling /api/reload rotates a key that comes from the env file.
func TestReloadPicksUpEnvFileEdits(t *testing.T) {
	const v = "TEST_CBRT_RELOAD_KEY"
	os.Unsetenv(v)
	t.Cleanup(func() { os.Unsetenv(v) })
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte(v+"=key-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := newFakeUpstream(t)
	cfg := localConfig(up.URL)
	cfg["api_key"] = "${" + v + "}"
	rs := startServeEnv(t, cfg, envPath)

	if code, body := authReq(t, "GET", rs.base+"/v1/models", "key-one"); code != http.StatusOK {
		t.Fatalf("initial key: %d %s", code, body)
	}
	if err := os.WriteFile(envPath, []byte(v+"=key-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := authReq(t, "POST", rs.base+"/api/reload", "key-one"); code != http.StatusOK {
		t.Fatalf("reload: %d %s", code, body)
	}
	if code, _ := authReq(t, "GET", rs.base+"/v1/models", "key-one"); code != http.StatusUnauthorized {
		t.Errorf("old key after rotation: %d, want 401", code)
	}
	if code, body := authReq(t, "GET", rs.base+"/v1/models", "key-two"); code != http.StatusOK {
		t.Errorf("new key after rotation: %d %s", code, body)
	}
}

// Changing listen on reload is refused: it would validate the config against an
// address that is not the one actually bound.
func TestReloadRefusesListenChange(t *testing.T) {
	up := newFakeUpstream(t)
	cfg := localConfig(up.URL)
	rs := startServe(t, cfg)

	cfg["listen"] = "0.0.0.0:0"
	cfg["api_key"] = "k"
	cfg["trust_proxy"] = true
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(rs.cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, body := authReq(t, "POST", rs.base+"/api/reload", "")
	if code != http.StatusBadRequest || !strings.Contains(body, "listen") || !strings.Contains(body, "restart") {
		t.Fatalf("reload with a new listen: %d %s", code, body)
	}
	// Auth is unchanged: the loopback server with no key still answers without one.
	if code, _ := authReq(t, "GET", rs.base+"/v1/models", ""); code != http.StatusOK {
		t.Errorf("config must not have been applied: %d", code)
	}
}
