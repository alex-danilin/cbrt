package jsonscan

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

var scanSeeds = []string{
	``, ` `, `null`, `true`, `false`, `nul`, `truex`, `0`, `-0`, `01`, `-`, `1.`, `1.5e`, `1e+5`, `1E-5`, `-12.5e10`,
	`""`, `"a\"b"`, `"\u00e9\ud83d\ude00"`, `"\ud800"`, `"\x"`, `"\u12"`, "\"\x01\"", "\"\xff\"", `"a`,
	`{}`, `[]`, `{"a":1}`, `{"a":1,}`, `[1,]`, `[1 2]`, `{"a" 1}`, `{"a":}`, `{1:2}`, `[}`, `{]`, `{"a":[1,{"b":null}]}`,
	` { "a" : [ 1 , 2 ] } `, `{} x`, `[[[[]]]]`, strings.Repeat("[", 10000) + strings.Repeat("]", 10000),
	strings.Repeat("[", 10001) + strings.Repeat("]", 10001), `"\/\b\f\n\r\t\\"`,
}

func FuzzValidJSON(f *testing.F) {
	for _, s := range scanSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if got, want := validJSON(b), json.Valid(b); got != want {
			t.Fatalf("validJSON(%q) = %v, want %v", b, got, want)
		}
		if !json.Valid(b) {
			return
		}
		v := TrimSpace(b)
		if e := valueEnd(v, 0); e != len(v) {
			t.Fatalf("valueEnd(%q) = %d, want %d", v, e, len(v))
		}
		var buf bytes.Buffer
		_ = json.Compact(&buf, v)
		if got := appendCompact(nil, v); !bytes.Equal(got, buf.Bytes()) || compactLen(v) != buf.Len() {
			t.Fatalf("appendCompact(%q) = %q, want %q", v, got, buf.Bytes())
		}
		if v[0] == '"' {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				t.Fatal(err)
			}
			if got := Unquote(v); got != s || StrLen(v) != len(s) {
				t.Fatalf("Unquote(%q) = %q, want %q", v, got, s)
			}
			var back string
			if err := json.Unmarshal(AppendQuoted(nil, s), &back); err != nil || back != s {
				t.Fatalf("AppendQuoted(%q) round trip = %q, %v", s, back, err)
			}
			if got, want := AppendQuoted(nil, s), marshal(s); !bytes.Equal(got, want) {
				t.Fatalf("AppendQuoted(%q) = %s, want %s", s, got, want)
			}
			if err := json.Unmarshal(AppendStrLit(nil, v), &back); err != nil || back != s {
				t.Fatalf("AppendStrLit(%q) = %q, %v", v, back, err)
			}
		}
	})
}

func TestFoldKey(t *testing.T) {
	var kb [32]byte
	for key, want := range map[string]string{
		`"type"`: "type", `"TYPE"`: "type", `"t\u0079pe"`: "type", `"toolſ"`: "tools", "\"\u212a\"": "k",
		`"call_ID"`: "call_id", `"é"`: "\u00c9",
	} {
		if got := string(FoldKey(&kb, []byte(key))); got != want {
			t.Errorf("FoldKey(%s) = %q, want %q", key, got, want)
		}
	}
}

// marshal encodes v like encoding/json with HTML escaping off.
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("jsonscan: marshal: " + err.Error())
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
