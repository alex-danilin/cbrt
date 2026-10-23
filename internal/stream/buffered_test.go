package stream

import (
	"strings"
	"testing"
)

func TestEventBuffered(t *testing.T) {
	r := NewReader(strings.NewReader("data: a\n\ndata: b\n\ndata: part"), 1<<10)
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if !r.EventBuffered() {
		t.Error("a complete second event is buffered")
	}
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if r.EventBuffered() {
		t.Error("only a partial event is buffered: the relay must flush")
	}
	if NewReader(strings.NewReader(""), 16).EventBuffered() {
		t.Error("empty reader")
	}
}
