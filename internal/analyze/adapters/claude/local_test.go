//go:build local

// Local checks over the user's own Claude Code store. They print counts only,
// never content, and write nothing:
//
//	go test -tags local ./internal/analyze/adapters/claude
package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/analyzetest"
)

func TestLocalStore(t *testing.T) {
	a := Adapter{}
	var roots []string
	for _, r := range a.DiscoveryRoots() {
		roots = append(roots, analyze.ExpandRoot(r))
	}
	start := time.Now()
	res := analyzetest.Run(t, a, roots, 50)

	files, empty, requests, mismatched, live := 0, 0, 0, 0, 0
	kinds := map[analyze.Kind]int{}
	partial := 0
	for _, src := range res.Sources {
		evs := res.Events[src.NativeSessionID]
		bySession := map[string][]analyze.Event{}
		for _, e := range evs {
			bySession[e.NativeSessionID] = append(bySession[e.NativeSessionID], e)
			kinds[e.Kind]++
			if e.Kind == analyze.KindModelRequest && e.TokensCoverage == analyze.Partial {
				partial++
			}
		}
		for i, m := range src.Members {
			files++
			sid := src.NativeSessionID
			if i > 0 {
				sid = subagentID(m.Path)
			}
			got := bySession[sid]
			if len(got) == 0 {
				empty++
				t.Logf("member with no events: source %d member %d", indexOf(res.Sources, src), i)
				continue
			}
			if fi, err := os.Stat(m.Path); err == nil && fi.ModTime().After(start) {
				live++ // written during the run: the adapter saw an older version
				continue
			}
			want, n := rawOutput(t, m.Path)
			sum, _, reqs := analyzetest.TokenSum(got)
			requests += reqs
			have := int64(0)
			for _, p := range []*int64{sum.OutVisible, sum.OutThinking} {
				if p != nil {
					have += *p
				}
			}
			if have != want || reqs != n {
				mismatched++
				t.Errorf("source %d member %d: output buckets %d over %d requests, transcript %d over %d",
					indexOf(res.Sources, src), i, have, reqs, want, n)
			}
		}
	}
	t.Logf("sources %d, member files %d, files without events %d, model requests %d (partial %d), output mismatches %d, live files skipped %d",
		len(res.Sources), files, empty, requests, partial, mismatched, live)
	t.Logf("events by kind: %v", kinds)
	logFieldCounts(t, res)
}

// logFieldCounts logs how many events carry Text, Cwd and an answer id
// (counts only), and fails when Text appears on a kind that must not have it.
func logFieldCounts(t *testing.T, res analyzetest.Result) {
	var prompts, promptText, answers, answerText, shell, shellText, reqs, reqCorr, events, cwd int
	for _, evs := range res.Events {
		for _, e := range evs {
			events++
			if e.Cwd != "" {
				cwd++
			}
			hasText := e.Text != ""
			switch {
			case e.Kind == analyze.KindUserPrompt:
				prompts++
				promptText += btoi(hasText)
			case e.Kind == analyze.KindAssistantText:
				answers++
				answerText += btoi(hasText)
			case e.Kind == analyze.KindToolCall && e.ToolName == analyze.ToolShell:
				shell++
				shellText += btoi(hasText)
			default:
				if hasText {
					t.Errorf("Text on a %s event", e.Kind)
				}
				if e.Kind == analyze.KindModelRequest {
					reqs++
					reqCorr += btoi(e.CorrelationID != "")
				}
			}
		}
	}
	t.Logf("Text: user_prompt %d/%d, assistant_text %d/%d, shell tool_call %d/%d; model_request with CorrelationID %d/%d; Cwd %d/%d events",
		promptText, prompts, answerText, answers, shellText, shell, reqCorr, reqs, cwd, events)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func indexOf(srcs []analyze.Source, s analyze.Source) int {
	for i := range srcs {
		if srcs[i].NativeSessionID == s.NativeSessionID {
			return i
		}
	}
	return -1
}

// rawOutput is an independent reading of a transcript: per requestId of a
// real (non-synthetic) response, the largest output_tokens any of its
// records carries, summed; and the number of such requestIds. Only complete
// lines count.
func rawOutput(t *testing.T, path string) (int64, int) {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	maxOut := map[string]int64{}
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		b, err := br.ReadBytes('\n')
		if err != nil {
			break // the last piece is either empty or a partial line
		}
		var r struct {
			Type      string `json:"type"`
			UUID      string `json:"uuid"`
			RequestID string `json:"requestId"`
			APIError  bool   `json:"isApiErrorMessage"`
			Message   struct {
				Model string `json:"model"`
				Usage *struct {
					Output int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &r) != nil || r.Type != "assistant" || r.RequestID == "" || r.UUID == "" ||
			r.APIError || r.Message.Model == syntheticModel || r.Message.Usage == nil {
			continue
		}
		if o, ok := maxOut[r.RequestID]; !ok || r.Message.Usage.Output > o {
			maxOut[r.RequestID] = r.Message.Usage.Output
		}
	}
	var sum int64
	for _, o := range maxOut {
		sum += o
	}
	return sum, len(maxOut)
}
