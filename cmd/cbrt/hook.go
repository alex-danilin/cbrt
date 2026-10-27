package main

import (
	"encoding/json"
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

const hookUsage = "usage: cbrt hook record <adapter_id> <event>   (reads the hook's JSON on stdin; always exits 0)\n" +
	"       cbrt hook warn claude                    (context warning, a UserPromptSubmit hook; always exits 0)\n"

// runHook is `cbrt hook record <adapter_id> <event>` and `cbrt hook warn
// claude`. A hook must never block or fail the agent, so once the arguments
// are right it exits 0 whatever happens; failures go to
// <home>/registry/errors.log only. Wrong arguments are a configuration mistake
// and exit 2 with usage.
func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	if len(args) == 2 && args[0] == "warn" && args[1] == "claude" {
		return runHookWarn(stdin, stdout)
	}
	if len(args) != 3 || args[0] != "record" || args[1] == "" || args[2] == "" {
		fmt.Fprint(stderr, hookUsage)
		return 2
	}
	adapterID, event := args[1], args[2]
	home := analyze.Home()
	defer func() {
		if r := recover(); r != nil {
			logHookError(home, "record "+adapterID+" "+event, fmt.Errorf("panic: %v", r))
			code = 0
		}
	}()
	if err := hook.Record(home, adapterID, event, stdin, hook.Options{}); err != nil {
		logHookError(home, "record "+adapterID+" "+event, err)
	}
	return 0
}

// runHookWarn is `cbrt hook warn claude`: it prints Claude Code's hook JSON
// with a systemMessage (shown to the user, not to the model) when the session
// is large or idle, and nothing otherwise.
func runHookWarn(stdin io.Reader, stdout io.Writer) (code int) {
	home := analyze.Home()
	defer func() {
		if r := recover(); r != nil {
			logHookError(home, "warn claude", fmt.Errorf("panic: %v", r))
			code = 0
		}
	}()
	cfg, err := analyze.LoadConfig(home)
	if err != nil {
		logHookError(home, "warn claude", err)
	}
	msg, err := hook.Warn(home, stdin, hook.WarnOptions{Threshold: cfg.ContextWarnTokens})
	if err != nil {
		logHookError(home, "warn claude", err)
	}
	if msg != "" {
		b, _ := json.Marshal(map[string]string{"systemMessage": msg})
		fmt.Fprintln(stdout, string(b))
	}
	return 0
}

// logHookError appends one line to <home>/registry/errors.log, best effort.
func logHookError(home, command string, err error) {
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
	fmt.Fprintf(f, "%s hook %s: %s\n", time.Now().UTC().Format(time.RFC3339), command, msg)
}
