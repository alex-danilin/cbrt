package anthropic

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// captureNames are the Claude Code captures with a request body.
var captureNames = []string{"0003-POST-v1_messages.json", "0015-POST-v1_messages.json", "0016-POST-v1_messages.json"}

// captureBody returns the compacted request_body of a capture file.
func captureBody(tb testing.TB, name string) []byte {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join(captureDir, name))
	if err != nil {
		tb.Fatal(err)
	}
	var c struct {
		RequestBody json.RawMessage `json:"request_body"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		tb.Fatal(err)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, c.RequestBody); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

func BenchmarkDecodeRequestClaudeCapture(b *testing.B) {
	for _, name := range captureNames {
		body := captureBody(b, name)
		b.Run(name[:4], func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := DecodeRequest(body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCountTokensClaudeCapture(b *testing.B) {
	for _, name := range captureNames {
		body := captureBody(b, name)
		b.Run(name[:4], func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := CountTokens(body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
