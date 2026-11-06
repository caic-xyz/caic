// CostTracker folds harness reports and per-request or aggregate token estimates into task costs.

package task

import (
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
	// Known reports whether any observed message established a cost. A task
	// with no billed tokens or reported cost leaves TotalUSD zero and Known
	// false, so callers can distinguish unknown from an exact zero.
	Known bool

	priorUSD            float64
	sessionPricedUSD    float64
	sessionCostReported bool
	lastTurnUSD         float64
	pendingTurnSource   CostSource
	pricedUsage         agent.Usage // Request tokens already priced since the last root result/session exit.
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
	case *agent.SessionMetadataMessage:
		if !m.Source.IsChild() && m.ReportedModel != "" {
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
			if m.Subtype == "context_cleared" || (c.Harness != harness.Claude && c.Harness != harness.OpenCode && !c.sessionCostReported) {
				c.priorUSD = c.TotalUSD
				c.sessionPricedUSD = 0
			}
			if m.Subtype == "context_cleared" {
				c.sessionCostReported = false
				c.pricedUsage = agent.Usage{}
			}
		}
	case *agent.UsageMessage:
		if m.ReportedModel != "" && !m.Source.IsChild() {
			c.Model = m.ReportedModel
		}
		if m.CumulativeCostUSD != nil && *m.CumulativeCostUSD > 0 {
			c.sessionCostReported = true
			c.TotalUSD = c.priorUSD + *m.CumulativeCostUSD
			c.pendingTurnSource = CostReported
			c.Known = true
		}
		if c.sessionCostReported {
			break // A cumulative session cost supersedes per-request estimates.
		}
		// PerCall records establish request context independently of the harness.
		// Older non-derived records carrying a model are also individual calls.
		priced := false
		if m.PerCall || (!m.ModelDerived && m.ReportedModel != "") {
			priced = c.priceRequest(m, at, pricer)
			if priced {
				c.pricedUsage.Add(m.Usage)
			}
		} else if c.Harness == harness.Pi && !m.ModelDerived {
			// Pi summary usage may aggregate multiple requests. Older records
			// without model or request detail also retain base-rate estimation.
			priced = c.priceUsage(m.Usage, at, pricer)
		}
		if !priced && c.Harness == harness.Pi && (m.PerCall || !m.ModelDerived) && m.ReportedCostUSD != nil {
			c.priorUSD += *m.ReportedCostUSD
			c.TotalUSD += *m.ReportedCostUSD
			c.Known = true
			c.markSource(CostReported)
		}
	case *agent.ResultMessage:
		if m.Source.IsChild() {
			break
		}
		if !c.sessionCostReported {
			switch c.Harness {
			case harness.OpenCode, harness.Codex:
				// Results sum every request, including child threads. Subtract calls
				// already priced, then estimate only the remaining aggregate at base
				// rates: missing per-call events do not establish request sizes.
				writes := m.Usage.CacheWriteBuckets().Subtract(c.pricedUsage.CacheWriteBuckets())
				remaining := agent.Usage{
					InputTokens:              max(0, m.Usage.InputTokens-c.pricedUsage.InputTokens),
					CacheCreationInputTokens: int(writes.Total()),
					CacheReadInputTokens:     max(0, m.Usage.CacheReadInputTokens-c.pricedUsage.CacheReadInputTokens),
					OutputTokens:             max(0, m.Usage.OutputTokens-c.pricedUsage.OutputTokens),
					CacheWrites:              writes,
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
			case harness.Dsh:
				// DSH ACP prompt results carry aggregate prompt tokens. Price them
				// at base rates when the harness reports them; context occupancy is
				// not billed usage and never establishes a cost.
				if !m.Usage.Equal(agent.Usage{}) {
					if !c.priceUsage(m.Usage, at, pricer) {
						c.applyResultReport(m.TotalCostUSD, false)
					}
				} else {
					c.applyResultReport(m.TotalCostUSD, false)
				}
			case harness.Pi:
				// Pi results report per-invocation costs, rather than a session
				// snapshot. Modern usage messages already account for each call.
				// Retain a result-only fallback for older logs without per-call costs.
				if c.pendingTurnSource == "" && m.TotalCostUSD > 0 {
					c.priorUSD += m.TotalCostUSD
					c.TotalUSD += m.TotalCostUSD
					c.Known = true
					c.markSource(CostReported)
				}
			default:
				if c.sessionPricedUSD == 0 {
					c.applyResultReport(m.TotalCostUSD, false)
				}
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
	return pricer.ModelPrice(model, at)
}

func (c *CostTracker) addEstimatedCost(usd float64) {
	c.sessionPricedUSD += usd
	c.TotalUSD = c.priorUSD + c.sessionPricedUSD
	c.Known = true
	c.markSource(CostEstimated)
}

func (c *CostTracker) applyResultReport(usd float64, zeroKnown bool) {
	if c.sessionPricedUSD > 0 || (usd == 0 && !zeroKnown) {
		return
	}
	c.TotalUSD = c.priorUSD + usd
	c.Known = true
	c.markSource(CostReported)
}

func (c *CostTracker) markSource(source CostSource) {
	if source == CostEstimated || c.pendingTurnSource == "" {
		c.pendingTurnSource = source
	}
}
