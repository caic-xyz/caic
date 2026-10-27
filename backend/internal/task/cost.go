// CostTracker folds harness reports and per-request or aggregate token estimates into task costs.

package task

import (
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// CostSource describes where a displayed turn cost came from.
type CostSource string

const (
	// CostEstimated is calculated from model token prices.
	CostEstimated CostSource = "estimated"
	// CostReported is supplied by the harness.
	CostReported CostSource = "reported"
)

// TurnCost is a completed turn's cost in USD and its provenance.
type TurnCost struct {
	USD    float64
	Source CostSource
}

// CostTracker maintains one task's cost through turns, compaction, and context clearing.
// It uses only durable agent messages, so live folds and streamed log replay agree.
type CostTracker struct {
	// Harness selects the cost-reporting semantics for this tracker.
	Harness harness.Name
	// Model is the current model used to price usage, updated by agent messages.
	Model string
	// TotalUSD is the cumulative task cost after the latest observed message.
	TotalUSD float64

	priorUSD          float64
	sessionPricedUSD  float64
	opencodeReported  bool
	lastTurnUSD       float64
	pendingTurnSource CostSource
	pricedUsage       agent.Usage // Request tokens already priced since the last root result/session exit.
}

// Observe folds one agent message and returns a cost only at a result boundary.
// A false result means the turn's cost is unknown, including unpriced models.
func (c *CostTracker) Observe(msg agent.Message, at time.Time, pricer usage.ModelPricer) (TurnCost, bool) {
	switch m := msg.(type) {
	case *agent.ExitMessage:
		if !m.Source.IsChild() {
			c.pricedUsage = agent.Usage{}
		}
	case *agent.InitMessage:
		if m.Source.IsChild() {
			break
		}
		// A new wire starts fresh result totals. SSE may filter the preceding
		// process exit, so session start must reset request coverage as well.
		c.pricedUsage = agent.Usage{}
		if m.ReportedModel != "" {
			c.Model = m.ReportedModel
		}
	case *agent.SystemMessage:
		if m.Source.IsChild() {
			break
		}
		switch m.Subtype {
		case agent.SystemSubtypeModelRerouted:
			if m.ReportedModel != "" {
				c.Model = m.ReportedModel
			}
		case agent.SystemSubtypeCompactBoundary, "context_cleared":
			if m.Subtype == "context_cleared" || (c.Harness != harness.Claude && c.Harness != harness.OpenCode) {
				c.priorUSD = c.TotalUSD
				c.sessionPricedUSD = 0
			}
			if m.Subtype == "context_cleared" {
				c.opencodeReported = false
				c.pricedUsage = agent.Usage{}
			}
		}
	case *agent.UsageMessage:
		if m.ReportedModel != "" && !m.Source.IsChild() {
			c.Model = m.ReportedModel
		}
		if c.Harness == harness.OpenCode && m.CumulativeCostUSD != nil && *m.CumulativeCostUSD > 0 {
			c.opencodeReported = true
			c.TotalUSD = c.priorUSD + *m.CumulativeCostUSD
			c.pendingTurnSource = CostReported
		}
		if c.Harness == harness.OpenCode && c.opencodeReported {
			break // ACP's reported session cost supersedes token estimates.
		}
		// PerCall records establish request context independently of the harness.
		// Older non-derived records carrying a model are also individual calls.
		priced := false
		if m.PerCall || (!m.ModelDerived && m.ReportedModel != "") {
			priced = c.priceRequest(m, at, pricer)
			if priced {
				c.pricedUsage.InputTokens += m.Usage.InputTokens
				c.pricedUsage.CacheCreationInputTokens += m.Usage.CacheCreationInputTokens
				c.pricedUsage.CacheReadInputTokens += m.Usage.CacheReadInputTokens
				c.pricedUsage.OutputTokens += m.Usage.OutputTokens
			}
		} else if c.Harness == harness.Pi && !m.ModelDerived {
			// Pi summary usage may aggregate multiple requests. Older records
			// without model or request detail also retain base-rate estimation.
			priced = c.priceUsage(m.Usage, at, pricer)
		}
		if !priced && c.Harness == harness.Pi && (m.PerCall || !m.ModelDerived) && m.ReportedCostUSD != nil {
			c.priorUSD += *m.ReportedCostUSD
			c.TotalUSD += *m.ReportedCostUSD
			c.markSource(CostReported)
		}
	case *agent.ResultMessage:
		if m.Source.IsChild() {
			break
		}
		switch c.Harness {
		case harness.OpenCode, harness.Codex:
			if c.Harness == harness.OpenCode && c.opencodeReported {
				break
			}
			// Results sum every request, including child threads. Subtract calls
			// already priced, then estimate only the remaining aggregate at base
			// rates: legacy logs and missing events do not establish request sizes.
			remaining := agent.Usage{
				InputTokens:              max(0, m.Usage.InputTokens-c.pricedUsage.InputTokens),
				CacheCreationInputTokens: max(0, m.Usage.CacheCreationInputTokens-c.pricedUsage.CacheCreationInputTokens),
				CacheReadInputTokens:     max(0, m.Usage.CacheReadInputTokens-c.pricedUsage.CacheReadInputTokens),
				OutputTokens:             max(0, m.Usage.OutputTokens-c.pricedUsage.OutputTokens),
				CacheTTLSeconds:          m.Usage.CacheTTLSeconds,
			}
			if !c.priceUsage(remaining, at, pricer) {
				if remaining.InputTokens > 0 || remaining.CacheCreationInputTokens > 0 || remaining.CacheReadInputTokens > 0 || remaining.OutputTokens > 0 {
					// Priced children do not establish the cost of unpriced
					// parent usage. Do not present a partial turn as known.
					c.pendingTurnSource = ""
				}
				c.applyResultReport(m.TotalCostUSD, false)
			}
		case harness.Claude:
			if c.sessionPricedUSD == 0 {
				c.applyResultReport(m.TotalCostUSD, true)
			}
		case harness.Pi:
			// Pi results report per-invocation costs, rather than a session
			// snapshot. Modern usage messages already account for each call.
			// Retain a result-only fallback for older logs without per-call costs.
			if c.pendingTurnSource == "" && m.TotalCostUSD > 0 {
				c.priorUSD += m.TotalCostUSD
				c.TotalUSD += m.TotalCostUSD
				c.markSource(CostReported)
			}
		default:
			if c.sessionPricedUSD == 0 {
				c.applyResultReport(m.TotalCostUSD, false)
			}
		}
		c.pricedUsage = agent.Usage{}
		cost := TurnCost{USD: c.TotalUSD - c.lastTurnUSD, Source: c.pendingTurnSource}
		c.lastTurnUSD = c.TotalUSD
		c.pendingTurnSource = ""
		return cost, cost.Source != "" && cost.USD >= 0
	}
	return TurnCost{}, false
}

func (c *CostTracker) priceUsage(u agent.Usage, at time.Time, pricer usage.ModelPricer) bool {
	price, ok := c.modelPrice(c.Model, at, pricer)
	if !ok {
		return false
	}
	c.addEstimatedCost(price.Cost(u))
	return true
}

func (c *CostTracker) priceRequest(m *agent.UsageMessage, at time.Time, pricer usage.ModelPricer) bool {
	model := m.ReportedModel
	if model == "" && !m.Source.IsChild() {
		model = c.Model
	}
	price, ok := c.modelPrice(model, at, pricer)
	if !ok {
		return false
	}
	c.addEstimatedCost(price.RequestCost(m.Usage))
	return true
}

func (c *CostTracker) modelPrice(model string, at time.Time, pricer usage.ModelPricer) (usage.ModelPrice, bool) {
	if model == "" || pricer == nil {
		return usage.ModelPrice{}, false
	}
	if at.IsZero() {
		at = time.Now()
	}
	// Pi and OpenCode identify the billing provider in the model ID. Native
	// Codex can report a bare model ID, which needs an OpenAI provider hint.
	var provider agent.QuotaProvider
	if c.Harness == harness.Codex && !strings.Contains(model, "/") {
		provider = agent.QuotaProviderCodex
	}
	return pricer.ModelPrice(provider, model, at)
}

func (c *CostTracker) addEstimatedCost(usd float64) {
	c.sessionPricedUSD += usd
	c.TotalUSD = c.priorUSD + c.sessionPricedUSD
	c.markSource(CostEstimated)
}

func (c *CostTracker) applyResultReport(usd float64, zeroKnown bool) {
	if c.sessionPricedUSD > 0 || (usd == 0 && !zeroKnown) {
		return
	}
	c.TotalUSD = c.priorUSD + usd
	c.markSource(CostReported)
}

func (c *CostTracker) markSource(source CostSource) {
	if source == CostEstimated || c.pendingTurnSource == "" {
		c.pendingTurnSource = source
	}
}
