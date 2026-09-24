// Per-model token pricing from quota providers, with time-dependent price tiers.

package usage

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	InputPerMTok        float64 `json:"input_per_mtok"`                    // Non-cached input (cache miss).
	CachedInputPerMTok  float64 `json:"cached_input_per_mtok"`             // Cache read (cache hit).
	CacheWritePerMTok   float64 `json:"cache_write_per_mtok,omitempty"`    // Cache creation; 0 = same as input.
	CacheWrite1hPerMTok float64 `json:"cache_write_1h_per_mtok,omitempty"` // One-hour cache creation; 0 = same as CacheWritePerMTok.
	OutputPerMTok       float64 `json:"output_per_mtok"`
}

// staticModelPrice prices model under a harness model-provider prefix, or ""
// when the prefix is not a known pricing provider.
func staticModelPrice(provider, model string, at time.Time) (ModelPrice, bool) {
	if provider == "openai" {
		return pricing.providers["openai"].lookup(model, at)
	}
	switch agent.QuotaProviderForModel(provider + "/x") {
	case agent.QuotaProviderAnthropic:
		return pricing.providers["anthropic"].lookup(model, at)
	case agent.QuotaProviderCodex:
		return pricing.providers["openai"].lookup(model, at)
	case agent.QuotaProviderZai:
		return pricing.providers["zai"].lookup(model, at)
	case agent.QuotaProviderDeepSeek:
		return pricing.providers["deepseek"].lookup(model, at)
	default:
		return ModelPrice{}, false
	}
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

// Validate reports whether every price field is non-negative, which every
// provider's published prices are.
func (p ModelPrice) Validate() error {
	fields := []struct {
		name  string
		value float64
	}{
		{"input_per_mtok", p.InputPerMTok},
		{"cached_input_per_mtok", p.CachedInputPerMTok},
		{"cache_write_per_mtok", p.CacheWritePerMTok},
		{"cache_write_1h_per_mtok", p.CacheWrite1hPerMTok},
		{"output_per_mtok", p.OutputPerMTok},
	}
	for _, f := range fields {
		if f.value < 0 {
			return fmt.Errorf("%s is negative: %v", f.name, f.value)
		}
	}
	return nil
}

// modelPriceTable maps the model IDs of one provider to their price
// schedules.
type modelPriceTable map[string]ModelPricing

// lookup resolves model in the table, matching the exact model ID or the
// longest table key that prefixes it on a "-" boundary (table key "glm-5"
// matches "glm-5-turbo" but not "glm-5v-turbo").
func (t modelPriceTable) lookup(model string, at time.Time) (ModelPrice, bool) {
	if pricing, ok := t[model]; ok {
		return pricing.Price(at)
	}
	best := ""
	for key := range t {
		if len(key) > len(best) && strings.HasPrefix(model, key+"-") {
			best = key
		}
	}
	if best == "" {
		return ModelPrice{}, false
	}
	return t[best].Price(at)
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
	// DeepSeek's peak hours are 01:00-04:00 and 06:00-10:00 UTC Monday through
	// Friday. All other hours are off-peak at half the peak rate, including
	// weekends and Chinese public holidays.
	//
	// The holiday schedule is announced each preceding November, so add the
	// following year before it begins; a year without an entry falls back to
	// the weekday rule, so an unpublished year reports peak hours on
	// holidays.
	// TODO(2026-11): Add the 2027 Chinese public holidays when the State
	// Council announces the schedule.
	for _, w := range pricing.peakWindows["deepseek"] {
		if _, end, ok := w.windowAt(at); ok {
			return PricingPhasePeak, end
		}
	}
	for _, w := range pricing.peakWindows["deepseek"] {
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
		// Z.ai bills cache creation at the input price and cache reads at the
		// cached-input price; its "cached input storage" fee is currently free.
		return pricing.providers["zai"].lookup(model, at)
	case agent.QuotaProviderDeepSeek:
		// DeepSeek bills cache reads at the cache-hit price and uncached input
		// (including cache writes) at the cache-miss price.
		return pricing.providers["deepseek"].lookup(model, at)
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
		// API-equivalent prices: Claude Code subscriptions are not billed per
		// token, and Claude Code's own reported total stays authoritative for
		// the claudecode provider. Cache writes bill at 1.25x input (5m) and
		// cache reads at the model's published cache-read rate; historical
		// estimates use the one-hour write rate where one is published.
		return pricing.providers["anthropic"].lookup(model, at)
	case agent.QuotaProviderCodex:
		// API-equivalent prices: Codex subscriptions are not billed per token.
		// Long-context pricing is not encoded; short-context prices are used.
		return pricing.providers["openai"].lookup(model, at)
	default:
		// Direct OpenAI API models ("openai/...") are not a quota provider
		// but bill at OpenAI's published rates.
		if provider == "" {
			return staticModelPrice(prefix, model, at)
		}
		return ModelPrice{}, false
	}
}

// pricingJSON embeds the provider price schedules, keeping the numbers,
// provider names, sources, and retrieval dates in pricing.json so a
// mechanical updater can rewrite them without editing Go code. See that file
// for the schema.
//
//go:embed pricing.json
var pricingJSON []byte

// pricing is the decoded pricing.json content.
var pricing = mustLoadPricing()

// pricingTables is the decoded pricing.json content: one model schedule map
// per provider name, plus every provider's peak windows in order for
// pricing-phase reporting.
type pricingTables struct {
	providers   map[string]modelPriceTable
	peakWindows map[string][]PeakWindow
}

// pricingFile mirrors the pricing.json document.
type pricingFile struct {
	Providers []pricingProvider `json:"providers"`
}

// Validate reports whether the document holds at least one provider, each
// named once and valid.
func (f *pricingFile) Validate() error {
	if len(f.Providers) == 0 {
		return errors.New("no providers")
	}
	seen := make(map[string]struct{}, len(f.Providers))
	for i := range f.Providers {
		p := &f.Providers[i]
		if p.Provider == "" {
			return errors.New("provider name is empty")
		}
		if _, ok := seen[p.Provider]; ok {
			return fmt.Errorf("provider %q is listed twice", p.Provider)
		}
		seen[p.Provider] = struct{}{}
		if err := p.Validate(); err != nil {
			return fmt.Errorf("provider %q: %w", p.Provider, err)
		}
	}
	return nil
}

// pricingProvider mirrors one provider entry of pricing.json.
type pricingProvider struct {
	Provider  string                     `json:"provider"`
	Source    string                     `json:"source"`
	Retrieved string                     `json:"retrieved"`
	Holidays  []string                   `json:"holidays"`
	Windows   map[string][]pricingWindow `json:"windows"`
	Models    []pricingModel             `json:"models"`
}

// Validate reports whether the entry is complete: it records its source and
// retrieval date, its windows and models are non-empty and valid, and no
// model is named twice.
func (p *pricingProvider) Validate() error {
	if p.Source == "" || p.Retrieved == "" {
		return errors.New("source and retrieved are required")
	}
	for _, holiday := range p.Holidays {
		if _, err := time.Parse(pricingDateLayout, holiday); err != nil {
			return fmt.Errorf("holiday %q: %w", holiday, err)
		}
	}
	for name, group := range p.Windows {
		if len(group) == 0 {
			return fmt.Errorf("window group %q is empty", name)
		}
		for _, w := range group {
			if err := w.Validate(); err != nil {
				return fmt.Errorf("window group %q: %w", name, err)
			}
		}
	}
	if len(p.Models) == 0 {
		return errors.New("no models")
	}
	seen := make(map[string]struct{}, len(p.Models))
	for _, m := range p.Models {
		if err := m.Validate(); err != nil {
			return err
		}
		if _, ok := seen[m.ID]; ok {
			return fmt.Errorf("model %q is listed twice", m.ID)
		}
		seen[m.ID] = struct{}{}
	}
	return nil
}

// build resolves the entry, which Validate must have accepted, into its model
// schedules and window groups.
func (p *pricingProvider) build() (providerPricing, error) {
	windows := make(map[string][]PeakWindow, len(p.Windows))
	for name, group := range p.Windows {
		built := make([]PeakWindow, 0, len(group))
		for _, w := range group {
			pw, err := w.toPeakWindow(p.Holidays)
			if err != nil {
				return providerPricing{}, fmt.Errorf("window group %q: %w", name, err)
			}
			built = append(built, pw)
		}
		windows[name] = built
	}
	models := make(modelPriceTable, len(p.Models))
	for _, m := range p.Models {
		if m.Alias != "" {
			continue
		}
		tiers := make(ModelPricing, 0, len(m.Tiers))
		for _, tier := range m.Tiers {
			bt, err := tier.toPriceTier(windows)
			if err != nil {
				return providerPricing{}, fmt.Errorf("model %q: %w", m.ID, err)
			}
			tiers = append(tiers, bt)
		}
		models[m.ID] = tiers
	}
	for _, m := range p.Models {
		if m.Alias == "" {
			continue
		}
		tiers, ok := models[m.Alias]
		if !ok {
			return providerPricing{}, fmt.Errorf("model %q aliases unpriced model %q", m.ID, m.Alias)
		}
		models[m.ID] = tiers
	}
	return providerPricing{models: models, windows: windows}, nil
}

// pricingModel mirrors one model entry: either tiers, or an alias of another
// priced model in the same provider.
type pricingModel struct {
	ID    string        `json:"id"`
	Alias string        `json:"alias"`
	Tiers []pricingTier `json:"tiers"`
}

// Validate reports whether the entry prices tiers or aliases another priced
// model, without doing both or neither.
func (m pricingModel) Validate() error {
	if m.ID == "" {
		return errors.New("model ID is empty")
	}
	if m.Alias != "" {
		if len(m.Tiers) > 0 {
			return fmt.Errorf("model %q has both alias and tiers", m.ID)
		}
		return nil
	}
	if len(m.Tiers) == 0 {
		return fmt.Errorf("model %q has neither alias nor tiers", m.ID)
	}
	for _, tier := range m.Tiers {
		if err := tier.Validate(); err != nil {
			return fmt.Errorf("model %q: %w", m.ID, err)
		}
	}
	return nil
}

// pricingTier mirrors one price tier. Windows names a window group of the
// model's provider, and Price is nil when the entry omits it.
type pricingTier struct {
	EffectiveFrom string      `json:"effective_from"`
	Windows       string      `json:"windows"`
	Price         *ModelPrice `json:"price"`
}

// Validate reports whether the tier carries a price and a parsable effective
// date.
func (t pricingTier) Validate() error {
	if t.Price == nil {
		return errors.New("price is required")
	}
	if err := t.Price.Validate(); err != nil {
		return err
	}
	if t.EffectiveFrom != "" {
		if _, err := time.Parse(pricingDateLayout, t.EffectiveFrom); err != nil {
			return fmt.Errorf("effective_from %q: %w", t.EffectiveFrom, err)
		}
	}
	return nil
}

// toPriceTier converts the tier, resolving its window group.
func (t pricingTier) toPriceTier(windows map[string][]PeakWindow) (PriceTier, error) {
	var peaks []PeakWindow
	if t.Windows != "" {
		var ok bool
		peaks, ok = windows[t.Windows]
		if !ok {
			return PriceTier{}, fmt.Errorf("unknown window group %q", t.Windows)
		}
	}
	return PriceTier{EffectiveFrom: t.EffectiveFrom, PeakWindows: peaks, Price: *t.Price}, nil
}

// pricingWindow mirrors one daily UTC range of a window group.
type pricingWindow struct {
	Days  []string `json:"days"`
	Start string   `json:"start"`
	End   string   `json:"end"`
}

// Validate reports whether the window names parsable weekdays and clock
// times.
func (w pricingWindow) Validate() error {
	if _, err := parseWeekdays(w.Days); err != nil {
		return err
	}
	if _, ok := parseClock(w.Start); !ok {
		return fmt.Errorf("start %q: want HH:MM", w.Start)
	}
	if _, ok := parseClock(w.End); !ok {
		return fmt.Errorf("end %q: want HH:MM", w.End)
	}
	return nil
}

// toPeakWindow converts the entry, applying the provider's holidays.
func (w pricingWindow) toPeakWindow(holidays []string) (PeakWindow, error) {
	days, err := parseWeekdays(w.Days)
	if err != nil {
		return PeakWindow{}, err
	}
	return PeakWindow{Days: days, Holidays: holidays, Start: w.Start, End: w.End}, nil
}

// mustLoadPricing decodes the embedded schedules, panicking on failure: the
// data ships in the binary and unit tests validate it, so a decode failure is
// a build defect rather than a recoverable runtime condition.
func mustLoadPricing() pricingTables {
	tables, err := parsePricingJSON(pricingJSON)
	if err != nil {
		panic(fmt.Sprintf("pricing.json: %v", err))
	}
	return tables
}

// parsePricingJSON decodes and validates a pricing.json document.
func parsePricingJSON(data []byte) (pricingTables, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var file pricingFile
	if err := dec.Decode(&file); err != nil {
		return pricingTables{}, err
	}
	if err := file.Validate(); err != nil {
		return pricingTables{}, err
	}
	tables := pricingTables{
		providers:   make(map[string]modelPriceTable, len(file.Providers)),
		peakWindows: make(map[string][]PeakWindow, len(file.Providers)),
	}
	for i := range file.Providers {
		p := &file.Providers[i]
		built, err := p.build()
		if err != nil {
			return pricingTables{}, fmt.Errorf("provider %q: %w", p.Provider, err)
		}
		tables.providers[p.Provider] = built.models
		for _, name := range slices.Sorted(maps.Keys(built.windows)) {
			tables.peakWindows[p.Provider] = append(tables.peakWindows[p.Provider], built.windows[name]...)
		}
	}
	return tables, nil
}

// providerPricing is one provider's validated model schedules and window
// groups.
type providerPricing struct {
	models  modelPriceTable
	windows map[string][]PeakWindow
}

// parseWeekdays converts pricing.json weekday abbreviations; an empty list
// means every day.
func parseWeekdays(names []string) ([]time.Weekday, error) {
	if len(names) == 0 {
		return nil, nil
	}
	days := make([]time.Weekday, 0, len(names))
	for _, name := range names {
		day, ok := weekdayNames[strings.ToLower(name)]
		if !ok {
			return nil, fmt.Errorf("weekday %q: want sun, mon, tue, wed, thu, fri or sat", name)
		}
		days = append(days, day)
	}
	return days, nil
}

// weekdayNames maps pricing.json weekday abbreviations to time.Weekday.
var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}
