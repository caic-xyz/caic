// Tests for shared agent message and quota provider types.

package agent

import "testing"

func TestQuotaProvider(t *testing.T) {
	t.Parallel()
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		if !QuotaProviderClaudeCode.Valid() {
			t.Error("QuotaProviderClaudeCode.Valid() = false, want true")
		}
		if QuotaProvider("unknown").Valid() {
			t.Error("QuotaProvider(unknown).Valid() = true, want false")
		}
	})
}

func TestRateLimitStatus(t *testing.T) {
	t.Parallel()
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		if !RateLimitStatusAllowedWarning.Valid() {
			t.Error("RateLimitStatusAllowedWarning.Valid() = false, want true")
		}
		if RateLimitStatus("unknown").Valid() {
			t.Error("RateLimitStatus(unknown).Valid() = true, want false")
		}
	})
}
