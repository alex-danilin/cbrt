package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// The context warning (`cbrt hook warn claude`, a UserPromptSubmit hook) tells
// the user, before the prompt is sent, that the session is large or that its
// prompt cache has expired, so a new task is better started in a new session.
const (
	// WarnStep is how much the context must grow past the last warning before
	// the size warning repeats.
	WarnStep = 100_000
	// WarnIdle and WarnIdleTokens: a session idle this long with at least this
	// much context has lost its cache; the next request re-sends all of it.
	WarnIdle       = 60 * time.Minute
	WarnIdleTokens = 100_000
	// warnTail is how much of the transcript's end is read to find the last
	// model response.
	warnTail = 1 << 20
	// warnStateMaxAge: per-session state files older than this are deleted.
	warnStateMaxAge = 30 * 24 * time.Hour
)

// WarnOptions adjusts Warn. Threshold <= 0 means analyze.DefaultContextWarnTokens.
type WarnOptions struct {
	Now       func() time.Time
	Threshold int64
	MaxStdin  int64
}

// warnState is <home>/warn/<session>.json: the context size last warned about.
type warnState struct {
	Warned int64 `json:"warned"`
}

// Warn reads a Claude Code UserPromptSubmit payload from stdin and returns the
// one-line warning to show the user, or "" for none. It reads only the end of
// the session's transcript and the usage numbers of its last main-thread model
// response, never conversation text.
func Warn(home string, stdin io.Reader, opts WarnOptions) (string, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Threshold <= 0 {
		opts.Threshold = analyze.DefaultContextWarnTokens
	}
	if opts.MaxStdin <= 0 {
		opts.MaxStdin = DefaultMaxStdin
	}
	b, err := io.ReadAll(io.LimitReader(stdin, opts.MaxStdin))
	if err != nil {
		return "", err
	}
	var in struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return "", fmt.Errorf("stdin: %w", err)
	}
	if in.TranscriptPath == "" {
		return "", errors.New("stdin: no transcript_path")
	}
	tokens, at, err := lastContext(in.TranscriptPath)
	if err != nil || tokens == 0 {
		return "", err
	}
	statePath := ""
	if safeSessionID(in.SessionID) {
		statePath = filepath.Join(home, "warn", in.SessionID+".json")
	}
	var st warnState
	if statePath != "" {
		if b, err := os.ReadFile(statePath); err == nil {
			_ = json.Unmarshal(b, &st)
		}
	}
	changed := false
	if tokens < st.Warned { // compacted since the last warning
		st.Warned, changed = 0, true
	}
	idle := opts.Now().Sub(at)
	var msg string
	switch {
	case idle >= WarnIdle && tokens >= WarnIdleTokens:
		msg = fmt.Sprintf("cbrt: this session holds %s of context and was idle %s, so its cache has expired and the next request re-sends all of it. "+
			"For a new task, /clear first (or ask for a short handoff note, then /clear).", kTokens(tokens), roundIdle(idle))
	case tokens >= opts.Threshold && (st.Warned == 0 || tokens >= st.Warned+WarnStep):
		msg = fmt.Sprintf("cbrt: this session's context is %s (warning at %s). Every request re-sends all of it. "+
			"For a new task, /clear first; if it needs this context, ask for a short handoff note first, or /compact.", kTokens(tokens), kTokens(opts.Threshold))
	}
	if msg != "" {
		st.Warned, changed = max(st.Warned, tokens), true
	}
	if statePath != "" && changed {
		if err := saveWarnState(statePath, st, opts.Now()); err != nil {
			return msg, err
		}
	}
	return msg, nil
}

// lastContext returns the context size of the next request as the last
// main-thread model response saw it (input, cache reads and writes, and its
// own output), and that response's time. 0 means none was found.
func lastContext(path string) (int64, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, time.Time{}, err
	}
	off := max(fi.Size()-warnTail, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return 0, time.Time{}, err
	}
	lines := bytes.Split(buf, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // starts mid-line
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if !bytes.Contains(lines[i], []byte(`"usage"`)) {
			continue
		}
		var r struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Timestamp   string `json:"timestamp"`
			Message     *struct {
				Model string `json:"model"`
				Usage *struct {
					Input       int64 `json:"input_tokens"`
					CacheCreate int64 `json:"cache_creation_input_tokens"`
					CacheRead   int64 `json:"cache_read_input_tokens"`
					Output      int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(lines[i], &r) != nil || r.Type != "assistant" || r.IsSidechain ||
			r.Message == nil || r.Message.Usage == nil || r.Message.Model == "<synthetic>" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			continue
		}
		u := r.Message.Usage
		return u.Input + u.CacheCreate + u.CacheRead + u.Output, at, nil
	}
	return 0, time.Time{}, nil
}

// safeSessionID reports whether id can name a state file.
func safeSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// saveWarnState writes one session's state and deletes stale ones.
func saveWarnState(path string, st warnState, now time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	if es, err := os.ReadDir(dir); err == nil {
		for _, e := range es {
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > warnStateMaxAge {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return nil
}

// kTokens formats a token count as "430k".
func kTokens(n int64) string {
	return strconv.FormatInt((n+500)/1000, 10) + "k"
}

// roundIdle formats an idle span as "3h" or "75 min".
func roundIdle(d time.Duration) string {
	if d >= 2*time.Hour {
		return strconv.FormatInt(int64(d.Round(time.Hour)/time.Hour), 10) + "h"
	}
	return strconv.FormatInt(int64(d/time.Minute), 10) + " min"
}
