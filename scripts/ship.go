//go:build ignore

// Ships main: push, wait for CI on all three OSes, then (with a version)
// tag, wait for the release build, and install the Windows binary over the
// one in %LOCALAPPDATA%\Programs\cbrt, restarting any `cbrt projects`
// dashboard it stopped. Run it from PowerShell (gh is logged in there):
//
//	go run scripts/ship.go               # push and wait for CI only
//	go run scripts/ship.go v0.6.3        # ... then release and install
//	go run scripts/ship.go -no-install v0.6.3
//
// A failing CI run prints only its failing test lines. Running processes
// are stopped by PID, and only `cbrt projects` dashboards started from the
// installed binary: never by image name (other agents may run cbrt).
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var (
	noInstall = flag.Bool("no-install", false, "release but do not install the binary locally")
	dryRun    = flag.Bool("dry-run", false, "print the steps without running them")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/ship.go [-no-install] [-dry-run] [vX.Y.Z]")
		flag.PrintDefaults()
	}
	flag.Parse()
	version := flag.Arg(0)
	if flag.NArg() > 1 || (version != "" && !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(version)) {
		flag.Usage()
		os.Exit(2)
	}
	if err := ship(version); err != nil {
		fmt.Fprintln(os.Stderr, "ship:", err)
		os.Exit(1)
	}
}

func ship(version string) error {
	if b := out("git", "rev-parse", "--abbrev-ref", "HEAD"); b != "main" {
		return fmt.Errorf("on branch %q, not main", b)
	}
	if s := out("git", "status", "--porcelain", "--untracked-files=no"); s != "" {
		return errors.New("uncommitted changes in tracked files:\n" + s)
	}
	sha := out("git", "rev-parse", "HEAD")
	if version != "" && out("git", "tag", "-l", version) != "" {
		return fmt.Errorf("tag %s exists already", version)
	}
	step("push main (%s)", sha[:7])
	if err := run("git", "push", "origin", "main"); err != nil {
		return err
	}
	if err := waitRun("ci", sha); err != nil {
		return err
	}
	if version == "" {
		step("done: CI is green; no version given, so no release")
		return nil
	}
	step("tag %s", version)
	if err := run("git", "tag", version, sha); err != nil {
		return err
	}
	if err := run("git", "push", "origin", version); err != nil {
		return err
	}
	if err := waitRun("release", sha); err != nil {
		return err
	}
	if *noInstall {
		step("done: %s released; not installed (-no-install)", version)
		return nil
	}
	return install(version)
}

// waitRun waits for the workflow run of sha to appear and finish, and on
// failure prints only the failing lines of its log.
func waitRun(workflow, sha string) error {
	step("wait for the %s run of %s", workflow, sha[:7])
	if *dryRun {
		return nil
	}
	var id string
	for i := 0; i < 40 && id == ""; i++ {
		id = out("gh", "run", "list", "-c", sha, "-w", workflow, "--json", "databaseId", "--jq", ".[0].databaseId")
		if id == "" {
			time.Sleep(5 * time.Second)
		}
	}
	if id == "" {
		return fmt.Errorf("no %s run for %s appeared", workflow, sha[:7])
	}
	watch := exec.Command("gh", "run", "watch", id, "--exit-status", "--interval", "30")
	if err := watch.Run(); err == nil {
		fmt.Println("  ", out("gh", "run", "view", id, "--json", "conclusion,jobs", "--jq",
			`[.conclusion] + [.jobs[] | (.name + "=" + .conclusion)] | join("  ")`))
		return nil
	}
	fmt.Println("  ", out("gh", "run", "view", id, "--json", "conclusion,jobs", "--jq",
		`[.conclusion] + [.jobs[] | (.name + "=" + .conclusion)] | join("  ")`))
	log, _ := exec.Command("gh", "run", "view", id, "--log-failed").Output()
	for _, l := range failingLines(log, 40) {
		fmt.Println("  ", l)
	}
	return fmt.Errorf("%s run %s failed (gh run view %s --log-failed for the whole log)", workflow, id, id)
}

var failing = regexp.MustCompile(`--- FAIL|_test\.go:\d+|^\S*\s*FAIL\s|panic:|\berror\b`)

// failingLines keeps the lines of a CI log that say what failed, without
// the job/step/timestamp prefix gh adds, at most max of them.
func failingLines(log []byte, max int) []string {
	var keep []string
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if f := strings.SplitN(l, "\t", 3); len(f) == 3 {
			l = f[2]
		}
		if i := strings.Index(l, "Z "); i > 0 && i < 40 {
			l = l[i+2:]
		}
		if failing.MatchString(l) {
			keep = append(keep, strings.TrimSpace(l))
			if len(keep) == max {
				break
			}
		}
	}
	return keep
}

// install puts the released Windows binary in place of the installed one.
func install(version string) error {
	if runtime.GOOS != "windows" {
		step("done: %s released; install is for the Windows machine only", version)
		return nil
	}
	exe := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "cbrt", "cbrt.exe")
	asset := "cbrt-windows-" + runtime.GOARCH + ".exe"
	step("install %s %s over %s", version, asset, exe)
	if *dryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil { // the first install
		return err
	}
	tmp := exe + ".new"
	if err := run("gh", "release", "download", version, "-p", asset, "-O", tmp, "--clobber"); err != nil {
		return err
	}
	sums, err := exec.Command("gh", "release", "download", version, "-p", "SHA256SUMS", "-O", "-").Output()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("SHA256SUMS: %w", err)
	}
	want := sumFor(sums, asset)
	b, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	got := sha256.Sum256(b)
	if want == "" || hex.EncodeToString(got[:]) != want {
		os.Remove(tmp)
		return fmt.Errorf("checksum of %s does not match SHA256SUMS; nothing installed", asset)
	}
	fmt.Println("   checksum ok")
	pids := dashboards(exe)
	for _, pid := range pids {
		fmt.Println("   stop dashboard pid", pid)
		exec.Command("taskkill", "/F", "/PID", pid).Run()
	}
	var renameErr error
	for i := 0; i < 10; i++ { // the stopped process may hold the file briefly
		if renameErr = os.Rename(tmp, exe); renameErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if renameErr != nil {
		return fmt.Errorf("replace %s: %w (still running?)", exe, renameErr)
	}
	fmt.Println("  ", out(exe, "version"))
	if len(pids) > 0 {
		fmt.Println("   restart the dashboard")
		exec.Command("powershell", "-NoProfile", "-Command",
			"Start-Process -FilePath '"+strings.ReplaceAll(exe, "'", "''")+"' -ArgumentList 'projects' -WindowStyle Minimized").Run()
	}
	step("done: %s released and installed; if the review skill changed, run: cbrt analyze install claude --write", version)
	return nil
}

// sumFor finds asset's hash in a SHA256SUMS file ("<hex>  <name>").
func sumFor(sums []byte, asset string) string {
	for _, l := range strings.Split(string(sums), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// dashboards is the PIDs of `cbrt projects` servers started from exe.
func dashboards(exe string) []string {
	ps := `Get-CimInstance Win32_Process -Filter "Name='cbrt.exe'" | ` +
		`Where-Object { $_.ExecutablePath -eq '` + strings.ReplaceAll(exe, "'", "''") + `' -and $_.CommandLine -match '\sprojects(\s+serve)?(\s|$)' } | ` +
		`ForEach-Object { $_.ProcessId }`
	var pids []string
	for _, f := range strings.Fields(out("powershell", "-NoProfile", "-Command", ps)) {
		if regexp.MustCompile(`^\d+$`).MatchString(f) {
			pids = append(pids, f)
		}
	}
	return pids
}

func step(format string, a ...any) {
	prefix := "==>"
	if *dryRun {
		prefix = "==> (dry run)"
	}
	fmt.Println(prefix, fmt.Sprintf(format, a...))
}

func run(name string, args ...string) error {
	if *dryRun {
		fmt.Println("   ", name, strings.Join(args, " "))
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func out(name string, args ...string) string {
	b, _ := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(b))
}
