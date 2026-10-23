package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alex-danilin/cbrt/internal/config"
)

func TestChatSendsBodyAndAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer nvapi-x" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers: %v", r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		w.Write(b)
	}))
	defer srv.Close()

	u := New(config.Provider{ID: "nv", BaseURL: srv.URL + "/v1", APIKey: "nvapi-x"})
	resp, err := u.Chat(context.Background(), []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != `{"model":"m"}` {
		t.Errorf("body not passed through: %s", b)
	}
}

func TestNoAuthHeaderWithoutKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Error("unexpected Authorization header")
		}
	}))
	defer srv.Close()
	resp, err := New(config.Provider{ID: "ol", BaseURL: srv.URL}).Chat(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"object":"list","data":[{"id":"b"},{"id":"a"},{"id":""}]}`))
			return
		}
		http.Error(w, "nope", http.StatusTeapot)
	}))
	defer srv.Close()

	ids, err := New(config.Provider{ID: "x", BaseURL: srv.URL + "/v1"}).Models(context.Background())
	if err != nil || !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("got %v %v", ids, err)
	}
	if _, err := New(config.Provider{ID: "x", BaseURL: srv.URL + "/other"}).Models(context.Background()); err == nil {
		t.Error("non-200 must be an error")
	}
}

func TestSetDialContextForTestRoutesDials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()

	var dialed []string
	restore := SetDialContextForTest(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return defaultDial(ctx, network, addr)
	})
	u := New(config.Provider{ID: "o", BaseURL: srv.URL})
	if _, err := u.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	restore()
	if len(dialed) != 1 || dialed[0] != srv.Listener.Addr().String() {
		t.Errorf("dials = %v, want [%s]", dialed, srv.Listener.Addr())
	}
	if dialHook.Load() != nil {
		t.Error("restore did not clear the hook")
	}
}

// A passthrough request carries the client's login: it is never redirected.
func TestForwardNeverFollowsRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	u := New(config.Provider{ID: "an", Type: "anthropic", BaseURL: up.URL})
	resp, err := u.Forward(context.Background(), "/v1/messages", "", http.Header{"Authorization": {"Bearer secret"}}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || elsewhere.Load() != 0 {
		t.Fatalf("status %d, redirect target hit %d times", resp.StatusCode, elsewhere.Load())
	}
}

// redirectPair starts a redirect target that counts every request it gets and
// an upstream that redirects every request there with the given status.
func redirectPair(t *testing.T, code int) (up *httptest.Server, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"data":[{"id":"stolen"}],"choices":[]}`))
	}))
	t.Cleanup(other.Close)
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, code)
	}))
	t.Cleanup(up.Close)
	return up, hits
}

// Chat and Models never follow a redirect: the target gets no request at all,
// so neither the prompt nor the credential reaches it.
func TestChatAndModelsNeverFollowRedirects(t *testing.T) {
	for _, typ := range []string{"openai", "ollama"} {
		for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
			up, hits := redirectPair(t, code)
			u := New(config.Provider{ID: "p", Type: typ, BaseURL: up.URL + "/v1", APIKey: "k"})
			resp, err := u.Chat(context.Background(), []byte(`{"model":"m","messages":[]}`))
			var re *RedirectError
			if resp != nil || !errors.As(err, &re) || re.Status != code || !strings.Contains(err.Error(), "fix base_url") {
				t.Errorf("%s %d chat: resp %v err %v", typ, code, resp, err)
			}
			if re != nil && re.Host == "" {
				t.Errorf("%s %d: redirect host missing: %v", typ, code, err)
			}
			if _, err := u.Models(context.Background()); !errors.As(err, &re) {
				t.Errorf("%s %d models: err %v", typ, code, err)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("%s %d: redirect target received %d requests", typ, code, n)
			}
		}
	}
}

func TestLocalAddr(t *testing.T) {
	for s, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "172.16.0.9": true, "192.168.1.5": true,
		"fd00::1": true, "169.254.1.1": true, "fe80::1": true, "0.0.0.0": true,
		"8.8.8.8": false, "203.0.113.7": false, "2001:db8::1": false, "100.64.0.1": false,
	} {
		if got := LocalAddr(net.ParseIP(s)); got != want {
			t.Errorf("LocalAddr(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestLocalLooking(t *testing.T) {
	for s, want := range map[string]bool{
		"http://localhost:11434/v1": true, "http://127.0.0.1:8080": true, "http://[::1]:1/v1": true,
		"http://ollama:11434": true, "http://gpu.lan/v1": true, "http://box.local": true, "http://192.168.0.2": true,
		"https://api.example.com/v1": false, "http://203.0.113.7/v1": false, "": false,
	} {
		if got := LocalLooking(s); got != want {
			t.Errorf("LocalLooking(%q) = %v, want %v", s, got, want)
		}
	}
}

// fakeResolver makes every name resolve to ips for the duration of the test.
func fakeResolver(t *testing.T, ips ...string) {
	t.Helper()
	prev := lookupIP
	lookupIP = func(context.Context, string) ([]net.IP, error) {
		out := make([]net.IP, len(ips))
		for i, s := range ips {
			out[i] = net.ParseIP(s)
		}
		return out, nil
	}
	t.Cleanup(func() { lookupIP = prev })
}

// Offline: a name that resolves to a public address is refused before any
// connection is attempted, whatever the provider's location label says.
func TestOfflineRefusesPublicAddresses(t *testing.T) {
	var dialed atomic.Int32
	restore := SetDialContextForTest(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed.Add(1)
		return nil, errors.New("test: no real network")
	})
	defer restore()
	fakeResolver(t, "203.0.113.7", "2001:db8::7")
	for _, base := range []string{"http://models.example.test/v1", "http://203.0.113.7:8080/v1", "http://[2001:db8::7]/v1"} {
		u := New(config.Provider{ID: "l", Location: "local", BaseURL: base}, Offline(true))
		if _, err := u.Chat(context.Background(), []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "offline mode") {
			t.Errorf("%s: err %v", base, err)
		}
		if _, err := u.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "offline mode") {
			t.Errorf("%s models: err %v", base, err)
		}
	}
	if n := dialed.Load(); n != 0 {
		t.Errorf("offline upstream dialed %d times", n)
	}
}

// Offline: loopback works, and a name is dialled at its checked (resolved)
// address, never re-resolved, so a second DNS answer cannot redirect it.
func TestOfflineAllowsLocalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	var dialed []string
	restore := SetDialContextForTest(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return defaultDial(ctx, network, addr)
	})
	defer restore()
	fakeResolver(t, "203.0.113.7", "127.0.0.1")

	for _, base := range []string{srv.URL, "http://local-model.example.test:" + port} {
		dialed = nil
		u := New(config.Provider{ID: "l", Location: "local", BaseURL: base}, Offline(true))
		if ids, err := u.Models(context.Background()); err != nil || len(ids) != 1 {
			t.Fatalf("%s: %v %v", base, ids, err)
		}
		if len(dialed) != 1 || dialed[0] != "127.0.0.1:"+port {
			t.Errorf("%s: dials %v, want only 127.0.0.1:%s", base, dialed, port)
		}
	}
}
