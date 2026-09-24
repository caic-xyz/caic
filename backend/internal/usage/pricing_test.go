// Tests for per-model pricing schedules and quota-provider price resolution.

package usage

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestModelPriceCost(t *testing.T) {
	t.Parallel()
	t.Run("Tokens", func(t *testing.T) {
		t.Parallel()
		p := ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.03, OutputPerMTok: 0.50}
		u := agent.Usage{InputTokens: 1_000_000, OutputTokens: 2_000_000, CacheReadInputTokens: 3_000_000}
		want := 0.15 + 2*0.50 + 3*0.03
		if got := p.Cost(u); got != want {
			t.Errorf("Cost = %v, want %v", got, want)
		}
	})
	t.Run("CacheWriteDefaultsToInput", func(t *testing.T) {
		t.Parallel()
		p := ModelPrice{InputPerMTok: 0.30, CachedInputPerMTok: 0.006, OutputPerMTok: 1.20}
		u := agent.Usage{InputTokens: 100_000, CacheCreationInputTokens: 400_000}
		want := 0.1*0.30 + 0.4*0.30
		if got := p.Cost(u); got != want {
			t.Errorf("Cost = %v, want %v", got, want)
		}
	})
	t.Run("OneHourCacheWrite", func(t *testing.T) {
		t.Parallel()
		p := ModelPrice{InputPerMTok: 4, CacheWritePerMTok: 5, CacheWrite1hPerMTok: 8, CachedInputPerMTok: 0.2, OutputPerMTok: 20}
		got := p.CostBuckets(1_000_000, 1_000_000, 1_000_000, 1_000_000, 1_000_000)
		if want := 37.2; got != want {
			t.Errorf("CostBuckets = %v, want %v", got, want)
		}
		usage := agent.Usage{CacheCreationInputTokens: 1_000_000, CacheTTLSeconds: 3600}
		if got := p.Cost(usage); got != 8 {
			t.Errorf("Cost with one-hour cache = %v, want 8", got)
		}
	})
}

func TestModelPricingPrice(t *testing.T) {
	t.Parallel()
	t.Run("CalendarEffectiveChange", func(t *testing.T) {
		t.Parallel()
		pricing := ModelPricing{
			{Price: ModelPrice{InputPerMTok: 1.0, OutputPerMTok: 2.0}},
			{EffectiveFrom: "2026-09-01", Price: ModelPrice{InputPerMTok: 1.5, OutputPerMTok: 3.0}},
		}
		before := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
		after := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		if got, _ := pricing.Price(before); got.InputPerMTok != 1.0 {
			t.Errorf("price before effective date = %+v, want old price", got)
		}
		if got, _ := pricing.Price(after); got.InputPerMTok != 1.5 {
			t.Errorf("price on effective date = %+v, want new price", got)
		}
	})
	t.Run("NoMatchingTier", func(t *testing.T) {
		t.Parallel()
		// A schedule with only peak-windowed tiers prices nothing outside
		// its windows.
		pricing := ModelPricing{
			{PeakWindows: []PeakWindow{{Start: "01:00", End: "02:00"}}, Price: ModelPrice{InputPerMTok: 1.0}},
		}
		if _, ok := pricing.Price(time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)); ok {
			t.Error("priced outside every peak window, want unpriced")
		}
	})
}

func TestPricerModelPrice(t *testing.T) {
	t.Parallel()
	t.Run("Zai", func(t *testing.T) {
		t.Parallel()
		p := NewPricer(nil)
		tests := []struct {
			model string
			want  ModelPrice
		}{
			{"zai/glm-5.3-flash", ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.03, OutputPerMTok: 0.50}},
			{"zai-coding-plan/glm-5.3-flash", ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.03, OutputPerMTok: 0.50}},
			{"zai/glm-5.3", ModelPrice{InputPerMTok: 1.4, CachedInputPerMTok: 0.26, OutputPerMTok: 4.4}},
			{"zai/glm-4.7-flash", ModelPrice{}},
		}
		for _, tc := range tests {
			got, ok := p.ModelPrice("", tc.model, time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
			if !ok {
				t.Errorf("ModelPrice(%q) not priced", tc.model)
				continue
			}
			if got != tc.want {
				t.Errorf("%s price = %+v, want %+v", tc.model, got, tc.want)
			}
		}
	})
	t.Run("DeepSeekPeakWindows", func(t *testing.T) {
		t.Parallel()
		p := NewPricer(nil)
		tests := []struct {
			name string
			at   time.Time
			want ModelPrice
		}{
			{
				"weekday peak morning",
				time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC), // Wednesday.
				ModelPrice{InputPerMTok: 0.30, CachedInputPerMTok: 0.006, OutputPerMTok: 1.20},
			},
			{
				"peak start boundary is inclusive",
				time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC),
				ModelPrice{InputPerMTok: 0.30, CachedInputPerMTok: 0.006, OutputPerMTok: 1.20},
			},
			{
				"peak end boundary is exclusive",
				time.Date(2026, 7, 15, 4, 0, 0, 0, time.UTC),
				ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.003, OutputPerMTok: 0.60},
			},
			{
				"gap between peak windows",
				time.Date(2026, 7, 15, 5, 0, 0, 0, time.UTC),
				ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.003, OutputPerMTok: 0.60},
			},
			{
				"weekend is always off-peak",
				time.Date(2026, 7, 18, 2, 0, 0, 0, time.UTC), // Saturday.
				ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.003, OutputPerMTok: 0.60},
			},
			{
				"legacy model name shares flash pricing",
				time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC),
				ModelPrice{InputPerMTok: 0.30, CachedInputPerMTok: 0.006, OutputPerMTok: 1.20},
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got, ok := p.ModelPrice("", "deepseek/deepseek-flash", tc.at)
				if !ok {
					t.Fatal("deepseek-flash not priced")
				}
				if got != tc.want {
					t.Errorf("price = %+v, want %+v", got, tc.want)
				}
			})
		}
	})
	t.Run("DeepSeekHolidayIsOffPeak", func(t *testing.T) {
		t.Parallel()
		p := NewPricer(nil)
		// 2026-09-25 (Friday) is the Mid-Autumn holiday; the following Monday
		// peaks normally.
		holiday := time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC)
		got, ok := p.ModelPrice("", "deepseek/deepseek-flash", holiday)
		if !ok {
			t.Fatal("deepseek-flash not priced")
		}
		if want := (ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.003, OutputPerMTok: 0.60}); got != want {
			t.Errorf("holiday peak-hour price = %+v, want off-peak %+v", got, want)
		}
		after := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)
		got, ok = p.ModelPrice("", "deepseek/deepseek-flash", after)
		if !ok {
			t.Fatal("deepseek-flash not priced")
		}
		if want := (ModelPrice{InputPerMTok: 0.30, CachedInputPerMTok: 0.006, OutputPerMTok: 1.20}); got != want {
			t.Errorf("post-holiday peak-hour price = %+v, want peak %+v", got, want)
		}
	})
	t.Run("Unpriced", func(t *testing.T) {
		t.Parallel()
		p := NewPricer(nil)
		for _, model := range []string{
			"unknown-provider/glm-5.3-flash",
			"",
		} {
			if _, ok := p.ModelPrice("", model, time.Now()); ok {
				t.Errorf("ModelPrice(%q) priced, want unpriced", model)
			}
		}
		// The claudecode provider never prices: Claude Code's own reported
		// total stays authoritative.
		if _, ok := p.ModelPrice(agent.QuotaProviderClaudeCode, "claude-sonnet-4-5", time.Now()); ok {
			t.Error("claudecode provider priced, want unpriced")
		}
	})
	t.Run("CodexOpenAIApiEquivalent", func(t *testing.T) {
		t.Parallel()
		p := NewPricer(nil)
		want := ModelPrice{InputPerMTok: 2.0, CachedInputPerMTok: 0.20, CacheWritePerMTok: 2.50, OutputPerMTok: 12.0}
		for _, model := range []string{
			"openai-codex/gpt-5.6-terra",
			"codex/gpt-5.6-terra",
			"openai/gpt-5.6-terra",
		} {
			got, ok := p.ModelPrice("", model, time.Now())
			if !ok {
				t.Errorf("ModelPrice(%q) not priced", model)
				continue
			}
			if got != want {
				t.Errorf("%s price = %+v, want %+v", model, got, want)
			}
		}
		// The Codex harness reports prefixless model IDs; the task hints the
		// provider.
		got, ok := p.ModelPrice(agent.QuotaProviderCodex, "gpt-5.6-terra", time.Now())
		if !ok || got != want {
			t.Errorf("hinted price = %+v/%v, want %+v", got, ok, want)
		}
		// Daybreak aliases price at the model they currently point to.
		got, ok = p.ModelPrice(agent.QuotaProviderCodex, "gpt-daybreak-blue-latest", time.Now())
		if !ok {
			t.Fatal("daybreak blue not priced")
		}
		if want := (ModelPrice{InputPerMTok: 4.0, CachedInputPerMTok: 0.40, CacheWritePerMTok: 5.0, OutputPerMTok: 20.0}); got != want {
			t.Errorf("daybreak blue price = %+v, want %+v", got, want)
		}
	})
	t.Run("NewModelPrices", func(t *testing.T) {
		t.Parallel()
		p := NewPricer(nil)
		at := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
		for _, tc := range []struct {
			provider agent.QuotaProvider
			model    string
			want     ModelPrice
		}{
			{agent.QuotaProviderAnthropic, "claude-opus-5-5", ModelPrice{InputPerMTok: 4, CachedInputPerMTok: 0.2, CacheWritePerMTok: 5, CacheWrite1hPerMTok: 8, OutputPerMTok: 20}},
			{agent.QuotaProviderCodex, "gpt-6-astra", ModelPrice{InputPerMTok: 10, CachedInputPerMTok: 1, CacheWritePerMTok: 12.5, OutputPerMTok: 50}},
			{agent.QuotaProviderCodex, "gpt-6-luna", ModelPrice{InputPerMTok: 0.1, CachedInputPerMTok: 0.01, CacheWritePerMTok: 0.125, OutputPerMTok: 0.5}},
			{agent.QuotaProviderCodex, "gpt-6-sol", ModelPrice{InputPerMTok: 2, CachedInputPerMTok: 0.2, CacheWritePerMTok: 2.5, OutputPerMTok: 10}},
		} {
			got, ok := p.ModelPrice(tc.provider, tc.model, at)
			if !ok || got != tc.want {
				t.Errorf("%s price = %+v/%v, want %+v", tc.model, got, ok, tc.want)
			}
		}
		for _, model := range []string{"openrouter/anthropic/claude-opus-5-5", "openrouter/openai/gpt-6-sol"} {
			if _, ok := p.ModelPrice("", model, at); !ok {
				t.Errorf("%s fallback price unavailable", model)
			}
		}
	})
	t.Run("AnthropicApiEquivalent", func(t *testing.T) {
		t.Parallel()
		p := NewPricer(nil)
		got, ok := p.ModelPrice("", "anthropic/claude-sonnet-4-5", time.Now())
		if !ok {
			t.Fatal("anthropic model not priced")
		}
		want := ModelPrice{InputPerMTok: 3.0, CachedInputPerMTok: 0.30, CacheWritePerMTok: 3.75, OutputPerMTok: 15.0}
		if got != want {
			t.Errorf("price = %+v, want %+v", got, want)
		}
		// Dated model IDs share the family price.
		got, ok = p.ModelPrice("", "anthropic/claude-opus-4-6-20260115", time.Now())
		if !ok {
			t.Fatal("dated anthropic model not priced")
		}
		if want := (ModelPrice{InputPerMTok: 5.0, CachedInputPerMTok: 0.50, CacheWritePerMTok: 6.25, OutputPerMTok: 25.0}); got != want {
			t.Errorf("price = %+v, want %+v", got, want)
		}
	})
	t.Run("OpenRouterViaFetcher", func(t *testing.T) {
		t.Parallel()
		var hits int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			_, _ = w.Write([]byte(openRouterModelsPayload))
		}))
		t.Cleanup(server.Close)
		p := NewPricer([]ProviderFetcher{stubOpenRouterFetcher(t, server)})

		price, ok := p.ModelPrice("", "openrouter/z-ai/glm-5.3-flash", time.Now())
		if !ok {
			t.Fatal("openrouter model not priced via fetcher")
		}
		want := ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.03, CacheWritePerMTok: 0.15, OutputPerMTok: 0.50}
		if price != want {
			t.Errorf("price = %+v, want %+v", price, want)
		}
		if hits != 1 {
			t.Errorf("models endpoint hit %d times, want 1", hits)
		}
	})
	t.Run("OpenRouterFallbackWithoutFetcher", func(t *testing.T) {
		t.Parallel()
		// Without a registered OpenRouter fetcher, pricing falls back to the
		// upstream provider's static published prices.
		p := NewPricer(nil)
		price, ok := p.ModelPrice("", "openrouter/z-ai/glm-5.3-flash", time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
		if !ok {
			t.Fatal("openrouter model not priced via upstream fallback")
		}
		if want := (ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.03, OutputPerMTok: 0.50}); price != want {
			t.Errorf("price = %+v, want %+v", price, want)
		}
	})
	t.Run("OpenRouterFallbackOnFetchError", func(t *testing.T) {
		t.Parallel()
		// A registered fetcher whose pricing fetch fails also falls back to
		// the upstream static prices.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)
		p := NewPricer([]ProviderFetcher{stubOpenRouterFetcher(t, server)})

		price, ok := p.ModelPrice("", "openrouter/z-ai/glm-5.3-flash", time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
		if !ok {
			t.Fatal("openrouter model not priced via upstream fallback")
		}
		if want := (ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.03, OutputPerMTok: 0.50}); price != want {
			t.Errorf("price = %+v, want %+v", price, want)
		}
	})
}

func TestPricingPhaseFor(t *testing.T) {
	t.Parallel()
	t.Run("OtherProvidersUnphased", func(t *testing.T) {
		t.Parallel()
		phase, transition := PricingPhaseFor(agent.QuotaProviderZai, time.Now())
		if phase != "" || !transition.IsZero() {
			t.Errorf("phase = %q/%v, want empty", phase, transition)
		}
	})
	t.Run("PeakReportsWindowEnd", func(t *testing.T) {
		t.Parallel()
		// Monday 02:00 UTC is inside the 01:00-04:00 window.
		phase, transition := PricingPhaseFor(agent.QuotaProviderDeepSeek, time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC))
		if phase != PricingPhasePeak || !transition.Equal(time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)) {
			t.Errorf("phase = %q/%v, want peak at 04:00", phase, transition)
		}
	})
	t.Run("PeakSoonReportsWindowStart", func(t *testing.T) {
		t.Parallel()
		// Monday 00:45 UTC is 15 minutes before the 01:00 window.
		phase, transition := PricingPhaseFor(agent.QuotaProviderDeepSeek, time.Date(2026, 9, 21, 0, 45, 0, 0, time.UTC))
		if phase != PricingPhasePeakSoon || !transition.Equal(time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)) {
			t.Errorf("phase = %q/%v, want peak-soon at 01:00", phase, transition)
		}
	})
	t.Run("OffPeakHasNoTransition", func(t *testing.T) {
		t.Parallel()
		// Saturday 02:00 UTC is never peak.
		phase, transition := PricingPhaseFor(agent.QuotaProviderDeepSeek, time.Date(2026, 9, 19, 2, 0, 0, 0, time.UTC))
		if phase != PricingPhaseOffPeak || !transition.IsZero() {
			t.Errorf("phase = %q/%v, want off-peak", phase, transition)
		}
	})
	t.Run("HolidayIsOffPeak", func(t *testing.T) {
		t.Parallel()
		phase, _ := PricingPhaseFor(agent.QuotaProviderDeepSeek, time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC))
		if phase != PricingPhaseOffPeak {
			t.Errorf("phase = %q, want off-peak on a holiday", phase)
		}
	})
}

func TestParsePricingJSON(t *testing.T) {
	t.Parallel()

	// provider builds a one-provider document from extra provider fields.
	provider := func(fields string) string {
		return `{"providers":[{"provider":"p","source":"https://example.test","retrieved":"2026-09",` + fields + `}]}`
	}
	const model = `{"id":"m","tiers":[{"price":{"input_per_mtok":1}}]}`

	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		tables, err := parsePricingJSON(pricingJSON)
		if err != nil {
			t.Fatalf("parsePricingJSON(pricingJSON) = %v", err)
		}
		for _, name := range []string{"anthropic", "openai", "zai", "deepseek"} {
			if len(tables.providers[name]) == 0 {
				t.Errorf("provider %q has no models", name)
			}
		}
		if got := len(tables.peakWindows["deepseek"]); got != 2 {
			t.Errorf("deepseek peak windows = %d, want 2", got)
		}
		// An alias resolves to its target's schedule.
		tiers, ok := tables.providers["deepseek"]["deepseek-v4-flash"]
		if !ok {
			t.Fatal("deepseek-v4-flash is not priced")
		}
		if want := tables.providers["deepseek"]["deepseek-flash"]; !slices.EqualFunc(tiers, want, func(a, b PriceTier) bool { return a.Price == b.Price }) {
			t.Errorf("deepseek-v4-flash tiers = %+v, want deepseek-flash %+v", tiers, want)
		}
	})

	t.Run("Errors", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			data string
		}{
			{"InvalidJSON", `{`},
			{"NoProviders", `{}`},
			{"UnknownField", provider(`"models":[` + model + `],"typo":1`)},
			{"DuplicateProvider", `{"providers":[{"provider":"p","source":"s","retrieved":"r","models":[` + model + `]},{"provider":"p","source":"s","retrieved":"r","models":[` + model + `]}]}`},
			{"EmptyProviderName", `{"providers":[{"source":"s","retrieved":"r","models":[` + model + `]}]}`},
			{"MissingSource", `{"providers":[{"provider":"p","retrieved":"2026-09","models":[` + model + `]}]}`},
			{"BadHoliday", provider(`"holidays":["2026-1-1"],"models":[` + model + `]`)},
			{"BadWeekday", provider(`"windows":{"peak":[{"days":["monday"],"start":"01:00","end":"02:00"}]},"models":[` + model + `]`)},
			{"BadStart", provider(`"windows":{"peak":[{"start":"01:60","end":"02:00"}]},"models":[` + model + `]`)},
			{"BadEnd", provider(`"windows":{"peak":[{"start":"01:00","end":""}]},"models":[` + model + `]`)},
			{"EmptyWindowGroup", provider(`"windows":{"peak":[]},"models":[` + model + `]`)},
			{"NoModels", provider(`"models":[]`)},
			{"EmptyModelID", provider(`"models":[{"tiers":[{"price":{}}]}]`)},
			{"DuplicateModel", provider(`"models":[` + model + `,` + model + `]`)},
			{"AliasAndTiers", provider(`"models":[` + model + `,{"id":"n","alias":"m","tiers":[{"price":{}}]}]`)},
			{"NeitherAliasNorTiers", provider(`"models":[{"id":"m"}]`)},
			{"UnpricedAlias", provider(`"models":[` + model + `,{"id":"n","alias":"missing"}]`)},
			{"UnknownWindowGroup", provider(`"models":[{"id":"m","tiers":[{"windows":"peak","price":{}}]}]`)},
			{"MissingPrice", provider(`"models":[{"id":"m","tiers":[{}]}]`)},
			{"NegativePrice", provider(`"models":[{"id":"m","tiers":[{"price":{"input_per_mtok":-1}}]}]`)},
			{"BadEffectiveFrom", provider(`"models":[{"id":"m","tiers":[{"effective_from":"2026-9-1","price":{}}]}]`)},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if tables, err := parsePricingJSON([]byte(tc.data)); err == nil {
					t.Errorf("parsePricingJSON(%s) = %+v, want error", tc.name, tables)
				}
			})
		}
	})
}

func TestModelPriceValidate(t *testing.T) {
	t.Parallel()
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			p    ModelPrice
		}{
			{"Priced", ModelPrice{InputPerMTok: 1, CachedInputPerMTok: 0.1, CacheWritePerMTok: 1.25, CacheWrite1hPerMTok: 2.5, OutputPerMTok: 5}},
			// A free model prices every field at zero.
			{"Free", ModelPrice{}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if err := tc.p.Validate(); err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			})
		}
	})
	t.Run("Errors", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			p    ModelPrice
		}{
			{"Input", ModelPrice{InputPerMTok: -1}},
			{"CachedInput", ModelPrice{CachedInputPerMTok: -0.01}},
			{"CacheWrite", ModelPrice{CacheWritePerMTok: -0.01}},
			{"CacheWrite1h", ModelPrice{CacheWrite1hPerMTok: -0.01}},
			{"Output", ModelPrice{OutputPerMTok: -1}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if err := tc.p.Validate(); err == nil {
					t.Error("Validate() = nil, want error")
				}
			})
		}
	})
}
