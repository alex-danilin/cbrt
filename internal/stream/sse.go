// Package stream reads and writes Server-Sent Events.
package stream

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// ErrEventTooLarge is returned when a line or an accumulated event exceeds
// the reader's size limit.
var ErrEventTooLarge = errors.New("stream: event exceeds size limit")

// Event is one dispatched SSE event.
type Event struct {
	Name string // value of the "event:" field; empty if none
	Data []byte // data lines joined with "\n"
}

// Reader parses an SSE byte stream into events.
type Reader struct {
	br      *bufio.Reader
	max     int
	line    []byte
	data    []byte
	name    string
	hasData bool
	skipLF  bool // previous line ended with '\r'; drop a following '\n'
	started bool // BOM check done
	err     error
}

// EventBuffered reports whether a complete further event is already buffered, so
// the next Next call will not block on the network. A relay can skip flushing
// while that is true and flush once it would wait: same latency, fewer syscalls.
// (A partially buffered event does not count: waiting for its rest could delay
// the event before it.)
func (r *Reader) EventBuffered() bool {
	n := r.br.Buffered()
	if n == 0 {
		return false
	}
	b, _ := r.br.Peek(n)
	return bytes.Contains(b, []byte("\n\n")) || bytes.Contains(b, []byte("\r\n\r\n")) || bytes.Contains(b, []byte("\r\r"))
}

// NewReader returns a Reader that fails with ErrEventTooLarge when a single
// line or an event's accumulated data exceeds maxEventBytes (<= 0 means 1 MiB).
func NewReader(r io.Reader, maxEventBytes int) *Reader {
	if maxEventBytes <= 0 {
		maxEventBytes = 1 << 20
	}
	size := 32 << 10
	if maxEventBytes < size {
		size = max(maxEventBytes, 16)
	}
	return &Reader{br: bufio.NewReaderSize(r, size), max: maxEventBytes}
}

// Next returns the next event. At a clean end of stream it returns io.EOF;
// an unterminated final event that has data is returned first. Errors from
// the underlying reader are returned unchanged. After an error, every
// subsequent call returns the same error.
func (r *Reader) Next() (Event, error) {
	if r.err != nil {
		return Event{}, r.err
	}
	for {
		line, err := r.readLine()
		if err != nil {
			r.err = err
			if err == io.EOF && r.hasData {
				return r.dispatch(), nil
			}
			r.reset()
			return Event{}, err
		}
		if len(line) == 0 {
			if r.hasData {
				return r.dispatch(), nil
			}
			r.reset()
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch string(field) {
		case "data":
			n := len(value)
			if r.hasData {
				n++
			}
			if len(r.data)+n > r.max {
				r.err = ErrEventTooLarge
				r.reset()
				return Event{}, r.err
			}
			if r.hasData {
				r.data = append(r.data, '\n')
			}
			r.data = append(r.data, value...)
			r.hasData = true
		case "event":
			r.name = string(value)
		}
	}
}

func (r *Reader) dispatch() Event {
	ev := Event{Name: r.name, Data: bytes.Clone(r.data)}
	if ev.Data == nil {
		ev.Data = []byte{}
	}
	r.reset()
	return ev
}

func (r *Reader) reset() {
	r.data = r.data[:0]
	r.name = ""
	r.hasData = false
}

// readLine returns the next line without its terminator (\n, \r\n or \r).
// An unterminated last line is returned before io.EOF. The returned slice
// is valid until the next call.
func (r *Reader) readLine() ([]byte, error) {
	r.line = r.line[:0]
	for {
		if _, err := r.br.Peek(1); err != nil {
			if err == io.EOF && len(r.line) > 0 {
				return r.line, nil
			}
			return nil, err
		}
		chunk, _ := r.br.Peek(r.br.Buffered())
		if !r.started {
			r.started = true
			if b, err := r.br.Peek(3); err == nil && string(b) == "\xEF\xBB\xBF" {
				r.br.Discard(3)
				continue
			}
			chunk, _ = r.br.Peek(r.br.Buffered())
		}
		if r.skipLF {
			r.skipLF = false
			if chunk[0] == '\n' {
				r.br.Discard(1)
				continue
			}
		}
		i := bytes.IndexAny(chunk, "\r\n")
		if i < 0 {
			if len(r.line)+len(chunk) > r.max {
				return nil, ErrEventTooLarge
			}
			r.line = append(r.line, chunk...)
			r.br.Discard(len(chunk))
			continue
		}
		if len(r.line)+i > r.max {
			return nil, ErrEventTooLarge
		}
		r.line = append(r.line, chunk[:i]...)
		r.skipLF = chunk[i] == '\r'
		r.br.Discard(i + 1)
		return r.line, nil
	}
}

var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// WriteData writes data as one event, one "data:" line per line of data.
func WriteData(w io.Writer, data []byte) error {
	return WriteEvent(w, "", data)
}

// WriteEvent writes an event with the given name (omitted when empty).
// Newlines in name are removed.
func WriteEvent(w io.Writer, name string, data []byte) error {
	var b bytes.Buffer
	b.Grow(len(name) + len(data) + 16)
	if name != "" {
		b.WriteString("event: ")
		b.WriteString(stripNewlines(name))
		b.WriteByte('\n')
	}
	s := string(data)
	if strings.ContainsRune(s, '\r') {
		s = lineBreaks.Replace(s)
	}
	for {
		line, rest, found := strings.Cut(s, "\n")
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
		if !found {
			break
		}
		s = rest
	}
	b.WriteByte('\n')
	_, err := w.Write(b.Bytes())
	return err
}

// WriteComment writes an SSE comment (": text\n\n"); newlines are removed.
func WriteComment(w io.Writer, text string) error {
	_, err := io.WriteString(w, ": "+stripNewlines(text)+"\n\n")
	return err
}

func stripNewlines(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
