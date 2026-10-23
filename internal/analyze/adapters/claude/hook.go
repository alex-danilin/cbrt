package claude

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// hookInput is the part of Claude Code's hook stdin the adapter reads.
type hookInput struct {
	SessionID string          `json:"session_id"`
	Model     json.RawMessage `json:"model"` // a string, or an object with an id
	Effort    string          `json:"effort"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id"`
}

// hookKinds maps the registered hook events to kinds. Stop fires when the
// agent ends its turn and hands control back: there is no turn-end kind, and
// the closest one is task_claimed_complete (the agent considers the requested
// work done for now), which is also what Codex's turn-closing event maps to.
var hookKinds = map[string]analyze.Kind{
	"SessionStart":      analyze.KindSessionStart,
	"SessionEnd":        analyze.KindSessionEnd,
	"Stop":              analyze.KindTurnEnd,
	"PermissionRequest": analyze.KindPermissionReq,
	"PreCompact":        analyze.KindCompaction,
	"PostModelSwitch":   analyze.KindModelSwitch,
}

// NormalizeHook maps one spooled hook envelope to a registry event. Hooks
// carry no entrypoint, so Origin is unknown. A PermissionRequest names the
// tool (normalized and raw) and hashes its arguments, for allowlist analysis.
func (Adapter) NormalizeHook(env analyze.Envelope) ([]analyze.Event, error) {
	kind, ok := hookKinds[env.Event]
	if !ok {
		return nil, nil
	}
	var in hookInput
	if err := json.Unmarshal([]byte(env.Stdin), &in); err != nil {
		return nil, fmt.Errorf("claude: hook %s stdin: %w", env.Event, err)
	}
	if in.SessionID == "" {
		return nil, errNoSession
	}
	if env.RecordedAt.IsZero() {
		return nil, fmt.Errorf("claude: hook %s without recorded_at", env.Event)
	}
	e := analyze.Event{
		Tool: adapterID, Store: store, Origin: analyze.OriginUnknown,
		NativeSessionID: in.SessionID,
		Seq:             "registry:" + filepath.Base(env.File),
		TS:              env.RecordedAt.UTC(), Kind: kind, ObservedBy: analyze.FromRegistry,
		Model: hookModel(in.Model), Effort: in.Effort,
		BranchCoverage: analyze.Unknown, DurationCoverage: analyze.Unknown,
	}
	if env.File == "" {
		e.Seq = "registry:" + env.RecordedAt.UTC().Format("20060102T150405.000000000Z")
	} else {
		e.Ref, _ = json.Marshal(ref{Path: env.File})
	}
	if g := env.Git; g != nil && g.Branch != "" {
		e.Branch, e.Commit, e.BranchCoverage = g.Branch, g.Commit, analyze.Measured
	}
	if kind == analyze.KindPermissionReq {
		e.CorrelationID = in.ToolUseID
		e.ToolNameRaw = in.ToolName
		if in.ToolName != "" {
			e.ToolName = normalizeTool(in.ToolName)
		}
		if len(in.ToolInput) > 0 {
			e.ToolArgsHash = analyze.ArgsHash(in.ToolInput)
			e.Files = argFiles(in.ToolInput)
		}
	}
	return []analyze.Event{e}, nil
}

func hookModel(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.ID
	}
	return ""
}
