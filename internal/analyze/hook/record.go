// Package hook is the analyzer's hook recorder (`cbrt hook record`) and the
// reader and compactor of the spool it writes (architecture doc §3.1).
//
// Every hook invocation becomes one file, registry/spool/<random>.json, written
// to a .tmp name, fsynced and renamed, so concurrent sessions never interleave
// and an interrupted write leaves only a .tmp that ReadSpool reports. Spool
// files of finished months are compacted into registry/YYYY-MM.jsonl.
package hook

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// GitBudget is the one deadline shared by both git calls of a recording.
const GitBudget = 2 * time.Second

// DefaultMaxStdin is the stdin cap when Options.MaxStdin is zero.
const DefaultMaxStdin = 4 << 20

// eventNoGit is the hook event that never runs git: it fires once per prompt
// shown to the user and carries no boundary information.
const eventNoGit = "PermissionRequest"

// Options adjusts Record. The zero value uses time.Now, the real git runner
// and a 4 MiB stdin cap.
type Options struct {
	Now func() time.Time
	// Git returns the branch and commit of dir. It is called with a context
	// whose deadline is GitBudget away; Record stops waiting for it at that
	// deadline even if Git ignores the context.
	Git      func(ctx context.Context, dir string) (*analyze.GitInfo, error)
	MaxStdin int64
}

// Record spools one hook invocation of adapterID's native event. stdin is read
// up to the cap (the rest is not read). Errors are returned; the hook command
// logs and ignores them.
func Record(home, adapterID, event string, stdin io.Reader, opts Options) error {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Git == nil {
		opts.Git = RunGit
	}
	if opts.MaxStdin <= 0 {
		opts.MaxStdin = DefaultMaxStdin
	}
	recordedAt := opts.Now().UTC()
	var raw []byte
	if stdin != nil {
		b, err := io.ReadAll(io.LimitReader(stdin, opts.MaxStdin))
		if err != nil && len(b) == 0 {
			return fmt.Errorf("reading stdin: %w", err)
		}
		raw = b
	}
	env := analyze.Envelope{
		V:          analyze.EnvelopeVersion,
		AdapterID:  adapterID,
		Event:      event,
		RecordedAt: recordedAt,
		Stdin:      strings.ToValidUTF8(string(raw), "�"),
	}
	if event != eventNoGit {
		if dir := workDir(raw); dir != "" {
			env.Git = gitInfo(opts.Git, dir)
		}
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeSpool(analyze.SpoolDir(home), data)
}

// workDir is the session's working directory from the hook's stdin JSON:
// "cwd", else the first of "workspacePaths". "" when there is none.
func workDir(raw []byte) string {
	var in struct {
		Cwd            any `json:"cwd"`
		WorkspacePaths any `json:"workspacePaths"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	if s, ok := in.Cwd.(string); ok && s != "" {
		return s
	}
	if ps, ok := in.WorkspacePaths.([]any); ok && len(ps) > 0 {
		s, _ := ps[0].(string)
		return s
	}
	return ""
}

// gitInfo runs git under one GitBudget deadline and returns nil on any
// failure, or when the deadline passes first.
func gitInfo(run func(context.Context, string) (*analyze.GitInfo, error), dir string) *analyze.GitInfo {
	ctx, cancel := context.WithTimeout(context.Background(), GitBudget)
	defer cancel()
	type result struct {
		g   *analyze.GitInfo
		err error
	}
	ch := make(chan result, 1)
	go func() {
		g, err := run(ctx, dir)
		ch <- result{g, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || r.g == nil || ctx.Err() != nil {
			return nil
		}
		return r.g
	case <-ctx.Done():
		return nil
	}
}

// RunGit is the real git runner: `git -C dir rev-parse --abbrev-ref HEAD` and
// `git -C dir rev-parse HEAD`, both under ctx.
func RunGit(ctx context.Context, dir string) (*analyze.GitInfo, error) {
	branch, err := gitOut(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	commit, err := gitOut(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return &analyze.GitInfo{Branch: branch, Commit: commit}, nil
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.WaitDelay = 100 * time.Millisecond // don't wait on pipes a killed git left open
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	s := string(bytes.TrimSpace(out))
	if s == "" {
		return "", errors.New("git printed nothing")
	}
	return s, nil
}

// writeSpool writes data to dir/<random>.json atomically: a .tmp file,
// fsync, rename.
func writeSpool(dir string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	final := filepath.Join(dir, hex.EncodeToString(rnd[:])+".json")
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
