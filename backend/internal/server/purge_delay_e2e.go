// Selects immediate task purges for the accelerated fake server.

//go:build e2e

package server

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/preferences"
)

func taskPurgeDelay(_ *preferences.Store, _ string) time.Duration {
	return 0
}
