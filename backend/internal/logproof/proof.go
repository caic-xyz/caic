// Package logproof defines the validated raw-log authority used by derived caches.
package logproof

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// CacheProof binds a rebuildable cache to a completed observation of one raw
// task log. The producer owns validation; cache consumers compare proofs but
// never infer harness authority from their own metadata.
type CacheProof struct {
	Device    uint64
	Inode     uint64
	Size      int64
	ModTimeNs int64
	Version   agent.LogVersion
	Harness   harness.Name
	RawHeader string
}
