// Per-model token pricing from quota providers, with time-dependent price tiers.

package usage

import (
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// pricingDateLayout is the EffectiveFrom date layout of a PriceTier.
const pricingDateLayout = "2006-01-02"

// ModelPrice holds USD per-million-token prices for one model. Providers that
// do not surcharge cache writes leave CacheWritePerMTok zero, which bills
// cache-creation tokens at the input price.
type ModelPrice struct {
	InputPerMTok        float64 // Non-cached input (cache miss).
	CachedInputPerMTok  float64 // Cache read (cache hit).
	CacheWritePerMTok   float64 // Cache creation; 0 = same as input.
	CacheWrite1hPerMTok float64 // One-hour cache creation; 0 = same as CacheWritePerMTok.
	OutputPerMTok       float64
}

// staticModelPrice prices model under a harness model-provider prefix, or ""
// when the prefix is not a known pricing provider.
func staticModelPrice(provider, model string, at time.Time) (ModelPrice, bool) {
	var table map[string]ModelPricing
	if provider == "openai" {
		return lookupModelPricing(openAIModelPricing, model, at)
	}
	switch agent.QuotaProviderForModel(provider + "/x") {
	case agent.QuotaProviderAnthropic:
		table = anthropicModelPricing
	case agent.QuotaProviderCodex:
		table = openAIModelPricing
	case agent.QuotaProviderZai:
		table = zaiModelPricing
	case agent.QuotaProviderDeepSeek:
		table = deepSeekModelPricing
	default:
		return ModelPrice{}, false
	}
	return lookupModelPricing(table, model, at)
}

// lookupModelPricing resolves model in a provider table, matching the exact
// model ID or the longest table key that prefixes it on a "-" boundary
// (table key "glm-5" matches "glm-5-turbo" but not "glm-5v-turbo").
func lookupModelPricing(table map[string]ModelPricing, model string, at time.Time) (ModelPrice, bool) {
	if pricing, ok := table[model]; ok {
		return pricing.Price(at)
	}
	best := ""
	for key := range table {
		if len(key) > len(best) && strings.HasPrefix(model, key+"-") {
			best = key
		}
	}
	if best == "" {
		return ModelPrice{}, false
	}
	return table[best].Price(at)
}

// Cost returns the USD cost of one usage report at p.
func (p ModelPrice) Cost(u agent.Usage) float64 {
	write5m, write1h := int64(u.CacheCreationInputTokens), int64(0)
	if u.CacheTTLSeconds >= 3600 {
		write5m, write1h = 0, write5m
	}
	return p.CostBuckets(int64(u.InputTokens), write5m, write1h, int64(u.CacheReadInputTokens), int64(u.OutputTokens))
}

// CostBuckets prices disjoint token buckets, including one-hour cache writes
// when the source records their TTL separately.
func (p ModelPrice) CostBuckets(input, cacheWrite5m, cacheWrite1h, cacheRead, output int64) float64 {
	write := p.CacheWritePerMTok
	if write == 0 {
		write = p.InputPerMTok
	}
	write1h := p.CacheWrite1hPerMTok
	if write1h == 0 {
		write1h = write
	}
	return (float64(input)*p.InputPerMTok +
		float64(cacheWrite5m)*write +
		float64(cacheWrite1h)*write1h +
		float64(cacheRead)*p.CachedInputPerMTok +
		float64(output)*p.OutputPerMTok) / 1_000_000
}

// PeakWindow is a daily UTC time range in which a peak price tier applies.
type PeakWindow struct {
	// Days restricts the window to these weekdays; nil means every day.
	Days []time.Weekday
	// Holidays lists UTC dates (YYYY-MM-DD) on which the window does not
	// apply. The peak windows span 01:00-10:00 UTC (09:00-18:00 Beijing), so
	// a holiday's Beijing calendar date matches the UTC date the window
	// falls on.
	Holidays []string
	Start    string // "HH:MM" UTC, inclusive.
	End      string // "HH:MM" UTC, exclusive.
}

// Contains reports whether at falls inside the window.
func (w *PeakWindow) Contains(at time.Time) bool {
	_, _, ok := w.windowAt(at)
	return ok
}

// windowAt returns the UTC [start, end) range of the window occurrence
// containing at, and whether at falls inside it.
func (w *PeakWindow) windowAt(at time.Time) (start, end time.Time, ok bool) {
	utc := at.UTC()
	if w.Days != nil && !slices.Contains(w.Days, utc.Weekday()) {
		return time.Time{}, time.Time{}, false
	}
	day := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	if slices.Contains(w.Holidays, day.Format(pricingDateLayout)) {
		return time.Time{}, time.Time{}, false
	}
	startMin, ok := parseClock(w.Start)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	endMin, ok := parseClock(w.End)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	minuteOfDay := utc.Hour()*60 + utc.Minute()
	if minuteOfDay < startMin || minuteOfDay >= endMin {
		return time.Time{}, time.Time{}, false
	}
	return day.Add(time.Duration(startMin) * time.Minute), day.Add(time.Duration(endMin) * time.Minute), true
}

// parseClock parses an "HH:MM" clock time into minutes of day.
func parseClock(s string) (int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// PricingPhase describes time-dependent pricing for a provider at a moment.
type PricingPhase string

const (
	// PricingPhasePeak means peak prices apply.
	PricingPhasePeak PricingPhase = "peak"
	// PricingPhasePeakSoon means peak prices start within
	// PricingPhaseSoonWindow.
	PricingPhasePeakSoon PricingPhase = "peak-soon"
	// PricingPhaseOffPeak means off-peak prices apply.
	PricingPhaseOffPeak PricingPhase = "off-peak"
)

// PricingPhaseSoonWindow is how long before a peak window starts the
// peak-soon phase is reported.
const PricingPhaseSoonWindow = 30 * time.Minute

// PricingPhaseFor returns the time-dependent pricing phase for provider at
// at, with the time the phase transitions, or "" when the provider's pricing
// does not vary by time of day.
func PricingPhaseFor(provider agent.QuotaProvider, at time.Time) (PricingPhase, time.Time) {
	if provider != agent.QuotaProviderDeepSeek {
		return "", time.Time{}
	}
	for _, w := range deepSeekPeakWindows {
		if _, end, ok := w.windowAt(at); ok {
			return PricingPhasePeak, end
		}
	}
	for _, w := range deepSeekPeakWindows {
		if start, _, ok := w.windowAt(at.Add(PricingPhaseSoonWindow)); ok {
			return PricingPhasePeakSoon, start
		}
	}
	return PricingPhaseOffPeak, time.Time{}
}

// PriceTier prices one model for a period of time. Provider price changes are
// modeled with dated tiers (for example, a new price effective on a calendar
// date) and time-of-day pricing with peak windows (for example, DeepSeek's
// weekday peak surcharge).
type PriceTier struct {
	// EffectiveFrom is the inclusive UTC date (YYYY-MM-DD) from which the
	// tier applies; empty means since always.
	EffectiveFrom string
	// PeakWindows restricts the tier to these daily UTC windows. When empty
	// the tier is the base price that applies outside every peak window of
	// the same effective date.
	PeakWindows []PeakWindow
	// Price is the per-million-token USD price the tier charges.
	Price ModelPrice
}

// ModelPricing is the price schedule for one model.
type ModelPricing []PriceTier

// Price returns the price effective at at: among the tiers whose effective
// date is not after at, the latest effective date wins, and within that date
// a peak window containing at wins over the base tier.
func (m ModelPricing) Price(at time.Time) (ModelPrice, bool) {
	latest := ""
	for _, tier := range m {
		if tier.EffectiveFrom == "" {
			continue
		}
		effective, err := time.Parse(pricingDateLayout, tier.EffectiveFrom)
		if err != nil || effective.After(at) {
			continue
		}
		if tier.EffectiveFrom > latest {
			latest = tier.EffectiveFrom
		}
	}
	for _, tier := range m {
		if tier.EffectiveFrom != latest || len(tier.PeakWindows) == 0 {
			continue
		}
		for _, window := range tier.PeakWindows {
			if window.Contains(at) {
				return tier.Price, true
			}
		}
	}
	for _, tier := range m {
		if tier.EffectiveFrom == latest && len(tier.PeakWindows) == 0 {
			return tier.Price, true
		}
	}
	return ModelPrice{}, false
}

// ModelPricer resolves per-model token pricing. Model IDs are harness-reported
// strings such as "zai/glm-5.3-flash" or "openrouter/z-ai/glm-5.3-flash".
type ModelPricer interface {
	// ModelPrice returns the per-million-token USD price for modelID that is
	// in effect at at, and whether the model is priced at all. provider hints
	// at the billing provider for harnesses that report unprefixed model IDs;
	// "" derives it from the model ID prefix. Subscription-backed providers
	// price at their API-equivalent rates.
	ModelPrice(provider agent.QuotaProvider, modelID string, at time.Time) (ModelPrice, bool)
}

// Pricer resolves per-model token pricing from the quota providers:
// fetcher-supplied live pricing (OpenRouter), static published schedules
// (Z.ai, DeepSeek), and API-equivalent schedules for subscription-backed
// providers (Anthropic, OpenAI via Claude Code and Codex).
type Pricer struct {
	fetchers map[agent.QuotaProvider]ModelPricer
}

// NewPricer creates a quota-provider model pricer from the registered
// provider fetchers. Fetchers may implement ModelPricer to supply live
// per-model pricing; the rest of the resolution uses static schedules.
func NewPricer(fetchers []ProviderFetcher) *Pricer {
	p := &Pricer{fetchers: make(map[agent.QuotaProvider]ModelPricer, len(fetchers))}
	for _, f := range fetchers {
		if pricer, ok := f.(ModelPricer); ok {
			p.fetchers[f.Provider()] = pricer
		}
	}
	return p
}

// ModelPrice implements ModelPricer.
func (p *Pricer) ModelPrice(provider agent.QuotaProvider, modelID string, at time.Time) (ModelPrice, bool) {
	lower := strings.ToLower(modelID)
	prefix, rest, hasPrefix := strings.Cut(lower, "/")
	if provider == "" {
		provider = agent.QuotaProviderForModel(modelID)
	}
	model := lower
	if hasPrefix {
		model = rest
	}
	// Fetcher-supplied pricing wins: it reflects what the provider actually
	// bills. The model ID is trimmed to the provider-native form first.
	if f, ok := p.fetchers[provider]; ok && hasPrefix {
		if price, ok := f.ModelPrice(provider, rest, at); ok {
			return price, true
		}
	}
	switch provider {
	case agent.QuotaProviderZai:
		return lookupModelPricing(zaiModelPricing, model, at)
	case agent.QuotaProviderDeepSeek:
		return lookupModelPricing(deepSeekModelPricing, model, at)
	case agent.QuotaProviderOpenRouter:
		// Live OpenRouter pricing is unavailable (no fetcher configured, or
		// the fetch failed): fall back to the upstream provider's published
		// prices, which approximate what OpenRouter passes through.
		if !hasPrefix {
			return ModelPrice{}, false
		}
		upstream, model, ok := strings.Cut(rest, "/")
		if !ok {
			return ModelPrice{}, false
		}
		return staticModelPrice(upstream, model, at)
	case agent.QuotaProviderAnthropic:
		return lookupModelPricing(anthropicModelPricing, model, at)
	case agent.QuotaProviderCodex:
		return lookupModelPricing(openAIModelPricing, model, at)
	default:
		// Direct OpenAI API models ("openai/...") are not a quota provider
		// but bill at OpenAI's published rates.
		if provider == "" {
			return staticModelPrice(prefix, model, at)
		}
		return ModelPrice{}, false
	}
}

// zaiModelPricing holds Z.ai's published per-million-token USD prices
// (https://docs.z.ai/guides/overview/pricing, retrieved 2026-09). Z.ai bills
// cache creation at the input price and cache reads at the cached-input
// price; its "cached input storage" fee is currently free.
var zaiModelPricing = map[string]ModelPricing{
	"glm-5.3-flash":       {{Price: ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.03, OutputPerMTok: 0.50}}},
	"glm-5.3-flashx":      {{Price: ModelPrice{InputPerMTok: 0.37, CachedInputPerMTok: 0.075, OutputPerMTok: 1.25}}},
	"glm-5.3":             {{Price: ModelPrice{InputPerMTok: 1.4, CachedInputPerMTok: 0.26, OutputPerMTok: 4.4}}},
	"glm-5.2":             {{Price: ModelPrice{InputPerMTok: 1.4, CachedInputPerMTok: 0.26, OutputPerMTok: 4.4}}},
	"glm-5.1":             {{Price: ModelPrice{InputPerMTok: 1.4, CachedInputPerMTok: 0.26, OutputPerMTok: 4.4}}},
	"glm-5":               {{Price: ModelPrice{InputPerMTok: 1.0, CachedInputPerMTok: 0.2, OutputPerMTok: 3.2}}},
	"glm-4.7":             {{Price: ModelPrice{InputPerMTok: 0.6, CachedInputPerMTok: 0.11, OutputPerMTok: 2.2}}},
	"glm-4.7-flashx":      {{Price: ModelPrice{InputPerMTok: 0.07, CachedInputPerMTok: 0.01, OutputPerMTok: 0.4}}},
	"glm-4.7-flash":       {{Price: ModelPrice{}}},
	"glm-4.6":             {{Price: ModelPrice{InputPerMTok: 0.6, CachedInputPerMTok: 0.11, OutputPerMTok: 2.2}}},
	"glm-4.5":             {{Price: ModelPrice{InputPerMTok: 0.6, CachedInputPerMTok: 0.11, OutputPerMTok: 2.2}}},
	"glm-4.5-x":           {{Price: ModelPrice{InputPerMTok: 2.2, CachedInputPerMTok: 0.45, OutputPerMTok: 8.9}}},
	"glm-4.5-air":         {{Price: ModelPrice{InputPerMTok: 0.2, CachedInputPerMTok: 0.03, OutputPerMTok: 1.1}}},
	"glm-4.5-airx":        {{Price: ModelPrice{InputPerMTok: 1.1, CachedInputPerMTok: 0.22, OutputPerMTok: 4.5}}},
	"glm-4.5-flash":       {{Price: ModelPrice{}}},
	"glm-4-32b-0414-128k": {{Price: ModelPrice{InputPerMTok: 0.1, CachedInputPerMTok: 0.1, OutputPerMTok: 0.1}}},
	"glm-4.6v":            {{Price: ModelPrice{InputPerMTok: 0.3, CachedInputPerMTok: 0.05, OutputPerMTok: 0.9}}},
	"glm-4.6v-flashx":     {{Price: ModelPrice{InputPerMTok: 0.04, CachedInputPerMTok: 0.004, OutputPerMTok: 0.4}}},
	"glm-4.6v-flash":      {{Price: ModelPrice{}}},
	"glm-4.5v":            {{Price: ModelPrice{InputPerMTok: 0.6, CachedInputPerMTok: 0.11, OutputPerMTok: 1.8}}},
	"glm-ocr":             {{Price: ModelPrice{InputPerMTok: 0.03, OutputPerMTok: 0.03}}},
}

// deepSeekHolidays holds Chinese public holidays (State Council days off
// work) that DeepSeek excludes from peak pricing. The schedule is announced
// each preceding November, so add the following year before it begins.
// Years without an entry fall back to the weekday rule, so an unpublished
// year reports peak hours on holidays.
var deepSeekHolidays = []string{
	// 2026: New Year (Jan 1-3), Spring Festival (Feb 15-23), Qingming
	// (Apr 4-6), Labor Day (May 1-5), Dragon Boat (Jun 19-21), Mid-Autumn
	// (Sep 25-27), National Day (Oct 1-7).
	"2026-01-01", "2026-01-02", "2026-01-03",
	"2026-02-15", "2026-02-16", "2026-02-17", "2026-02-18", "2026-02-19",
	"2026-02-20", "2026-02-21", "2026-02-22", "2026-02-23",
	"2026-04-04", "2026-04-05", "2026-04-06",
	"2026-05-01", "2026-05-02", "2026-05-03", "2026-05-04", "2026-05-05",
	"2026-06-19", "2026-06-20", "2026-06-21",
	"2026-09-25", "2026-09-26", "2026-09-27",
	"2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05",
	"2026-10-06", "2026-10-07",
}

// TODO(2026-11): Add the 2027 Chinese public holidays when the State Council
// announces the schedule.

// deepSeekPeakWindows holds DeepSeek's peak hours: 01:00-04:00 and 06:00-10:00
// UTC Monday through Friday. All other hours are off-peak at half the peak
// rate, including weekends and Chinese public holidays.
var deepSeekPeakWindows = []PeakWindow{
	{Days: workdays, Holidays: deepSeekHolidays, Start: "01:00", End: "04:00"},
	{Days: workdays, Holidays: deepSeekHolidays, Start: "06:00", End: "10:00"},
}

var workdays = []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}

// deepSeekModelPricing holds DeepSeek's published per-million-token USD
// prices (https://api-docs.deepseek.com/quick_start/pricing, retrieved
// 2026-09). DeepSeek bills cache reads at the cache-hit price and uncached
// input (including cache writes) at the cache-miss price.
var deepSeekModelPricing = map[string]ModelPricing{
	// deepSeekFlashTiers prices the DeepSeek-V4.1-Flash model and the retired
	// legacy names that DeepSeek still serves and bills at the same price.
	"deepseek-flash":               deepSeekFlashTiers,
	"deepseek-v4-flash":            deepSeekFlashTiers,
	"deepseek-v4-flash-vision-exp": deepSeekFlashTiers,
	"deepseek-v4-pro": {
		{PeakWindows: deepSeekPeakWindows, Price: ModelPrice{InputPerMTok: 1.32, CachedInputPerMTok: 0.044, OutputPerMTok: 3.96}},
		{Price: ModelPrice{InputPerMTok: 0.66, CachedInputPerMTok: 0.022, OutputPerMTok: 1.98}},
	},
}

// deepSeekFlashTiers holds the peak and off-peak tiers shared by the
// DeepSeek-V4.1-Flash model and its legacy names.
var deepSeekFlashTiers = ModelPricing{
	{PeakWindows: deepSeekPeakWindows, Price: ModelPrice{InputPerMTok: 0.30, CachedInputPerMTok: 0.006, OutputPerMTok: 1.20}},
	{Price: ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.003, OutputPerMTok: 0.60}},
}

// openAIModelPricing holds OpenAI's published per-million-token USD prices
// (https://developers.openai.com/api/docs/pricing, retrieved 2026-09) for
// models Codex can run, priced at API-equivalent rates: Codex subscriptions
// are not billed per token. OpenAI bills cached input at a discount and cache
// writes at the published write rate. Long-context pricing is not encoded;
// short-context prices are used.
var openAIModelPricing = map[string]ModelPricing{
	"gpt-5.3-codex": {{Price: ModelPrice{InputPerMTok: 1.75, CachedInputPerMTok: 0.175, OutputPerMTok: 14.0}}},
	"gpt-5.6-cyber": openAICyberTiers,
	"gpt-5.6-luna":  {{Price: ModelPrice{InputPerMTok: 0.20, CachedInputPerMTok: 0.02, CacheWritePerMTok: 0.25, OutputPerMTok: 1.20}}},
	"gpt-5.6-sol":   openAISolTiers,
	"gpt-5.6-terra": {{Price: ModelPrice{InputPerMTok: 2.0, CachedInputPerMTok: 0.20, CacheWritePerMTok: 2.50, OutputPerMTok: 12.0}}},
	"gpt-6-astra":   {{Price: ModelPrice{InputPerMTok: 10.0, CachedInputPerMTok: 1.0, CacheWritePerMTok: 12.50, OutputPerMTok: 50.0}}},
	"gpt-6-luna":    {{Price: ModelPrice{InputPerMTok: 0.10, CachedInputPerMTok: 0.01, CacheWritePerMTok: 0.125, OutputPerMTok: 0.50}}},
	"gpt-6-sol":     {{Price: ModelPrice{InputPerMTok: 2.0, CachedInputPerMTok: 0.20, CacheWritePerMTok: 2.50, OutputPerMTok: 10.0}}},
	// OpenAI re-points its Daybreak aliases at the latest flagship models as
	// the program rotates; when it does, update the referenced entry.
	"gpt-daybreak-blue-latest": openAISolTiers,
	"gpt-daybreak-red-latest":  openAICyberTiers,
}

// openAISolTiers and openAICyberTiers hold the prices shared by the GPT-5.6
// flagships and the Daybreak aliases that point at them.
var (
	openAISolTiers   = ModelPricing{{Price: ModelPrice{InputPerMTok: 4.0, CachedInputPerMTok: 0.40, CacheWritePerMTok: 5.0, OutputPerMTok: 20.0}}}
	openAICyberTiers = ModelPricing{{Price: ModelPrice{InputPerMTok: 12.50, CachedInputPerMTok: 1.25, CacheWritePerMTok: 15.625, OutputPerMTok: 75.0}}}
)

// anthropicModelPricing holds Anthropic's published per-million-token USD
// prices (https://platform.claude.com/docs/en/about-claude/pricing,
// retrieved 2026-09) for models Pi can run through the anthropic provider,
// priced at API-equivalent rates: Claude Code subscriptions are not billed
// per token, and Claude Code's own reported total stays authoritative for
// the claudecode provider. Cache writes bill at 1.25x input (5m) and cache
// reads at the model's published cache-read rate. Historical estimates use
// the one-hour write rate where one is published.
var anthropicModelPricing = map[string]ModelPricing{
	"claude-fable-5":    {{Price: ModelPrice{InputPerMTok: 10.0, CachedInputPerMTok: 1.0, CacheWritePerMTok: 12.50, OutputPerMTok: 50.0}}},
	"claude-fable-5-1":  {{Price: ModelPrice{InputPerMTok: 10.0, CachedInputPerMTok: 0.25, CacheWritePerMTok: 12.50, OutputPerMTok: 50.0}}},
	"claude-haiku-4-5":  {{Price: ModelPrice{InputPerMTok: 1.0, CachedInputPerMTok: 0.10, CacheWritePerMTok: 1.25, OutputPerMTok: 5.0}}},
	"claude-mythos-5":   {{Price: ModelPrice{InputPerMTok: 10.0, CachedInputPerMTok: 1.0, CacheWritePerMTok: 12.50, OutputPerMTok: 50.0}}},
	"claude-mythos-5-1": {{Price: ModelPrice{InputPerMTok: 10.0, CachedInputPerMTok: 0.25, CacheWritePerMTok: 12.50, OutputPerMTok: 50.0}}},
	"claude-opus-4-5":   {{Price: ModelPrice{InputPerMTok: 5.0, CachedInputPerMTok: 0.50, CacheWritePerMTok: 6.25, OutputPerMTok: 25.0}}},
	"claude-opus-4-6":   {{Price: ModelPrice{InputPerMTok: 5.0, CachedInputPerMTok: 0.50, CacheWritePerMTok: 6.25, OutputPerMTok: 25.0}}},
	"claude-opus-4-7":   {{Price: ModelPrice{InputPerMTok: 5.0, CachedInputPerMTok: 0.50, CacheWritePerMTok: 6.25, OutputPerMTok: 25.0}}},
	"claude-opus-4-8":   {{Price: ModelPrice{InputPerMTok: 5.0, CachedInputPerMTok: 0.50, CacheWritePerMTok: 6.25, OutputPerMTok: 25.0}}},
	"claude-opus-5":     {{Price: ModelPrice{InputPerMTok: 5.0, CachedInputPerMTok: 0.50, CacheWritePerMTok: 6.25, OutputPerMTok: 25.0}}},
	"claude-opus-5-5":   {{Price: ModelPrice{InputPerMTok: 4.0, CachedInputPerMTok: 0.20, CacheWritePerMTok: 5.0, CacheWrite1hPerMTok: 8.0, OutputPerMTok: 20.0}}},
	"claude-sonnet-4-5": {{Price: ModelPrice{InputPerMTok: 3.0, CachedInputPerMTok: 0.30, CacheWritePerMTok: 3.75, OutputPerMTok: 15.0}}},
	"claude-sonnet-4-6": {{Price: ModelPrice{InputPerMTok: 3.0, CachedInputPerMTok: 0.30, CacheWritePerMTok: 3.75, OutputPerMTok: 15.0}}},
	"claude-sonnet-5":   {{Price: ModelPrice{InputPerMTok: 2.0, CachedInputPerMTok: 0.20, CacheWritePerMTok: 2.50, OutputPerMTok: 10.0}}},
}
