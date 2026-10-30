// Defines section summaries, immutable history resources, mutable analytics, and exact SSE cursor contracts.

package v1

import "time"

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

// HistoryBootstrap identifies the raw prefix captured with a HistoryManifest.
// ResourceBase scopes immutable resources to the account, projection version,
// and task generation. Generation changes on server restart/task replacement;
// a stale reference returns 409 and requires manifest rediscovery.
// RecordsPerChunk defines fixed raw-record page alignment for section slices.
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
	Cursor  string `json:"cursor"`
	Through uint64 `json:"through"`
}

// HistoryReset tells an SSE client to close the stream and rediscover bootstrap.
type HistoryReset struct {
	ManifestURL string `json:"manifestURL"`
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

// HistoryManifest discovers context sections without downloading their bodies.
// Metadata is mutable at Bootstrap.Through; bodies are immutable finite slices.
type HistoryManifest struct {
	Bootstrap HistoryBootstrap `json:"bootstrap"`
	Sections  []HistorySection `json:"sections"`
	Metadata  HistoryMetadata  `json:"metadata"`
}

// HistorySlice selects inclusive normalized positions, independently of raw
// chunk alignment. An absent slice denotes an empty section.
type HistorySlice struct {
	First HistoryPosition `json:"first"`
	Last  HistoryPosition `json:"last"`
}

// HistorySection describes one root context. Only the final section is current.
// Boundary and EndBoundary contain init/system metadata, never transcript text.
type HistorySection struct {
	ID            string           `json:"id"`
	Body          *HistorySlice    `json:"body,omitempty"`
	Boundary      *HistoryEvent    `json:"boundary,omitempty"`
	EndBoundary   *HistoryEvent    `json:"endBoundary,omitempty"`
	ReportedModel string           `json:"reportedModel,omitempty"`
	TurnCount     int              `json:"turnCount"`
	TextCount     int              `json:"textCount"`
	ToolCount     int              `json:"toolCount"`
	DurationMS    int64            `json:"durationMs"`
	Skills        []EventSkillLoad `json:"skills"`
}

// HistoryTurn retains numeric invocation facts independently of transcript
// lifetime. SectionID associates the turn with its context. Pricing is obtained
// separately through getHistoryCosts using the embedded exact position.
type HistoryTurn struct {
	HistoryPosition

	SectionID     string           `json:"sectionID"`
	At            time.Time        `json:"at"`
	Model         string           `json:"model"`
	Usage         EventUsage       `json:"usage"`
	DurationMS    int64            `json:"durationMs"`
	DurationAPIMS int64            `json:"durationAPIMs"`
	WaitInput     *HistoryPosition `json:"waitInput,omitempty"`
	WaitMS        *int64           `json:"waitMs,omitempty"`
	ChangeStat    *EventChangeStat `json:"changeStat,omitempty"`
	Cost          EventCost        `json:"cost,omitzero"`
}

// HistoryToolTiming aggregates completed tools across root and child actors.
type HistoryToolTiming struct {
	Name       string `json:"name"`
	Calls      int    `json:"calls"`
	DurationMS int64  `json:"durationMs"`
}

// HistoryActivity is payload-free lifecycle and routing state. Kind is
// nativeSubagent or backgroundCommand. Prompt, result, and child transcripts
// are fetched through the finite activity resource only on disclosure.
// BodyThrough is a stable finite prefix containing all actor payload and
// ownership observations known at the manifest snapshot. Unrelated task
// records do not change it, allowing disclosures to reuse the HTTP cache.
// OwnershipThrough advances only for related routing/namespace observations;
// clients use it to distinguish ownership revisions from new actor output.
type HistoryActivity struct {
	Kind EventKind `json:"kind"`
	ID   string    `json:"id"`
	// SectionID identifies the body containing the earliest actual lifecycle or
	// resolved transcript origin, including an unfinished turn carried forward.
	SectionID        string                   `json:"sectionID"`
	Source           EventMessageSource       `json:"source,omitzero"`
	ToolUseID        string                   `json:"toolUseID,omitempty"`
	Scope            EventNativeSubagentScope `json:"scope,omitempty"`
	GroupID          string                   `json:"groupID,omitempty"`
	Label            string                   `json:"label,omitempty"`
	Status           string                   `json:"status"`
	Background       bool                     `json:"background,omitempty"`
	StartedAt        *time.Time               `json:"startedAt,omitempty"`
	EndedAt          *time.Time               `json:"endedAt,omitempty"`
	AnchorAt         time.Time                `json:"anchorAt"`
	First            HistoryPosition          `json:"first"`
	BodyThrough      uint64                   `json:"bodyThrough"`
	OwnershipThrough uint64                   `json:"ownershipThrough"`
	ExitCode         *int                     `json:"exitCode,omitempty"`
	OutputRef        string                   `json:"outputRef,omitempty"`
}

// HistoryRoute is one authoritative delegation relation or namespace observation.
// An absent AgentID records a namespace that participates in ambiguity checks.
type HistoryRoute struct {
	ToolUseID string `json:"toolUseID"`
	Namespace string `json:"namespace"`
	AgentID   string `json:"agentID,omitempty"`
}

// HistoryMetadata contains compact task-wide facts needed without archived
// bodies. Setup bounds the separately disclosed, filtered getHistorySetup
// resource; it must not be fetched as an unfiltered slice. InitialInput
// identifies the first root input, permitting prompt deduplication by position.
type HistoryMetadata struct {
	// RoutingRevision changes when earlier delegated ownership may have changed.
	RoutingRevision  uint64              `json:"routingRevision"`
	Routing          []HistoryRoute      `json:"routing"`
	HasInitialPrompt bool                `json:"hasInitialPrompt"`
	SetupEndedAt     time.Time           `json:"setupEndedAt,omitzero"`
	Turns            []HistoryTurn       `json:"turns"`
	Tools            []HistoryToolTiming `json:"tools"`
	Activities       []HistoryActivity   `json:"activities"`
	InitialInit      *HistoryEvent       `json:"initialInit,omitempty"`
	InitialInput     *HistoryPosition    `json:"initialInput,omitempty"`
	Setup            *HistorySlice       `json:"setup,omitempty"`
	Todo             *EventTodo          `json:"todo,omitempty"`
}
