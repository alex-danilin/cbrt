package report

import (
	"fmt"
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze/projects"
)

// openable is path with only the home folder turned into "~" when that is
// all redaction changes; otherwise the fully redacted path (safe, though it
// may not open).
func openable(path string, red *Redactor) string {
	if p, ok := red.ApplyPath(path); ok {
		return p
	}
	return red.Apply(path)
}

// redactProject redacts every path and name of p. Paths only get the home
// folder turned into "~", so the reviewer can still open them; a tool path
// that would need more redaction (the user name, a home-folder slug) is
// withheld and only its counts are kept.
func redactProject(p projects.Project, red *Redactor) projects.Project {
	p.Project = projectName(p.Project, red)
	p.Root = openable(p.Root, red)
	p.Reason = red.Apply(p.Reason)
	rf := func(fs []projects.File) []projects.File {
		out := make([]projects.File, len(fs))
		for i, f := range fs {
			out[i] = projects.File{Path: openable(f.Path, red), Bytes: f.Bytes}
		}
		return out
	}
	rs := func(ss []string) []string {
		if ss == nil {
			return nil
		}
		out := make([]string, len(ss))
		for i, s := range ss {
			out[i] = red.Apply(s)
		}
		return out
	}
	p.Instructions, p.Readme, p.Docs = rf(p.Instructions), rf(p.Readme), rs(p.Docs)
	bs := make([]projects.Build, len(p.Build))
	for i, b := range p.Build {
		bs[i] = projects.Build{File: b.File, Names: rs(b.Names), More: b.More}
	}
	p.Build = bs
	ts := make([]projects.ToolContext, len(p.ToolContext))
	for i, t := range p.ToolContext {
		if path, ok := red.ApplyPath(t.Path); ok {
			t.Path = path
		} else {
			t.Path, t.PathWithheld = "", true // only its counts are reported
		}
		t.Names = rs(t.Names)
		ts[i] = t
	}
	p.ToolContext = ts
	if len(p.Instructions) == 0 {
		p.Instructions = nil
	}
	if len(p.Readme) == 0 {
		p.Readme = nil
	}
	if len(p.Build) == 0 {
		p.Build = nil
	}
	if len(p.ToolContext) == 0 {
		p.ToolContext = nil
	}
	return p
}

// renderProjects is summary.md's "Project context" section.
func renderProjects(b *strings.Builder, in Input, red *Redactor) {
	p := func(format string, a ...any) { fmt.Fprintf(b, format, a...) }
	p("## Project context\n\n")
	if len(in.Projects) == 0 {
		p("No projects in the window.\n\n")
		return
	}
	p("What exists in each project's folder, from projects.jsonl (names and sizes only). Read a project's instruction files, README and the build entry points the evidence involves before recommending anything for it.\n\n")
	p("| Project | Sessions | Status | Instruction files | Build entry points |\n|---|---|---|---|---|\n")
	for _, raw := range in.Projects {
		pr := redactProject(raw, red)
		status := pr.Status
		if pr.Status != projects.Known {
			status += ": " + pr.Reason
		} else if pr.Git {
			status += " (git)"
		}
		var instr []string
		for _, f := range pr.Instructions {
			instr = append(instr, f.Path)
		}
		for _, t := range pr.ToolContext {
			switch {
			case t.Kind != "instructions":
			case t.Files > 0:
				instr = append(instr, fmt.Sprintf("%s (%d files)", t.Path, t.Files))
			default:
				instr = append(instr, t.Path)
			}
		}
		var build []string
		for _, bd := range pr.Build {
			n := len(bd.Names) + bd.More
			switch {
			case bd.File == "package.json":
				build = append(build, fmt.Sprintf("package.json (%d scripts)", n))
			case n > 0:
				build = append(build, fmt.Sprintf("%s (%d targets)", bd.File, n))
			default:
				build = append(build, bd.File)
			}
		}
		p("| %s | %d | %s | %s | %s |\n", oneLine(pr.Project), pr.Sessions, oneLine(status), cell(instr), cell(build))
	}
	if in.ExcludedProjects > 0 {
		p("\n%d excluded projects are not inventoried.\n", in.ExcludedProjects)
	}
	p("\n")
}

func cell(ss []string) string {
	if len(ss) == 0 {
		return "none"
	}
	return oneLine(strings.Join(ss, ", "))
}
