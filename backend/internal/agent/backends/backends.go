// Package backends constructs configured agent backend sets.
package backends

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
)

// Default returns the standard caic agent backend set.
func Default(cacheDir string, harnessEnv map[string][]string) map[harness.Name]agent.Backend {
	return map[harness.Name]agent.Backend{
		harness.Claude:   claudecode.New(),
		harness.Codex:    codex.New(cacheDir, harnessEnv[string(harness.Codex)]),
		harness.OpenCode: opencode.New(cacheDir, harnessEnv[string(harness.OpenCode)]),
		harness.Pi:       pi.New(cacheDir, harnessEnv[string(harness.Pi)]),
	}
}
