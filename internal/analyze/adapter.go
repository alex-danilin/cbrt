package analyze

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Capabilities is what an adapter declares about its tool's data. The core
// derives every coverage flag from it; string fields take the values listed.
type Capabilities struct {
	SourceKind        string   // "jsonl" | "directory" | "none"
	Stores            []string // e.g. {"ide", "cli"} or {"default"}
	Tokens            string   // "measured" | "unknown"
	TokenGranularity  string   // "request" | "turn" | "none"
	CacheWriteOverlap string   // "disjoint" | "inside_input" | "unknown"
	Branch            string   // "per_event" | "at_start" | "none"
	ToolDuration      string   // "paired" | "embedded" | "none"
	PermissionSignal  string   // "request" | "denial_only" | "none"
	SessionStartEvent bool
	HookEvents        []string // native hook event names the recorder registers
	HookTemplate      string   // hook snippet written by install-hooks; "{cbrt}" is the binary path
	CorrelationIDs    []string // e.g. {"tool_use_id", "requestId"}
}

var capValues = map[string][]string{
	"SourceKind":        {"jsonl", "directory", "none"},
	"Tokens":            {"measured", "unknown"},
	"TokenGranularity":  {"request", "turn", "none"},
	"CacheWriteOverlap": {"disjoint", "inside_input", "unknown"},
	"Branch":            {"per_event", "at_start", "none"},
	"ToolDuration":      {"paired", "embedded", "none"},
	"PermissionSignal":  {"request", "denial_only", "none"},
}

// Validate reports every field outside its allowed values.
func (c Capabilities) Validate() error {
	var errs []error
	for name, v := range map[string]string{
		"SourceKind": c.SourceKind, "Tokens": c.Tokens, "TokenGranularity": c.TokenGranularity,
		"CacheWriteOverlap": c.CacheWriteOverlap, "Branch": c.Branch, "ToolDuration": c.ToolDuration,
		"PermissionSignal": c.PermissionSignal,
	} {
		if !slices.Contains(capValues[name], v) {
			errs = append(errs, fmt.Errorf("%s %q: want one of %v", name, v, capValues[name]))
		}
	}
	if len(c.Stores) == 0 {
		errs = append(errs, errors.New("Stores is empty"))
	}
	if (c.Tokens == "unknown") != (c.TokenGranularity == "none") {
		errs = append(errs, errors.New(`Tokens "unknown" goes with TokenGranularity "none", and only with it`))
	}
	if len(c.HookEvents) > 0 && !strings.Contains(c.HookTemplate, "{cbrt}") {
		errs = append(errs, errors.New(`HookTemplate must contain "{cbrt}" when HookEvents are set`))
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errors.Join(errs...)
}

// Adapter reads one tool's sessions. Implementations are stateless and safe
// for concurrent use.
type Adapter interface {
	ID() string
	// Version changes whenever parsing or normalization changes.
	Version() string
	// DiscoveryRoots are the default store roots; "~" and "$VAR" are expanded
	// by the core (ExpandRoot). A config can override them per machine.
	DiscoveryRoots() []string
	Capabilities() Capabilities

	// Discover lists every logical source under the (expanded) roots. Missing
	// roots are not an error. Sources are returned in a stable order.
	Discover(roots []string) ([]Source, error)
	// Events emits a source's events in order. A malformed record is skipped
	// and reported through the returned error only if nothing usable remains;
	// an emit error stops the walk and is returned. A trailing partial line
	// (no final newline) is ignored.
	Events(src Source, emit func(Event) error) error
	// NormalizeHook maps one spooled hook envelope of this tool to events
	// (ObservedBy FromRegistry). Unknown hook events yield no events.
	NormalizeHook(env Envelope) ([]Event, error)
	// Bodies emits src's events exactly as Events does, with Body set on each
	// (the text TextVolumeUnits measures; "" when there is none). Only
	// excerpts call it: the core renders, truncates and redacts the bodies,
	// so excerpt sizes can be estimated from TextVolumeUnits alone.
	Bodies(src Source, emit func(Event) error) error
}

// ProjectContexter is optional. An adapter whose tool reads per-project
// context under tool-specific names or locations implements it; the project
// inventory (`projects.jsonl`) then lists what exists there, by name, size
// and file count only, never contents.
type ProjectContexter interface {
	// ProjectContext returns the places the tool reads context from for the
	// project rooted at root; storeRoots are the adapter's expanded store
	// roots on this machine. Places that do not exist are fine (skipped).
	ProjectContext(root string, storeRoots []string) []ContextPath
}

// ContextPath is one place a tool reads project context from.
type ContextPath struct {
	// Kind is "instructions", "settings", "skills" or "memory".
	Kind string
	// Path is slash-separated and relative to the project root, or absolute
	// for context the tool keeps outside the project.
	Path string
	// Dir marks a folder: its entries are listed by name (capped) and its
	// files counted, one level deep.
	Dir bool
}

var (
	regMu    sync.RWMutex
	registry = map[string]Adapter{}
)

// Register adds an adapter; call it from the adapter package's init. It
// panics on an empty or duplicate ID and on invalid Capabilities, so a broken
// adapter fails every test and every run instead of producing quiet zeros.
func Register(a Adapter) {
	regMu.Lock()
	defer regMu.Unlock()
	id := a.ID()
	if id == "" {
		panic("analyze: adapter with empty ID")
	}
	if _, dup := registry[id]; dup {
		panic("analyze: duplicate adapter " + id)
	}
	if err := a.Capabilities().Validate(); err != nil {
		panic(fmt.Sprintf("analyze: adapter %s: invalid capabilities: %v", id, err))
	}
	registry[id] = a
}

// Adapters returns the registered adapters sorted by ID.
func Adapters() []Adapter {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Adapter, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Get returns the adapter with the given ID.
func Get(id string) (Adapter, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	a, ok := registry[id]
	return a, ok
}

// ExpandRoot expands a leading "~" (the user's home) and $VAR / ${VAR}.
func ExpandRoot(root string) string {
	if root == "~" || strings.HasPrefix(root, "~/") || strings.HasPrefix(root, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			root = home + root[1:]
		}
	}
	return filepath.Clean(os.ExpandEnv(root))
}

// TextVolume is ceil(chars/4) for s: the volume proxy every adapter computes
// the same way, so tools can be compared on it.
func TextVolume(s string) int {
	n := utf8.RuneCountInString(s)
	return (n + 3) / 4
}

// ArgsHash is the hex sha1 of a tool call's arguments in canonical form
// (object keys sorted, insignificant whitespace removed); raw bytes when the
// arguments are not JSON.
func ArgsHash(args []byte) string {
	canon := args
	var v any
	if err := json.Unmarshal(args, &v); err == nil {
		if b, err := json.Marshal(v); err == nil { // encoding/json sorts map keys
			canon = b
		}
	} else {
		canon = bytes.TrimSpace(args)
	}
	sum := sha1.Sum(canon)
	return hex.EncodeToString(sum[:])
}

// Int64 returns a pointer to n, for Tokens and DurationMS.
func Int64(n int64) *int64 { return &n }

// Bool returns a pointer to b, for ToolSuccess.
func Bool(b bool) *bool { return &b }
