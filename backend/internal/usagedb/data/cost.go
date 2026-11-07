// Additive cost provenance retains exact zeros and estimate replacement corrections.

package data

// CostProvenance separates known amounts and signed coverage components.
// Counts preserve exact zero costs. A later native report can subtract an
// earlier estimate's amount and parts before adding the reported component.
type CostProvenance struct {
	ReportedUSD    float64 `json:"reported_usd,omitzero"`
	EstimatedUSD   float64 `json:"estimated_usd,omitzero"`
	ReportedParts  int     `json:"reported_parts,omitzero"`
	EstimatedParts int     `json:"estimated_parts,omitzero"`
	UnknownParts   int     `json:"unknown_parts,omitzero"`
}

// Add combines additive amounts and coverage, including signed corrections.
func (c *CostProvenance) Add(o CostProvenance) {
	c.ReportedUSD += o.ReportedUSD
	c.EstimatedUSD += o.EstimatedUSD
	c.ReportedParts += o.ReportedParts
	c.EstimatedParts += o.EstimatedParts
	c.UnknownParts += o.UnknownParts
}

// Difference returns the movement from o to c.
func (c *CostProvenance) Difference(o CostProvenance) CostProvenance {
	return CostProvenance{ReportedUSD: c.ReportedUSD - o.ReportedUSD, EstimatedUSD: c.EstimatedUSD - o.EstimatedUSD, ReportedParts: c.ReportedParts - o.ReportedParts, EstimatedParts: c.EstimatedParts - o.EstimatedParts, UnknownParts: c.UnknownParts - o.UnknownParts}
}

// IsZero reports whether no amount or coverage changed.
func (c *CostProvenance) IsZero() bool { return *c == (CostProvenance{}) }
