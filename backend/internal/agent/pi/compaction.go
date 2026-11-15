// Native Pi compaction budgets use an SDK launcher without rewriting user settings.

package pi

import (
	"embed"
	"strconv"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

//go:embed compaction.mjs
var compactionLauncher embed.FS

const compactionLauncherPath = agent.RelayDir + "/compaction.mjs"

func compactionArgs(c agent.CompactionBudget) []string {
	args := []string{"node", compactionLauncherPath}
	if c.ReserveTokens != nil {
		args = append(args, "--reserve-tokens", strconv.Itoa(*c.ReserveTokens))
	}
	if c.KeepRecentTokens != nil {
		args = append(args, "--keep-recent-tokens", strconv.Itoa(*c.KeepRecentTokens))
	}
	return args
}
