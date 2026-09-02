// Resolves task purge recovery windows from user preferences.

//go:build !e2e

package server

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/preferences"
)

func taskPurgeDelay(prefs *preferences.Store, userID string) time.Duration {
	return prefs.Get(userID).Settings.PurgeDelay
}
