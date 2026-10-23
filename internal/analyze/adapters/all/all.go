// Package all registers every analyzer adapter. Import it for its side
// effect; adding a tool is one new adapter package plus one line here.
package all

import (
	_ "github.com/alex-danilin/cbrt/internal/analyze/adapters/antigravity"
	_ "github.com/alex-danilin/cbrt/internal/analyze/adapters/claude"
	_ "github.com/alex-danilin/cbrt/internal/analyze/adapters/codex"
	_ "github.com/alex-danilin/cbrt/internal/analyze/adapters/fixturetool"
)
