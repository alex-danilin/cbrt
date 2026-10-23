//go:build local

// Real-data checks over the user's own Codex store. Run on the user's
// machine only: go test -tags local ./internal/analyze/adapters/codex -v
// Everything printed is a count; no value from a rollout is printed.
package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/analyzetest"
)

func TestLocalStore(t *testing.T) {
	a := Adapter{}
	res := analyzetest.Run(t, a, a.DiscoveryRoots(), 25)
	t.Logf("sessions %d", len(res.Sources))

	inv := map[string]int{}
	var requests, withUsage, noModel, unknown, merged, byResponseID, byIndex int
	callsPerTurn := map[int]int{} // per-response turns: responses in the turn → turns
	var tcEvents, cwPresent, cwNonZero, totIsInOut, totIsInOutCw, totOther int
	for _, src := range res.Sources {
		evs := res.Events[src.NativeSessionID]
		_, with, reqs := analyzetest.TokenSum(evs)
		requests += reqs
		withUsage += with
		inv[invariant(t, src, evs)]++

		perTurn := map[string]int{}
		var turns []string
		for _, e := range evs {
			if e.Kind != analyze.KindModelRequest {
				continue
			}
			if e.Model == "" {
				noModel++ // turn_context unread: a parse regression
			}
			switch {
			case e.Tokens == nil:
				unknown++
			case e.RequestID == e.CorrelationID:
				merged++ // one request for the whole turn
			default:
				if strings.HasPrefix(e.RequestID, e.CorrelationID+"#") {
					byIndex++
				} else {
					byResponseID++
				}
				if perTurn[e.CorrelationID] == 0 {
					turns = append(turns, e.CorrelationID)
				}
				perTurn[e.CorrelationID]++
			}
		}
		for _, id := range turns {
			callsPerTurn[min(perTurn[id], 8)]++
		}

		// cache_write_input_tokens and total_tokens, straight from the file.
		f, err := os.Open(src.Members[0].Path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<28)
		for sc.Scan() {
			var r struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
					Info *struct {
						Last map[string]*int64 `json:"last_token_usage"`
					} `json:"info"`
				} `json:"payload"`
			}
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.Type != "event_msg" || r.Payload.Type != "token_count" ||
				r.Payload.Info == nil || r.Payload.Info.Last == nil {
				continue
			}
			l := r.Payload.Info.Last
			v := func(k string) int64 {
				if p := l[k]; p != nil {
					return *p
				}
				return 0
			}
			tcEvents++
			if l["cache_write_input_tokens"] != nil {
				cwPresent++
				if v("cache_write_input_tokens") != 0 {
					cwNonZero++
				}
			}
			switch v("total_tokens") {
			case v("input_tokens") + v("output_tokens"):
				totIsInOut++
			case v("input_tokens") + v("output_tokens") + v("cache_write_input_tokens"):
				totIsInOutCw++
			default:
				totOther++
			}
		}
		f.Close()
	}
	t.Logf("model_request: %d, with usage %d, unknown %d, without model %d", requests, withUsage, unknown, noModel)
	if noModel > 0 {
		t.Errorf("%d model_request events without a model", noModel)
	}
	t.Logf("per-response requests: by response_id %d, by turn index %d; merged-turn requests %d", byResponseID, byIndex, merged)
	var ks []int
	for k := range callsPerTurn {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	var dist []string
	for _, k := range ks {
		label := fmt.Sprint(k)
		if k == 8 {
			label = "8+"
		}
		dist = append(dist, fmt.Sprintf("%s:%d", label, callsPerTurn[k]))
	}
	t.Logf("responses per turn (turns): %s", strings.Join(dist, " "))
	t.Logf("session sum vs final running total: holds %d, excused by unknown turns %d, fails %d",
		inv["holds"], inv["excused"], inv["fails"])
	if inv["fails"] > 0 {
		t.Errorf("%d sessions break the sum invariant", inv["fails"])
	}
	t.Logf("token_count with usage: %d; cache_write_input_tokens present %d, non-zero %d", tcEvents, cwPresent, cwNonZero)
	t.Logf("total_tokens == input+output: %d; == input+output+cache_write (and not the former): %d; other: %d",
		totIsInOut, totIsInOutCw, totOther)
}
