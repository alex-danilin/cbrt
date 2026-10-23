package projects

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// toggleLocks serializes Toggle per PLAN.md path.
var toggleLocks sync.Map

// PlanHash identifies the PLAN.md content a page was rendered from.
func PlanHash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ToggleError is a refusal of the park toggle; Code is the HTTP status the
// page answers with.
type ToggleError struct {
	Code int
	Msg  string
}

func (e *ToggleError) Error() string { return e.Msg }

func conflict(format string, args ...any) error {
	return &ToggleError{Code: http.StatusConflict, Msg: fmt.Sprintf(format, args...)}
}

// Toggle parks an open or active task ("[ ]" or "[>]" becomes "[~]") or
// unparks a parked one ("[~]" becomes "[ ]") in the PLAN.md in dir. The task
// is the one at line (1-based) whose text is text; hash is the PlanHash of
// the content the caller saw. It refuses (a *ToggleError, 409) when the file
// changed since, when no task with that text is at that line, or when the
// text is ambiguous (two tasks with it). It rewrites exactly the one marker
// byte, atomically; everything else stays byte for byte. It returns the new
// marker.
func Toggle(dir string, line int, text, hash string) (string, error) {
	link := filepath.Join(dir, "PLAN.md")
	path, err := resolveLink(link)
	if err != nil {
		return "", conflict("%v", err)
	}
	if path != link && outside(path, dir) {
		return "", conflict("PLAN.md links to a file outside the project: not edited")
	}
	// one toggle of a file at a time: the hash check and the write must not
	// interleave (two tabs: the second gets the 409)
	mu, _ := toggleLocks.LoadOrStore(strings.ToLower(filepath.Clean(path)), new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", conflict("the project has no PLAN.md")
	case err != nil:
		return "", err
	}
	if hash == "" || PlanHash(b) != hash {
		return "", conflict("PLAN.md changed since the page was loaded: reload the page and try again")
	}
	p, err := Parse(b)
	if err != nil {
		return "", conflict("PLAN.md cannot be read: %v", err)
	}
	var target *Task
	same := 0
	for mi := range p.Milestones {
		for ti := range p.Milestones[mi].Tasks {
			t := &p.Milestones[mi].Tasks[ti]
			if t.Text == text {
				same++
			}
			if t.Line == line {
				target = t
			}
		}
	}
	if target == nil || target.Text != text {
		return "", conflict("no task %q at line %d of PLAN.md: reload the page", Clean(text), line)
	}
	if same > 1 {
		return "", conflict("%d tasks read %q: make the text unique in PLAN.md to toggle one", same, Clean(text))
	}
	var marker string
	switch target.State {
	case Open, Active:
		marker = "~"
	case Parked:
		marker = " "
	default:
		return "", conflict("only open, active or parked tasks can be parked or unparked; %q is %s", Clean(text), target.State)
	}

	// The line's bytes in the file, as Parse saw them.
	start := 0
	for i := 1; i < line; i++ {
		j := bytes.IndexByte(b[start:], '\n')
		if j < 0 {
			return "", fmt.Errorf("PLAN.md line %d: past the end", line)
		}
		start += j + 1
	}
	end := len(b)
	if j := bytes.IndexByte(b[start:], '\n'); j >= 0 {
		end = start + j
	}
	ln := strings.TrimSuffix(string(b[start:end]), "\r")
	if line == 1 && strings.HasPrefix(ln, bom) {
		start += len(bom)
		ln = ln[len(bom):]
	}
	off, size, ok := markerAt(ln)
	if !ok || ln[off:off+size] != target.Marker || size != 1 {
		return "", fmt.Errorf("PLAN.md line %d: the marker is not where the parser saw it", line)
	}
	out := make([]byte, 0, len(b))
	out = append(out, b[:start+off]...)
	out = append(out, marker...)
	out = append(out, b[start+off+size:]...)
	if err := replaceFile(path, out); err != nil {
		return "", err
	}
	return marker, nil
}
