package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRedact(t *testing.T) {
	nvidia := `{"status":404,"title":"Not Found","detail":"Function '23d4f03a-b8a6-4adb-a183-7daa083a09cc': Not found for account 'bkWmZdyUML9Q4Zs9up_NDlmtnbI_Lm4uUn8T2VoHaAQ'"}`
	tests := []struct {
		name    string
		in      string
		want    string   // exact output when non-empty
		absent  []string // substrings that must not appear
		present []string // substrings that must appear
	}{
		{name: "empty", in: "", want: ""},
		{name: "plain", in: "model not found", want: "model not found"},
		{name: "whitespace", in: "  rate\n\tlimited \r\n  now ", want: "rate limited now"},
		{
			name:    "nvidia body",
			in:      nvidia,
			absent:  []string{"23d4f03a-b8a6-4adb-a183-7daa083a09cc", "bkWmZdyUML9Q4Zs9up_NDlmtnbI_Lm4uUn8T2VoHaAQ", "bkWmZdy"},
			present: []string{"Function '<id>'", "account '<account>'", "Not found"},
		},
		{name: "nvapi key", in: "bad key nvapi-AbCdEf0123456789_xyz-Q given", want: "bad key [redacted] given"},
		{name: "openai key", in: "Incorrect API key provided: sk-proj1234abcd", want: "Incorrect API key provided: [redacted]"},
		{name: "anthropic key", in: "key sk-ant-api03-abcdefgh-XYZ", want: "key [redacted]"},
		{name: "short sk word kept", in: "use sk-learn", want: "use sk-learn"},
		{name: "bearer", in: "header Authorization: Bearer abc.def.ghi rejected", want: "header Authorization: Bearer [redacted] rejected"},
		{name: "bare uuid", in: "request 123e4567-E89B-12d3-a456-426614174000 failed", want: "request <uuid> failed"},
		{name: "hex token", in: "trace 0123456789abcdef0123456789abcdef end", want: "trace <id> end"},
		{name: "base64 token", in: "id=QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", want: "id=<id>"},
		{name: "short token kept", in: "code abc123", want: "code abc123"},
		{name: "account", in: "for account 'someone@example.com' denied", want: "for account '<account>' denied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if tc.want != "" || tc.in == "" {
				if got != tc.want {
					t.Fatalf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("output %q contains %q", got, a)
				}
			}
			for _, p := range tc.present {
				if !strings.Contains(got, p) {
					t.Errorf("output %q lacks %q", got, p)
				}
			}
		})
	}
}

func TestRedactCap(t *testing.T) {
	long := strings.Repeat("ab ", 150) // 450 runes
	got := Redact(long)
	if n := utf8.RuneCountInString(got); n != maxDetailRunes+1 {
		t.Fatalf("rune count = %d, want %d", n, maxDetailRunes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("missing ellipsis: %q", got)
	}
	multi := strings.Repeat("ж", 250)
	got = Redact(multi)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != maxDetailRunes+1 {
		t.Fatalf("bad multibyte cut: %d runes", utf8.RuneCountInString(got))
	}
	exact := strings.Repeat("x ", 100)[:199] + "y" // exactly 200 runes
	if got := Redact(exact); got != exact {
		t.Fatalf("200-rune input changed")
	}
}

func fixedNow() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }

func TestHubRecentAndLast(t *testing.T) {
	h := NewHub(3, nil, nil)
	h.now = fixedNow
	if _, ok := h.Last(); ok {
		t.Fatal("Last on empty hub reported ok")
	}
	if r := h.Recent(5); len(r) != 0 {
		t.Fatalf("Recent on empty = %v", r)
	}
	for i := 0; i < 5; i++ {
		h.Emit(Event{Kind: KindSwitch, Route: fmt.Sprint("r", i)})
	}
	tests := []struct {
		n    int
		want []string
	}{
		{0, nil},
		{-1, nil},
		{1, []string{"r4"}},
		{2, []string{"r4", "r3"}},
		{3, []string{"r4", "r3", "r2"}},
		{10, []string{"r4", "r3", "r2"}},
	}
	for _, tc := range tests {
		got := h.Recent(tc.n)
		var routes []string
		for _, e := range got {
			routes = append(routes, e.Route)
		}
		if fmt.Sprint(routes) != fmt.Sprint(tc.want) {
			t.Errorf("Recent(%d) = %v, want %v", tc.n, routes, tc.want)
		}
	}
	last, ok := h.Last()
	if !ok || last.Route != "r4" {
		t.Fatalf("Last = %+v, %v", last, ok)
	}
	if !last.Time.Equal(fixedNow()) {
		t.Fatalf("Time not filled: %v", last.Time)
	}
}

func TestHubEmitKeepsTimeRedactsAndSinks(t *testing.T) {
	var got []Event
	h := NewHub(0, nil, func(e Event) { got = append(got, e) })
	h.now = fixedNow
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	retry := at.Add(time.Hour)
	h.Emit(Event{Time: at, Kind: KindCooldown, Detail: "key nvapi-0123456789abcdef\nbad", RetryAt: &retry})
	retry = retry.Add(time.Hour) // caller mutation must not leak into stored event
	if len(got) != 1 {
		t.Fatalf("sink calls = %d", len(got))
	}
	e := got[0]
	if !e.Time.Equal(at) {
		t.Errorf("Time overwritten: %v", e.Time)
	}
	if e.Detail != "key [redacted] bad" {
		t.Errorf("Detail = %q", e.Detail)
	}
	last, _ := h.Last()
	if last.Detail != e.Detail || !last.RetryAt.Equal(at.Add(time.Hour)) {
		t.Errorf("stored event = %+v", last)
	}
}

func TestHubLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := NewHub(4, logger, nil)
	h.now = fixedNow
	retry := fixedNow().Add(time.Hour)
	tests := []struct {
		ev    Event
		level string
	}{
		{Event{Kind: KindSwitch, Route: "coder", From: "nvidia:a", To: "ollama:b"}, "INFO"},
		{Event{Kind: KindRecovery, Provider: "nvidia"}, "INFO"},
		{Event{Kind: KindCooldown, Provider: "nvidia", Target: "nvidia:a", Reason: "quota_exhausted", Detail: "x", RetryAt: &retry}, "WARN"},
		{Event{Kind: KindDisabled, Provider: "groq", Reason: "auth"}, "WARN"},
	}
	for _, tc := range tests {
		buf.Reset()
		h.Emit(tc.ev)
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("log line %q: %v", buf.String(), err)
		}
		if m["level"] != tc.level {
			t.Errorf("%s: level = %v, want %s", tc.ev.Kind, m["level"], tc.level)
		}
		if m["kind"] != string(tc.ev.Kind) {
			t.Errorf("kind attr = %v", m["kind"])
		}
		for k, v := range map[string]string{"route": tc.ev.Route, "from": tc.ev.From, "to": tc.ev.To,
			"provider": tc.ev.Provider, "target": tc.ev.Target, "reason": tc.ev.Reason, "detail": tc.ev.Detail} {
			if v != "" && m[k] != v {
				t.Errorf("%s: attr %s = %v, want %s", tc.ev.Kind, k, m[k], v)
			}
		}
		if tc.ev.RetryAt != nil && m["retry_at"] == nil {
			t.Errorf("retry_at missing")
		}
	}
}

func TestEventJSON(t *testing.T) {
	b, err := json.Marshal(Event{Time: fixedNow(), Kind: KindSwitch, Route: "r"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"time", "kind", "route"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q in %s", k, b)
		}
	}
	for _, k := range []string{"from", "to", "provider", "target", "reason", "detail", "retry_at"} {
		if _, ok := m[k]; ok {
			t.Errorf("empty %q not omitted in %s", k, b)
		}
	}
}

func TestHubConcurrent(t *testing.T) {
	var mu sync.Mutex
	count := 0
	h := NewHub(50, nil, func(Event) { mu.Lock(); count++; mu.Unlock() })
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				h.Emit(Event{Kind: KindSwitch})
				h.Recent(10)
			}
		}()
	}
	wg.Wait()
	if count != 1600 {
		t.Fatalf("sink count = %d", count)
	}
	if n := len(h.Recent(100)); n != 50 {
		t.Fatalf("stored = %d, want 50", n)
	}
}
