package anthropic

import (
	"encoding/json"
	"reflect"
	"testing"
)

const okEnv = `"model":"m","max_tokens":5`

// equivCorpus are synthetic requests covering every mapping rule, every
// rejection and the JSON corner cases (null, duplicate and case-folded keys,
// escapes, invalid UTF-8, lone surrogates, whitespace) on which a hand-written
// decoder could diverge from encoding/json.
var equivCorpus = []string{
	// tool strict (dropped, reported when true) and a top level past the linear-scan size
	`{` + okEnv + `,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"a","input_schema":{},"strict":true},{"name":"b","input_schema":{},"strict":false},{"type":"bash_20250124","name":"bash","strict":true}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"a","input_schema":{},"Strict":true,"strict":null},{"name":"b","strict":true}]}`,
	`{"model":"x","k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10,"k11":11,"k12":12,"k13":13,"k14":14,"k15":15,"k16":16,"k17":17,` + okEnv + `,"k3":null,"k17":null,"k18":1,"messages":[{"role":"user","content":"hi"}]}`,
	// envelope
	`{` + okEnv + `,"messages":[{"role":"user","content":"hi"}]}`,
	` { "model" : "m" , "max_tokens" : 5 , "messages" : [ { "role" : "user" , "content" : "hi" } ] , "stream" : true } `,
	`{"model":"m","max_tokens":5,"max_tokens":6,"messages":[{"role":"user","content":"hi"}],"stream":null,"temperature":null,"top_p":null,"stop_sequences":null}`,
	`{"model":"m","max_tokens":1e3,"messages":[{"role":"user","content":"hi"}],"temperature":0.5,"top_p":1E-400,"stop_sequences":[]}`,
	`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"temperature":1e400}`,
	`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"temperature":"x"}`,
	`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"stop_sequences":["a","\u00e9"]}`,
	`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"stop_sequences":"a"}`,
	`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"stream":"x"}`,
	`{"model":"m","max_tokens":null,"messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","max_tokens":"5","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","max_tokens":2147483648,"messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","max_tokens":2147483647,"messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":null,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`,
	`{"MODEL":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`,
	`{"mod\u0065l":"m\ud800","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`,
	"{\"model\":\"m\xff\",\"max_tokens\":5,\"messages\":[{\"role\":\"user\",\"content\":\"a\xc3(b\"}]}",
	`null`, `[]`, `"x"`, `5`, `{`, `{"model":"m",}`, `{"model":"m"} x`, ``, ` `, `{}`,
	`{` + okEnv + `}`,
	`{` + okEnv + `,"messages":null}`,
	`{` + okEnv + `,"messages":[]}`,
	`{` + okEnv + `,"messages":{}}`,
	`{` + okEnv + `,"messages":[null]}`,
	`{` + okEnv + `,"messages":[5]}`,
	`{` + okEnv + `,"messages":[{"role":5,"content":"x"}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"a"},{"role":5}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"hologram"}]},{"role":5}]}`,
	`{` + okEnv + `,"messages":[{"ROLE":"user","Content":"x"}]}`,
	`{` + okEnv + `,"messages":[{"role":"user"}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":null}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":5}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[null]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":["x"]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x","content":null}]}`,
	`{` + okEnv + `,"messages":[{"role":"robot","content":"x"}]}`,
	`{` + okEnv + `,"messages":[{"role":"system","content":"s"},{"role":"system","content":[{"type":"image"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"system","content":null}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"zzz":1,"aaa":null,"thinking":{"type":"enabled"},"metadata":{}}`,
	// system
	`{` + okEnv + `,"system":"sys","messages":[{"role":"user","content":"x"}]}`,
	`{` + okEnv + `,"system":"","messages":[{"role":"user","content":"x"}]}`,
	`{` + okEnv + `,"system":5,"messages":[{"role":"user","content":"x"}]}`,
	`{` + okEnv + `,"system":[],"messages":[{"role":"user","content":"x"}]}`,
	`{` + okEnv + `,"system":[{"type":"text","text":"x-anthropic-billing-header: a"},{"type":"text","text":"x-anthropic-billing-header:"},{"type":"text","text":"\u0078-anthropic-billing-header: b"},{"type":"text","text":" x-anthropic-billing-header:"},{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":null}],"messages":[{"role":"user","content":"x"}]}`,
	`{` + okEnv + `,"system":[{"type":"image"}],"messages":[{"role":"user","content":"x"}]}`,
	`{` + okEnv + `,"system":[{"text":"no type"}],"messages":[{"role":"user","content":"x"}]}`,
	`{` + okEnv + `,"system":[{"type":"text","text":5}],"messages":[{"role":"user","content":"x"}]}`,
	`{` + okEnv + `,"system":[{"type":"text","text":"x-anthropic-billing-header:"}],"messages":[{"role":"user","content":"x"}]}`,
	// user
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"thinking"},{"type":"text","text":"c","cache_control":{}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"text","text":"\ud83d"},{"type":"text","text":"\ude00"}]}]}`,
	"{" + okEnv + ",\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"\xf0\x9f\"},{\"type\":\"text\",\"text\":\"\x98\x80\"}]}]}",
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA\/A"}},{"type":"text","text":"t"},{"type":"image","source":{"type":"url","url":"https://x"}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":null}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":5}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png"}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"x"}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url"}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"f"}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"a"},"source":{"type":"base64","media_type":"m","data":"d"}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"a"},"source":null}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"a"},"source":{"type":"url","url":null}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","SOURCE":{"TYPE":"url","URL":"a"}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":5}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":null,"is_error":true}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"","is_error":null}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[],"is_error":"x"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"a"},{"type":"text"},{"type":"image","source":{"type":"url","url":"u"}},{"type":"text","text":"b","cache_control":{}}]},{"type":"text","text":"after"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image"}]}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"document"}]}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":5}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":{}}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[5]}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"\ud800x"},{"type":"tool_result","tool_use_id":"u","is_error":true,"content":"e"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":5}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"document"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":5}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":[{"type":"text","text":"a","unknown":{"deep":[1,{"a":"\""}]}}]}]}`,
	// assistant
	`{` + okEnv + `,"messages":[{"role":"assistant","content":"a"}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[]}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x"},{"type":"redacted_thinking"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"tool_use","id":"i","name":"n","input":{ "a" : [1, "b c"] }},{"type":"text","text":"b"},{"type":"tool_use","id":"j","name":"n"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"i","name":"n","input":null}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"i","name":"n","input":[1]}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"i","name":"n","input":"s"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"i","name":"n","input":{"s":"<\u2028>\"\\\ud800"}}]}]}`,
	"{" + okEnv + ",\"messages\":[{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"i\",\"name\":\"n\",\"input\":{\"s\":\"\xff\"}}]}]}",
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"n"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"i"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"image"}]}]}`,
	`{` + okEnv + `,"messages":[{"role":"assistant","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]}`,
	// tools
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":{}}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[null]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[5]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","input_schema":{ "type" : "object" }},{"type":"custom","name":"b","description":"d","input_schema":"s"},{"type":"bash_20250124","name":"bash"}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a"}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","input_schema":null}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"input_schema":{}}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":5,"input_schema":{}}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","description":"","input_schema":{},"cache_control":{"type":"ephemeral"}}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"NAME":"a","Input_Schema":{"x":"\u00e9"},"description":"\ud800"}]}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","input_schema":{},"input_schema":null}]}`,
	// tool_choice
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"auto"}}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","input_schema":{}}],"tool_choice":{"type":"tool","name":"a","disable_parallel_tool_use":true}}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","input_schema":{}}],"tool_choice":{"type":"tool"}}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","input_schema":{}}],"tool_choice":"auto"}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","input_schema":{}}],"tool_choice":{"type":"none"},"output_config":{"effort":"low"}}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"output_config":"x"}`,
	`{` + okEnv + `,"messages":[{"role":"user","content":"x"}],"output_config":null}`,
}

// captureBodies returns every Claude Code capture request body, both as
// captured (pretty-printed) and compacted.
func captureBodies(tb testing.TB) [][]byte {
	var out [][]byte
	for _, name := range captureNames {
		out = append(out, loadCaptureTB(tb, name), captureBody(tb, name))
	}
	return out
}

func loadCaptureTB(tb testing.TB, name string) []byte {
	tb.Helper()
	b := captureBody(tb, name)
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		tb.Fatal(err)
	}
	pretty, _ := json.MarshalIndent(v, "", "  ")
	return pretty
}

// checkEquivalent fails unless DecodeRequest and CountTokens agree with the
// reference implementations on body.
func checkEquivalent(tb testing.TB, body []byte) {
	tb.Helper()
	got, gotInfo, gotErr := DecodeRequest(body)
	want, wantInfo, wantErr := refDecodeRequest(body)
	if (gotErr == nil) != (wantErr == nil) || gotErr != nil && gotErr.Error() != wantErr.Error() {
		tb.Fatalf("body %.300q:\nerr  = %v\nwant = %v", body, gotErr, wantErr)
	}
	if gotErr != nil {
		if _, ok := gotErr.(*Error); !ok {
			tb.Fatalf("error %T is not *Error", gotErr)
		}
	}
	if !reflect.DeepEqual(gotInfo, wantInfo) {
		tb.Fatalf("body %.300q:\ninfo = %+v\nwant = %+v", body, gotInfo, wantInfo)
	}
	if gotErr == nil {
		if !json.Valid(got) {
			tb.Fatalf("body %.300q: invalid chat JSON %.500q", body, got)
		}
		var g, w any
		if err := json.Unmarshal(got, &g); err != nil {
			tb.Fatal(err)
		}
		if err := json.Unmarshal(want, &w); err != nil {
			tb.Fatal(err)
		}
		if !reflect.DeepEqual(g, w) {
			tb.Fatalf("body %.300q:\nchat = %.2000s\nwant = %.2000s", body, got, want)
		}
	}

	n, err := CountTokens(body)
	wn, werr := refCountTokens(body)
	if n != wn || (err == nil) != (werr == nil) || err != nil && err.Error() != werr.Error() {
		tb.Fatalf("body %.300q: CountTokens = %d, %v; want %d, %v", body, n, err, wn, werr)
	}
}

// countCorpus are extra CountTokens-specific shapes.
var countCorpus = []string{
	`{"messages":[{"role":"user","content":[{"type":"text","text":"abc"},{"type":"image"},{"type":"thinking","thinking":"xx"},{"type":"tool_use","name":"n","input":{ "a" : 1 }},{"type":"tool_result","content":[{"type":"text","text":"r"},{"type":"image"}]}]}]}`,
	`{"messages":[{"role":"user","content":[{"type":"text","text":5}]}]}`,
	`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","thinking":5}]}]}`,
	`{"messages":[{"role":"user","content":[{"type":"tool_result","content":{"x":1}}]}]}`,
	`{"messages":[{"role":"user","content":"\u00e9\ud800` + "\xff" + `"}]}`,
	`{"Messages":[{"role":"user","content":"abc"}],"SYSTEM":"s","TOOLS":[{"NAME":"n","Description":"d","input_schema":{ }}]}`,
	`{"messages":[{"role":"user","content":"a"}],"messages":[{"role":"x"}]}`,
	`{"messages":[{"content":"a"},{"content":"b"}],"messages":[{"role":"x"}],"messages":[null,null]}`,
	`{"messages":[{"content":"a"}],"messages":[]}`,
	`{"messages":null,"tools":null,"system":null}`,
	`{"messages":"x"}`,
	`{"messages":[5]}`,
	`{"tools":[{"name":"a","input_schema":null},{"name":"b"},{"input_schema":[1, 2]}]}`,
	`{"tools":[{"name":5}]}`,
	`{"tools":{}}`,
	`{"system":[{"type":"text","text":"a"},{"type":"image"}],"messages":[]}`,
	`{"system":{"type":"text"}}`,
	`{"messages":[{"role":"user","content":[{"type":"text","text":"a","input":null,"content":null}]}]}`,
	`{"messages":[{"role":"user","content":[{"type":"text","text":"a","text":null,"TEXT":"bb"}]}]}`,
	`[]`, `null`, `5`, `{`,
}

func TestDecodeEquivalenceCaptures(t *testing.T) {
	for _, body := range captureBodies(t) {
		checkEquivalent(t, body)
	}
}

func TestDecodeEquivalenceCorpus(t *testing.T) {
	for _, body := range equivCorpus {
		checkEquivalent(t, []byte(body))
	}
	for _, body := range countCorpus {
		checkEquivalent(t, []byte(body))
	}
}

// TestDecodeEquivalenceMutations decodes each capture with every top-level
// field removed and with every message and tool on its own.
func TestDecodeEquivalenceMutations(t *testing.T) {
	for _, name := range captureNames {
		body := captureBody(t, name)
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			t.Fatal(err)
		}
		for k := range top {
			m := map[string]json.RawMessage{}
			for k2, v := range top {
				if k2 != k {
					m[k2] = v
				}
			}
			b, _ := json.Marshal(m)
			checkEquivalent(t, b)
		}
		var msgs, tools []json.RawMessage
		_ = json.Unmarshal(top["messages"], &msgs)
		_ = json.Unmarshal(top["tools"], &tools)
		for _, m := range msgs {
			checkEquivalent(t, []byte(`{`+okEnv+`,"messages":[`+string(m)+`]}`))
		}
		for _, tl := range tools {
			checkEquivalent(t, []byte(`{`+okEnv+`,"messages":[{"role":"user","content":"x"}],"tools":[`+string(tl)+`]}`))
		}
	}
}

func FuzzDecodeEquivalence(f *testing.F) {
	for _, body := range captureBodies(f) {
		f.Add(body)
	}
	for _, body := range equivCorpus {
		f.Add([]byte(body))
	}
	for _, body := range countCorpus {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkEquivalent(t, body)
	})
}
