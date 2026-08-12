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
	Device    uint64           `json:"device"`
	Inode     uint64           `json:"inode"`
	Size      int64            `json:"size"`
	ModTimeNs int64            `json:"modTimeNs"`
	Version   agent.LogVersion `json:"version"`
	Harness   harness.Name     `json:"harness"`
	RawHeader string           `json:"rawHeader"`
}
