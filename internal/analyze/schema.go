// Package analyze is the core of the agent session analyzer (`cbrt analyze`):
// the common event schema that every tool adapter produces, the adapter
// contract and registry, and the checks every adapter must pass.
//
// Adapters live in internal/analyze/adapters/<id>/ and register themselves
// from an init function. The core never names a tool: it only reads the
// Capabilities an adapter declares.
package analyze

import (
	"encoding/json"
	"time"
)

// Coverage says how a value was obtained. The zero value is not valid: a
// field that has a coverage must set one.
type Coverage string

const (
	Measured        Coverage = "measured"
	MeasuredAtStart Coverage = "measured_at_start" // branch: recorded once, at session start
	Partial         Coverage = "partial"           // tokens: some buckets null (overlap unverified)
	Estimated       Coverage = "estimated"
	Inferred        Coverage = "inferred"
	Unknown         Coverage = "unknown"
)

// Kind is the normalized event kind.
type Kind string

const (
	KindUserPrompt     Kind = "user_prompt"
	KindAssistantText  Kind = "assistant_text"
	KindThinking       Kind = "assistant_thinking"
	KindToolCall       Kind = "tool_call"
	KindToolResult     Kind = "tool_result"
	KindPermissionReq  Kind = "permission_request"
	KindPermissionDeny Kind = "permission_denied"
	KindError          Kind = "error"
	KindInterrupt      Kind = "interrupt"
	KindCompaction     Kind = "compaction"
	KindCheckpoint     Kind = "checkpoint"
	KindModelSwitch    Kind = "model_switch"
	KindSessionStart   Kind = "session_start"
	KindSessionEnd     Kind = "session_end"
	KindClaimComplete  Kind = "task_claimed_complete" // the assistant claims it is done (derived from its text)
	KindTurnEnd        Kind = "turn_end"              // the agent finished responding (a Stop hook, a turn-complete record)
	KindTestResult     Kind = "test_result"
	KindSystemContext  Kind = "system_context" // context the tool injected (not typed by the user)
	// KindModelRequest is one call to a model: exactly one per request, with
	// Tokens set when the tool recorded usage for it. The core computes token
	// totals and coverage fractions from these events only.
	KindModelRequest Kind = "model_request"
)

// Normalized tool names (Event.ToolName). Anything else is ToolOther.
const (
	ToolReadFile = "read_file"
	ToolSearch   = "search"
	ToolShell    = "shell"
	ToolEdit     = "edit"
	ToolWeb      = "web"
	ToolSubagent = "subagent"
	ToolAskUser  = "ask_user"
	ToolMCP      = "mcp"
	ToolOther    = "other"
)

// Origin of a session.
const (
	OriginCLI     = "cli"
	OriginIDE     = "ide"
	OriginSDK     = "sdk"
	OriginUnknown = "unknown"
)

// ObservedBy says where an event came from.
const (
	FromTranscript = "transcript"
	FromRegistry   = "registry"
)

// Tokens are non-overlapping usage buckets of one model request. Each is nil
// when the tool does not record it, or its semantics are unverified (then the
// event's TokensCoverage is Partial). When a tool reports output without
// splitting off thinking, all output goes in OutVisible and OutThinking is nil
// (Partial), so totals stay complete. See the architecture doc §3.2.2 for the
// per-tool arithmetic; adapters own it.
type Tokens struct {
	InUncached   *int64 `json:"in_uncached"`
	InCacheRead  *int64 `json:"in_cache_read"`
	InCacheWrite *int64 `json:"in_cache_write"`
	OutVisible   *int64 `json:"out_visible"`
	OutThinking  *int64 `json:"out_thinking"`
}

// Event is one normalized event of a session. Fields an adapter cannot know
// stay at their zero value, except the Coverage fields, which then say
// Unknown.
type Event struct {
	Tool            string `json:"tool"`  // adapter ID
	Store           string `json:"store"` // one of Capabilities.Stores
	Origin          string `json:"origin"`
	NativeSessionID string `json:"native_session_id"`
	// ParentSessionID is the parent's NativeSessionID for a subagent's events
	// (same adapter and store), "" for the main session.
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// Seq is the event's natural key within its session: the tool's own ordinal
	// or record id when there is one, else "<member>:<line>". Unique per session.
	Seq        string    `json:"seq"`
	TS         time.Time `json:"ts"` // UTC; never zero
	Kind       Kind      `json:"kind"`
	ObservedBy string    `json:"observed_by"`

	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	RequestID string `json:"request_id,omitempty"` // model request identity (set on KindModelRequest)

	// Cwd is the working directory the tool recorded for this event (project
	// grouping), "" when unknown.
	Cwd string `json:"cwd,omitempty"`
	// Text is the event's own words, for pattern-based metrics (completion
	// claims, corrections, test runs): the prompt on user_prompt, the answer on
	// assistant_text, the command line on a shell tool_call; "" otherwise.
	// It is conversation content: it stays in memory (and the local parse
	// cache) and reaches outputs only through redacted anchors and excerpts.
	Text string `json:"text,omitempty"`

	// Usage, only on KindModelRequest. Tokens nil with TokensCoverage Unknown
	// means the request happened but its usage was not recorded.
	TokensNative   json.RawMessage `json:"tokens_native,omitempty"`
	Tokens         *Tokens         `json:"tokens,omitempty"`
	TokensCoverage Coverage        `json:"tokens_coverage,omitempty"`

	// TextVolumeUnits is ceil(chars/4) of the text this event persisted (prompt,
	// answer, thinking, tool input or output). A volume proxy, never tokens.
	TextVolumeUnits int `json:"text_volume_units"`
	TextLen         int `json:"text_len"` // characters of user/assistant text
	// Body is that persisted text itself, so TextVolume(Body) ==
	// TextVolumeUnits. Adapters set it only in Adapter.Bodies, for excerpts;
	// Events leaves it "", and it is never serialized (cache, state, outputs).
	Body string `json:"-"`

	Branch         string   `json:"branch,omitempty"`
	Commit         string   `json:"commit,omitempty"`
	BranchCoverage Coverage `json:"branch_coverage"`

	// Tool calls and results.
	ToolName     string `json:"tool_name,omitempty"`      // normalized (Tool* constants)
	ToolNameRaw  string `json:"tool_name_raw,omitempty"`  // as the tool wrote it
	ToolArgsHash string `json:"tool_args_hash,omitempty"` // hex sha1 of canonical args JSON
	ToolSuccess  *bool  `json:"tool_success,omitempty"`   // on results: nil = not recorded
	// CorrelationID links a call to its result and a registry envelope to a
	// transcript event (tool_use_id, call_id, turn_id, requestId). On a
	// model_request it is the id of the answer the client stored (Anthropic
	// message id, Responses response id) when known: cbrt records the same
	// id, which links a request to the provider and model that served it.
	CorrelationID    string   `json:"correlation_id,omitempty"`
	DurationMS       *int64   `json:"duration_ms,omitempty"`
	DurationCoverage Coverage `json:"duration_coverage"`
	Files            []string `json:"files,omitempty"` // paths read or touched, from the arguments

	// Ref points back into the source; adapter-defined. Transcript events with
	// a Ref can be excerpted.
	Ref json.RawMessage `json:"ref,omitempty"`
}

// Member is one physical file of a logical source.
type Member struct {
	Path string `json:"path"`
}

// Source is one logical conversation: one session, possibly spread over
// several files (members, in reading order).
type Source struct {
	AdapterID       string   `json:"adapter_id"`
	Store           string   `json:"store"`
	NativeSessionID string   `json:"native_session_id"`
	Locator         string   `json:"locator"` // a path, a directory, or "registry:<native_session_id>"
	Members         []Member `json:"members"` // empty for registry-only sessions
}

// GitInfo is the branch and commit a hook saw at event time.
type GitInfo struct {
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// Envelope is one hook invocation as the recorder spooled it (§3.1).
type Envelope struct {
	V          int       `json:"v"`
	AdapterID  string    `json:"adapter_id"`
	Event      string    `json:"event"` // the tool's native hook event name
	RecordedAt time.Time `json:"recorded_at"`
	Git        *GitInfo  `json:"git"`   // nil when not captured or git failed
	Stdin      string    `json:"stdin"` // raw stdin, decoded as UTF-8 with replacement
	// File is the spool file the envelope was read from (set by the reader,
	// not stored).
	File string `json:"-"`
}

// EnvelopeVersion is the current Envelope.V.
const EnvelopeVersion = 1
