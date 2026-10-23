package antigravity

import "github.com/alex-danilin/cbrt/internal/analyze"

// ProjectContext lists where Antigravity reads workspace context: GEMINI.md
// and the workspace's rule and workflow folders.
func (Adapter) ProjectContext(string, []string) []analyze.ContextPath {
	return []analyze.ContextPath{
		{Kind: "instructions", Path: "GEMINI.md"},
		{Kind: "instructions", Path: ".agent/rules", Dir: true},
		{Kind: "skills", Path: ".agent/workflows", Dir: true},
	}
}
