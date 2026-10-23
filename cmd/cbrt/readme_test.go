package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// README.md's command reference covers every command and flag the built-in
// help names, so the two cannot drift apart.
func TestReadmeCoversHelp(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	flagRe := regexp.MustCompile(`(?:^|[\s\[|(])(--?[a-z][a-z-]*)`)
	for name, help := range map[string]string{"cbrt help": mainUsage, "cbrt analyze help": analyzeUsage, "cbrt projects help": projectsUsage} {
		for _, m := range flagRe.FindAllStringSubmatch(help, -1) {
			if !strings.Contains(readme, m[1]) {
				t.Errorf("%s names %s, README.md does not", name, m[1])
			}
		}
	}
	for _, cmd := range []string{
		"cbrt claude", "cbrt codex", "cbrt init", "cbrt [serve]", "cbrt record", "cbrt statusline", "cbrt healthcheck", "cbrt version",
		"cbrt hook record",
		"cbrt analyze run", "cbrt analyze excerpt", "cbrt analyze rec", "cbrt analyze run-note", "cbrt analyze prune",
		"cbrt analyze install", "cbrt analyze uninstall",
		"cbrt projects [serve]", "cbrt projects connect", "cbrt projects disconnect", "cbrt projects list", "cbrt projects check",
	} {
		if !strings.Contains(readme, "`"+cmd) {
			t.Errorf("README.md has no row for %s", cmd)
		}
	}
	// Every command the help texts list is in the list above.
	cmdRe := regexp.MustCompile(`(?m)^  cbrt (\[?[a-z]+)`)
	for _, m := range cmdRe.FindAllStringSubmatch(mainUsage, -1) {
		if !strings.Contains(readme, "`cbrt "+m[1]) {
			t.Errorf("cbrt help lists cbrt %s, README.md does not", m[1])
		}
	}
	subRe := regexp.MustCompile(`(?m)^  ([a-z][a-z-]+)`)
	for name, help := range map[string]string{"analyze": analyzeUsage, "projects": projectsUsage} {
		for _, m := range subRe.FindAllStringSubmatch(help, -1) {
			if !strings.Contains(readme, "`cbrt "+name+" "+m[1]) {
				t.Errorf("cbrt %s help lists %s, README.md does not", name, m[1])
			}
		}
	}
}
