// Command loadtest measures the gateway binary's memory and latency under the
// target workload: N concurrent streams plus M concurrent non-stream
// requests, each with a ≈50 KB body, against an in-process fake upstream.
//
//	go run ./scripts/loadtest -bin dist/cbrt.exe -duration 20s
//
// It starts the fake upstream, writes a temporary config pointing cbrt at
// it, launches the binary as a child process, waits for /healthz, samples the
// idle RSS, runs the workload while sampling the child's RSS every 500 ms, and
// prints a summary. RSS is read with OS tools (tasklist on Windows,
// /proc/<pid>/status on Linux, ps elsewhere); this is a dev script only.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	bin := flag.String("bin", "dist/cbrt.exe", "gateway binary")
	duration := flag.Duration("duration", 20*time.Second, "load phase length")
	streams := flag.Int("streams", 16, "concurrent streaming clients")
	nonStream := flag.Int("nonstream", 16, "concurrent non-stream clients")
	bodyKB := flag.Int("body-kb", 50, "request body size in KB")
	chunks := flag.Int("chunks", 50, "SSE chunks per upstream stream")
	chunkDelay := flag.Duration("chunk-delay", 5*time.Millisecond, "delay between upstream SSE chunks")
	idleWait := flag.Duration("idle-wait", 3*time.Second, "settle time before the idle RSS sample")
	flag.BoolVar(&showLog, "show-log", false, "print the child's output at the end (e.g. with GODEBUG=gctrace=1 set)")
	flag.Parse()
	if err := run(*bin, *duration, *streams, *nonStream, *bodyKB, *chunks, *chunkDelay, *idleWait); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

var showLog bool

func run(bin string, duration time.Duration, streams, nonStream, bodyKB, chunks int, chunkDelay, idleWait time.Duration) error {
	binAbs, err := filepath.Abs(bin)
	if err != nil {
		return err
	}
	st, err := os.Stat(binAbs)
	if err != nil {
		return fmt.Errorf("binary: %w", err)
	}

	up, err := startUpstream(chunks, chunkDelay)
	if err != nil {
		return err
	}
	defer up.Close()

	dir, err := os.MkdirTemp("", "cbrt-loadtest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	port, err := freePort()
	if err != nil {
		return err
	}
	gwURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := map[string]any{
		"listen":   fmt.Sprintf("127.0.0.1:%d", port),
		"data_dir": filepath.Join(dir, "data"),
		"offline":  true,
		// Admission limits are raised so the load test measures memory, not 429s:
		// the default 60 requests/minute would reject almost all of the workload,
		// and slots are released a moment after the client sees the last byte.
		"limits": map[string]any{
			"requests_per_minute":     1_000_000,
			"max_concurrent_requests": 2 * (streams + nonStream),
			"max_concurrent_streams":  2 * streams,
		},
		"providers": []any{map[string]any{"id": "fake", "type": "openai", "location": "local", "base_url": "http://" + up.Addr().String() + "/v1"}},
		"routes":    map[string]any{"r": []string{"fake:m1"}},
	}
	cfgPath := filepath.Join(dir, "cbrt.json")
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		return err
	}

	var logBuf bytes.Buffer
	cmd := exec.Command(binAbs, "serve", "-config", cfgPath, "-env-file", filepath.Join(dir, "no.env"))
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = &logBuf, &logBuf
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer stopChild(cmd, exited)

	if err := waitHealthy(gwURL, exited, 15*time.Second); err != nil {
		return fmt.Errorf("%w\nchild output:\n%s", err, logBuf.String())
	}
	fmt.Printf("gateway pid %d at %s, fake upstream at %s\n", pid, gwURL, up.Addr())

	time.Sleep(idleWait)
	idle, err := rss(pid)
	if err != nil {
		return fmt.Errorf("rss: %w", err)
	}

	// Load phase.
	body := func(stream bool) []byte { return requestBody(bodyKB<<10, stream) }
	streamBody, plainBody := body(true), body(false)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: streams + nonStream, MaxConnsPerHost: 0}}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var (
		mu                       sync.Mutex
		plainLat, streamLat      []time.Duration
		plainOK, streamOK, errsN atomic.Int64
		firstErrs                []string
	)
	fail := func(err error) {
		if ctx.Err() != nil {
			return // cut off by the end of the load phase
		}
		errsN.Add(1)
		mu.Lock()
		if len(firstErrs) < 5 {
			firstErrs = append(firstErrs, err.Error())
		}
		mu.Unlock()
	}
	var wg sync.WaitGroup
	worker := func(stream bool) {
		defer wg.Done()
		for ctx.Err() == nil {
			t0 := time.Now()
			err := doRequest(ctx, client, gwURL+"/v1/chat/completions", streamBody, plainBody, stream, chunks)
			d := time.Since(t0)
			if err != nil {
				fail(err)
				continue
			}
			mu.Lock()
			if stream {
				streamLat = append(streamLat, d)
				streamOK.Add(1)
			} else {
				plainLat = append(plainLat, d)
				plainOK.Add(1)
			}
			mu.Unlock()
		}
	}

	var samples []int64
	var sampleErr error
	sampDone := make(chan struct{})
	go func() {
		defer close(sampDone)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				v, err := rss(pid)
				if err != nil {
					sampleErr = err
					continue
				}
				samples = append(samples, v)
			}
		}
	}()

	start := time.Now()
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go worker(true)
	}
	for i := 0; i < nonStream; i++ {
		wg.Add(1)
		go worker(false)
	}
	wg.Wait()
	elapsed := time.Since(start)
	<-sampDone
	client.CloseIdleConnections()

	select {
	case err := <-exited:
		return fmt.Errorf("gateway exited during the load phase: %v\n%s", err, logBuf.String())
	default:
	}
	time.Sleep(2 * time.Second)
	after, _ := rss(pid)

	// Report.
	mb := func(v int64) string { return fmt.Sprintf("%.1f MB", float64(v)/(1<<20)) }
	fmt.Printf("\nworkload: %d streams + %d non-stream clients, %d KB bodies, %d chunks x %s per stream, %s\n",
		streams, nonStream, bodyKB, chunks, chunkDelay, elapsed.Round(time.Millisecond))
	fmt.Printf("binary size:      %s (%d bytes) %s\n", mb(st.Size()), st.Size(), filepath.Base(binAbs))
	fmt.Printf("idle RSS:         %s\n", mb(idle))
	if len(samples) > 0 {
		sorted := slices.Clone(samples)
		slices.Sort(sorted)
		fmt.Printf("RSS under load:   peak %s, p95 %s, p50 %s (%d samples)\n",
			mb(sorted[len(sorted)-1]), mb(pctInt(sorted, 0.95)), mb(pctInt(sorted, 0.50)), len(sorted))
	}
	if sampleErr != nil {
		fmt.Printf("RSS sampling errors: %v\n", sampleErr)
	}
	fmt.Printf("RSS 2 s after load: %s\n", mb(after))
	total := plainOK.Load() + streamOK.Load()
	fmt.Printf("requests:         %d ok (%d non-stream, %d stream), %.0f req/s, %d errors\n",
		total, plainOK.Load(), streamOK.Load(), float64(total)/elapsed.Seconds(), errsN.Load())
	fmt.Printf("throughput in:    %.1f MB/s of request bodies\n",
		float64(plainOK.Load()*int64(len(plainBody))+streamOK.Load()*int64(len(streamBody)))/elapsed.Seconds()/(1<<20))
	slices.Sort(plainLat)
	slices.Sort(streamLat)
	if len(plainLat) > 0 {
		fmt.Printf("non-stream latency: p50 %s, p95 %s, p99 %s\n",
			pctDur(plainLat, 0.50), pctDur(plainLat, 0.95), pctDur(plainLat, 0.99))
	}
	if len(streamLat) > 0 {
		fmt.Printf("stream duration:    p50 %s, p95 %s (upstream floor %s)\n",
			pctDur(streamLat, 0.50), pctDur(streamLat, 0.95), time.Duration(chunks)*chunkDelay)
	}
	for _, e := range firstErrs {
		fmt.Println("  error:", e)
	}
	if showLog {
		fmt.Printf("\nchild output:\n%s", logBuf.String())
	}
	return nil
}

func pctInt(s []int64, p float64) int64 { return s[int(p*float64(len(s)-1))] }

func pctDur(s []time.Duration, p float64) time.Duration {
	return s[int(p*float64(len(s)-1))].Round(10 * time.Microsecond)
}

// requestBody is a Chat Completions request padded to about size bytes.
func requestBody(size int, stream bool) []byte {
	const line = "The quick brown fox jumps over the lazy dog while the gateway relays tokens. "
	pad := strings.Repeat(line, size/len(line)+1)[:size-200]
	b, _ := json.Marshal(map[string]any{
		"model":      "r",
		"stream":     stream,
		"max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": pad},
		},
	})
	return b
}

func doRequest(ctx context.Context, c *http.Client, url string, streamBody, plainBody []byte, stream bool, chunks int) error {
	body := plainBody
	if stream {
		body = streamBody
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	if !stream {
		var out struct {
			Choices []json.RawMessage `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
			return fmt.Errorf("bad non-stream answer: %v", err)
		}
		return nil
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n, done := 0, false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			if line == "data: [DONE]" {
				done = true
			} else {
				n++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !done || n < chunks {
		return fmt.Errorf("short stream: %d chunks, done=%v", n, done)
	}
	return nil
}

// startUpstream serves a Chat Completions fake: an instant JSON answer, or an
// SSE stream of chunks spaced by delay.
func startUpstream(chunks int, delay time.Duration) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	const answer = `{"id":"x","object":"chat.completion","created":1,"model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"Hello from the fake upstream."},"finish_reason":"stop"}],"usage":{"prompt_tokens":12000,"completion_tokens":7,"total_tokens":12007}}`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"list","data":[{"id":"m1","object":"model"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, answer)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			role := ""
			if i == 0 {
				role = `"role":"assistant",`
			}
			fmt.Fprintf(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{%s\"content\":\"tok%d \"},\"finish_reason\":null}]}\n\n", role, i)
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		io.WriteString(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1\",\"choices\":[],\"usage\":{\"prompt_tokens\":12000,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", chunks, 12000+chunks)
		io.WriteString(w, "data: [DONE]\n\n")
	})
	go http.Serve(ln, mux)
	return ln, nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitHealthy(base string, exited <-chan error, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("gateway exited before becoming healthy: %v", err)
		default:
		}
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("gateway did not answer /healthz in time")
}

func stopChild(cmd *exec.Cmd, exited <-chan error) {
	if runtime.GOOS != "windows" {
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
			return
		case <-time.After(5 * time.Second):
		}
	}
	cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
}

// rss returns the resident set size (working set on Windows) of pid in bytes.
func rss(pid int) (int64, error) {
	switch runtime.GOOS {
	case "windows":
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
		if err != nil {
			return 0, err
		}
		// "cbrt.exe","1234","Console","1","12,345 K" — the separator in the
		// number depends on the locale, so keep only the digits.
		fields := strings.Split(strings.TrimSpace(string(out)), `","`)
		if len(fields) < 5 {
			return 0, fmt.Errorf("unexpected tasklist output %q", out)
		}
		kb, err := digits(fields[4])
		return kb << 10, err
	case "linux":
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				kb, err := digits(v)
				return kb << 10, err
			}
		}
		return 0, errors.New("VmRSS not found")
	default:
		out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return 0, err
		}
		kb, err := digits(string(out))
		return kb << 10, err
	}
}

func digits(s string) (int64, error) {
	var d strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			d.WriteRune(r)
		}
	}
	return strconv.ParseInt(d.String(), 10, 64)
}
