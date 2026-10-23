// Package fixturetool is a synthetic, hooks-only tool (acceptance test 14):
// it has no transcript, so its sessions exist only through the hook
// recorder's envelopes. It proves an adapter registers without core edits.
//
// Its hooks send a JSON object on stdin with a "session" field; the events
// are Start, End and Ask (a permission prompt).
package fixturetool

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/hook"
)

// ID is the adapter ID.
const ID = "fixturetool"

const store = "default"

// Adapter implements analyze.Adapter.
type Adapter struct{}

func init() { analyze.Register(Adapter{}) }

func (Adapter) ID() string      { return ID }
func (Adapter) Version() string { return "1" }

// DiscoveryRoots is empty: the tool has no store of its own. Discover takes
// analyzer homes as roots.
func (Adapter) DiscoveryRoots() []string { return nil }

func (Adapter) Capabilities() analyze.Capabilities {
	return analyze.Capabilities{
		SourceKind:        "none",
		Stores:            []string{store},
		Tokens:            "unknown",
		TokenGranularity:  "none",
		CacheWriteOverlap: "unknown",
		Branch:            "none",
		ToolDuration:      "none",
		PermissionSignal:  "request",
		SessionStartEvent: true,
		HookEvents:        []string{"Start", "End", "Ask"},
		HookTemplate:      `"{cbrt}" hook record fixturetool`,
		CorrelationIDs:    []string{"session"},
	}
}

var kinds = map[string]analyze.Kind{
	"Start": analyze.KindSessionStart,
	"End":   analyze.KindSessionEnd,
	"Ask":   analyze.KindPermissionReq,
}

func sessionOf(env analyze.Envelope) string {
	var in struct {
		Session any `json:"session"`
	}
	if json.Unmarshal([]byte(env.Stdin), &in) != nil {
		return ""
	}
	s, _ := in.Session.(string)
	return s
}

// Discover treats each root as an analyzer home and returns one registry-only
// Source per distinct session among its fixturetool envelopes, sorted by
// session. Missing roots yield nothing.
func (Adapter) Discover(roots []string) ([]analyze.Source, error) {
	seen := map[string]bool{}
	for _, root := range roots {
		envs, _, err := hook.ReadSpool(root)
		if err != nil {
			return nil, err
		}
		for _, env := range envs {
			if env.AdapterID != ID {
				continue
			}
			if s := sessionOf(env); s != "" {
				seen[s] = true
			}
		}
	}
	sessions := make([]string, 0, len(seen))
	for s := range seen {
		sessions = append(sessions, s)
	}
	sort.Strings(sessions)
	var out []analyze.Source
	for _, s := range sessions {
		out = append(out, analyze.Source{
			AdapterID:       ID,
			Store:           store,
			NativeSessionID: s,
			Locator:         "registry:" + s,
		})
	}
	return out, nil
}

// Events emits nothing: the tool keeps no transcript.
func (Adapter) Events(analyze.Source, func(analyze.Event) error) error { return nil }

// NormalizeHook maps Start, End and Ask to session_start, session_end and
// permission_request. The envelope's git info, when present, is a measured
// branch observation at that moment.
func (Adapter) NormalizeHook(env analyze.Envelope) ([]analyze.Event, error) {
	if env.AdapterID != ID {
		return nil, fmt.Errorf("fixturetool: envelope of adapter %q", env.AdapterID)
	}
	kind, ok := kinds[env.Event]
	if !ok {
		return nil, nil
	}
	session := sessionOf(env)
	if session == "" {
		return nil, errors.New("fixturetool: hook stdin has no session")
	}
	// The spool file name identifies the envelope before and after compaction.
	seq := "hook:" + env.RecordedAt.UTC().Format(time.RFC3339Nano)
	if env.File != "" {
		seq = "hook:" + strings.TrimSuffix(hook.SpoolName(env), ".json")
	}
	e := analyze.Event{
		Tool:             ID,
		Store:            store,
		Origin:           analyze.OriginUnknown,
		NativeSessionID:  session,
		Seq:              seq,
		TS:               env.RecordedAt.UTC(),
		Kind:             kind,
		ObservedBy:       analyze.FromRegistry,
		BranchCoverage:   analyze.Unknown,
		DurationCoverage: analyze.Unknown,
		CorrelationID:    session,
	}
	if g := env.Git; g != nil {
		e.Branch, e.Commit, e.BranchCoverage = g.Branch, g.Commit, analyze.Measured
	}
	return []analyze.Event{e}, nil
}

// Bodies emits nothing either: there is no transcript to quote.
func (Adapter) Bodies(analyze.Source, func(analyze.Event) error) error { return nil }
