package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// step is one transcript line.
type step struct {
	Index     *int       `json:"step_index"`
	Source    string     `json:"source"`
	Type      string     `json:"type"`
	Status    string     `json:"status"`
	CreatedAt string     `json:"created_at"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking"`
	ToolCalls []toolCall `json:"tool_calls"`
	ExitCode  *int       `json:"exit_code"`
	Error     string     `json:"error"`

	// Usage on newer IDE planner steps (top-level fields, raw as written).
	InputTokens     json.RawMessage `json:"input_tokens"`
	CacheReadTokens json.RawMessage `json:"cache_read_tokens"`
	OutputTokens    json.RawMessage `json:"output_tokens"`

	ref ref // where the record starts
}

type toolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ref is Event.Ref: a 1-based line of one member file. A record split across
// two chunk files points at the line where it starts.
type ref struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// readStats counts what readSteps saw, for the local checks.
type readStats struct {
	Records   int // parsed records, before dedupe
	Dups      int // records dropped because a later copy of the step exists
	Malformed int
	Partial   int // trailing records without a final newline (still being written)
}

// readSteps reads a source's members and returns its steps, deduplicated by
// step_index (the later copy wins: the live file after the chunks) and sorted
// by step_index. Consecutive chunk files are read as one byte stream, because
// a record can straddle two chunks; every other member is a stream of its own.
func readSteps(src analyze.Source) ([]step, readStats, error) {
	var st readStats
	byIdx := map[int]step{}
	add := func(rec []byte, r ref) {
		rec = bytes.TrimSpace(rec)
		if len(rec) == 0 {
			return
		}
		var s step
		if err := json.Unmarshal(rec, &s); err != nil || s.Index == nil {
			st.Malformed++
			return
		}
		st.Records++
		s.ref = r
		if _, dup := byIdx[*s.Index]; dup {
			st.Dups++
		}
		byIdx[*s.Index] = s
	}

	var stream []string
	flush := func() error {
		if len(stream) == 0 {
			return nil
		}
		err := readStream(stream, add, &st)
		stream = nil
		return err
	}
	for _, m := range src.Members {
		stream = append(stream, m.Path)
		if !isChunk(m.Path) {
			if err := flush(); err != nil {
				return nil, st, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, st, err
	}

	steps := make([]step, 0, len(byIdx))
	for _, s := range byIdx {
		steps = append(steps, s)
	}
	sort.Slice(steps, func(i, j int) bool { return *steps[i].Index < *steps[j].Index })
	if len(steps) == 0 && st.Malformed > 0 {
		return nil, st, fmt.Errorf("no usable step (%d malformed records)", st.Malformed)
	}
	return steps, st, nil
}

// readStream splits the concatenation of files into newline-terminated
// records and calls add for each with the position where it starts. The
// unterminated tail of the last file is a record still being written and is
// skipped. A file that has disappeared since discovery is skipped.
func readStream(files []string, add func([]byte, ref), st *readStats) error {
	var pending []byte
	var pendingRef ref
	have := false
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		line, start := 1, 0
		for {
			j := bytes.IndexByte(data[start:], '\n')
			if j < 0 {
				break
			}
			seg := data[start : start+j]
			if have {
				add(append(pending, seg...), pendingRef)
				pending, have = nil, false
			} else {
				add(seg, ref{Path: f, Line: line})
			}
			start += j + 1
			line++
		}
		if start < len(data) {
			if !have {
				pendingRef, have = ref{Path: f, Line: line}, true
			}
			pending = append(pending, data[start:]...)
		}
	}
	if have && len(bytes.TrimSpace(pending)) > 0 {
		st.Partial++
	}
	return nil
}
