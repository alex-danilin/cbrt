package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The usage store assumes it is the only writer of its data directory, but
// several launchers (e.g. two terminals running `cbrt claude`) share one. The
// first claims it with a lock directory (creating a directory is atomic on
// every OS) that holds its PID; a later launcher, while that process is alive,
// keeps its usage in a temporary session directory instead. A lock whose
// process is gone (a crash) is taken over.

const lockName = ".cbrt-lock"

// claimDataDir returns the data directory to use, a release func, and a note to
// show the user when the shared directory was busy.
func claimDataDir(base, dir string) (use string, release func(), note string, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, "", err
	}
	lock := filepath.Join(dir, lockName)
	for attempt := 0; attempt < 2; attempt++ {
		if err := os.Mkdir(lock, 0o700); err == nil {
			pidFile := filepath.Join(lock, "pid")
			if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				os.RemoveAll(lock)
				return "", nil, "", err
			}
			return dir, func() { os.RemoveAll(lock) }, "", nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", nil, "", err
		}
		if pid, ok := lockOwner(lock); ok && processAlive(pid) {
			break
		}
		os.RemoveAll(lock) // stale: its launcher is gone
	}
	tmp, err := os.MkdirTemp(base, "session-")
	if err != nil {
		return "", nil, "", err
	}
	note = fmt.Sprintf("another cbrt launcher is using %s; this session's usage is kept only until it ends", dir)
	return tmp, func() { os.RemoveAll(tmp) }, note, nil
}

// lockOwner reads the PID in a lock directory. A lock just created by another
// launcher may not have its PID yet; that counts as owned (pid 0) unless the
// lock is older than a few seconds (its creator crashed before writing it).
func lockOwner(lock string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(lock, "pid"))
	if err != nil {
		st, serr := os.Stat(lock)
		return 0, errors.Is(err, os.ErrNotExist) && serr == nil && time.Since(st.ModTime()) < 10*time.Second
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil
}

// processAlive reports whether pid runs. pid 0 stands for "being created".
func processAlive(pid int) bool {
	if pid == 0 {
		return true
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false // on Windows FindProcess opens the process and fails if it's gone
	}
	defer p.Release()
	if runtime.GOOS == "windows" {
		return true
	}
	return p.Signal(syscall.Signal(0)) == nil
}
