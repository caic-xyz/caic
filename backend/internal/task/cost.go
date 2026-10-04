// CostTracker folds harness cost reports and token prices into task and turn costs.

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

	priorUSD          float64
	sessionPricedUSD  float64
	opencodeReported  bool
	lastTurnUSD       float64
	pendingTurnSource CostSource
}

// Observe folds one agent message and returns a cost only at a result boundary.
// A false result means the turn's cost is unknown, including unpriced models.
func (c *CostTracker) Observe(msg agent.Message, at time.Time, pricer usage.ModelPricer) (TurnCost, bool) {
	switch m := msg.(type) {
	case *agent.InitMessage:
		if m.ReportedModel != "" {
			c.Model = m.ReportedModel
		}
	case *agent.SystemMessage:
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
			}
		}
	case *agent.UsageMessage:
		if m.ReportedModel != "" {
			c.Model = m.ReportedModel
		}
		if c.Harness == harness.OpenCode && m.CumulativeCostUSD != nil && *m.CumulativeCostUSD > 0 {
			c.opencodeReported = true
			c.TotalUSD = c.priorUSD + *m.CumulativeCostUSD
			c.markSource(CostReported)
		}
		if !m.ModelDerived && (m.ReportedModel != "" || c.Harness == harness.Pi) {
			if !c.priceUsage(m.Usage, "", at, pricer) && c.Harness == harness.Pi && m.ReportedCostUSD != nil {
				c.priorUSD += *m.ReportedCostUSD
				c.TotalUSD += *m.ReportedCostUSD
				c.markSource(CostReported)
			}
		}
	case *agent.ResultMessage:
		switch c.Harness {
		case harness.OpenCode:
			if !c.opencodeReported && !c.priceUsage(m.Usage, "", at, pricer) {
				c.applyResultReport(m.TotalCostUSD, false)
			}
		case harness.Codex:
			if !c.priceUsage(m.Usage, agent.QuotaProviderCodex, at, pricer) {
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
		cost := TurnCost{USD: c.TotalUSD - c.lastTurnUSD, Source: c.pendingTurnSource}
		c.lastTurnUSD = c.TotalUSD
		c.pendingTurnSource = ""
		return cost, cost.Source != "" && cost.USD >= 0
	}
	return TurnCost{}, false
}

func (c *CostTracker) priceUsage(u agent.Usage, provider agent.QuotaProvider, at time.Time, pricer usage.ModelPricer) bool {
	if c.Model == "" || pricer == nil {
		return false
	}
	if at.IsZero() {
		at = time.Now()
	}
	price, ok := pricer.ModelPrice(provider, c.Model, at)
	if !ok {
		return false
	}
	c.sessionPricedUSD += price.Cost(u)
	c.TotalUSD = c.priorUSD + c.sessionPricedUSD
	c.markSource(CostEstimated)
	return true
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
