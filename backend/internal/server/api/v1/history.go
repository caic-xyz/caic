// Defines raw-record history resources, cache discovery, mutable pricing, and exact SSE cursor contracts.

package v1

// HistoryPosition identifies one normalized output in a one-based physical raw
// record. Index is zero-based across every output of that record. Metadata and
// zero-output controls occupy record IDs too; IDs are not semantic event counts.
type HistoryPosition struct {
	RecordID uint64 `json:"recordID"`
	Index    uint32 `json:"index"`
}

// HistoryEvent is an immutable normalized output; the identity is scoped to the
// projection version and timeline generation supplied by HistoryBootstrap.
type HistoryEvent struct {
	RecordID uint64       `json:"recordID"`
	Index    uint32       `json:"index"`
	Message  EventMessage `json:"message"`
}

// HistoryBootstrap is small revalidated discovery for task history. ResourceBase
// ends in /history/{scope}/{version}/{generation}; append /chunks/{first}/{last}
// for direct arithmetic access. Scope isolates the
// authenticated account, including legacy shared tasks. Browser HTTP caching
// owns storage/eviction; clients need no inactive decoded transcript cache.
// Generation changes on server restart/task replacement. A stale reference
// returns 409; bootstrap again and discard old cursor metadata.
// For a one-based record r, first=floor((r-1)/RecordsPerChunk)*RecordsPerChunk+1
// and last=min(first+RecordsPerChunk-1,Through). Through=0 has no chunks.
type HistoryBootstrap struct {
	ProjectionVersion string `json:"projectionVersion"`
	Generation        string `json:"generation"`
	Scope             string `json:"scope"`
	RecordsPerChunk   uint64 `json:"recordsPerChunk"`
	Through           uint64 `json:"through"`
	Cursor            string `json:"cursor"`
	ResourceBase      string `json:"resourceBase"`
}

// HistoryCheckpoint advances through all outputs of a physical record,
// including a record with zero outputs. Cursor is opaque; it encodes projection
// version, generation, raw record and either output index or end-of-record.
// SSE resumes strictly after it. The Last-Event-ID header overrides the initial
// cursor query on reconnect. Named stats events never replace this raw cursor.
type HistoryCheckpoint struct {
	Cursor string `json:"cursor"`
}

// HistoryReset tells an SSE client to close the stream and rediscover bootstrap.
type HistoryReset struct {
	BootstrapURL string `json:"bootstrapURL"`
}

// HistoryTurnEstimate is a revalidated price for a canonical result. TotalUSD
// includes the prefix so session-cumulative reports retain their meaning. Model
// is the authoritative root model at the result, including reroutes, even when
// pricing is unknown; canonical output bytes are unchanged.
type HistoryTurnEstimate struct {
	HistoryPosition

	Model    string    `json:"model,omitempty"`
	Cost     EventCost `json:"cost,omitzero"`
	TotalUSD float64   `json:"totalUSD"`
}

// HistoryStats contains the bounded 60-sample runtime ring, oldest first.
// Replace samples sharing a timestamp: disk usage can revise the latest sample.
// These host observations are separate from immutable raw task history.
type HistoryStats struct {
	Samples []EventStats `json:"samples"`
}
