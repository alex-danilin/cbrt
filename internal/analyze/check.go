package analyze

import (
	"fmt"
	"slices"
	"time"
)

var (
	validKinds = []Kind{KindUserPrompt, KindAssistantText, KindThinking, KindToolCall, KindToolResult,
		KindPermissionReq, KindPermissionDeny, KindError, KindInterrupt, KindCompaction, KindCheckpoint,
		KindModelSwitch, KindSessionStart, KindSessionEnd, KindClaimComplete, KindTurnEnd, KindTestResult,
		KindSystemContext, KindModelRequest}
	validTools    = []string{ToolReadFile, ToolSearch, ToolShell, ToolEdit, ToolWeb, ToolSubagent, ToolAskUser, ToolMCP, ToolOther}
	validOrigins  = []string{OriginCLI, OriginIDE, OriginSDK, OriginUnknown}
	validCoverage = []Coverage{Measured, MeasuredAtStart, Partial, Estimated, Inferred, Unknown}
)

// CheckEvents returns every violation of the event contract in one source's
// events, as emitted by adapter a. Adapter tests run it on their fixtures;
// the local (-tags local) checks run it on real stores.
func CheckEvents(a Adapter, src Source, events []Event) []error {
	caps := a.Capabilities()
	var errs []error
	bad := func(i int, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s event %d: %s", src.NativeSessionID, i, fmt.Sprintf(format, args...)))
	}
	seqs := map[string]map[string]bool{}
	reqs := map[string]map[string]bool{}
	for i, e := range events {
		if e.Tool != a.ID() {
			bad(i, "Tool %q, want %q", e.Tool, a.ID())
		}
		if !slices.Contains(caps.Stores, e.Store) || e.Store != src.Store {
			bad(i, "Store %q, want the source's %q (one of %v)", e.Store, src.Store, caps.Stores)
		}
		if !slices.Contains(validOrigins, e.Origin) {
			bad(i, "Origin %q", e.Origin)
		}
		if e.NativeSessionID != src.NativeSessionID && e.ParentSessionID != src.NativeSessionID {
			bad(i, "session %q (parent %q) belongs to neither this source nor a subagent of it", e.NativeSessionID, e.ParentSessionID)
		}
		if e.Seq == "" {
			bad(i, "empty Seq")
		}
		if seqs[e.NativeSessionID] == nil {
			seqs[e.NativeSessionID] = map[string]bool{}
		}
		if seqs[e.NativeSessionID][e.Seq] {
			bad(i, "duplicate Seq %q", e.Seq)
		}
		seqs[e.NativeSessionID][e.Seq] = true
		if e.TS.IsZero() || e.TS.Location() != time.UTC {
			bad(i, "TS %v must be non-zero UTC", e.TS)
		}
		if !slices.Contains(validKinds, e.Kind) {
			bad(i, "Kind %q", e.Kind)
		}
		if e.ObservedBy != FromTranscript {
			bad(i, "ObservedBy %q, want %q from Events", e.ObservedBy, FromTranscript)
		}
		for name, c := range map[string]Coverage{"BranchCoverage": e.BranchCoverage, "DurationCoverage": e.DurationCoverage} {
			if !slices.Contains(validCoverage, c) {
				bad(i, "%s %q", name, c)
			}
		}
		if caps.Branch == "none" && (e.Branch != "" || e.BranchCoverage != Unknown) {
			bad(i, "Branch %q / %s from a tool that records no branch", e.Branch, e.BranchCoverage)
		}
		if e.DurationMS != nil && (*e.DurationMS < 0 || e.DurationCoverage == Unknown) {
			bad(i, "DurationMS %d with coverage %s", *e.DurationMS, e.DurationCoverage)
		}
		if e.DurationMS == nil && e.DurationCoverage != Unknown {
			bad(i, "no DurationMS but coverage %s", e.DurationCoverage)
		}
		if e.TextVolumeUnits < 0 || e.TextLen < 0 {
			bad(i, "negative text volume or length")
		}

		if e.Kind == KindModelRequest {
			if e.RequestID == "" {
				bad(i, "model_request without RequestID")
			}
			if reqs[e.NativeSessionID] == nil {
				reqs[e.NativeSessionID] = map[string]bool{}
			}
			if reqs[e.NativeSessionID][e.RequestID] {
				bad(i, "second model_request for RequestID %q (usage must be counted once)", e.RequestID)
			}
			reqs[e.NativeSessionID][e.RequestID] = true
			if !slices.Contains(validCoverage, e.TokensCoverage) {
				bad(i, "TokensCoverage %q", e.TokensCoverage)
			}
			switch {
			case caps.Tokens == "unknown" && (e.Tokens != nil || e.TokensCoverage != Unknown):
				bad(i, "tokens from a tool that records none")
			case e.Tokens == nil && e.TokensCoverage != Unknown:
				bad(i, "no Tokens but coverage %s", e.TokensCoverage)
			case e.Tokens != nil && e.TokensCoverage != Measured && e.TokensCoverage != Partial:
				bad(i, "Tokens with coverage %s", e.TokensCoverage)
			}
			if t := e.Tokens; t != nil {
				buckets := []*int64{t.InUncached, t.InCacheRead, t.InCacheWrite, t.OutVisible, t.OutThinking}
				hasNil := false
				for _, b := range buckets {
					if b != nil && *b < 0 {
						bad(i, "negative token bucket")
					}
					hasNil = hasNil || b == nil
				}
				if hasNil && e.TokensCoverage != Partial {
					bad(i, "a null token bucket needs TokensCoverage partial")
				}
			}
		} else if e.Tokens != nil || e.TokensCoverage != "" || len(e.TokensNative) > 0 {
			bad(i, "usage on a %s event (only model_request carries it)", e.Kind)
		}

		switch e.Kind {
		case KindToolCall, KindToolResult:
			if !slices.Contains(validTools, e.ToolName) || e.ToolNameRaw == "" {
				bad(i, "tool name %q (raw %q)", e.ToolName, e.ToolNameRaw)
			}
			if e.Kind == KindToolCall && e.ToolArgsHash == "" {
				bad(i, "tool_call without ToolArgsHash")
			}
		case KindPermissionReq, KindPermissionDeny:
			// May name the tool being asked about (for allowlist recommendations).
			if e.ToolName != "" && !slices.Contains(validTools, e.ToolName) {
				bad(i, "tool name %q", e.ToolName)
			}
			if e.ToolSuccess != nil {
				bad(i, "ToolSuccess on a %s event", e.Kind)
			}
		default:
			if e.ToolName != "" || e.ToolSuccess != nil {
				bad(i, "tool fields on a %s event", e.Kind)
			}
		}
	}
	return errs
}
