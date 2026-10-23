package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// rustModelInfo mirrors the fields serde requires when Codex rust-v0.154.0
// decodes codex_protocol::openai_models::ModelInfo. These are the non-Option
// fields without #[serde(default)]. Pointers let the test tell "missing"
// apart from "zero value". Codex also requires base_instructions or
// model_messages.instructions_template, checked separately.
type rustModelInfo struct {
	Slug                     *string `json:"slug"`
	DisplayName              *string `json:"display_name"`
	SupportedReasoningLevels *[]struct {
		Effort      *string `json:"effort"`
		Description *string `json:"description"`
	} `json:"supported_reasoning_levels"`
	ShellType        *string `json:"shell_type"`
	Visibility       *string `json:"visibility"`
	SupportedInAPI   *bool   `json:"supported_in_api"`
	Priority         *int32  `json:"priority"`
	SupportVerbosity *bool   `json:"support_verbosity"`
	TruncationPolicy *struct {
		Mode  *string `json:"mode"`
		Limit *int64  `json:"limit"`
	} `json:"truncation_policy"`
	ExperimentalSupportedTools *[]string `json:"experimental_supported_tools"`

	// Optional fields whose values we check.
	BaseInstructions *string `json:"base_instructions"`
	ModelMessages    *struct {
		InstructionsTemplate *string `json:"instructions_template"`
	} `json:"model_messages"`
	DefaultReasoningLevel    *string  `json:"default_reasoning_level"`
	ApplyPatchToolType       *string  `json:"apply_patch_tool_type"`
	ContextWindow            *int64   `json:"context_window"`
	InputModalities          []string `json:"input_modalities"`
	SupportsReasoningSummary *bool    `json:"supports_reasoning_summary_parameter"`
	DefaultReasoningSummary  *string  `json:"default_reasoning_summary"`
	WebSearchToolType        *string  `json:"web_search_tool_type"`
}

type rustModelsResponse struct {
	Models *[]rustModelInfo `json:"models"`
}

var (
	shellTypes      = map[string]bool{"unified_exec": true, "disabled": true, "default": true, "local": true, "shell_command": true}
	visibilities    = map[string]bool{"list": true, "hide": true, "none": true}
	truncModes      = map[string]bool{"bytes": true, "tokens": true}
	applyPatchTypes = map[string]bool{"freeform": true}
	efforts         = map[string]bool{"none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	summaries       = map[string]bool{"auto": true, "concise": true, "detailed": true, "none": true}
	webSearchTypes  = map[string]bool{"text": true, "text_and_image": true}
	modalities      = map[string]bool{"text": true, "image": true, "audio": true}
)

// validate fails the way Codex's serde decode would: a missing required
// field, a wrong JSON type, or an unknown enum value.
func validate(body []byte) ([]rustModelInfo, error) {
	var resp rustModelsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode: %v", err)
	}
	if resp.Models == nil {
		return nil, errors.New("missing field `models`")
	}
	for i, m := range *resp.Models {
		req := []struct {
			name string
			ok   bool
		}{
			{"slug", m.Slug != nil},
			{"display_name", m.DisplayName != nil},
			{"supported_reasoning_levels", m.SupportedReasoningLevels != nil},
			{"shell_type", m.ShellType != nil},
			{"visibility", m.Visibility != nil},
			{"supported_in_api", m.SupportedInAPI != nil},
			{"priority", m.Priority != nil},
			{"support_verbosity", m.SupportVerbosity != nil},
			{"truncation_policy", m.TruncationPolicy != nil && m.TruncationPolicy.Mode != nil && m.TruncationPolicy.Limit != nil},
			{"experimental_supported_tools", m.ExperimentalSupportedTools != nil},
			{"base_instructions|model_messages.instructions_template", m.BaseInstructions != nil ||
				(m.ModelMessages != nil && m.ModelMessages.InstructionsTemplate != nil)},
		}
		for _, r := range req {
			if !r.ok {
				return nil, fmt.Errorf("model %d: missing required field %q", i, r.name)
			}
		}
		checks := []struct {
			field string
			v     *string
			ok    map[string]bool
		}{
			{"shell_type", m.ShellType, shellTypes},
			{"visibility", m.Visibility, visibilities},
			{"truncation_policy.mode", m.TruncationPolicy.Mode, truncModes},
			{"apply_patch_tool_type", m.ApplyPatchToolType, applyPatchTypes},
			{"default_reasoning_level", m.DefaultReasoningLevel, efforts},
			{"default_reasoning_summary", m.DefaultReasoningSummary, summaries},
			{"web_search_tool_type", m.WebSearchToolType, webSearchTypes},
		}
		for _, p := range *m.SupportedReasoningLevels {
			if p.Effort == nil || p.Description == nil {
				return nil, fmt.Errorf("model %d: reasoning preset missing effort/description", i)
			}
			checks = append(checks, struct {
				field string
				v     *string
				ok    map[string]bool
			}{"effort", p.Effort, efforts})
		}
		for _, mod := range m.InputModalities {
			checks = append(checks, struct {
				field string
				v     *string
				ok    map[string]bool
			}{"input_modalities", &mod, modalities})
		}
		for _, c := range checks {
			if c.v != nil && !c.ok[*c.v] {
				return nil, fmt.Errorf("model %d: %s value %q not accepted by Codex", i, c.field, *c.v)
			}
		}
	}
	return *resp.Models, nil
}

func decodeStrict(t *testing.T, body []byte) []rustModelInfo {
	t.Helper()
	models, err := validate(body)
	if err != nil {
		t.Fatalf("%v\nbody: %s", err, body)
	}
	return models
}

func TestModelsJSONSchema(t *testing.T) {
	body := ModelsJSON([]Model{
		{Slug: "coder", DisplayName: "Coder", ContextWindow: 131072, SupportsReasoning: true},
		{Slug: "nvidia:openai/gpt-oss-20b"},
	})
	models := decodeStrict(t, body)
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	for _, m := range models {
		if *m.Visibility != "list" || !*m.SupportedInAPI || *m.ShellType != "unified_exec" {
			t.Errorf("%s: unexpected visibility/api/shell: %s %v %s", *m.Slug, *m.Visibility, *m.SupportedInAPI, *m.ShellType)
		}
		if m.ApplyPatchToolType == nil || *m.ApplyPatchToolType != "freeform" {
			t.Errorf("%s: apply_patch_tool_type = %v, want freeform", *m.Slug, m.ApplyPatchToolType)
		}
		if *m.SupportVerbosity {
			t.Errorf("%s: support_verbosity must be false for non-OpenAI models", *m.Slug)
		}
		if m.BaseInstructions == nil || *m.BaseInstructions != DefaultBaseInstructions {
			t.Errorf("%s: expected default base_instructions", *m.Slug)
		}
		if len(m.InputModalities) != 1 || m.InputModalities[0] != "text" {
			t.Errorf("%s: input_modalities = %v, want [text]", *m.Slug, m.InputModalities)
		}
	}
	if strings.Contains(string(body), `"data"`) || strings.Contains(string(body), `"object"`) {
		t.Errorf("body must not use the OpenAI list shape: %s", body)
	}
}

func TestMissingRequiredFieldIsDetected(t *testing.T) {
	// Sanity-check the mirror itself: dropping a required field must fail decodeStrict.
	var raw map[string][]map[string]any
	if err := json.Unmarshal(ModelsJSON([]Model{{Slug: "x"}}), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"slug", "shell_type", "truncation_policy", "experimental_supported_tools", "base_instructions"} {
		m := map[string]any{}
		for k, v := range raw["models"][0] {
			m[k] = v
		}
		delete(m, field)
		b, _ := json.Marshal(map[string]any{"models": []any{m}})
		if _, err := validate(b); err == nil {
			t.Errorf("removing %q was not detected", field)
		}
	}
	// An unknown enum value must be rejected too.
	bad := bytes.Replace(ModelsJSON([]Model{{Slug: "x"}}), []byte(`"unified_exec"`), []byte(`"shell"`), 1)
	if _, err := validate(bad); err == nil {
		t.Error("unknown shell_type was not detected")
	}
}

func TestReasoningLevels(t *testing.T) {
	models := decodeStrict(t, ModelsJSON([]Model{
		{Slug: "a", SupportsReasoning: true},
		{Slug: "b", SupportsReasoning: false},
	}))
	a, b := models[0], models[1]
	if len(*a.SupportedReasoningLevels) != 3 || a.DefaultReasoningLevel == nil || *a.DefaultReasoningLevel != "medium" {
		t.Errorf("reasoning model: levels=%v default=%v", a.SupportedReasoningLevels, a.DefaultReasoningLevel)
	}
	if !*a.SupportsReasoningSummary || *a.DefaultReasoningSummary != "auto" {
		t.Errorf("reasoning model: summary param should be enabled")
	}
	if len(*b.SupportedReasoningLevels) != 0 || b.DefaultReasoningLevel != nil {
		t.Errorf("non-reasoning model must have no levels/default: %v %v", b.SupportedReasoningLevels, b.DefaultReasoningLevel)
	}
	// supports_reasoning_summary_parameter defaults to true in Codex, so it must be sent explicitly as false.
	if b.SupportsReasoningSummary == nil || *b.SupportsReasoningSummary || *b.DefaultReasoningSummary != "none" {
		t.Errorf("non-reasoning model must disable reasoning summary: %v", b.SupportsReasoningSummary)
	}
}

func TestContextWindowPropagation(t *testing.T) {
	models := decodeStrict(t, ModelsJSON([]Model{
		{Slug: "big", ContextWindow: 131072},
		{Slug: "unknown", ContextWindow: 0},
	}))
	if models[0].ContextWindow == nil || *models[0].ContextWindow != 131072 {
		t.Errorf("big: context_window = %v, want 131072", models[0].ContextWindow)
	}
	if models[1].ContextWindow != nil {
		t.Errorf("unknown: context_window should be omitted, got %d", *models[1].ContextWindow)
	}
}

func TestDisplayNameAndInstructionsOverride(t *testing.T) {
	models := decodeStrict(t, ModelsJSON([]Model{{Slug: "s", BaseInstructions: "custom"}}))
	if *models[0].DisplayName != "s" {
		t.Errorf("display_name should default to slug, got %q", *models[0].DisplayName)
	}
	if *models[0].BaseInstructions != "custom" {
		t.Errorf("base_instructions override lost: %q", *models[0].BaseInstructions)
	}
}

func TestDeterministicOrdering(t *testing.T) {
	a := ModelsJSON([]Model{{Slug: "zeta"}, {Slug: "alpha"}, {Slug: "mid"}, {Slug: "alpha", DisplayName: "dup"}, {Slug: ""}})
	b := ModelsJSON([]Model{{Slug: "mid"}, {Slug: "alpha"}, {Slug: "zeta"}})
	if !bytes.Equal(a, b) {
		t.Fatalf("output depends on input order:\n%s\n%s", a, b)
	}
	models := decodeStrict(t, a)
	var got []string
	for _, m := range models {
		got = append(got, *m.Slug)
	}
	if strings.Join(got, ",") != "alpha,mid,zeta" {
		t.Errorf("order = %v", got)
	}
	if !bytes.Equal(a, ModelsJSON([]Model{{Slug: "zeta"}, {Slug: "alpha"}, {Slug: "mid"}})) {
		t.Error("repeated call not byte-identical")
	}
}

func TestEmptyModels(t *testing.T) {
	body := ModelsJSON(nil)
	if string(body) != `{"models":[]}` {
		t.Errorf("empty = %s", body)
	}
	decodeStrict(t, body)
}

func TestIsCodexRequest(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		originator string
		want       bool
	}{
		{"client_version query", "/v1/models?client_version=0.154.0", "", true},
		{"empty client_version still codex", "/v1/models?client_version=", "", true},
		{"originator codex_exec", "/v1/models", "codex_exec", true},
		{"originator codex_cli_rs", "/v1/models", "codex_cli_rs", true},
		{"originator uppercase", "/v1/models", "Codex_VSCode", true},
		{"plain openai client", "/v1/models", "", false},
		{"other originator", "/v1/models", "opencode", false},
		{"unrelated query", "/v1/models?version=1", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", c.target, nil)
			if c.originator != "" {
				r.Header.Set("Originator", c.originator)
			}
			if got := IsCodexRequest(r); got != c.want {
				t.Errorf("IsCodexRequest = %v, want %v", got, c.want)
			}
		})
	}
	if IsCodexRequest(nil) {
		t.Error("nil request must not be codex")
	}
}
