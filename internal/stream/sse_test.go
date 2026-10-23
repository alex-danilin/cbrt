package stream

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func readAll(t *testing.T, r io.Reader, max int) ([]Event, error) {
	t.Helper()
	sr := NewReader(r, max)
	var evs []Event
	for {
		ev, err := sr.Next()
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
		if len(evs) > 100000 {
			t.Fatal("runaway reader")
		}
	}
}

// chunkReader returns the input in random-sized pieces.
type chunkReader struct {
	data []byte
	rnd  *rand.Rand
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := 1 + c.rnd.Intn(min(len(c.data), 97))
	n = min(n, len(p))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func fixtures(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "upstream", "*", "*.sse"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	return files
}

func TestFixtures(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			want, err := readAll(t, bytes.NewReader(raw), 1<<20)
			if err != io.EOF {
				t.Fatalf("err = %v, want io.EOF", err)
			}
			if len(want) < 2 || string(want[len(want)-1].Data) != "[DONE]" {
				t.Fatalf("got %d events, last not [DONE]", len(want))
			}
			for _, ev := range want[:len(want)-1] {
				if ev.Data[0] != '{' {
					t.Fatalf("event data not JSON: %q", ev.Data)
				}
			}

			got, err := readAll(t, iotest.OneByteReader(bytes.NewReader(raw)), 1<<20)
			if err != io.EOF || !reflect.DeepEqual(got, want) {
				t.Fatalf("one-byte reader differs (err %v)", err)
			}
			for seed := int64(0); seed < 20; seed++ {
				got, err := readAll(t, &chunkReader{data: raw, rnd: rand.New(rand.NewSource(seed))}, 1<<20)
				if err != io.EOF || !reflect.DeepEqual(got, want) {
					t.Fatalf("seed %d: random chunks differ (err %v)", seed, err)
				}
			}
			crlf := bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
			got, err = readAll(t, iotest.OneByteReader(bytes.NewReader(crlf)), 1<<20)
			if err != io.EOF || !reflect.DeepEqual(got, want) {
				t.Fatalf("CRLF differs (err %v)", err)
			}
		})
	}
}

// TestSplitEveryOffset feeds the input as two reads split at every offset.
func TestSplitEveryOffset(t *testing.T) {
	in := "event: a\r\ndata: x\r\rdata:y\n: c\ndata\n\ndata: [DONE]\n\n"
	want, err := readAll(t, strings.NewReader(in), 1024)
	if err != io.EOF {
		t.Fatal(err)
	}
	for i := 0; i <= len(in); i++ {
		r := io.MultiReader(strings.NewReader(in[:i]), strings.NewReader(in[i:]))
		got, err := readAll(t, r, 1024)
		if err != io.EOF || !reflect.DeepEqual(got, want) {
			t.Fatalf("split at %d: got %q (err %v), want %q", i, got, err, want)
		}
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Event
		err  error
	}{
		{"basic", "data: hello\n\n", []Event{{Data: []byte("hello")}}, io.EOF},
		{"no space", "data:hello\n\n", []Event{{Data: []byte("hello")}}, io.EOF},
		{"one space only", "data:  two\n\n", []Event{{Data: []byte(" two")}}, io.EOF},
		{"multiline", "data: a\ndata: b\n\n", []Event{{Data: []byte("a\nb")}}, io.EOF},
		{"named", "event: ping\ndata: {}\n\n", []Event{{Name: "ping", Data: []byte("{}")}}, io.EOF},
		{"crlf", "event: x\r\ndata: 1\r\n\r\ndata: 2\r\n\r\n", []Event{{Name: "x", Data: []byte("1")}, {Data: []byte("2")}}, io.EOF},
		{"cr only", "data: 1\r\rdata: 2\r\r", []Event{{Data: []byte("1")}, {Data: []byte("2")}}, io.EOF},
		{"comments", ": keepalive\n\n: x\ndata: a\n: y\n\n", []Event{{Data: []byte("a")}}, io.EOF},
		{"no data skipped", "event: x\nid: 3\nretry: 10\n\ndata: a\n\n", []Event{{Data: []byte("a")}}, io.EOF},
		{"name reset", "event: x\n\ndata: a\n\n", []Event{{Data: []byte("a")}}, io.EOF},
		{"unknown fields", "foo: bar\ndata: a\nbaz\n\n", []Event{{Data: []byte("a")}}, io.EOF},
		{"empty data", "data\n\ndata:\n\n", []Event{{Data: []byte{}}, {Data: []byte{}}}, io.EOF},
		{"truncated with newline", "data: a\n\ndata: b\n", []Event{{Data: []byte("a")}, {Data: []byte("b")}}, io.EOF},
		{"truncated no newline", "data: a\n\ndata: b", []Event{{Data: []byte("a")}, {Data: []byte("b")}}, io.EOF},
		{"truncated no data", "data: a\n\nevent: x\n", []Event{{Data: []byte("a")}}, io.EOF},
		{"empty", "", nil, io.EOF},
		{"bom", "\xEF\xBB\xBFdata: a\n\n", []Event{{Data: []byte("a")}}, io.EOF},
		{"colon in value", "data: {\"a\":\"b:c\"}\n\n", []Event{{Data: []byte(`{"a":"b:c"}`)}}, io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readAll(t, iotest.OneByteReader(strings.NewReader(tt.in)), 1024)
			if err != tt.err {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTooLarge(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		n    int // events before the error
	}{
		{"long line", "data: ok\n\ndata: " + strings.Repeat("x", 200) + "\n\n", 64, 1},
		{"long line without newline", "data: " + strings.Repeat("x", 200), 64, 0},
		{"many lines", strings.Repeat("data: 0123456789\n", 10) + "\n", 64, 0},
		{"comment line", ": " + strings.Repeat("x", 200) + "\n\ndata: a\n\n", 64, 0},
		{"exactly max", "data: " + strings.Repeat("x", 58) + "\n\n", 64, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs, err := readAll(t, strings.NewReader(tt.in), tt.max)
			if len(evs) != tt.n {
				t.Fatalf("got %d events, want %d", len(evs), tt.n)
			}
			wantErr := ErrEventTooLarge
			if tt.name == "exactly max" {
				wantErr = io.EOF
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("err = %v, want %v", err, wantErr)
			}
		})
	}
}

// TestTooLargeBounded checks that an endless line fails without reading it all.
func TestTooLargeBounded(t *testing.T) {
	inf := &countingReader{}
	_, err := NewReader(inf, 4096).Next()
	if err != ErrEventTooLarge {
		t.Fatalf("err = %v", err)
	}
	if inf.n > 64<<10 {
		t.Fatalf("read %d bytes for a 4 KiB limit", inf.n)
	}
}

type countingReader struct{ n int }

func (c *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	c.n += len(p)
	return len(p), nil
}

func TestUnderlyingError(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("data: a\n\ndata: partial\n"), iotest.ErrReader(boom))
	sr := NewReader(r, 1024)
	if ev, err := sr.Next(); err != nil || string(ev.Data) != "a" {
		t.Fatalf("first: %q %v", ev.Data, err)
	}
	for range 2 {
		if _, err := sr.Next(); err != boom {
			t.Fatalf("err = %v, want boom", err)
		}
	}
}

func TestDataIsCopied(t *testing.T) {
	sr := NewReader(strings.NewReader("data: aaaa\n\ndata: bbbb\n\n"), 1024)
	a, _ := sr.Next()
	b, _ := sr.Next()
	if string(a.Data) != "aaaa" || string(b.Data) != "bbbb" {
		t.Fatalf("got %q %q", a.Data, b.Data)
	}
}

func TestWrite(t *testing.T) {
	tests := []struct {
		name string
		fn   func(w io.Writer) error
		want string
	}{
		{"data", func(w io.Writer) error { return WriteData(w, []byte(`{"a":1}`)) }, "data: {\"a\":1}\n\n"},
		{"data multiline", func(w io.Writer) error { return WriteData(w, []byte("a\nb\r\nc\rd")) }, "data: a\ndata: b\ndata: c\ndata: d\n\n"},
		{"data empty", func(w io.Writer) error { return WriteData(w, nil) }, "data: \n\n"},
		{"event", func(w io.Writer) error { return WriteEvent(w, "message_start", []byte("{}")) }, "event: message_start\ndata: {}\n\n"},
		{"event multiline", func(w io.Writer) error { return WriteEvent(w, "x", []byte("1\n2")) }, "event: x\ndata: 1\ndata: 2\n\n"},
		{"event unnamed", func(w io.Writer) error { return WriteEvent(w, "", []byte("1")) }, "data: 1\n\n"},
		{"comment", func(w io.Writer) error { return WriteComment(w, "cbrt: nvidia\nx") }, ": cbrt: nvidiax\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := tt.fn(&b); err != nil {
				t.Fatal(err)
			}
			if b.String() != tt.want {
				t.Fatalf("got %q, want %q", b.String(), tt.want)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	var b bytes.Buffer
	WriteComment(&b, "hi")
	WriteEvent(&b, "e1", []byte("line1\nline2"))
	WriteData(&b, []byte("  spaced"))
	WriteData(&b, []byte("[DONE]"))
	got, err := readAll(t, &b, 1024)
	want := []Event{{Name: "e1", Data: []byte("line1\nline2")}, {Data: []byte("  spaced")}, {Data: []byte("[DONE]")}}
	if err != io.EOF || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q (%v)", got, err)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestWriteError(t *testing.T) {
	if WriteData(errWriter{}, []byte("x")) != io.ErrClosedPipe || WriteComment(errWriter{}, "x") != io.ErrClosedPipe {
		t.Fatal("write error not propagated")
	}
}
