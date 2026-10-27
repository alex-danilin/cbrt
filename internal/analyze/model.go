package analyze

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The types below are what the Phase 1 modules pass to each other:
//
//	collect  -> Dataset                     (sessions with events, cbrt joins)
//	metrics  -> []SessionMetrics            (per session, window-restricted)
//	tasks    -> []Task, TaskIdentity        (candidate tasks, stable ids)
//	state    <- SessionMetrics, Task, Run   (durable JSON: archive, ids, recs, runs)
//	report   <- Dataset, metrics, tasks     (summary.md, anchors.jsonl, excerpt_candidates.jsonl)

// SessionKey identifies a session across tools.
type SessionKey struct {
	AdapterID string `json:"adapter_id"`
	Store     string `json:"store"`
	NativeID  string `json:"native_id"`
}

func (k SessionKey) String() string { return k.AdapterID + "/" + k.Store + "/" + k.NativeID }

// EventKey identifies one event: its session and Seq.
type EventKey struct {
	Session SessionKey `json:"session"`
	Seq     string     `json:"seq"`
}

// Served is what cbrt recorded for one model request it relayed
// (usage.Record), joined to a model_request by answer id.
type Served struct {
	ResponseID string    `json:"response_id"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Route      string    `json:"route"`
	Location   string    `json:"location"` // local | cloud
	Fallback   bool      `json:"fallback"` // attempt_no > 1
	Outcome    string    `json:"outcome"`
	TS         time.Time `json:"ts"`
}

// Session is one session with all its events (main or subagent).
type Session struct {
	Key    SessionKey  `json:"key"`
	Parent *SessionKey `json:"parent,omitempty"` // subagents
	Origin string      `json:"origin"`
	// Project is the session's project root: the most common event Cwd,
	// normalized (cleaned, case-folded on Windows); "" when unknown.
	Project string `json:"project"`
	// Events are sorted by TS, ties keeping source order. Transcript events and
	// registry events (ObservedBy) are both here; LinkedTo says which
	// transcript event a registry event corresponds to.
	Events       []Event           `json:"events"`
	LinkedTo     map[string]string `json:"linked_to,omitempty"` // registry Seq -> transcript Seq (by CorrelationID only)
	Capabilities Capabilities      `json:"-"`
	Source       Source            `json:"source"`
	Served       map[string]Served `json:"served,omitempty"` // model_request Seq -> what cbrt served
	Flags        []string          `json:"flags,omitempty"`  // e.g. branch_drift, branch_partial, model_unverified
}

// Window is the reporting window; events with Since <= TS < Until count.
type Window struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// Contains reports whether t falls in the window.
func (w Window) Contains(t time.Time) bool { return !t.Before(w.Since) && t.Before(w.Until) }

// Dataset is everything one run collected. Sessions holds every session with
// at least one event in the window (with its full history); Problems lists
// unreadable sources, leftover spool files and similar, by name only.
type Dataset struct {
	Window   Window     `json:"window"`
	Sessions []*Session `json:"sessions"`
	// Present lists every session whose transcript still exists, in or out of
	// the window (the archive freezes the others).
	Present  []SessionKey `json:"present,omitempty"`
	Problems []string     `json:"problems,omitempty"`
}

// Value is one metric value with its coverage. Frac is the coverage fraction
// where coverage is partial (e.g. measured requests / all requests).
type Value struct {
	N    float64  `json:"n"`
	Cov  Coverage `json:"cov"`
	Frac *float64 `json:"frac,omitempty"`
}

// SessionMetrics are one session's derived facts over the window (§3.2.3).
// Metric names are constants in package metrics.
type SessionMetrics struct {
	Session        SessionKey       `json:"session"`
	Project        string           `json:"project"`
	Window         Window           `json:"window"`
	MetricsVersion string           `json:"metrics_version"`
	Values         map[string]Value `json:"values"`
	Flags          []string         `json:"flags,omitempty"`
}

// Range is a contiguous span of one session's events (§3.2.4).
type Range struct {
	Session SessionKey `json:"session"`
	FromSeq string     `json:"from_seq"`
	ToSeq   string     `json:"to_seq"`
	From    time.Time  `json:"from"`
	To      time.Time  `json:"to"`
}

// Task confidence values.
const (
	ConfExplicit = "explicit"
	ConfManual   = "manual"
	ConfStrong   = "strong"
	ConfWeak     = "weak"
	ConfSingle   = "single"
)

// Task is a candidate task: ranges that plausibly serve one goal.
type Task struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"`
	Branch     string    `json:"branch,omitempty"`
	Confidence string    `json:"confidence"`
	Members    []Range   `json:"members"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	SplitFrom  string    `json:"split_from,omitempty"`
	Aliases    []string  `json:"aliases,omitempty"`
	Label      string    `json:"label,omitempty"`
}

// TaskIdentity persists task ids across runs: founding key -> task id, and
// absorbed ids -> surviving id. The founding key is
// "<adapter>/<store>/<native>#<first event seq of the founding range>".
type TaskIdentity struct {
	V       int               `json:"v"`
	ByFound map[string]string `json:"by_founding"`
	Aliases map[string]string `json:"aliases"`
}

// Config is CBRT_ANALYZE_HOME/config.json. Every field has a default; a
// missing file is all defaults.
type Config struct {
	V                  int      `json:"v"`
	WindowDays         int      `json:"window_days"`          // default 7
	RangeGapHours      float64  `json:"range_gap_hours"`      // default 4
	ExcerptTokens      int      `json:"excerpt_tokens"`       // default 40000
	DeepReadTokens     int      `json:"deep_read_tokens"`     // default 20000
	RetentionDays      int      `json:"retention_days"`       // out/ and registry months; default 90
	CbrtDataDirs       []string `json:"cbrt_data_dirs"`       // default <UserConfigDir>/cbrt/data
	ExcludeHomeProject *bool    `json:"exclude_home_project"` // default true
	// ContextWarnTokens is the context size at which the opt-in context
	// warning hook speaks up; default DefaultContextWarnTokens.
	ContextWarnTokens int64                 `json:"context_warn_tokens"`
	Tools             map[string]ToolConfig `json:"tools"`
}

// DefaultContextWarnTokens is Config.ContextWarnTokens's default.
const DefaultContextWarnTokens = 300_000

// ToolConfig overrides one adapter's defaults on this machine.
type ToolConfig struct {
	Roots          []string `json:"roots"`
	DisabledEvents []string `json:"disabled_events"`
	Disabled       bool     `json:"disabled"`
}

// ConfigVersion is the current Config.V.
const ConfigVersion = 1

// LoadConfig reads <home>/config.json and applies defaults.
func LoadConfig(home string) (Config, error) {
	c := Config{V: ConfigVersion}
	b, err := os.ReadFile(filepath.Join(home, "config.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, err
	default:
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("config.json: %w", err)
		}
		if c.V > ConfigVersion {
			return c, fmt.Errorf("config.json: version %d is newer than this cbrt understands (%d)", c.V, ConfigVersion)
		}
	}
	if c.WindowDays <= 0 {
		c.WindowDays = 7
	}
	if c.RangeGapHours <= 0 {
		c.RangeGapHours = 4
	}
	if c.ExcerptTokens <= 0 {
		c.ExcerptTokens = 40000
	}
	if c.DeepReadTokens <= 0 {
		c.DeepReadTokens = 20000
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = 90
	}
	if len(c.CbrtDataDirs) == 0 {
		if d, err := os.UserConfigDir(); err == nil {
			c.CbrtDataDirs = []string{filepath.Join(d, "cbrt", "data")}
		}
	}
	if c.ExcludeHomeProject == nil {
		c.ExcludeHomeProject = Bool(true)
	}
	if c.ContextWarnTokens <= 0 {
		c.ContextWarnTokens = DefaultContextWarnTokens
	}
	c.V = ConfigVersion
	return c, nil
}

// Roots returns the store roots for adapter a: the config's override, else
// the adapter's defaults, each expanded.
func (c Config) Roots(a Adapter) []string {
	roots := a.DiscoveryRoots()
	if tc, ok := c.Tools[a.ID()]; ok && len(tc.Roots) > 0 {
		roots = tc.Roots
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, ExpandRoot(r))
	}
	return out
}
