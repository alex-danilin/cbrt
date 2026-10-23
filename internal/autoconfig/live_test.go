//go:build live

package autoconfig

import (
	"context"
	"testing"

	"github.com/alex-danilin/cbrt/internal/config"
)

// TestLiveOllama runs Generate against the real local Ollama (OLLAMA_HOST or
// 127.0.0.1:11434) and logs what it detected:
//
//	go test -tags live -count=1 -v -run TestLiveOllama ./internal/autoconfig
func TestLiveOllama(t *testing.T) {
	doc, rep, err := Generate(context.Background(), Options{})
	for _, f := range rep.Found {
		t.Log("found:", f)
	}
	for _, w := range rep.Warnings {
		t.Log("warning:", w)
	}
	if err != nil {
		t.Skipf("no usable provider: %v", err)
	}
	t.Logf("document:\n%s", doc)
	if _, err := config.Parse(doc); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}
