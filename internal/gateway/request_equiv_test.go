package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alex-danilin/cbrt/internal/api/anthropic"
	"github.com/alex-danilin/cbrt/internal/api/responses"
	"github.com/alex-danilin/cbrt/internal/config"
)

// ---- reference implementation: the multi-pass pipeline before the single-pass rewrite ----

type refRequest struct {
	fields             map[string]json.RawMessage
	Model              string
	Stream             bool
	ClientWantsUsage   bool
	Needs              []string
	ToolChoiceRequired bool
	EstPromptTokens    int
	MaxOut             int
	tokenField         string
}

func refParseChat(body []byte) (*refRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, errBadRequest
	}
	r := &refRequest{fields: fields}
	if err := json.Unmarshal(fields["model"], &r.Model); err != nil || r.Model == "" {
		return nil, errBadRequest
	}
	// exact (case-sensitive) keys, like the provider APIs: encoding/json struct
	// decoding would match "Content" or "tYpe" case-insensitively
	var msgs []map[string]json.RawMessage
	if raw, ok := fields["messages"]; !ok || json.Unmarshal(raw, &msgs) != nil || len(msgs) == 0 {
		return nil, errBadRequest
	}
	if raw, ok := fields["stream"]; ok {
		if err := json.Unmarshal(raw, &r.Stream); err != nil {
			return nil, errBadRequest
		}
	}
	if raw, ok := fields["stream_options"]; ok {
		var so map[string]json.RawMessage
		json.Unmarshal(raw, &so)
		json.Unmarshal(so["include_usage"], &r.ClientWantsUsage)
	}
	if raw, ok := fields["tools"]; ok && string(raw) != "null" && string(raw) != "[]" {
		r.Needs = append(r.Needs, "tools")
	}
	if raw, ok := fields["tool_choice"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s == "required" {
			r.ToolChoiceRequired = true
		}
	}
	images, imageBytes := 0, 0
	for _, m := range msgs {
		var parts []json.RawMessage
		if c := m["content"]; len(c) > 0 && c[0] == '[' && json.Unmarshal(c, &parts) == nil {
			for _, raw := range parts {
				var p map[string]json.RawMessage
				if json.Unmarshal(raw, &p) != nil {
					continue
				}
				if ty := string(p["type"]); ty == `"image_url"` || ty == `"input_image"` || ty == `"image"` {
					images++
					u := p["image_url"]
					var obj map[string]json.RawMessage
					if len(u) > 0 && u[0] == '{' && json.Unmarshal(u, &obj) == nil {
						u = obj["url"]
					}
					if strings.HasPrefix(string(u), `"data:`) {
						imageBytes += len(u)
					}
				}
			}
		}
	}
	if images > 0 {
		r.Needs = append(r.Needs, "vision")
	}
	r.EstPromptTokens = estimateTokens(len(body)-imageBytes) + images*imageTokenEstimate
	if raw, ok := fields["response_format"]; ok {
		var rf map[string]json.RawMessage
		if json.Unmarshal(raw, &rf) == nil && (string(rf["type"]) == `"json_object"` || string(rf["type"]) == `"json_schema"`) {
			r.Needs = append(r.Needs, "json")
		}
	}
	for _, f := range []string{"max_completion_tokens", "max_tokens"} {
		if raw, ok := fields[f]; ok {
			if json.Unmarshal(raw, &r.MaxOut) == nil {
				r.tokenField = f
				break
			}
		}
	}
	return r, nil
}

func (r *refRequest) upstreamBody(t config.Target, maxContext int) []byte {
	m := make(map[string]json.RawMessage, len(r.fields)+1)
	for k, v := range r.fields {
		m[k] = v
	}
	m["model"], _ = json.Marshal(t.Model)
	if t.MidSystem != "keep" {
		if msgs, changed := refNormalizeSystem(m["messages"]); changed {
			m["messages"] = msgs
		}
	}
	if r.Stream && t.WantsStreamUsage() {
		so := map[string]json.RawMessage{}
		if raw, ok := m["stream_options"]; ok {
			json.Unmarshal(raw, &so)
		}
		so["include_usage"] = json.RawMessage("true")
		m["stream_options"], _ = json.Marshal(so)
	}
	maxOut, field := r.MaxOut, r.tokenField
	if maxContext > 0 && maxOut > 0 && r.EstPromptTokens+maxOut > maxContext {
		maxOut = max(1, maxContext-r.EstPromptTokens)
	}
	if t.TokenParam != "" && field != "" && field != t.TokenParam {
		delete(m, field)
		field = t.TokenParam
	}
	if field != "" {
		m[field], _ = json.Marshal(maxOut)
	}
	b, _ := json.Marshal(m)
	return b
}

func refNormalizeSystem(raw json.RawMessage) (json.RawMessage, bool) {
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return raw, false
	}
	role := func(m map[string]json.RawMessage) string {
		var r string
		json.Unmarshal(m["role"], &r)
		return r
	}
	leading := 0
	for leading < len(msgs) && role(msgs[leading]) == "system" {
		leading++
	}
	late := false
	for _, m := range msgs[leading:] {
		if role(m) == "system" {
			late = true
			break
		}
	}
	if !late && leading < 2 {
		return raw, false
	}
	var sys []string
	for _, m := range msgs[:leading] {
		sys = append(sys, contentText(m["content"]))
	}
	out := make([]map[string]json.RawMessage, 0, len(msgs))
	seenAssistant := false
	for _, m := range msgs[leading:] {
		switch role(m) {
		case "assistant":
			seenAssistant = true
		case "system":
			text := contentText(m["content"])
			if !seenAssistant {
				sys = append(sys, text)
				continue
			}
			reminder := "<system-reminder>\n" + text + "\n</system-reminder>"
			if n := len(out); n > 0 && role(out[n-1]) == "user" {
				var prev string
				if json.Unmarshal(out[n-1]["content"], &prev) == nil {
					merged := make(map[string]json.RawMessage, len(out[n-1]))
					for k, v := range out[n-1] {
						merged[k] = v
					}
					merged["content"], _ = json.Marshal(prev + "\n\n" + reminder)
					out[n-1] = merged
					continue
				}
			}
			rem, _ := json.Marshal(reminder)
			out = append(out, map[string]json.RawMessage{"role": json.RawMessage(`"user"`), "content": rem})
			continue
		}
		out = append(out, m)
	}
	if len(sys) > 0 {
		c, _ := json.Marshal(strings.Join(sys, "\n\n"))
		out = append([]map[string]json.RawMessage{{"role": json.RawMessage(`"system"`), "content": c}}, out...)
	}
	b, _ := json.Marshal(out)
	return b, true
}

// ---- equivalence checks ----

var equivTargets = func() []config.Target {
	no := false
	return []config.Target{
		{Model: "x"},
		{Model: "x", MidSystem: "keep"},
		{Model: "y", TokenParam: "max_tokens", StreamUsage: &no},
		{Model: "z", TokenParam: "max_completion_tokens"},
	}
}()

func checkEquivalent(t *testing.T, name string, body []byte) {
	t.Helper()
	got, gerr := parseChat(body)
	ref, rerr := refParseChat(body)
	if (gerr == nil) != (rerr == nil) {
		t.Fatalf("%s: parse error mismatch: new=%v ref=%v", name, gerr, rerr)
	}
	if gerr != nil {
		return
	}
	if got.Model != ref.Model || got.Stream != ref.Stream || got.ClientWantsUsage != ref.ClientWantsUsage ||
		!reflect.DeepEqual(got.Needs, ref.Needs) || got.ToolChoiceRequired != ref.ToolChoiceRequired ||
		got.EstPromptTokens != ref.EstPromptTokens || got.MaxOut != ref.MaxOut || got.tokenField != ref.tokenField {
		t.Fatalf("%s: parsed fields differ:\n new %+v\n ref %+v", name, got, ref)
	}
	for _, tg := range equivTargets {
		for _, ctx := range []int{0, 1 << 20, 300} {
			nb, err := got.upstreamBody(tg, ctx)
			if err != nil {
				t.Fatal(err)
			}
			var a, b any
			if err := json.Unmarshal(nb, &a); err != nil {
				t.Fatalf("%s: new upstream body is not valid JSON: %v\n%s", name, err, nb)
			}
			json.Unmarshal(ref.upstreamBody(tg, ctx), &b)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("%s: upstream body differs for target %+v ctx %d:\n new %s\n ref %s", name, tg, ctx, nb, ref.upstreamBody(tg, ctx))
			}
		}
	}
}

func equivCaptureBody(t testing.TB, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		RequestBody json.RawMessage `json:"request_body"`
	}
	json.Unmarshal(raw, &ex)
	return ex.RequestBody
}

func TestSinglePassEquivalentOnCaptures(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/clients/claude-code-*/*v1_messages.json")
	for _, f := range files {
		chat, _, err := anthropic.DecodeRequest(equivCaptureBody(t, f))
		if err != nil {
			t.Fatal(err)
		}
		checkEquivalent(t, filepath.Base(f), chat)
	}
	files, _ = filepath.Glob("../../testdata/clients/codex-*/*v1_responses*.json") // incl. -apply-patch, -compaction
	for _, f := range files {
		chat, _, err := responses.DecodeRequest(equivCaptureBody(t, f))
		if err != nil {
			t.Fatal(err)
		}
		checkEquivalent(t, filepath.Base(f), chat)
	}
	if len(files) == 0 {
		t.Fatal("no captures found")
	}
}

var equivSeeds = []string{
	`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","stream":true,"stream_options":{"include_usage":false,"x":1},"max_completion_tokens":50,"messages":[{"role":"user","content":"hi"}],"x_unknown":{"a":[1,2]}}`,
	`{"model":"m","messages":[{"role":"system","content":"A"},{"role":"system","content":[{"type":"text","text":"B"}]},{"role":"user","content":"q"},{"role":"system","content":"E"},{"role":"assistant","content":"a"},{"role":"user","content":"u","name":"n"},{"role":"system","content":"R1"},{"role":"system","content":"R2"},{"role":"assistant","content":null,"tool_calls":[{"id":"c"}]},{"role":"tool","tool_call_id":"c","content":"r"},{"role":"system","content":"T"},{"role":"system","content":"T2"}],"max_tokens":32000}`,
	`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:"}}]}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"required","response_format":{"type":"json_schema"}}`,
	`{"model":"m","messages":[{"role":"user","content":"image of a cat"}],"tools":[]}`,
	`{"model":"m","max_tokens":9000,"messages":[{"role":"user","content":[1,{"type":"input_image","image_url":"data:image/png;base64,AAAA"},{"type":"image_url","image_url":{"url":"https://x/y.png"}},{"type":"image"}]},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,BBBBBBBB","detail":"low"}}]}]}`,
	`{"model":"m","model":"m2","messages":[{"role":"user","content":"dup keys"}],"stream":false}`,
	`{"model":"","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","messages":[]}`,
	`{"model":"m","messages":"no"}`,
	`{"model":"m","messages":[1,"x",null]}`,
	`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":"yes"}`,
	`[1]`,
}

func TestSinglePassEquivalentOnSeeds(t *testing.T) {
	for i, s := range equivSeeds {
		checkEquivalent(t, "seed"+string(rune('A'+i)), []byte(s))
	}
}

func FuzzSinglePassEquivalent(f *testing.F) {
	for _, s := range equivSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkEquivalent(t, "fuzz", body)
	})
}
