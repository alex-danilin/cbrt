package projects

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake agent CLI: with CBRT_FAKE_AGENT set
// it records its argv, stdin and working folder in CBRT_FAKE_LOG and
// answers (or hangs) instead of running the tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("CBRT_FAKE_AGENT"); mode != "" {
		fakeAgent(mode)
		return
	}
	os.Exit(m.Run())
}

type fakeCall struct {
	Args  []string
	Stdin string
	Dir   string
}

func fakeAgent(mode string) {
	args := os.Args[1:]
	var stdin []byte
	if len(args) == 0 || !strings.HasPrefix(args[0], "--print=") { // agy reads no stdin
		stdin, _ = io.ReadAll(os.Stdin)
	}
	wd, _ := os.Getwd()
	b, _ := json.Marshal(fakeCall{Args: args, Stdin: string(stdin), Dir: wd})
	os.WriteFile(os.Getenv("CBRT_FAKE_LOG"), b, 0o644)
	switch mode {
	case "sleep":
		time.Sleep(10 * time.Minute)
	case "tree": // start a grandchild that would outlive this process
		exe, _ := os.Executable()
		gc := exec.Command(exe)
		gc.Env = append(os.Environ(), "CBRT_FAKE_AGENT=beat", "CBRT_FAKE_LOG="+os.Getenv("CBRT_FAKE_LOG")+".beatlog")
		gc.Start()
		time.Sleep(10 * time.Minute)
	case "beat": // the grandchild: a heartbeat file while it lives (2 minutes at most)
		for i := 0; i < 2400; i++ {
			os.WriteFile(os.Getenv("CBRT_FAKE_BEAT"), []byte(strconv.Itoa(i)), 0o644)
			time.Sleep(50 * time.Millisecond)
		}
		os.Exit(0)
	case "fail":
		os.Stderr.WriteString("model unavailable\n")
		os.Exit(3)
	}
	answer := os.Getenv("CBRT_FAKE_ANSWER")
	for i, a := range args {
		if a == "--output-last-message" { // codex: the answer goes to the file
			os.Stdout.WriteString("codex transcript noise\n")
			os.WriteFile(args[i+1], []byte(answer), 0o644)
			os.Exit(0)
		}
	}
	os.Stdout.WriteString(answer)
	os.Exit(0)
}

// useFakeAgent points lookPath at the test binary in mode.
func useFakeAgent(t *testing.T, mode, answer string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "call.json")
	t.Setenv("CBRT_FAKE_AGENT", mode)
	t.Setenv("CBRT_FAKE_LOG", log)
	t.Setenv("CBRT_FAKE_ANSWER", answer)
	old := lookPath
	lookPath = func(string) (string, error) { return exe, nil }
	t.Cleanup(func() { lookPath = old })
	return log
}

func TestNewCommandArgv(t *testing.T) {
	dir, tmp := filepath.FromSlash("/w/ink well"), filepath.FromSlash("/t/run")
	for _, c := range []struct {
		agent string
		args  []string
		stdin bool
	}{
		{"claude", []string{"-p", "--output-format", "text", "--permission-mode", "plan", "--tools", "Read,Grep,Glob", "--strict-mcp-config"}, true},
		{"codex", []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "-c", "mcp_servers={}", "--output-last-message", filepath.Join(tmp, "answer.md"), "-"}, true},
		{"agy", []string{"--print=check it", "--mode", "plan", "--sandbox", "--add-dir", dir, "--output-format", "text", "--print-timeout", "9m"}, false},
	} {
		cmd, err := newCommand(c.agent, dir, "check it", 10*time.Minute, tmp)
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Name != c.agent || cmd.Dir != dir || !reflect.DeepEqual(cmd.Args, c.args) {
			t.Errorf("%s: %s %q in %s", c.agent, cmd.Name, cmd.Args, cmd.Dir)
		}
		if (cmd.Stdin == "check it") != c.stdin {
			t.Errorf("%s: stdin %q", c.agent, cmd.Stdin)
		}
		for _, a := range cmd.Args {
			if strings.Contains(a, "dangerously") || strings.Contains(a, "bypass") || a == "--full-auto" || a == "--yolo" {
				t.Errorf("%s: permission-skipping flag %s", c.agent, a)
			}
		}
	}
	if _, err := newCommand("gemini", dir, "x", time.Minute, tmp); err == nil {
		t.Errorf("unknown agent accepted")
	}
	// agy: a prompt too long for a command line goes through a file
	long := strings.Repeat("p", agyArgMax+1)
	cmd, _ := newCommand("agy", dir, long, 10*time.Minute, tmp)
	if strings.Contains(cmd.Args[0], long) || !strings.Contains(cmd.Args[0], filepath.Join(tmp, "prompt.md")) ||
		cmd.promptFile != filepath.Join(tmp, "prompt.md") || cmd.prompt != long ||
		!reflect.DeepEqual(cmd.Args[1:8], []string{"--mode", "plan", "--sandbox", "--add-dir", dir, "--add-dir", tmp}) {
		t.Errorf("long agy prompt: %q", cmd.Args[1:])
	}
	for d, want := range map[time.Duration]string{10 * time.Minute: "9m", 3 * time.Minute: "2m", 100 * time.Second: "1m30s"} {
		if got := agyTimeout(d); got != want {
			t.Errorf("agyTimeout(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestFakeAgentsExactArgv runs each agent's real command line against the
// fake executable and checks what the process received.
func TestFakeAgentsExactArgv(t *testing.T) {
	dir := t.TempDir()
	for _, agent := range Agents {
		log := useFakeAgent(t, "answer", "Verdict: on track\nfine\n")
		out, err := runAgent(context.Background(), nil, agent, dir, "check it", 10*time.Minute)
		if err != nil {
			t.Fatalf("%s: %v", agent, err)
		}
		if out != "Verdict: on track\nfine" {
			t.Errorf("%s: answer %q", agent, out)
		}
		var call fakeCall
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(b, &call)
		var want []string
		switch agent {
		case "claude":
			want = []string{"-p", "--output-format", "text", "--permission-mode", "plan", "--tools", "Read,Grep,Glob", "--strict-mcp-config"}
		case "codex":
			// the answer file is in the run's own temp folder, outside the project
			if len(call.Args) != 9 || !strings.Contains(call.Args[7], "cbrt-agent-") || strings.HasPrefix(call.Args[7], dir) {
				t.Fatalf("codex argv %q", call.Args)
			}
			want = []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "-c", "mcp_servers={}", "--output-last-message", call.Args[7], "-"}
			if _, err := os.Stat(call.Args[7]); err == nil {
				t.Errorf("codex answer file left behind")
			}
		case "agy":
			want = []string{"--print=check it", "--mode", "plan", "--sandbox", "--add-dir", dir, "--output-format", "text", "--print-timeout", "9m"}
		}
		if !reflect.DeepEqual(call.Args, want) {
			t.Errorf("%s argv\n%q\nwant\n%q", agent, call.Args, want)
		}
		if wantIn := agent != "agy"; (call.Stdin == "check it") != wantIn {
			t.Errorf("%s stdin %q", agent, call.Stdin)
		}
		if !sameDir(call.Dir, dir) {
			t.Errorf("%s ran in %s", agent, call.Dir)
		}
	}
}

func TestRunAgentFailures(t *testing.T) {
	dir := t.TempDir()
	useFakeAgent(t, "sleep", "")
	_, err := runAgent(context.Background(), nil, "claude", dir, "x", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not answer within 300ms") {
		t.Errorf("timeout: %v", err)
	}
	useFakeAgent(t, "fail", "")
	if _, err := runAgent(context.Background(), nil, "codex", dir, "x", time.Minute); err == nil || !strings.Contains(err.Error(), "model unavailable") {
		t.Errorf("failure: %v", err)
	}
	useFakeAgent(t, "answer", "  \n")
	if _, err := runAgent(context.Background(), nil, "agy", dir, "x", time.Minute); err == nil || !strings.Contains(err.Error(), "empty answer") {
		t.Errorf("empty: %v", err)
	}
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	if _, err := runAgent(context.Background(), nil, "agy", dir, "x", time.Minute); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("missing: %v", err)
	}
}

// TestRunAgentTimeoutKillsTree: a timeout also stops what the agent
// started (codex.cmd -> cmd.exe -> node).
func TestRunAgentTimeoutKillsTree(t *testing.T) {
	useFakeAgent(t, "tree", "")
	beat := filepath.Join(t.TempDir(), "beat")
	t.Setenv("CBRT_FAKE_BEAT", beat)
	_, err := runAgent(context.Background(), nil, "claude", t.TempDir(), "x", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("timeout: %v", err)
	}
	if _, err := os.Stat(beat); err != nil {
		t.Fatalf("the grandchild never started: %v", err)
	}
	// the heartbeat stops once the grandchild is dead
	for range 60 {
		a, _ := os.ReadFile(beat)
		time.Sleep(500 * time.Millisecond)
		b, _ := os.ReadFile(beat)
		if string(a) == string(b) {
			return
		}
	}
	t.Errorf("the grandchild outlived the timeout")
}

func TestExecRunRefusesBatchMetacharacters(t *testing.T) {
	bat := filepath.Join(t.TempDir(), "codex.cmd")
	old := lookPath
	lookPath = func(string) (string, error) { return bat, nil }
	t.Cleanup(func() { lookPath = old })
	for _, a := range []string{`C:\a&b\answer.md`, "100%", "x|y", "a^b", `"q"`} {
		_, err := ExecRun(context.Background(), Command{Name: "codex", Args: []string{"exec", a}})
		if err == nil || !strings.Contains(err.Error(), "is a batch file") {
			t.Errorf("%q: %v", a, err)
		}
	}
}

// TestRunAgentLongAgyPrompt: the prompt file exists while agy runs and is
// removed after.
func TestRunAgentLongAgyPrompt(t *testing.T) {
	long := strings.Repeat("plan ", agyArgMax/4)
	var seen string
	run := func(ctx context.Context, c Command) (string, error) {
		b, err := os.ReadFile(c.promptFile)
		seen = string(b)
		return "ok", err
	}
	var file string
	wrapped := func(ctx context.Context, c Command) (string, error) { file = c.promptFile; return run(ctx, c) }
	if _, err := runAgent(context.Background(), wrapped, "agy", t.TempDir(), long, time.Minute); err != nil {
		t.Fatal(err)
	}
	if seen != long {
		t.Errorf("prompt file held %d bytes", len(seen))
	}
	if _, err := os.Stat(file); err == nil {
		t.Errorf("prompt file left behind")
	}
}
