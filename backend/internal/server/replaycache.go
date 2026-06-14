// Server adapters for on-disk EventMessage replay caches.

package server

import "github.com/caic-xyz/caic/backend/internal/eventreplay"

func replayCachePath(logPath string) string {
	return eventreplay.CachePath(logPath)
}
