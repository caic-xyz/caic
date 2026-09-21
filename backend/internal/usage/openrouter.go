// OpenRouter API key credit balance and live per-model pricing fetcher.

package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers/openrouter"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

const openRouterCreditsURL = "https://openrouter.ai/api/v1/credits" //nolint:gosec // URL, not a credential

// openRouterPricingTTL is how long fetched OpenRouter prices stay fresh.
// OpenRouter adjusts prices occasionally, so refresh periodically.
const openRouterPricingTTL = time.Hour

// openRouterPricingFetchTimeout bounds a single pricing refresh.
const openRouterPricingFetchTimeout = 30 * time.Second

// openRouterCreditsPayload mirrors the OpenRouter GET /api/v1/credits response.
type openRouterCreditsPayload struct {
	Data openRouterCreditsData `json:"data"`
}

type openRouterCreditsData struct {
	TotalCredits float64 `json:"total_credits"`
	TotalUsage   float64 `json:"total_usage"`
}

// OpenRouterFetcher fetches the OpenRouter credit balance and serves live
// per-model pricing from OpenRouter's model listing.
type OpenRouterFetcher struct {
	baseFetcher

	client *http.Client
	models *openrouter.Client // Model listing client for pricing.
	apiKey string

	pricingMu        sync.Mutex
	prices           map[string]ModelPrice
	pricingFetchedAt time.Time
	pricingBackoff   time.Duration
	pricingErrorAt   time.Time
}

// NewOpenRouterFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewOpenRouterFetcher(ctx context.Context, apiKey string) *OpenRouterFetcher {
	if apiKey == "" {
		return nil
	}
	f := &OpenRouterFetcher{
		baseFetcher: newBaseFetcher(agent.QuotaProviderOpenRouter, AuthKindAPIKey, "https://openrouter.ai/settings/credits"),
		client:      &http.Client{Timeout: 10 * time.Second},
		apiKey:      apiKey,
	}
	models, err := openrouter.New(ctx, genai.ProviderOptionAPIKey(apiKey))
	if err != nil {
		slog.WarnContext(ctx, "openrouter model pricing client unavailable", "err", err)
	} else {
		f.models = models
	}
	return f
}

// Get returns the cached credit balance.
func (f *OpenRouterFetcher) Get(ctx context.Context) *ProviderQuota {
	return f.get(ctx, f.fetch)
}

// ModelPrice implements ModelPricer with OpenRouter's live per-model pricing,
// refreshed at openRouterPricingTTL. modelID is an OpenRouter model ID such
// as "z-ai/glm-5.3-flash"; a ":variant" suffix is ignored. The prices are
// current, so at is unused.
func (f *OpenRouterFetcher) ModelPrice(modelID string, _ time.Time) (ModelPrice, bool) {
	if i := strings.IndexByte(modelID, ':'); i >= 0 {
		modelID = modelID[:i]
	}
	modelID = strings.ToLower(modelID)
	f.pricingMu.Lock()
	defer f.pricingMu.Unlock()
	if f.prices == nil || time.Since(f.pricingFetchedAt) >= openRouterPricingTTL {
		if f.pricingBackoff == 0 || time.Since(f.pricingErrorAt) >= f.pricingBackoff {
			f.refreshPricing()
		}
	}
	price, ok := f.prices[modelID]
	return price, ok
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

// refreshPricing fetches the model price table, backing off after failures.
// The caller holds f.pricingMu.
func (f *OpenRouterFetcher) refreshPricing() {
	ctx, cancel := context.WithTimeout(context.Background(), openRouterPricingFetchTimeout)
	defer cancel()
	prices, err := f.fetchPricing(ctx)
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch OpenRouter model pricing", "err", err)
		f.pricingErrorAt = time.Now()
		if f.pricingBackoff == 0 {
			f.pricingBackoff = backoffMin
		} else {
			f.pricingBackoff = min(f.pricingBackoff*2, backoffMax)
		}
		return
	}
	f.pricingBackoff = 0
	f.prices = prices
	f.pricingFetchedAt = time.Now()
}

// fetchPricing fetches the OpenRouter model listing and converts per-token
// USD prices to per-million-token ModelPrice values. The caller holds
// f.pricingMu.
func (f *OpenRouterFetcher) fetchPricing(ctx context.Context) (map[string]ModelPrice, error) {
	if f.models == nil {
		return nil, errors.New("openrouter pricing client unavailable")
	}
	models, err := f.models.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, errors.New("openrouter model listing is empty")
	}
	prices := make(map[string]ModelPrice, len(models))
	for _, model := range models {
		m, ok := model.(*openrouter.Model)
		if !ok {
			continue
		}
		price, ok := f.modelPrice(m)
		if !ok {
			continue
		}
		prices[strings.ToLower(m.ID)] = price
	}
	return prices, nil
}

// modelPrice converts one OpenRouter model's per-token USD prices. The caller
// holds f.pricingMu.
func (f *OpenRouterFetcher) modelPrice(m *openrouter.Model) (ModelPrice, bool) {
	prompt, err1 := strconv.ParseFloat(m.Pricing.Prompt, 64)
	completion, err2 := strconv.ParseFloat(m.Pricing.Completion, 64)
	if err1 != nil || err2 != nil {
		return ModelPrice{}, false
	}
	price := ModelPrice{
		InputPerMTok:  prompt * 1_000_000,
		OutputPerMTok: completion * 1_000_000,
	}
	if v, err := strconv.ParseFloat(m.Pricing.InputCacheRead, 64); err == nil {
		price.CachedInputPerMTok = v * 1_000_000
	}
	if v, err := strconv.ParseFloat(m.Pricing.InputCacheWrite, 64); err == nil {
		price.CacheWritePerMTok = v * 1_000_000
	}
	return price, true
}
