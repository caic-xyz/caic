// Alibaba Cloud Model Studio (DashScope) API provider stub. No programmatic
// balance endpoint is available to DashScope API keys; this fetcher registers
// the provider so the UI can display its logo and link to the console for
// manual balance checks.

package usage

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// AlibabaFetcher registers the Alibaba Cloud Model Studio provider without
// balance data. DashScope model API keys cannot query account balance.
type AlibabaFetcher struct {
	baseFetcher
}

// NewAlibabaFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewAlibabaFetcher(apiKey string) *AlibabaFetcher {
	if apiKey == "" {
		return nil
	}
	return &AlibabaFetcher{baseFetcher: newBaseFetcher(agent.QuotaProviderAlibaba, "Alibaba", AuthKindAPIKey, "https://modelstudio.console.alibabacloud.com/us-east-1/model/model-telemetry")}
}

// Get returns provider metadata with no balance data.
func (f *AlibabaFetcher) Get(_ context.Context) *ProviderQuota {
	return f.quota()
}
