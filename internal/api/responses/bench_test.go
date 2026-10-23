package responses

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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

func BenchmarkDecodeRequestCodexCapture(b *testing.B) {
	body := captureBody(b, "0003-POST-v1_responses.json")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := DecodeRequest(body); err != nil {
			b.Fatal(err)
		}
	}
}
