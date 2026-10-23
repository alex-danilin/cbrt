//go:build ignore

// Cross-compiles cbrt for every supported OS/arch into dist/ (no make or bash needed):
//
//	go run scripts/build.go
//
// VERSION in the environment (e.g. VERSION=v0.1.0) is stamped into the binaries
// ("cbrt version"); without it they report "dev".
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

var targets = [][2]string{
	{"windows", "amd64"}, {"windows", "arm64"},
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
}

func main() {
	for _, t := range targets {
		name := fmt.Sprintf("cbrt-%s-%s", t[0], t[1])
		if t[0] == "windows" {
			name += ".exe"
		}
		out := filepath.Join("dist", name)
		ldflags := "-s -w"
		if v := os.Getenv("VERSION"); v != "" {
			ldflags += " -X main.version=" + v
		}
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, "./cmd/cbrt")
		cmd.Env = append(os.Environ(), "GOOS="+t[0], "GOARCH="+t[1], "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "build failed:", name, err)
			os.Exit(1)
		}
		st, _ := os.Stat(out)
		fmt.Printf("%-32s %6.1f MB\n", out, float64(st.Size())/(1<<20))
	}
}
