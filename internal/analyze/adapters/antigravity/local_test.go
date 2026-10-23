//go:build local

package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/analyzetest"
)

// TestLocalStores runs the contract checks over the user's real Antigravity
// stores and logs counts per store. It prints counts only, never content,
// paths or ids. Run on the user's machine:
//
//	go test -tags local -run TestLocalStores -v ./internal/analyze/adapters/antigravity
func TestLocalStores(t *testing.T) {
	a := Adapter{}
	var roots []string
	for _, r := range a.DiscoveryRoots() {
		roots = append(roots, analyze.ExpandRoot(r))
	}
	res := analyzetest.Run(t, a, roots, 10)

	type counts struct {
		convs, compactOnly, chunked, steps, dups, malformed, partial int
		timed, results, requests, interrupts, denials, withModel     int
		u                                                            usageChecks
	}
	per := map[string]*counts{StoreIDE: {}, StoreCLI: {}}
	for _, root := range roots {
		c := per[storeForRoot(root)]
		ents, _ := os.ReadDir(root)
		for _, e := range ents {
			if !e.IsDir() || len(conversationMembers(filepath.Join(root, e.Name()))) > 0 {
				continue
			}
			if _, err := os.Stat(filepath.Join(logsDir(filepath.Join(root, e.Name())), "transcript.jsonl")); err == nil {
				c.compactOnly++
			}
		}
	}
	for _, src := range res.Sources {
		c := per[src.Store]
		c.convs++
		if len(src.Members) > 0 && isChunk(src.Members[0].Path) {
			c.chunked++
		}
		steps, st, err := readSteps(src)
		if err != nil {
			t.Errorf("a source failed to read: %v", err)
		}
		c.steps += len(steps)
		c.dups += st.Dups
		c.malformed += st.Malformed
		c.partial += st.Partial
		c.u.add(steps)
		model := false
		for _, e := range res.Events[src.NativeSessionID] {
			switch e.Kind {
			case analyze.KindToolResult:
				c.results++
				if e.DurationCoverage == analyze.Measured {
					c.timed++
				}
			case analyze.KindModelRequest:
				c.requests++
				model = model || e.Model != ""
			case analyze.KindInterrupt:
				c.interrupts++
			case analyze.KindPermissionDeny:
				c.denials++
			}
		}
		if model {
			c.withModel++
		}
	}
	for _, s := range []string{StoreIDE, StoreCLI} {
		c := per[s]
		t.Logf("%s: conversations=%d compact_only_skipped=%d chunked=%d steps=%d overlap_dups_removed=%d malformed=%d partial_tail=%d",
			s, c.convs, c.compactOnly, c.chunked, c.steps, c.dups, c.malformed, c.partial)
		t.Logf("%s: tool_results=%d with_embedded_timing=%d model_requests=%d interrupts=%d permission_denied=%d convs_with_model=%d",
			s, c.results, c.timed, c.requests, c.interrupts, c.denials, c.withModel)
		u := c.u
		t.Logf("%s: planner_steps=%d with_usage=%d without_usage=%d all_three_fields=%d usage_on_other_step_types=%d bad_counts=%d",
			s, u.planners, u.with, u.planners-u.with, u.allThree, u.onOther, u.bad)
		t.Logf("%s: input<cache_read %d/%d; context (input+cache_read) non-decreasing %d/%d pairs, input alone non-decreasing %d/%d; output fell in %d/%d pairs (per request, not cumulative)",
			s, u.inBelowCR, u.with, u.ctxUp, u.pairs, u.inUp, u.pairs, u.outDown, u.pairs)
		t.Logf("%s: output >= volume of text+args+thinking %d/%d steps with thinking; output >= volume of text+args %d/%d",
			s, u.outCoversThinking, u.thinking, u.outCoversVisible, u.with)
		if u.bad > 0 {
			t.Errorf("%s: %d usage fields are not non-negative integers", s, u.bad)
		}
	}
}

// usageChecks gathers the evidence behind usage(): counts only.
type usageChecks struct {
	planners, with, allThree, onOther, bad        int
	inBelowCR, pairs, ctxUp, inUp, outDown        int
	thinking, outCoversThinking, outCoversVisible int
}

func (u *usageChecks) add(steps []step) {
	var prevIn, prevCtx, prevOut int64 = -1, -1, -1
	for i := range steps {
		s := &steps[i]
		has := len(s.InputTokens)+len(s.CacheReadTokens)+len(s.OutputTokens) > 0
		if s.Type != "PLANNER_RESPONSE" {
			if has {
				u.onOther++
			}
			continue
		}
		u.planners++
		if !has {
			continue
		}
		u.with++
		in, ok1 := count(s.InputTokens)
		cr, ok2 := count(s.CacheReadTokens)
		out, ok3 := count(s.OutputTokens)
		for _, f := range []json.RawMessage{s.InputTokens, s.CacheReadTokens, s.OutputTokens} {
			if _, ok := count(f); len(f) > 0 && !ok {
				u.bad++
			}
		}
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		u.allThree++
		if in < cr {
			u.inBelowCR++
		}
		if prevCtx >= 0 {
			u.pairs++
			if in+cr >= prevCtx {
				u.ctxUp++
			}
			if in >= prevIn {
				u.inUp++
			}
			if out < prevOut {
				u.outDown++
			}
		}
		prevIn, prevCtx, prevOut = in, in+cr, out
		visible := analyze.TextVolume(s.Content)
		for _, tc := range s.ToolCalls {
			visible += analyze.TextVolume(string(tc.Args))
		}
		if out >= int64(visible) {
			u.outCoversVisible++
		}
		if s.Thinking != "" {
			u.thinking++
			if out >= int64(visible+analyze.TextVolume(s.Thinking)) {
				u.outCoversThinking++
			}
		}
	}
}
