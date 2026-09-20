// Z.ai API key credit balance fetcher with caching and exponential backoff.

package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

var zaiCreditGrantsURL = "https://api.z.ai/api/paas/v4/user/credit_grants" //nolint:gosec // URL, not a credential; swapped in tests

// zaiCreditGrantsPayload mirrors the Z.ai PAYG credit grants response, which
// follows the OpenAI /dashboard/billing/credit_grants shape.
type zaiCreditGrantsPayload struct {
	TotalGranted   float64 `json:"total_granted"`
	TotalUsed      float64 `json:"total_used"`
	TotalAvailable float64 `json:"total_available"`
}

// ZaiFetcher fetches the Z.ai pay-as-you-go credit balance.
type ZaiFetcher struct {
	baseFetcher

	client *http.Client
	apiKey string
}

// NewZaiFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewZaiFetcher(apiKey string) *ZaiFetcher {
	if apiKey == "" {
		return nil
	}
	return &ZaiFetcher{
		baseFetcher: newBaseFetcher(agent.QuotaProviderZai, "Z.ai", AuthKindAPIKey, "https://z.ai/manage-apikey/billing"),
		client:      &http.Client{Timeout: 10 * time.Second},
		apiKey:      apiKey,
	}
}

// Get returns the cached credit balance, refreshing if stale.
func (f *ZaiFetcher) Get(ctx context.Context) *ProviderQuota {
	return f.get(ctx, f.fetch)
}

func (f *ZaiFetcher) fetch(ctx context.Context) (*ProviderQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, zaiCreditGrantsURL, http.NoBody)
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
		return nil, fmt.Errorf("z.ai credit grants API returned %d: %s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	payload, err := parseZaiCreditGrants(body)
	if err != nil {
		return nil, err
	}

	out := f.quota()
	out.Balance = QuotaBalance{
		Currency: "USD",
		Total:    payload.TotalAvailable,
		Granted:  payload.TotalGranted,
	}
	return out, nil
}

// parseZaiCreditGrants accepts the OpenAI-style payload at the top level or
// wrapped in a {code, data} monitor envelope; both appear on z.ai hosts.
func parseZaiCreditGrants(body []byte) (zaiCreditGrantsPayload, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return zaiCreditGrantsPayload{}, fmt.Errorf("decode Z.ai credit grants: %w", err)
	}
	if _, ok := fields["total_granted"]; ok {
		var payload zaiCreditGrantsPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			return zaiCreditGrantsPayload{}, fmt.Errorf("decode Z.ai credit grants: %w", err)
		}
		return payload, nil
	}
	if data, ok := fields["data"]; ok {
		var payload zaiCreditGrantsPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			return zaiCreditGrantsPayload{}, fmt.Errorf("decode Z.ai credit grants: %w", err)
		}
		return payload, nil
	}
	return zaiCreditGrantsPayload{}, fmt.Errorf("unrecognized Z.ai credit grants response shape: %s", body)
}
