package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/hook"
)

// hookErrorLogMax bounds registry/errors.log: past it, the log is rotated to
// errors.log.1 (replacing the previous one).
const hookErrorLogMax = 256 << 10

const hookUsage = "usage: cbrt hook record <adapter_id> <event>   (reads the hook's JSON on stdin; always exits 0)\n"

// runHook is `cbrt hook record <adapter_id> <event>`. A hook must never
// block or fail the agent, so once the arguments are right it exits 0 whatever
// happens; failures go to <home>/registry/errors.log only. Wrong arguments are
// a configuration mistake and exit 2 with usage.
func runHook(args []string, stdin io.Reader, stderr io.Writer) (code int) {
	if len(args) != 3 || args[0] != "record" || args[1] == "" || args[2] == "" {
		fmt.Fprint(stderr, hookUsage)
		return 2
	}
	adapterID, event := args[1], args[2]
	home := analyze.Home()
	defer func() {
		if r := recover(); r != nil {
			logHookError(home, adapterID, event, fmt.Errorf("panic: %v", r))
			code = 0
		}
	}()
	if err := hook.Record(home, adapterID, event, stdin, hook.Options{}); err != nil {
		logHookError(home, adapterID, event, err)
	}
	return 0
}

// logHookError appends one line to <home>/registry/errors.log, best effort.
func logHookError(home, adapterID, event string, err error) {
	dir := analyze.RegistryDir(home)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, "errors.log")
	if st, serr := os.Stat(path); serr == nil && st.Size() >= hookErrorLogMax {
		_ = os.Rename(path, path+".1")
	}
	f, ferr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	fmt.Fprintf(f, "%s hook record %s %s: %s\n", time.Now().UTC().Format(time.RFC3339), adapterID, event, msg)
}
