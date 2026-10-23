package analyze

import (
	"strings"
	"testing"
	"time"
)

func validCaps() Capabilities {
	return Capabilities{
		SourceKind: "jsonl", Stores: []string{"default"}, Tokens: "measured", TokenGranularity: "request",
		CacheWriteOverlap: "disjoint", Branch: "per_event", ToolDuration: "paired", PermissionSignal: "request",
		HookEvents: []string{"Stop"}, HookTemplate: `{cbrt} hook record x Stop`,
	}
}

func TestCapabilitiesValidate(t *testing.T) {
	if err := validCaps().Validate(); err != nil {
		t.Fatal(err)
	}
	c := validCaps()
	c.Tokens, c.Stores, c.HookTemplate = "maybe", nil, "no placeholder"
	err := c.Validate()
	for _, want := range []string{`Tokens "maybe"`, "Stores is empty", "{cbrt}"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error mentioning %q, got %v", want, err)
		}
	}
	c = validCaps()
	c.Tokens = "unknown" // granularity still "request"
	if c.Validate() == nil {
		t.Error("unknown tokens with request granularity accepted")
	}
}

type stub struct{ id string }

func (s stub) ID() string                            { return s.id }
func (stub) Version() string                         { return "1" }
func (stub) DiscoveryRoots() []string                { return nil }
func (stub) Capabilities() Capabilities              { return validCaps() }
func (stub) Discover([]string) ([]Source, error)     { return nil, nil }
func (stub) Events(Source, func(Event) error) error  { return nil }
func (stub) NormalizeHook(Envelope) ([]Event, error) { return nil, nil }
func (stub) Bodies(Source, func(Event) error) error  { return nil }

func TestRegister(t *testing.T) {
	Register(stub{"stub-a"})
	if a, ok := Get("stub-a"); !ok || a.ID() != "stub-a" {
		t.Fatal("not registered")
	}
	defer func() {
		if recover() == nil {
			t.Error("duplicate registration did not panic")
		}
	}()
	Register(stub{"stub-a"})
}

func TestArgsHashCanonical(t *testing.T) {
	a := ArgsHash([]byte(`{"b":1, "a":{"y":2,"x":[1, 2]}}`))
	b := ArgsHash([]byte(`{"a":{"x":[1,2],"y":2},"b":1}`))
	if a != b || len(a) != 40 {
		t.Fatalf("%s != %s", a, b)
	}
	if ArgsHash([]byte("ls -la")) == ArgsHash([]byte("ls")) {
		t.Fatal("non-JSON arguments collide")
	}
}

func TestTextVolume(t *testing.T) {
	for s, want := range map[string]int{"": 0, "a": 1, "abcd": 1, "abcde": 2, "ééééé": 2} {
		if got := TextVolume(s); got != want {
			t.Errorf("TextVolume(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestCheckEventsCatches(t *testing.T) {
	a := stub{"chk"}
	src := Source{AdapterID: "chk", Store: "default", NativeSessionID: "s1"}
	ok := Event{Tool: "chk", Store: "default", Origin: OriginCLI, NativeSessionID: "s1", Seq: "1",
		TS: time.Unix(0, 0).UTC(), Kind: KindUserPrompt, ObservedBy: FromTranscript,
		BranchCoverage: Measured, DurationCoverage: Unknown}
	if errs := CheckEvents(a, src, []Event{ok}); len(errs) != 0 {
		t.Fatalf("valid event rejected: %v", errs)
	}
	req := ok
	req.Seq, req.Kind, req.RequestID, req.TokensCoverage = "2", KindModelRequest, "r1", Partial
	req.Tokens = &Tokens{InUncached: Int64(1)}
	dupReq := req
	dupReq.Seq = "3"
	usageOnPrompt := ok
	usageOnPrompt.Seq, usageOnPrompt.Tokens = "4", &Tokens{}
	noCoverage := ok
	noCoverage.Seq, noCoverage.BranchCoverage = "5", ""
	localTS := ok
	localTS.Seq, localTS.TS = "6", time.Unix(0, 0).In(time.FixedZone("x", 3600))
	errs := CheckEvents(a, src, []Event{ok, req, dupReq, usageOnPrompt, noCoverage, localTS, ok})
	for _, want := range []string{"second model_request", "usage on a user_prompt", "BranchCoverage", "non-zero UTC", "duplicate Seq"} {
		found := false
		for _, e := range errs {
			found = found || strings.Contains(e.Error(), want)
		}
		if !found {
			t.Errorf("not caught: %s (got %v)", want, errs)
		}
	}
}
