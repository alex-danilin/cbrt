package antigravity

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// hookInput is the part of Antigravity's hook stdin the adapter uses. Other
// fields (workspacePaths, artifactDirectoryPath, initialNumSteps, toolCall)
// are ignored.
type hookInput struct {
	ConversationID string `json:"conversationId"`
	TranscriptPath string `json:"transcriptPath"`
	ModelName      string `json:"modelName"`
	InvocationNum  *int   `json:"invocationNum"`
}

// NormalizeHook maps a spooled Antigravity hook to one event:
//
//   - PostInvocation with invocationNum 1: session_start (Antigravity has no
//     session-start hook; the first invocation finishing stands in for it).
//   - any later PostInvocation: checkpoint, the closest kind to "an agent
//     invocation (turn) finished"; it marks a boundary with a branch reading.
//   - Stop: session_end, the agent stopped.
//
// Every event carries Model = modelName (it may be an alias), the
// conversationId as NativeSessionID and CorrelationID, the store from the
// transcript path, and the branch and commit the recorder captured
// (measured), else unknown. Other hook events yield nothing.
func (Adapter) NormalizeHook(env analyze.Envelope) ([]analyze.Event, error) {
	var kind analyze.Kind
	switch env.Event {
	case "PostInvocation", "Stop":
	default:
		return nil, nil
	}
	var in hookInput
	if err := json.Unmarshal([]byte(env.Stdin), &in); err != nil {
		return nil, fmt.Errorf("antigravity hook %s: %w", env.Event, err)
	}
	if strings.TrimSpace(in.ConversationID) == "" {
		return nil, errors.New("antigravity hook " + env.Event + ": no conversationId")
	}
	if env.RecordedAt.IsZero() {
		return nil, errors.New("antigravity hook " + env.Event + ": no recorded_at")
	}
	switch {
	case env.Event == "Stop":
		kind = analyze.KindSessionEnd
	case in.InvocationNum != nil && *in.InvocationNum == 1:
		kind = analyze.KindSessionStart
	default:
		kind = analyze.KindTurnEnd // a later invocation finished
	}
	store := storeForTranscript(in.TranscriptPath)
	seq := "registry:" + filepath.Base(env.File)
	if env.File == "" {
		seq = "registry:" + env.Event + ":" + env.RecordedAt.UTC().Format("20060102T150405.000000000Z")
	}
	e := analyze.Event{
		Tool: ID, Store: store, Origin: originFor(store), NativeSessionID: in.ConversationID,
		Seq: seq, TS: env.RecordedAt.UTC(), Kind: kind, ObservedBy: analyze.FromRegistry,
		Model: in.ModelName, CorrelationID: in.ConversationID,
		BranchCoverage: analyze.Unknown, DurationCoverage: analyze.Unknown,
	}
	if g := env.Git; g != nil && (g.Branch != "" || g.Commit != "") {
		e.Branch, e.Commit, e.BranchCoverage = g.Branch, g.Commit, analyze.Measured
	}
	return []analyze.Event{e}, nil
}
