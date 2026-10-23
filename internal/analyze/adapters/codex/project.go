package codex

import "github.com/alex-danilin/cbrt/internal/analyze"

// ProjectContext lists Codex's own instruction file name (AGENTS.md itself
// is checked for every project by the core).
func (Adapter) ProjectContext(string, []string) []analyze.ContextPath {
	return []analyze.ContextPath{{Kind: "instructions", Path: "AGENTS.override.md"}}
}
