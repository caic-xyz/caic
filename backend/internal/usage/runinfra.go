// RunInfra API key credit balance fetcher with caching and exponential backoff.

package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

var runInfraCreditsURL = "https://api.runinfra.ai/v1/credits" //nolint:gosec // URL, not a credential; swapped in tests

// runInfraCreditsPayload mirrors the RunInfra GET /v1/credits response. All
// monetary amounts are integer US cents.
type runInfraCreditsPayload struct {
	BalanceCents   int64  `json:"balance_cents"`
	AvailableCents int64  `json:"available_cents"`
	Currency       string `json:"currency"`
	Period         struct {
		SpentCents int64 `json:"spent_cents"`
	} `json:"period"`
	SpendCap *struct {
		LimitCents     int64 `json:"limit_cents"`
		Hard           bool  `json:"hard"`
		UsedCents      int64 `json:"used_cents"`
		RemainingCents int64 `json:"remaining_cents"`
	} `json:"spend_cap"`
}

// RunInfraFetcher fetches the RunInfra prepaid credit balance.
type RunInfraFetcher struct {
	baseFetcher

	client *http.Client
	apiKey string
}

// NewRunInfraFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewRunInfraFetcher(apiKey string) *RunInfraFetcher {
	if apiKey == "" {
		return nil
	}
	return &RunInfraFetcher{
		baseFetcher: newBaseFetcher(agent.QuotaProviderRunInfra, "RunInfra", AuthKindAPIKey, "https://runinfra.ai/inference/usage"),
		client:      &http.Client{Timeout: 10 * time.Second},
		apiKey:      apiKey,
	}
}

// Get returns the cached credit balance, refreshing if stale.
func (f *RunInfraFetcher) Get(ctx context.Context) *ProviderQuota {
	return f.get(ctx, f.fetch)
}

func (f *RunInfraFetcher) fetch(ctx context.Context) (*ProviderQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runInfraCreditsURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	req.Header.Set("User-Agent", "caic")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("RunInfra credits API returned %d: %s", resp.StatusCode, body)
	}

	var raw runInfraCreditsPayload
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode RunInfra credits: %w", err)
	}

	currency := strings.ToUpper(raw.Currency)
	if currency == "" {
		currency = "USD"
	}
	out := f.quota()
	out.Balance = QuotaBalance{
		Currency: currency,
		Total:    float64(raw.AvailableCents) / 100,
	}
	if raw.SpendCap != nil && raw.SpendCap.LimitCents > 0 {
		out.ExtraUsage = QuotaExtraUsage{
			Currency:     currency,
			IsEnabled:    raw.SpendCap.Hard,
			UsedCredits:  float64(raw.SpendCap.UsedCents) / 100,
			MonthlyLimit: float64(raw.SpendCap.LimitCents) / 100,
			UsedPct:      float64(raw.SpendCap.UsedCents) / float64(raw.SpendCap.LimitCents) * 100,
		}
	}
	return out, nil
}
