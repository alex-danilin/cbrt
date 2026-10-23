// Package claude is the session-analyzer adapter for Claude Code transcripts
// (~/.claude/projects/<project-slug>/<session-id>.jsonl).
//
// Store layout (observed locally, structure only):
//
//	<root>/<project-slug>/<session-id>.jsonl                       main session
//	<root>/<project-slug>/<session-id>/subagents/agent-<id>.jsonl  one subagent each
//	<root>/<project-slug>/<session-id>/subagents/agent-<id>.meta.json
//
// The main file's name is its sessionId. Subagent records carry the parent's
// sessionId, isSidechain true and their own agentId (equal to the file name's
// <id>). A subagent's events are emitted with NativeSessionID = agentId and
// ParentSessionID = the main session's id, from the main session's Source.
package claude

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// Adapter reads Claude Code sessions. It is stateless.
type Adapter struct{}

func init() { analyze.Register(Adapter{}) }

const (
	adapterID = "claude"
	store     = "default"
)

func (Adapter) ID() string      { return adapterID }
func (Adapter) Version() string { return "3" }

func (Adapter) DiscoveryRoots() []string { return []string{"~/.claude/projects"} }

func (Adapter) Capabilities() analyze.Capabilities {
	return analyze.Capabilities{
		SourceKind:        "jsonl",
		Stores:            []string{store},
		Tokens:            "measured",
		TokenGranularity:  "request",
		CacheWriteOverlap: "disjoint",
		Branch:            "per_event",
		ToolDuration:      "paired",
		PermissionSignal:  "request",
		SessionStartEvent: true,
		HookEvents:        []string{"SessionStart", "SessionEnd", "Stop", "PermissionRequest", "PreCompact", "PostModelSwitch"},
		// install-hooks substitutes {cbrt} with the absolute binary path and
		// appends the native event name, e.g. `"C:\...\cbrt.exe" hook record
		// claude Stop`, one hooks entry per HookEvents element.
		HookTemplate:   `"{cbrt}" hook record claude`,
		CorrelationIDs: []string{"tool_use_id", "message.id"},
	}
}

// Discover returns one Source per <project>/<session>.jsonl under each root,
// with that session's subagent transcripts as further members (sorted by
// name). Order: root, then project directory, then file name.
func (Adapter) Discover(roots []string) ([]analyze.Source, error) {
	var out []analyze.Source
	seen := map[string]bool{}
	for _, root := range roots {
		projects, err := os.ReadDir(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, p := range projects { // ReadDir sorts by name
			if !p.IsDir() {
				continue
			}
			dir := filepath.Join(root, p.Name())
			files, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			for _, f := range files {
				name := f.Name()
				if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
					continue
				}
				id := strings.TrimSuffix(name, ".jsonl")
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				main := filepath.Join(dir, name)
				src := analyze.Source{
					AdapterID: adapterID, Store: store, NativeSessionID: id, Locator: main,
					Members: []analyze.Member{{Path: main}},
				}
				for _, s := range subagentFiles(filepath.Join(dir, id, "subagents")) {
					src.Members = append(src.Members, analyze.Member{Path: s})
				}
				out = append(out, src)
			}
		}
	}
	return out, nil
}

func subagentFiles(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "agent-") && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// subagentID is the agent id in a subagent file name (agent-<id>.jsonl).
func subagentID(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl")
}

// ref is the Event.Ref of a transcript event: the member file and the 1-based
// line of the record. A registry event points at its spool file (Line 0).
type ref struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

// Bodies emits the source's events like Events, with Body set.
func (Adapter) Bodies(src analyze.Source, emit func(analyze.Event) error) error {
	return events(src, true, emit)
}
