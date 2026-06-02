// Package registry constructs configured agent backend sets.
package registry

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
)

// DefaultBackends returns the standard caic agent backend set.
func DefaultBackends(cacheDir string, harnessEnv map[string][]string) map[agent.Harness]agent.Backend {
	return map[agent.Harness]agent.Backend{
		agent.Claude:   claudecode.New(),
		agent.Codex:    codex.New(cacheDir, harnessEnv[string(agent.Codex)]),
		agent.OpenCode: opencode.New(cacheDir, harnessEnv[string(agent.OpenCode)]),
		agent.Pi:       pi.New(cacheDir, harnessEnv[string(agent.Pi)]),
	}
}
