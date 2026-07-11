// DeepSeek API key usage quota fetcher with caching and exponential backoff.

package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
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
		baseFetcher: newBaseFetcher("deepseek", "DeepSeek", "apikey", "https://platform.deepseek.com/usage"),
		client:      &http.Client{Timeout: 10 * time.Second},
		apiKey:      apiKey,
	}
}

// Get returns the cached balance, refreshing if stale.
func (f *DeepSeekFetcher) Get(ctx context.Context) *ProviderQuota {
	return f.get(ctx, f.fetch)
}

func (f *DeepSeekFetcher) fetch(ctx context.Context) (*ProviderQuota, error) {
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

	out := f.quota()
	// Take the first balance info (usually CNY).
	for _, bi := range raw.BalanceInfos {
		total, _ := parseFloat(bi.TotalBalance)
		granted, _ := parseFloat(bi.GrantedBalance)
		toppedUp, _ := parseFloat(bi.ToppedUpBalance)
		out.Balance = QuotaBalance{
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
