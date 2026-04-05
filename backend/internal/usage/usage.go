// Package usage provides cached fetchers for coding agent usage quotas.
package usage

import "time"

const (
	// CacheTTL is the duration before cached usage data is considered stale.
	CacheTTL = 5 * time.Minute

	// Exponential backoff parameters for fetch errors.
	backoffMin = 5 * time.Minute
	backoffMax = 1 * time.Hour
)
