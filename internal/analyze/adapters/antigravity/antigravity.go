// Package antigravity is the session-analyzer adapter for Google Antigravity:
// the IDE store (~/.gemini/antigravity/brain) and the CLI store
// (~/.gemini/antigravity-cli/brain), which share one transcript format.
//
// Layout of one conversation:
//
//	<root>/<conversation-id>/.system_generated/logs/
//	  transcript.jsonl              compact copy (truncated fields): never read
//	  transcript_full.jsonl         the live, complete transcript
//	  chunks/transcript_full/NNNNNNNN.jsonl   older parts rolled out of the live file
//
// Chunk files are byte slices of one stream: a record may start in one chunk
// and end in the next. The live file repeats some or all chunked steps, so
// steps are deduplicated by step_index (the later copy wins), and a step that
// survives only in a chunk is kept.
//
// Each line is one step: step_index, source (MODEL, SYSTEM, USER_EXPLICIT),
// type (USER_INPUT, PLANNER_RESPONSE, a tool type, GENERIC, ERROR_MESSAGE,
// SYSTEM_MESSAGE, CHECKPOINT, CONVERSATION_HISTORY, ...), status (DONE, ERROR,
// RUNNING), created_at, content, and on planner steps thinking and tool_calls
// (name plus args); newer IDE versions add input_tokens, cache_read_tokens
// and output_tokens to planner steps. Tool steps follow their planner step in the order of its
// tool_calls; tool steps embed "Created At:" / "Completed At:" lines in their
// content, which give exact durations.
package antigravity

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// ID is the adapter ID.
const ID = "antigravity"

// Stores.
const (
	StoreIDE = "ide"
	StoreCLI = "cli"
)

// Adapter reads Antigravity transcripts. It is stateless.
type Adapter struct{}

func init() { analyze.Register(Adapter{}) }

func (Adapter) ID() string      { return ID }
func (Adapter) Version() string { return "3" }

func (Adapter) DiscoveryRoots() []string {
	return []string{"~/.gemini/antigravity/brain", "~/.gemini/antigravity-cli/brain"}
}

func (Adapter) Capabilities() analyze.Capabilities {
	return analyze.Capabilities{
		SourceKind: "directory",
		Stores:     []string{StoreIDE, StoreCLI},
		// Newer IDE versions write per-request input/cache-read/output counts
		// on planner steps; older versions and the CLI store write none, so
		// only some model_requests carry Tokens (coverage partial: see
		// usage), and the core's coverage fraction shows that share. Cache
		// writes are not recorded at all.
		Tokens:            "measured",
		TokenGranularity:  "request",
		CacheWriteOverlap: "unknown",
		Branch:            "none", // only hook envelopes carry a branch
		ToolDuration:      "embedded",
		PermissionSignal:  "denial_only",
		SessionStartEvent: false, // the first PostInvocation (invocationNum 1) stands in
		HookEvents:        []string{"PostInvocation", "Stop"},
		// install-hooks appends the native event name. Antigravity runs hook
		// commands through `cmd /c` on Windows, so the binary path is quoted
		// for paths with spaces.
		HookTemplate:   `"{cbrt}" hook record antigravity`,
		CorrelationIDs: []string{"conversationId"},
	}
}

// storeForRoot decides the store from a brain root: the CLI store lives under
// a directory whose name ends in "-cli" (antigravity-cli/brain), anything else
// is the IDE store. Overridden roots keep working as long as they keep that
// parent directory name.
func storeForRoot(root string) string {
	// Split on both separators: a root written with backslashes must work
	// the same on every OS.
	parts := strings.FieldsFunc(root, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) >= 2 && strings.HasSuffix(strings.ToLower(parts[len(parts)-2]), "-cli") {
		return StoreCLI
	}
	return StoreIDE
}

// storeForTranscript decides the store from a transcript path that a hook
// reports: the directory above the "brain" segment, as in storeForRoot.
func storeForTranscript(p string) string {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	for i := len(parts) - 1; i > 0; i-- {
		if strings.EqualFold(parts[i], "brain") {
			if strings.HasSuffix(strings.ToLower(parts[i-1]), "-cli") {
				return StoreCLI
			}
			return StoreIDE
		}
	}
	if strings.Contains(strings.ToLower(p), "antigravity-cli") {
		return StoreCLI
	}
	return StoreIDE
}

func originFor(store string) string {
	if store == StoreCLI {
		return analyze.OriginCLI
	}
	return analyze.OriginIDE
}

const (
	liveName = "transcript_full.jsonl"
	chunkDir = "transcript_full" // under logs/chunks/
)

func logsDir(conv string) string { return filepath.Join(conv, ".system_generated", "logs") }

// Discover returns one Source per conversation directory that has a full
// transcript (chunks, live file, or both). Conversations with only the compact
// transcript.jsonl are not sources: that file truncates thinking and tool
// output. Order: roots as given, then conversation directory name.
func (a Adapter) Discover(roots []string) ([]analyze.Source, error) {
	var out []analyze.Source
	for _, root := range roots {
		ents, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		store := storeForRoot(root)
		for _, e := range ents { // ReadDir sorts by name
			if !e.IsDir() {
				continue
			}
			conv := filepath.Join(root, e.Name())
			members := conversationMembers(conv)
			if len(members) == 0 {
				continue
			}
			out = append(out, analyze.Source{
				AdapterID: ID, Store: store, NativeSessionID: e.Name(), Locator: conv, Members: members,
			})
		}
	}
	return out, nil
}

// conversationMembers lists the chunk files in numeric order, then the live file.
func conversationMembers(conv string) []analyze.Member {
	logs := logsDir(conv)
	chunks, _ := filepath.Glob(filepath.Join(logs, "chunks", chunkDir, "*.jsonl"))
	sort.Slice(chunks, func(i, j int) bool { return chunkLess(chunks[i], chunks[j]) })
	var ms []analyze.Member
	for _, c := range chunks {
		ms = append(ms, analyze.Member{Path: c})
	}
	if fi, err := os.Stat(filepath.Join(logs, liveName)); err == nil && fi.Mode().IsRegular() {
		ms = append(ms, analyze.Member{Path: filepath.Join(logs, liveName)})
	}
	return ms
}

// chunkLess orders chunk files by their number, falling back to the name.
func chunkLess(a, b string) bool {
	na, ea := strconv.ParseUint(strings.TrimSuffix(filepath.Base(a), ".jsonl"), 10, 64)
	nb, eb := strconv.ParseUint(strings.TrimSuffix(filepath.Base(b), ".jsonl"), 10, 64)
	if ea == nil && eb == nil && na != nb {
		return na < nb
	}
	return filepath.Base(a) < filepath.Base(b)
}

// isChunk reports whether p is a chunks/transcript_full/*.jsonl file.
func isChunk(p string) bool {
	dir := filepath.Dir(p)
	return filepath.Base(dir) == chunkDir && filepath.Base(filepath.Dir(dir)) == "chunks"
}
