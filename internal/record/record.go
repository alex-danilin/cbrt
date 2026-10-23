// Package record implements a transparent recording reverse proxy used to
// capture real client/provider traffic as test fixtures.
package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxRequestBytes  = 64 << 20
	maxCapturedBytes = 16 << 20
)

// Headers whose values are never written to disk.
var secretHeaders = []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Cookie", "Set-Cookie"}

// Exchange is one recorded request/response pair as written to disk.
type Exchange struct {
	Seq         int64           `json:"seq"`
	Time        time.Time       `json:"time"`
	DurationMS  int64           `json:"duration_ms"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	ReqHeaders  http.Header     `json:"request_headers"`
	ReqBody     json.RawMessage `json:"request_body,omitempty"`
	Status      int             `json:"status"`
	RespHeaders http.Header     `json:"response_headers"`
	RespBody    json.RawMessage `json:"response_body,omitempty"`
	Truncated   bool            `json:"response_truncated,omitempty"`
}

// Recorder forwards every request to Upstream and writes each exchange to Dir.
type Recorder struct {
	Upstream *url.URL
	Dir      string
	// OnError reports failures to persist an exchange; forwarding is never affected.
	OnError func(error)

	seq   atomic.Int64
	proxy *httputil.ReverseProxy
	once  sync.Once
}

func (r *Recorder) init() {
	r.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(r.Upstream)
			pr.Out.Host = r.Upstream.Host
		},
		FlushInterval: -1, // flush every write so SSE streams stay live
	}
}

func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.once.Do(r.init)
	start := time.Now()

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))

	ex := &Exchange{
		Seq:        r.seq.Add(1),
		Time:       start.UTC(),
		Method:     req.Method,
		Path:       req.URL.RequestURI(),
		ReqHeaders: redact(req.Header),
		ReqBody:    asJSON(body),
	}

	cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
	r.proxy.ServeHTTP(cw, req)

	ex.DurationMS = time.Since(start).Milliseconds()
	ex.Status = cw.status
	ex.RespHeaders = redact(w.Header())
	ex.RespBody = asJSON(cw.buf.Bytes())
	ex.Truncated = cw.truncated

	if err := r.write(ex); err != nil && r.OnError != nil {
		r.OnError(err)
	}
}

func (r *Recorder) write(ex *Exchange) error {
	data, err := json.MarshalIndent(ex, "", "  ")
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%04d-%s-%s.json", ex.Seq, ex.Method, slug(ex.Path))
	return os.WriteFile(filepath.Join(r.Dir, name), data, 0o600)
}

// captureWriter tees the response into a bounded buffer while forwarding it.
type captureWriter struct {
	http.ResponseWriter
	status    int
	buf       bytes.Buffer
	truncated bool
}

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if room := maxCapturedBytes - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else {
		c.truncated = true
	}
	return c.ResponseWriter.Write(p)
}

func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func redact(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range secretHeaders {
		if out.Get(k) != "" {
			out.Set(k, "[redacted]")
		}
	}
	return out
}

// asJSON keeps valid JSON as-is and stores anything else (e.g. SSE) as a JSON string.
func asJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	s, _ := json.Marshal(string(b))
	return s
}

func slug(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	s := strings.Trim(strings.NewReplacer("/", "_", "\\", "_", ":", "_", ".", "_").Replace(path), "_")
	if s == "" {
		return "root"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}
