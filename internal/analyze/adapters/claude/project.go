package claude

import (
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// ProjectContext lists where Claude Code reads a project's context: its
// instruction files, the project's .claude settings and skills, and the
// auto-memory folder it keeps per project in the store
// (<store root>/<project slug>/memory).
func (Adapter) ProjectContext(root string, storeRoots []string) []analyze.ContextPath {
	out := []analyze.ContextPath{
		{Kind: "instructions", Path: "CLAUDE.md"},
		{Kind: "instructions", Path: ".claude/CLAUDE.md"},
		{Kind: "instructions", Path: "CLAUDE.local.md"},
		{Kind: "settings", Path: ".claude/settings.json"},
		{Kind: "settings", Path: ".claude/settings.local.json"},
		{Kind: "skills", Path: ".claude/skills", Dir: true},
	}
	slug := projectSlug(root)
	for _, r := range storeRoots {
		s := "/"
		if strings.Contains(r, `\`) && !strings.Contains(r, "/") {
			s = `\`
		}
		out = append(out, analyze.ContextPath{Kind: "memory", Path: strings.TrimRight(r, `/\`) + s + slug + s + "memory", Dir: true})
	}
	return out
}

// projectSlug is the store folder name Claude Code uses for a project root:
// every character other than an ASCII letter or digit becomes "-".
func projectSlug(root string) string {
	root = strings.TrimRight(root, `/\`)
	b := []byte(root)
	for i, c := range b {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}
