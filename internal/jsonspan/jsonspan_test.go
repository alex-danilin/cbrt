package jsonspan

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestObjectSpans(t *testing.T) {
	doc := []byte(` { "model" : "m", "abc":[1, {"x":"y"}], "n":-1.5e+3, "t":true,"z":null, "model":"last" } `)
	ms, err := Object(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ key, keyRaw, val string }{
		{"model", `"model"`, `"m"`},
		{"abc", `"abc"`, `[1, {"x":"y"}]`},
		{"n", `"n"`, `-1.5e+3`},
		{"t", `"t"`, `true`},
		{"z", `"z"`, `null`},
		{"model", `"model"`, `"last"`},
	}
	if len(ms) != len(want) {
		t.Fatalf("got %d members", len(ms))
	}
	for i, w := range want {
		if ms[i].Key != w.key || string(ms[i].KeyRaw) != w.keyRaw || string(ms[i].Value) != w.val {
			t.Errorf("member %d = %q %q %q, want %+v", i, ms[i].Key, ms[i].KeyRaw, ms[i].Value, w)
		}
	}
	if string(Get(ms, "model")) != `"last"` || Get(ms, "missing") != nil {
		t.Error("Get must return the last duplicate, like encoding/json")
	}
}

func TestArraySpans(t *testing.T) {
	els, err := Array([]byte(`[ {"role":"user"} , "s", [], 0 ]`))
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, e := range els {
		got = append(got, string(e))
	}
	if !reflect.DeepEqual(got, []string{`{"role":"user"}`, `"s"`, `[]`, `0`}) {
		t.Errorf("got %q", got)
	}
	if els, err := Array([]byte(`[]`)); err != nil || len(els) != 0 {
		t.Errorf("empty array: %v %v", els, err)
	}
}

func TestInvalid(t *testing.T) {
	for _, s := range []string{
		``, `{`, `{"a"}`, `{"a":}`, `{"a":1,}`, `{"a":1 "b":2}`, `{a:1}`, `{"a":01}`, `{"a":1.}`, `{"a":1e}`,
		`{"a":"\x"}`, `{"a":"\u12"}`, "{\"a\":\"\x01\"}", `{"a":tru}`, `{"a":[1,]}`, `{"a":1} x`, `[1] 2`, `{"a":-}`,
	} {
		if _, err := Object([]byte(s)); !errors.Is(err, ErrSyntax) {
			t.Errorf("Object(%q) = %v, want ErrSyntax", s, err)
		}
	}
	if _, err := Object([]byte(`[1]`)); err == nil {
		t.Error("an array is not an object")
	}
}

// The scanner must agree with encoding/json on every input.
func FuzzAgreesWithEncodingJSON(f *testing.F) {
	for _, s := range []string{`{}`, `{"a":[1,2,{"b":null}],"c":"é\n"}`, `[1,"x",true]`, `{"a":1e5,"b":-0.0}`, `"s"`, `{"a":"\ud800"}`} {
		f.Add([]byte(s))
	}
	files, _ := filepath.Glob("../../testdata/clients/*/*.json")
	for _, fn := range files {
		if b, err := os.ReadFile(fn); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if Valid(data) != json.Valid(data) {
			t.Fatalf("Valid mismatch for %q: ours %v, encoding/json %v", data, Valid(data), json.Valid(data))
		}
		ms, err := Object(data)
		var ref map[string]json.RawMessage
		refErr := json.Unmarshal(data, &ref)
		isObj := refErr == nil && bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("{"))
		if (err == nil) != isObj {
			t.Fatalf("Object(%q) err=%v, encoding/json object=%v (%v)", data, err, isObj, refErr)
		}
		if err != nil {
			return
		}
		for k, v := range ref {
			got := Get(ms, k)
			if got == nil || !bytes.Equal(compact(t, got), compact(t, v)) {
				t.Fatalf("key %q: got %q want %q", k, got, v)
			}
		}
	})
}

func compact(t *testing.T, b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func BenchmarkObject50KB(b *testing.B) {
	doc, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": string(bytes.Repeat([]byte("hello world "), 4300))}}})
	b.SetBytes(int64(len(doc)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Object(doc); err != nil {
			b.Fatal(err)
		}
	}
}
