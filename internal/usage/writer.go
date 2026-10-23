package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
)

// openFile is one appended JSONL file.
type openFile struct {
	f    *os.File
	w    *bufio.Writer
	used bool // written since the last closeIdle
}

// writer appends queued lines to monthly files. It is used only by the
// store's loop goroutine.
type writer struct {
	s       *Store
	files   map[string]*openFile
	pending int
}

func newWriter(s *Store) *writer {
	return &writer{s: s, files: map[string]*openFile{}}
}

func (w *writer) write(it queued) {
	line := it.line
	if it.kind == attemptsKind {
		b, err := json.Marshal(it.rec)
		if err != nil {
			w.s.persistError("marshal record", err)
			return
		}
		line = b
	}
	name := w.s.fileName(it.kind, it.month)
	of := w.files[name]
	if of == nil {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			w.s.persistError("open "+name, err)
			return
		}
		of = &openFile{f: f, w: bufio.NewWriterSize(f, 64<<10)}
		w.files[name] = of
	}
	of.used = true
	line = append(line, '\n')
	if _, err := of.w.Write(line); err != nil {
		w.fail(name, err)
		return
	}
	w.pending++
	if w.pending >= flushEvery {
		w.flush()
	}
}

// drain writes everything currently queued.
func (w *writer) drain(q <-chan queued) {
	for {
		select {
		case it := <-q:
			w.write(it)
		default:
			return
		}
	}
}

func (w *writer) flush() {
	for name, of := range w.files {
		if err := of.w.Flush(); err != nil {
			w.fail(name, err)
		}
	}
	w.pending = 0
}

// fail counts a write error and drops the file so the next write reopens it
// (bufio.Writer errors are sticky).
func (w *writer) fail(name string, err error) {
	w.s.persistError("write "+name, err)
	if of := w.files[name]; of != nil {
		of.f.Close()
		delete(w.files, name)
	}
}

// closeIdle closes files not written since the previous call, so old
// months do not keep handles open.
func (w *writer) closeIdle() {
	for name, of := range w.files {
		if !of.used {
			w.closeOne(name, of)
			continue
		}
		of.used = false
	}
}

func (w *writer) closeAll() error {
	var errs []error
	for name, of := range w.files {
		if err := w.closeOne(name, of); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (w *writer) closeOne(name string, of *openFile) error {
	delete(w.files, name)
	err := of.w.Flush()
	if cerr := of.f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		w.s.persistError("close "+name, err)
	}
	return err
}
