# Development

Building, testing and releasing cbrt itself. Notes for coding agents working on the repository are in its AGENTS.md (local).


```sh
go vet ./... && go test ./...      # add -race on Linux/macOS (needs cgo)
go run ./scripts/synth-clients && go run ./scripts/synth-upstream   # regenerate the synthetic gateway fixtures
go run ./scripts/synth-transcripts/claude   # … and codex, antigravity: synthetic transcripts for the analyzer
go test -tags local ./internal/analyze/adapters/...   # check the adapters against your own stores (prints counts only; never in CI)
go test ./scripts/privacy           # privacy audit of the whole repository (run before pushing)
go run scripts/ship.go [vX.Y.Z]     # push main and wait for CI; with a version also tag, wait for the release build and install it (Windows)
cbrt record -upstream http://127.0.0.1:11434 -out captures/x   # inspect real client traffic locally (captures/ is git-ignored)
```
