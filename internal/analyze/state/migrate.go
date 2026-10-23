package state

import (
	"encoding/json"
	"fmt"
	"strings"
)

// File kinds with their own version line.
const (
	kindRecs     = "recommendations"
	kindIdentity = "task_identity"
	kindRun      = "run" // one runs.jsonl line
	kindArchive  = "archive"
)

// Current versions. Bump one together with a new entry in upgrades.
var current = map[string]int{
	kindRecs:     1,
	kindIdentity: 1,
	kindRun:      1,
	kindArchive:  1,
}

// upgrades[kind][n] turns a version-n document into version n+1. A version
// with no entry (and below current) has no upgrade path and fails to load.
// A missing "v" is version 0.
var upgrades = map[string]map[int]func(map[string]any) error{
	kindRecs: {0: recsV0toV1},
}

// upgrade decodes a document of kind, upgrades it step by step to the
// current version and returns it re-encoded with "v" set.
func upgrade(kind string, b []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("%s: not a JSON object", kind)
	}
	v := 0
	if raw, ok := doc["v"]; ok {
		f, ok := raw.(float64)
		if !ok || f != float64(int(f)) || f < 0 {
			return nil, fmt.Errorf("%s: invalid version %v", kind, raw)
		}
		v = int(f)
	}
	cur := current[kind]
	if v > cur {
		return nil, fmt.Errorf("%s: version %d is newer than this cbrt understands (%d)", kind, v, cur)
	}
	if v == cur {
		return b, nil
	}
	for ; v < cur; v++ {
		step := upgrades[kind][v]
		if step == nil {
			return nil, fmt.Errorf("%s: no upgrade from version %d", kind, v)
		}
		if err := step(doc); err != nil {
			return nil, fmt.Errorf("%s: upgrading version %d: %w", kind, v, err)
		}
	}
	doc["v"] = cur
	return json.Marshal(doc)
}

// recsV0toV1 upgrades the pre-release recommendations shape:
//
//	{"recs": [{"id", "status": proposed|applied|declined|reverted|improved|
//	  no_change|worsened|inconclusive, "applied_at", "notes", "evidence": "a, b",
//	  "baseline_metric", "baseline_value", ...}]}
//
// v0 had one status; v1 splits it into the user's impl_status and the
// reviewer's eval_status. An evaluated v0 item was necessarily applied.
func recsV0toV1(doc map[string]any) error {
	items, _ := doc["recs"].([]any)
	delete(doc, "recs")
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return fmt.Errorf("recommendation is not an object")
		}
		rename := func(from, to string) {
			if x, ok := m[from]; ok {
				m[to] = x
				delete(m, from)
			}
		}
		rename("id", "rec_id")
		rename("applied_at", "impl_at")
		rename("notes", "impl_notes")
		status, _ := m["status"].(string)
		delete(m, "status")
		switch status {
		case "", ImplProposed:
			m["impl_status"], m["eval_status"] = ImplProposed, EvalPending
		case ImplApplied, ImplDeclined, ImplReverted:
			m["impl_status"], m["eval_status"] = status, EvalPending
		case "improved":
			m["impl_status"], m["eval_status"] = ImplApplied, EvalImproved
		case EvalNoChange, EvalWorsened, EvalInconclusive:
			m["impl_status"], m["eval_status"] = ImplApplied, status
		default:
			return fmt.Errorf("unknown status %q", status)
		}
		if s, ok := m["evidence"].(string); ok {
			var refs []any
			for _, p := range strings.Split(s, ",") {
				if p = strings.TrimSpace(p); p != "" {
					refs = append(refs, p)
				}
			}
			m["evidence_refs"] = refs
			delete(m, "evidence")
		}
		bl := map[string]any{}
		if x, ok := m["baseline_metric"]; ok {
			bl["metric"] = x
			delete(m, "baseline_metric")
		}
		if x, ok := m["baseline_value"]; ok {
			bl["value"] = x
			delete(m, "baseline_value")
		}
		if len(bl) > 0 {
			m["baseline"] = bl
		}
	}
	if items == nil {
		items = []any{}
	}
	doc["items"] = items
	return nil
}
