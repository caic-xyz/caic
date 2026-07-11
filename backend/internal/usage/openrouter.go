// OpenRouter API key usage quota fetcher with caching and exponential backoff.

package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const openRouterCreditsURL = "https://openrouter.ai/api/v1/credits" //nolint:gosec // URL, not a credential

// openRouterCreditsPayload mirrors the OpenRouter GET /api/v1/credits response.
type openRouterCreditsPayload struct {
	Data openRouterCreditsData `json:"data"`
}

type openRouterCreditsData struct {
	TotalCredits float64 `json:"total_credits"`
	TotalUsage   float64 `json:"total_usage"`
}

// OpenRouterFetcher fetches OpenRouter credit balance.
type OpenRouterFetcher struct {
	baseFetcher

	client *http.Client
	apiKey string
}

// NewOpenRouterFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewOpenRouterFetcher(apiKey string) *OpenRouterFetcher {
	if apiKey == "" {
		return nil
	}
	return &OpenRouterFetcher{
		baseFetcher: newBaseFetcher("openrouter", "OpenRouter", "apikey", "https://openrouter.ai/settings/credits"),
		client:      &http.Client{Timeout: 10 * time.Second},
		apiKey:      apiKey,
	}
}

// Get returns the cached credit balance.
func (f *OpenRouterFetcher) Get(ctx context.Context) *ProviderQuota {
	return f.get(ctx, f.fetch)
}

func (f *OpenRouterFetcher) fetch(ctx context.Context) (*ProviderQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterCreditsURL, http.NoBody)
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
		return nil, fmt.Errorf("OpenRouter credits API returned %d: %s", resp.StatusCode, body)
	}

	// Decode into RawMessage first to handle potential shape variations.
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode OpenRouter credits: %w", err)
	}

	// OpenRouter returns {data:{total_credits, total_usage}}.
	// Balance = total_credits - total_usage.
	var payload openRouterCreditsPayload
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Data.TotalCredits != 0 {
		out := f.quota()
		out.Balance = QuotaBalance{
			Currency: "USD",
			Total:    payload.Data.TotalCredits - payload.Data.TotalUsage,
		}
		return out, nil
	}

	slog.WarnContext(ctx, "unrecognized OpenRouter credits response shape", "body", string(raw))
	return nil, fmt.Errorf("unrecognized OpenRouter credits response shape: %s", string(raw))
}
