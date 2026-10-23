// Package codex is the session-analyzer adapter for Codex CLI rollouts
// (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl) and Codex hook envelopes.
//
// Format notes (Codex CLI 0.118–0.161, see docs/internal architecture §2.2):
//
//   - Every line is {"timestamp", "type", "payload"} with an optional
//     "ordinal". Types: session_meta (first line), turn_context, response_item,
//     event_msg, compacted, world_state, token_usage_record.
//   - A turn runs from event_msg task_started to task_complete (or
//     turn_aborted). turn_context carries turn_id, model and effort.
//   - event_msg token_count is written after every model response:
//     info.last_token_usage is THAT response's usage and info.total_token_usage
//     the session's running total. Some are repeats of the previous one (the
//     running total does not move); info is null before a turn's first
//     response. Codex 0.153+ also writes token_usage_record right before each
//     response's token_count: response_id, usage (= last_token_usage),
//     turn_token_usage and thread_token_usage (= total_token_usage).
//   - One model_request per model response: see parser.tokenCount for the
//     dedup and the per-turn fallback.
package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// ID is the adapter ID.
const ID = "codex"

const store = "default"

// Adapter reads Codex CLI rollouts. It is stateless.
type Adapter struct{}

func init() { analyze.Register(Adapter{}) }

func (Adapter) ID() string { return ID }

// Version changes whenever parsing or normalization changes.
func (Adapter) Version() string { return "3" }

func (Adapter) DiscoveryRoots() []string { return []string{"~/.codex/sessions"} }

// Capabilities: TokenGranularity is "request" because each model response
// gets its own model_request. A turn whose responses cannot be separated
// (see parser.tokenCount) falls back to one model_request for the whole turn;
// that coarser event is recognizable by its RequestID, which is the bare
// turn_id, where a per-response one is the response_id (then also its
// CorrelationID, the answer id) or "<turn_id>#<n>" (no CorrelationID).
func (Adapter) Capabilities() analyze.Capabilities {
	return analyze.Capabilities{
		SourceKind:        "jsonl",
		Stores:            []string{store},
		Tokens:            "measured",
		TokenGranularity:  "request",
		CacheWriteOverlap: "unknown",
		Branch:            "at_start",
		ToolDuration:      "paired",
		PermissionSignal:  "request",
		SessionStartEvent: true,
		HookEvents:        []string{"SessionStart", "SessionEnd", "Stop", "PermissionRequest", "Interrupt", "PreCompact"},
		// The command Codex runs for each registered hook (hooks.json "command",
		// and "commandWindows" on Windows). install-hooks appends the native event
		// name as the last argument, e.g. `"…/cbrt" hook record codex Stop`.
		HookTemplate:   `"{cbrt}" hook record codex`,
		CorrelationIDs: []string{"call_id", "response_id"},
	}
}

// Discover returns one source per rollout file, ordered by path.
func (Adapter) Discover(roots []string) ([]analyze.Source, error) {
	var paths []string
	for _, root := range roots {
		root = analyze.ExpandRoot(root)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == root && errors.Is(err, fs.ErrNotExist) {
					return fs.SkipAll
				}
				return err
			}
			name := d.Name()
			if !d.IsDir() && strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
				paths = append(paths, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	seen := map[string]bool{}
	var out []analyze.Source
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		id := sessionID(p)
		if seen["id:"+id] { // a resumed or copied rollout with the same id
			id += "#" + filepath.Base(p)
		}
		seen["id:"+id] = true
		out = append(out, analyze.Source{
			AdapterID:       ID,
			Store:           store,
			NativeSessionID: id,
			Locator:         p,
			Members:         []analyze.Member{{Path: p}},
		})
	}
	return out, nil
}

// sessionID reads session_meta.payload.id from the first line; the file name
// (without "rollout-" and ".jsonl") is the fallback.
func sessionID(path string) string {
	fallback := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "rollout-"), ".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	if err != nil {
		return fallback // empty file, or only a partial line
	}
	var r struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "session_meta" || r.Payload.ID == "" {
		return fallback
	}
	return r.Payload.ID
}

type ref struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// hookKinds maps the registered native hook events to normalized kinds. Stop
// fires when the agent ends its turn and hands control back: the closest kind
// is task_claimed_complete (the agent says it is done), as task_complete is in
// the transcript.
var hookKinds = map[string]analyze.Kind{
	"SessionStart":      analyze.KindSessionStart,
	"SessionEnd":        analyze.KindSessionEnd,
	"Stop":              analyze.KindTurnEnd,
	"PermissionRequest": analyze.KindPermissionReq,
	"Interrupt":         analyze.KindInterrupt,
	"PreCompact":        analyze.KindCompaction,
}

// NormalizeHook maps one spooled Codex hook invocation to a registry event.
func (Adapter) NormalizeHook(env analyze.Envelope) ([]analyze.Event, error) {
	kind, ok := hookKinds[env.Event]
	if !ok {
		return nil, nil
	}
	var in struct {
		SessionID string `json:"session_id"`
		TurnID    string `json:"turn_id"`
		Model     string `json:"model"`
	}
	if err := json.Unmarshal([]byte(env.Stdin), &in); err != nil {
		return nil, fmt.Errorf("codex hook %s: stdin is not JSON: %w", env.Event, err)
	}
	if in.SessionID == "" {
		return nil, fmt.Errorf("codex hook %s: no session_id", env.Event)
	}
	if env.RecordedAt.IsZero() {
		return nil, fmt.Errorf("codex hook %s: no recorded_at", env.Event)
	}
	e := analyze.Event{
		Tool:             ID,
		Store:            store,
		Origin:           analyze.OriginUnknown,
		NativeSessionID:  in.SessionID,
		Seq:              "registry:" + filepath.Base(env.File),
		TS:               env.RecordedAt.UTC(),
		Kind:             kind,
		ObservedBy:       analyze.FromRegistry,
		Model:            in.Model,
		CorrelationID:    in.TurnID,
		BranchCoverage:   analyze.Unknown,
		DurationCoverage: analyze.Unknown,
	}
	if env.Git != nil && (env.Git.Branch != "" || env.Git.Commit != "") {
		e.Branch, e.Commit, e.BranchCoverage = env.Git.Branch, env.Git.Commit, analyze.Measured
	}
	return []analyze.Event{e}, nil
}
