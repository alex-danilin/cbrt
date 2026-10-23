package record

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newProxy(t *testing.T, upstream http.Handler) (*httptest.Server, string) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	rec := &Recorder{Upstream: u, Dir: dir, OnError: func(err error) { t.Errorf("persist: %v", err) }}
	px := httptest.NewServer(rec)
	t.Cleanup(px.Close)
	return px, dir
}

func readOnly(t *testing.T, dir string) Exchange {
	t.Helper()
	// The exchange file is written after the response completes; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		if len(files) == 1 {
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var ex Exchange
			if err := json.Unmarshal(data, &ex); err != nil {
				t.Fatalf("bad capture file: %v", err)
			}
			return ex
		}
		if time.Now().After(deadline) {
			t.Fatalf("want 1 capture file, got %d", len(files))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForwardsAndRecordsJSON(t *testing.T) {
	px, dir := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.URL.Query().Get("beta") != "true" {
			t.Errorf("unexpected upstream URL %s", r.URL)
		}
		if r.Header.Get("X-Api-Key") != "sk-secret" {
			t.Errorf("auth header must be forwarded unchanged")
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"echo":%s}`, body)
	}))

	req, _ := http.NewRequest("POST", px.URL+"/v1/messages?beta=true", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-Api-Key", "sk-secret")
	req.Header.Set("Authorization", "Bearer sk-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(got) != `{"echo":{"model":"m"}}` {
		t.Fatalf("client got %d %s", resp.StatusCode, got)
	}

	ex := readOnly(t, dir)
	if ex.Method != "POST" || ex.Path != "/v1/messages?beta=true" || ex.Status != http.StatusCreated {
		t.Errorf("bad metadata: %+v", ex)
	}
	if compact(t, ex.ReqBody) != `{"model":"m"}` || compact(t, ex.RespBody) != `{"echo":{"model":"m"}}` {
		t.Errorf("bodies not stored as JSON: %s / %s", ex.ReqBody, ex.RespBody)
	}
	if ex.ReqHeaders.Get("X-Api-Key") != "[redacted]" || ex.ReqHeaders.Get("Authorization") != "[redacted]" {
		t.Errorf("secrets not redacted: %v", ex.ReqHeaders)
	}
	data, _ := os.ReadFile(mustGlob(t, dir))
	if strings.Contains(string(data), "sk-secret") {
		t.Error("secret leaked into capture file")
	}
}

func TestStreamsLiveAndRecordsSSE(t *testing.T) {
	release := make(chan struct{})
	px, dir := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release // the client must see "first" before the upstream continues
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	lineCh := make(chan string, 1)
	go func() { l, _ := br.ReadString('\n'); lineCh <- l }()
	select {
	case l := <-lineCh:
		if l != "data: first\n" {
			t.Fatalf("got %q", l)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("stream was buffered: first event not delivered while upstream was blocked")
	}
	close(release)
	io.Copy(io.Discard, br)

	ex := readOnly(t, dir)
	var sse string
	if err := json.Unmarshal(ex.RespBody, &sse); err != nil {
		t.Fatalf("SSE body should be stored as a JSON string: %v", err)
	}
	if sse != "data: first\n\ndata: [DONE]\n\n" {
		t.Errorf("recorded stream %q", sse)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"/v1/messages/count_tokens?beta=true": "v1_messages_count_tokens",
		"/":                                   "root",
		"/api/tags":                           "api_tags",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return b.String()
}

func mustGlob(t *testing.T, dir string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	return files[0]
}
