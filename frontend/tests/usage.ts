// Constructs explicit reported-cost provenance for dashboard fixtures.

import type { UsageDashboardCost } from "../../sdk/caic/ts/v1/types.gen";

export function reportedUsageCost(usd: number): UsageDashboardCost {
  return { reportedUSD: usd, estimatedUSD: 0, reportedParts: 1, estimatedParts: 0, unknownParts: 0 };
}
