package responses

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// equivCorpus are synthetic requests covering every mapping rule, every
// rejection and the JSON corner cases (null, duplicate and case-folded keys,
// escapes, invalid UTF-8, lone surrogates, whitespace) on which a hand-written
// decoder could diverge from encoding/json.
var equivCorpus = []string{
	// function strict (dropped, reported when true) and a top level past the linear-scan size
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"a","strict":true},{"type":"function","name":"b","strict":false},{"type":"custom","name":"c","strict":true},{"type":"namespace","name":"n","tools":[{"type":"function","name":"d","STRICT":true}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"a","strict":true,"strict":null},{"type":"function","name":"b","strict":"true"}]}`,
	`{"model":"x","k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10,"k11":11,"k12":12,"k13":13,"k14":14,"k15":15,"k16":16,"k17":17,"input":"hi","k3":null,"model":"m","k17":null,"k18":1}`,
	// envelope
	`{"model":"m","input":"hi"}`,
	` { "model" : "m" , "input" : "hi" , "stream" : true } `,
	`{"model":"m","input":"hi","stream":null,"max_output_tokens":null,"temperature":null,"top_p":null}`,
	`{"model":"m","input":"hi","stream":false,"max_output_tokens":1e3,"temperature":0,"top_p":1.0}`,
	`{"model":"m","input":"hi","temperature":-0,"top_p":1E-400}`,
	`{"model":"m","input":"hi","temperature":1e400}`,
	`{"model":"m","input":"hi","temperature":"1"}`,
	`{"model":"m","input":"hi","max_output_tokens":2147483647}`,
	`{"model":"m","input":"hi","max_output_tokens":2147483648}`,
	`{"model":"m","input":"hi","max_output_tokens":1.5}`,
	`{"model":"m","input":"hi","max_output_tokens":-1}`,
	`{"model":"m","input":"hi","max_output_tokens":"5"}`,
	`{"model":"m","input":"hi","stream":"yes"}`,
	`{"model":"m","input":"hi","stream":1}`,
	`{"model":"m","model":"n","input":"hi"}`,
	`{"model":"m","model":null,"input":"hi"}`,
	`{"MODEL":"m","input":"hi"}`,
	`{"Model":"m","model":"x","input":"hi"}`,
	`{"mod\u0065l":"m","input":"hi","\u0073tream":true}`,
	`{"model":"\u006d\n\u00e9\ud83d\ude00","input":"hi"}`,
	`{"model":"m\ud800","input":"x\udc00y\ud800\u0041"}`,
	"{\"model\":\"m\xff\xfe\",\"input\":\"a\xc3(b\"}",
	`{"model":"m","input":"hi","previous_response_id":null}`,
	`{"model":"m","input":"hi","previous_response_id":""}`,
	`{"model":"m","input":"hi","previous_response_id":"r"}`,
	`{"model":"m","input":"hi","previous_response_id":5}`,
	`{"model":5,"input":"hi"}`,
	`{"model":"","input":"hi"}`,
	`{"model":"m"}`,
	`{"model":"m","input":null}`,
	`{"model":"m","input":""}`,
	`{"model":"m","input":[]}`,
	`{"model":"m","input":{}}`,
	`{"model":"m","input":5}`,
	`{"model":"m","input":[null]}`,
	`{"model":"m","input":["x"]}`,
	`{"model":"m","input":[5]}`,
	`null`, `[]`, `"x"`, `5`, `{`, `{"model":"m",}`, `{"model":"m","input":"x"} x`, ``, ` `,
	`{"model":"m","input":"hi","store":true,"store":false}`,
	`{"model":"m","input":"hi","store":"true","reasoning":{"effort":"high","summary":"auto"}}`,
	`{"model":"m","input":"hi","reasoning":{"Effort":"low"}}`,
	`{"model":"m","input":"hi","reasoning":"x"}`,
	`{"model":"m","input":"hi","reasoning":null,"store":null,"text":null,"include":null,"zzz":null}`,
	`{"model":"m","input":"hi","include":[],"zzz":1,"aaa":{},"metadata":{"a":1},"user":"u","truncation":"auto"}`,
	`{"model":"m","input":"hi","instructions":"sys"}`,
	`{"model":"m","input":"hi","instructions":""}`,
	`{"model":"m","input":"hi","instructions":5}`,
	`{"model":"m","input":"hi","instructions":"a\n\"b\"\\c\u2028\t<>&"}`,
	// text / response_format
	`{"model":"m","input":"hi","text":{"format":{"type":"json_object"},"verbosity":"low"}}`,
	`{"model":"m","input":"hi","text":{"format":{"type":"json_schema","schema":{ "type" : "object" }}}}`,
	`{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"n","description":"d","strict":false,"schema":{"a":[1, 2]}}}}`,
	`{"model":"m","input":"hi","text":{"format":{"type":"json_schema"}}}`,
	`{"model":"m","input":"hi","text":{"format":{"type":"text"}}}`,
	`{"model":"m","input":"hi","text":{"format":{"type":"xml"}}}`,
	`{"model":"m","input":"hi","text":{"format":"x"}}`,
	`{"model":"m","input":"hi","text":[]}`,
	// tools
	`{"model":"m","input":"hi","tools":[]}`,
	`{"model":"m","input":"hi","tools":null,"tool_choice":"auto"}`,
	`{"model":"m","input":"hi","tools":{}}`,
	`{"model":"m","input":"hi","tools":[null]}`,
	`{"model":"m","input":"hi","tools":[5]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function"}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f"}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":null,"description":null}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{ "type" : "object", "properties" : { "a b" : { "type" : "string" } } }}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":[]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":"x"}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{},"parameters":1}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","description":5}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":5}]}`,
	`{"model":"m","input":"hi","tools":[{"TYPE":"function","Name":"f","DESCRIPTION":"d","Parameters":{"x":"\u00e9"}}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","description":"a","description":null}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","description":"\ud800 lone"}]}`,
	"{\"model\":\"m\",\"input\":\"hi\",\"tools\":[{\"type\":\"function\",\"name\":\"f\",\"description\":\"bad \xff utf8\",\"parameters\":{\"d\":\"\xfe\"}}]}",
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f"},{"type":"function","name":"f"}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"web_search"},{"type":"file_search"},{"type":"web_search"},{}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"custom","name":"c"}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"custom","name":"c","description":"  d  ","format":{"type":"text"}}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"custom","name":"c","format":{"type":"grammar","definition":"start: x"}}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"custom","name":"c","description":" pre ","format":{"type":"grammar","syntax":"regex","definition":"` + strings.Repeat("é", 3000) + `"}}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"custom","name":"c","format":{"type":"other"}}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"custom","name":"c","format":"x"}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"custom","name":"c","format":null}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","description":"  nsd ","tools":[{"type":"function","name":"a","description":"  ad  "},{"type":"custom","name":"b"},{"type":"web_search"},{"type":"function","name":"c","description":"cd"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"}]},{"type":"function","name":"ns__a"}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"ns__a"},{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"},{"type":"function","name":"a"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns.x y","tools":[{"type":"function","name":"a/b"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"mcp__codex_apps__acmecorp_web_tools_with_a_long_name","tools":[{"type":"function","name":"_list_widget_inventory_detail_xxxxxxxxx"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","tools":[{"type":"function","name":"a"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a","parameters":5}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":null}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":{}}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[null,{"type":"function","name":"a"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],"tools":[{"name":"c"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],"tools":[{"name":"c"}],"tools":[null,null,{"type":"function","name":"d"}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"}],"tools":[]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","TOOLS":[{"type":"function","name":"a","type":null}]}]}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"namespace","name":"x","tools":[]}]}]}`,
	// tool_choice / parallel_tool_calls
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f"}],"tool_choice":"none","parallel_tool_calls":false}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f"}],"tool_choice":"sometimes"}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f"}],"tool_choice":5}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f"}],"tool_choice":{"type":"function"}}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f"}],"tool_choice":{"type":"function","name":"f"},"parallel_tool_calls":"x"}`,
	`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"}]}],"tool_choice":{"type":"function","namespace":"ns","name":"a"}}`,
	`{"model":"m","input":"hi","tools":[{"type":"web_search"}],"tool_choice":"bogus","parallel_tool_calls":"bogus"}`,
	`{"model":"m","input":"hi","tools":[{"type":"function","name":"f"}],"tool_choice":{"type":"web_search"}}`,
	// input items
	`{"model":"m","input":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","input":[{"type":"message","role":"user","content":null}]}`,
	`{"model":"m","input":[{"type":"message","role":"user"}]}`,
	`{"model":"m","input":[{"type":"message","role":"user","content":[]}]}`,
	`{"model":"m","input":[{"type":"message","role":"user","content":5}]}`,
	`{"model":"m","input":[{"type":"message","role":"user","content":[null]}]}`,
	`{"model":"m","input":[{"type":"message","role":"user","content":["x"]}]}`,
	`{"model":"m","input":[{"type":"message","content":"x"}]}`,
	`{"model":"m","input":[{"role":"tool","content":"x"}]}`,
	`{"model":"m","input":[{"role":5,"content":"x"}]}`,
	`{"model":"m","input":[{"ROLE":"user","Content":"x","TYPE":"message"}]}`,
	`{"model":"m","input":[{"role":"developer","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"},{"type":"refusal","refusal":"c"},{"type":"text"}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"u","detail":"auto"},{"type":"input_text","text":"b"},{"type":"input_text","text":"c"},{"type":"input_image","image_url":"v","detail":"low"}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"u"}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":""}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":null,"file_id":"f"}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"u","image_url":null}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":5}]}]}`,
	`{"model":"m","input":[{"role":"assistant","content":[{"type":"input_image","image_url":"u"}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"f"}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":5}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"a","text":null,"Text":"b"}]}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"\ud83d"},{"type":"input_text","text":"\ude00"}]}]}`,
	"{\"model\":\"m\",\"input\":[{\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"\xf0\x9f\"},{\"type\":\"input_text\",\"text\":\"\x98\x80\"}]}]}",
	`{"model":"m","input":[{"role":"assistant","content":"a"},{"type":"reasoning"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"reasoning"},{"type":"function_call","call_id":"c2","name":"g"}]}`,
	`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":{ "a" : [1, 2] }},{"type":"function_call","call_id":"c2","name":"f","arguments":"  "},{"type":"function_call","call_id":"c3","name":"f","arguments":"\u0020\n"},{"type":"function_call","call_id":"c4","name":"f","arguments":"\u00a0"},{"type":"function_call","call_id":"c5","name":"f","arguments":" \u0041"}]}`,
	`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":5}]}`,
	`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":[1]}]}`,
	`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"x","arguments":null}]}`,
	`{"model":"m","input":[{"type":"function_call","call_id":"c1"}]}`,
	`{"model":"m","input":[{"type":"function_call","name":"f"}]}`,
	`{"model":"m","input":[{"type":"function_call","call_id":5,"name":"f"}]}`,
	`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"a","namespace":"ns"}],"tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"}]}]}`,
	`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"a","namespace":"other ns"}]}`,
	`{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"p","input":"x \"y\" <z> \u2028 \n"}]}`,
	`{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"p"}]}`,
	`{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"p","input":"x","input":null}]}`,
	`{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"p","input":5}]}`,
	`{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"p","input":"\ud800"}]}`,
	`{"model":"m","input":[{"type":"function_call_output","call_id":"c1"}]}`,
	`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":null}]}`,
	`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":""}]}`,
	`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[]}]}`,
	`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"a"},{"type":"text"},{"type":"output_text","text":"b"}]}]}`,
	`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"u"}]}]}`,
	`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":5}]}`,
	`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":{}}]}`,
	`{"model":"m","input":[{"type":"function_call_output","output":"x"}]}`,
	`{"model":"m","input":[{"type":"custom_tool_call_output","call_id":"c1","output":"x"}]}`,
	`{"model":"m","input":[{"type":"web_search_call"}]}`,
	`{"model":"m","input":[{"type":"item_reference"}]}`,
	`{"model":"m","input":[{"type":5}]}`,
	`{"model":"m","input":[{"role":"user","content":"a"},{"role":"user","content":[{"type":"input_audio"}]}]}`,
	`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"f"},{"role":"user","content":"x"},{"type":"function_call","call_id":"c2","name":"f"}],"instructions":"i"}`,
	`{"model":"m","input":[{"role":"assistant","content":""},{"type":"function_call","call_id":"c1","name":"f"},{"type":"function_call_output","call_id":"c1","output":"o"},{"type":"function_call","call_id":"c2","name":"f"}]}`,
	`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"x"}],"content":"y"}]}`,
	`{"model":"m","input":[{"role":"user","content":"x","content":null}]}`,
	`{"model":"m","input":[{"role":"user","content":"x","unknown":{"deep":[1,{"a":"\""}]}}]}`,
}

// captureBodies returns every Codex capture request body, both as captured
// (pretty-printed) and compacted.
func captureBodies(tb testing.TB) [][]byte {
	matches, _ := filepath.Glob(filepath.Join(captureDir, "*.json"))
	var out [][]byte
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			tb.Fatal(err)
		}
		var c struct {
			RequestBody json.RawMessage `json:"request_body"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			tb.Fatal(err)
		}
		if len(c.RequestBody) == 0 || string(c.RequestBody) == "null" {
			continue
		}
		out = append(out, []byte(c.RequestBody), captureBody(tb, filepath.Base(m)))
	}
	if len(out) == 0 {
		tb.Fatal("no captures")
	}
	return out
}

// checkEquivalent fails unless DecodeRequest and the reference decoder agree
// on body: the same error (message) or chat bodies that unmarshal to equal
// values, and equal RequestInfo either way.
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
	if gotErr != nil {
		return
	}
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

func TestDecodeEquivalenceCaptures(t *testing.T) {
	for _, body := range captureBodies(t) {
		checkEquivalent(t, body)
	}
}

func TestDecodeEquivalenceCorpus(t *testing.T) {
	for _, body := range equivCorpus {
		checkEquivalent(t, []byte(body))
	}
}

// TestDecodeEquivalenceMutations decodes the capture with each top-level
// field removed and with every input item and tool on its own.
func TestDecodeEquivalenceMutations(t *testing.T) {
	body := captureBody(t, "0003-POST-v1_responses.json")
	var top map[string]json.RawMessage
	mustJSON(t, body, &top)
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
	var items, tools []json.RawMessage
	mustJSON(t, top["input"], &items)
	mustJSON(t, top["tools"], &tools)
	for _, it := range items {
		checkEquivalent(t, []byte(`{"model":"m","input":[`+string(it)+`]}`))
	}
	for _, tl := range tools {
		checkEquivalent(t, []byte(`{"model":"m","input":"x","tool_choice":"auto","tools":[`+string(tl)+`]}`))
	}
}

func FuzzDecodeEquivalence(f *testing.F) {
	for _, body := range captureBodies(f) {
		f.Add(body)
	}
	for _, body := range equivCorpus {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkEquivalent(t, body)
	})
}
