//go:build local

package codex

import (
	"testing"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/analyzetest"
)

// TestLocalFields logs how many events carry Text, Cwd and an answer id
// (counts only), and fails when Text appears on a kind that must not have it.
func TestLocalFields(t *testing.T) {
	a := Adapter{}
	res := analyzetest.Run(t, a, a.DiscoveryRoots(), 25)
	noText := map[string]int{}
	var prompts, promptText, answers, answerText, shell, shellText, reqs, reqCorr, events, cwd int
	for _, evs := range res.Events {
		for _, e := range evs {
			events++
			if e.Cwd != "" {
				cwd++
			}
			has := 0
			if e.Text != "" {
				has = 1
			}
			switch {
			case e.Kind == analyze.KindUserPrompt:
				prompts++
				promptText += has
			case e.Kind == analyze.KindAssistantText:
				answers++
				answerText += has
			case e.Kind == analyze.KindToolCall && e.ToolName == analyze.ToolShell:
				shell++
				shellText += has
				if has == 0 {
					noText[e.ToolNameRaw]++
				}
			default:
				if has == 1 {
					t.Errorf("Text on a %s event", e.Kind)
				}
				if e.Kind == analyze.KindModelRequest {
					reqs++
					if e.CorrelationID != "" {
						reqCorr++
					}
				}
			}
		}
	}
	t.Logf("Text: user_prompt %d/%d, assistant_text %d/%d, shell tool_call %d/%d; model_request with CorrelationID %d/%d; Cwd %d/%d events",
		promptText, prompts, answerText, answers, shellText, shell, reqCorr, reqs, cwd, events)
	t.Logf("shell tool_calls without a command line, by tool: %v", noText)
}
