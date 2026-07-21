// Tests for Codex OAuth usage quota parsing.

package usage

import "testing"

func TestCodexFetcher(t *testing.T) {
	t.Parallel()

	t.Run("labels primary seven-day window from its duration", func(t *testing.T) {
		t.Parallel()

		got := codexWindowLabel(codexWindowSnapshot{LimitWindowSeconds: 7 * 24 * 60 * 60}, "primary")
		if got != "7d" {
			t.Errorf("codexWindowLabel() = %q, want %q", got, "7d")
		}
	})
}
