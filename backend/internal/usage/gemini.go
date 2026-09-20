// Google Gemini API provider stub. AI Studio API keys carry no programmatic
// billing data (spend lives on the Google Cloud project); this fetcher
// registers the provider so the UI can display its logo and link to the
// console for manual usage checks.

package usage

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// GeminiFetcher registers the Google Gemini provider without balance data.
// AI Studio keys expose no billing endpoint; usage is visible in AI Studio
// and the linked Google Cloud project.
type GeminiFetcher struct {
	baseFetcher
}

// NewGeminiFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewGeminiFetcher(apiKey string) *GeminiFetcher {
	if apiKey == "" {
		return nil
	}
	return &GeminiFetcher{baseFetcher: newBaseFetcher(agent.QuotaProviderGemini, "Gemini", AuthKindAPIKey, "https://aistudio.google.com/spend")}
}

// Get returns provider metadata with no balance data.
func (f *GeminiFetcher) Get(_ context.Context) *ProviderQuota {
	return f.quota()
}
