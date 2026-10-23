// Package codex renders gateway responses in the shapes the OpenAI Codex CLI
// expects where they differ from the plain OpenAI API.
//
// Codex (codex-rs, checked against tag rust-v0.154.0) calls
// GET {base_url}/models?client_version=X.Y.Z and decodes the body as
// codex_protocol::openai_models::ModelsResponse ({"models":[ModelInfo...]}),
// not as the OpenAI {"object":"list","data":[...]} list. See
// codex-rs/protocol/src/openai_models.rs and
// codex-rs/codex-api/src/endpoint/models.rs.
package codex

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// Model describes one route/model the gateway exposes to Codex.
type Model struct {
	// Slug is the id Codex sends back as "model", e.g. a route alias
	// "coder" or "nvidia:openai/gpt-oss-20b".
	Slug        string
	DisplayName string
	// ContextWindow is in tokens; 0 = unknown, and the field is omitted so
	// Codex does not derive an auto-compaction limit from a guess.
	ContextWindow     int
	SupportsReasoning bool
	// BaseInstructions is the system prompt Codex sends as Responses
	// "instructions" for this model. Empty means DefaultBaseInstructions.
	// Codex 0.154.0 rejects the whole /models response when a model has
	// neither base_instructions nor model_messages.instructions_template,
	// so something must always be sent. A user's model_instructions_file
	// in config.toml still overrides it on the Codex side.
	BaseInstructions string
}

// DefaultBaseInstructions is a short system prompt written for this gateway.
// It is not OpenAI's bundled Codex prompt. The tool definitions (exec_command,
// apply_patch with its grammar, update_plan, ...) carry their own
// descriptions, so this prompt only sets working conventions.
const DefaultBaseInstructions = `You are a coding agent running in the Codex CLI on the user's machine. You and the user share the same workspace.

- Work autonomously until the task is fully resolved, then end your turn with a brief summary of what you changed and anything left to do.
- Use the provided tools to read files, run commands and edit code. Prefer fast search tools such as rg. Do not guess at file contents; read them.
- Edit files with the apply_patch tool when it is available. Keep changes minimal and focused, follow the existing code style, and do not add license headers or unrelated refactors.
- Run the relevant tests or build after changes when that is practical, and fix failures you introduced.
- Never run destructive commands (for example git reset --hard or rm -rf) unless the user explicitly asked for them.
- Be concise. Refer to files by path, and quote code only when it helps.`

// Per-model defaults:
//   - priority: Codex sorts ascending (lower = preferred); bundled OpenAI
//     models use 1..N and the unknown-model fallback uses 99, so 50 is neutral.
//   - truncation: the same 10,000-byte tool-output cap Codex's own fallback
//     metadata uses. It is safe for small context windows.
const (
	defaultPriority        = 50
	defaultTruncationMode  = "bytes"
	defaultTruncationLimit = 10000
)

// IsCodexRequest reports whether a GET /v1/models request comes from Codex
// (client_version query param, or Originator header starting with "codex").
func IsCodexRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.URL != nil && r.URL.Query().Has("client_version") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Originator")), "codex")
}

type reasoningPreset struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type truncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int64  `json:"limit"`
}

// modelInfo mirrors the fields of codex_protocol::openai_models::ModelInfo
// (rust-v0.154.0) that we set. Every field serde requires is always emitted,
// and so are the Option fields without #[serde(default)], for robustness.
// Unknown fields are ignored by Codex (no deny_unknown_fields).
type modelInfo struct {
	Slug                      string            `json:"slug"`
	DisplayName               string            `json:"display_name"`
	Description               *string           `json:"description"`
	DefaultReasoningLevel     string            `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels  []reasoningPreset `json:"supported_reasoning_levels"`
	ShellType                 string            `json:"shell_type"`
	Visibility                string            `json:"visibility"`
	SupportedInAPI            bool              `json:"supported_in_api"`
	Priority                  int               `json:"priority"`
	AvailabilityNux           *struct{}         `json:"availability_nux"`
	Upgrade                   *struct{}         `json:"upgrade"`
	BaseInstructions          string            `json:"base_instructions"`
	IncludeSkillsUsage        bool              `json:"include_skills_usage_instructions"`
	IncludePluginUsage        bool              `json:"include_plugin_usage_instructions"`
	IncludeAppsUsage          bool              `json:"include_apps_usage_instructions"`
	SupportsReasoningSummary  bool              `json:"supports_reasoning_summary_parameter"`
	DefaultReasoningSummary   string            `json:"default_reasoning_summary"`
	SupportVerbosity          bool              `json:"support_verbosity"`
	DefaultVerbosity          *string           `json:"default_verbosity"`
	ApplyPatchToolType        *string           `json:"apply_patch_tool_type"`
	WebSearchToolType         string            `json:"web_search_tool_type"`
	TruncationPolicy          truncationPolicy  `json:"truncation_policy"`
	ContextWindow             *int64            `json:"context_window,omitempty"`
	EffectiveContextWindowPct int64             `json:"effective_context_window_percent"`
	ExperimentalSupported     []string          `json:"experimental_supported_tools"`
	InputModalities           []string          `json:"input_modalities"`
	SupportsSearchTool        bool              `json:"supports_search_tool"`
	SupportsParallelToolCalls bool              `json:"supports_parallel_tool_calls"`
}

type modelsResponse struct {
	Models []modelInfo `json:"models"`
}

var reasoningLevels = []reasoningPreset{
	{Effort: "low", Description: "Fast responses with lighter reasoning"},
	{Effort: "medium", Description: "Balances speed and reasoning depth"},
	{Effort: "high", Description: "Greater reasoning depth for complex problems"},
}

func toModelInfo(m Model) modelInfo {
	freeform := "freeform"
	display := m.DisplayName
	if display == "" {
		display = m.Slug
	}
	instr := m.BaseInstructions
	if instr == "" {
		instr = DefaultBaseInstructions
	}
	desc := "Served by the cbrt gateway"
	mi := modelInfo{
		Slug:                      m.Slug,
		DisplayName:               display,
		Description:               &desc,
		SupportedReasoningLevels:  []reasoningPreset{},
		ShellType:                 "unified_exec",
		Visibility:                "list",
		SupportedInAPI:            true,
		Priority:                  defaultPriority,
		BaseInstructions:          instr,
		DefaultReasoningSummary:   "none",
		ApplyPatchToolType:        &freeform,
		WebSearchToolType:         "text",
		TruncationPolicy:          truncationPolicy{Mode: defaultTruncationMode, Limit: defaultTruncationLimit},
		EffectiveContextWindowPct: 95,
		ExperimentalSupported:     []string{},
		InputModalities:           []string{"text"},
	}
	if m.SupportsReasoning {
		mi.DefaultReasoningLevel = "medium"
		mi.SupportedReasoningLevels = append([]reasoningPreset(nil), reasoningLevels...)
		mi.SupportsReasoningSummary = true
		mi.DefaultReasoningSummary = "auto"
	}
	if m.ContextWindow > 0 {
		cw := int64(m.ContextWindow)
		mi.ContextWindow = &cw
	}
	return mi
}

// ModelsJSON renders the Codex `/models` response body for the given models.
// Models are sorted by slug, and duplicate slugs keep the first occurrence,
// so the output is deterministic whatever order the caller passes them in.
// Entries with an empty slug are skipped.
func ModelsJSON(models []Model) []byte {
	sorted := make([]Model, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if m.Slug == "" || seen[m.Slug] {
			continue
		}
		seen[m.Slug] = true
		sorted = append(sorted, m)
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Slug < sorted[j].Slug })

	resp := modelsResponse{Models: make([]modelInfo, 0, len(sorted))}
	for _, m := range sorted {
		resp.Models = append(resp.Models, toModelInfo(m))
	}
	b, err := json.Marshal(resp)
	if err != nil {
		// Only plain strings, ints and bools are marshalled; this cannot fail.
		return []byte(`{"models":[]}`)
	}
	return b
}
