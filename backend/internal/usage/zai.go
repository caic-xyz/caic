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

var zaiAccountReportURL = "https://api.z.ai/api/biz/account/query-customer-account-report"

// zaiAccountReportPayload mirrors the data object of the Z.ai account report
// endpoint. Amounts are USD; null fields are absent on some accounts.
type zaiAccountReportPayload struct {
	Balance          *float64 `json:"balance"`
	AvailableBalance *float64 `json:"availableBalance"`
	GiveAmount       *float64 `json:"giveAmount"`
	RechargeAmount   *float64 `json:"rechargeAmount"`
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
		baseFetcher: newBaseFetcher(agent.QuotaProviderZai, AuthKindAPIKey, "https://z.ai/manage-apikey/billing"),
		client:      &http.Client{Timeout: 10 * time.Second},
		apiKey:      apiKey,
	}
}

// Get returns the cached credit balance, refreshing if stale.
func (f *ZaiFetcher) Get(ctx context.Context) *ProviderQuota {
	return f.get(ctx, f.fetch)
}

func (f *ZaiFetcher) fetch(ctx context.Context) (*ProviderQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, zaiAccountReportURL, http.NoBody)
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
		return nil, fmt.Errorf("z.ai account report API returned %d: %s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	payload, err := parseZaiAccountReport(body)
	if err != nil {
		return nil, err
	}

	total := payload.Balance
	if payload.AvailableBalance != nil {
		total = payload.AvailableBalance
	}
	out := f.quota()
	out.Balance = QuotaBalance{
		Currency: "USD",
		Total:    derefFloat(total),
		Granted:  derefFloat(payload.GiveAmount),
		ToppedUp: derefFloat(payload.RechargeAmount),
	}
	return out, nil
}

// parseZaiAccountReport accepts the {code, data} envelope the endpoint
// returns, or the report object at the top level.
func parseZaiAccountReport(body []byte) (zaiAccountReportPayload, error) {
	var envelope struct {
		Code int                     `json:"code"`
		Data zaiAccountReportPayload `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return zaiAccountReportPayload{}, fmt.Errorf("decode z.ai account report: %w", err)
	}
	if envelope.Data.Balance != nil || envelope.Data.AvailableBalance != nil {
		return envelope.Data, nil
	}
	var top zaiAccountReportPayload
	if err := json.Unmarshal(body, &top); err != nil {
		return zaiAccountReportPayload{}, fmt.Errorf("decode z.ai account report: %w", err)
	}
	if top.Balance == nil && top.AvailableBalance == nil {
		return zaiAccountReportPayload{}, fmt.Errorf("unrecognized z.ai account report response shape: %s", body)
	}
	return top, nil
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
