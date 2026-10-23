package analyze

import (
	"os"
	"path/filepath"
)

// HomeEnv overrides where the analyzer keeps its configuration and state.
const HomeEnv = "CBRT_ANALYZE_HOME"

// Home is $CBRT_ANALYZE_HOME, else <UserConfigDir>/cbrt/analyze, else
// ~/.cbrt-analyze when the platform has no config directory.
func Home() string {
	if h := os.Getenv(HomeEnv); h != "" {
		return filepath.Clean(h)
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "cbrt", "analyze")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cbrt-analyze")
	}
	return ".cbrt-analyze"
}

// SpoolDir is where the hook recorder writes one file per invocation.
func SpoolDir(home string) string { return filepath.Join(home, "registry", "spool") }

// RegistryDir holds the spool and the compacted months (registry/YYYY-MM.jsonl).
func RegistryDir(home string) string { return filepath.Join(home, "registry") }
