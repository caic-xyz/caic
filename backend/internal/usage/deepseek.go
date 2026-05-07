// DeepSeek API key usage quota fetcher with caching and exponential backoff.
// Implements ProviderFetcher.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

const deepseekBalanceURL = "https://api.deepseek.com/user/balance"

// deepseekBalancePayload mirrors the DeepSeek GET /user/balance response.
type deepseekBalancePayload struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency        string `json:"currency"`
		TotalBalance    string `json:"total_balance"`
		GrantedBalance  string `json:"granted_balance"`
		ToppedUpBalance string `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

// DeepSeekFetcher fetches DeepSeek balance via API key. The key is static
// (from config/env), so no file watcher is needed.
type DeepSeekFetcher struct {
	baseFetcher

	client *http.Client
	apiKey string
}

// NewDeepSeekFetcher creates a fetcher with the given API key. Returns nil
// when apiKey is empty.
func NewDeepSeekFetcher(apiKey string) *DeepSeekFetcher {
	if apiKey == "" {
		return nil
	}
	return &DeepSeekFetcher{
		client: &http.Client{Timeout: 10 * time.Second},
		apiKey: apiKey,
	}
}

// Provider returns the provider identifier.
func (f *DeepSeekFetcher) Provider() string { return "deepseek" }

// Label returns the human-readable provider name.
func (f *DeepSeekFetcher) Label() string { return "DeepSeek" }

// AuthKind returns the authentication method.
func (f *DeepSeekFetcher) AuthKind() string { return "apikey" }

// UsageURL returns the link to the provider's usage/billing page.
func (f *DeepSeekFetcher) UsageURL() string { return "https://platform.deepseek.com/usage" }

// Get returns the cached balance, refreshing if stale.
func (f *DeepSeekFetcher) Get(ctx context.Context) *v1.ProviderQuota {
	return f.get(ctx, f.fetch, "DeepSeek")
}

func (f *DeepSeekFetcher) fetch(ctx context.Context) (*v1.ProviderQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, deepseekBalanceURL, http.NoBody)
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
		return nil, fmt.Errorf("DeepSeek balance API returned %d: %s", resp.StatusCode, body)
	}

	var raw deepseekBalancePayload
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode DeepSeek balance: %w", err)
	}

	out := &v1.ProviderQuota{
		Provider: f.Provider(),
		Label:    f.Label(),
		AuthKind: f.AuthKind(),
	}
	// Take the first balance info (usually CNY).
	for _, bi := range raw.BalanceInfos {
		total, _ := parseFloat(bi.TotalBalance)
		granted, _ := parseFloat(bi.GrantedBalance)
		toppedUp, _ := parseFloat(bi.ToppedUpBalance)
		out.Balance = v1.QuotaBalance{
			Currency: bi.Currency,
			Total:    total,
			Granted:  granted,
			ToppedUp: toppedUp,
		}
		break
	}
	return out, nil
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}
