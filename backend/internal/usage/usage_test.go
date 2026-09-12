// Tests cached provider freshness and fetch-failure metadata.

package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestBaseFetcherGet(t *testing.T) {
	t.Parallel()

	b := newBaseFetcher(agent.QuotaProviderCodex, "Codex", AuthKindOAuth, "")
	got := b.get(t.Context(), func(context.Context) (*ProviderQuota, error) {
		return &ProviderQuota{Provider: agent.QuotaProviderCodex}, nil
	})
	if got.FetchedAt.IsZero() || got.FetchError {
		t.Fatalf("first fetch metadata = (%v, %v), want timestamp without error", got.FetchedAt, got.FetchError)
	}

	b.fetchAt = time.Now().Add(-CacheTTL)
	got = b.get(t.Context(), func(context.Context) (*ProviderQuota, error) {
		return nil, errors.New("provider unavailable")
	})
	if !got.FetchError {
		t.Fatal("failed refresh FetchError = false, want true")
	}
	if b.cached.FetchError {
		t.Fatal("failed refresh mutated the cached successful snapshot")
	}
}
