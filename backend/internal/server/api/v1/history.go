// Defines section summaries, immutable history resources, mutable analytics and turn timelines, and exact SSE cursor contracts.

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

// HistoryStats contains the bounded runtime ring, oldest first.
// Replace samples sharing a timestamp: disk usage can revise the latest sample.
// These host observations are separate from immutable raw task history.
type HistoryStats struct {
	Samples []EventStats `json:"samples"`
	// Retained is the ring's capacity: the server keeps at most this many
	// latest samples, so a full ring has dropped older ones. Clients keep as
	// many while live samples arrive.
	Retained int `json:"retained"`
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
//
// Number counts root turns from 1 across every context section of the task;
// delegated child results are not turns. StartedAt is the first root prompt or
// root activity after the previous root result, as in the task's turn metrics.
// A turn the harness queues by itself, such as one delivering the end of
// background work, starts at its first root activity. Compaction keeps the
// start; a context clear discards it, and so does a failed delivery of the
// starting prompt before root output accepted it. A turn without any root
// prompt or activity starts at its result. At is the result. WaitMS is the
// time from At to the prompt at WaitInput, which starts the next turn.
// Effort is the root reasoning effort reported by the last init.
// ContextTokens is the root context size when the turn ended, as the task's
// context meter measures it: the native occupancy when the harness reports it,
// else the input context of the last root API call. Zero means unknown.
type HistoryTurn struct {
	HistoryPosition

	Number        int              `json:"number"`
	SectionID     string           `json:"sectionID"`
	StartedAt     time.Time        `json:"startedAt"`
	At            time.Time        `json:"at"`
	Model         string           `json:"model"`
	Effort        string           `json:"effort,omitempty"`
	Usage         EventUsage       `json:"usage"`
	DurationMS    int64            `json:"durationMs"`
	DurationAPIMS int64            `json:"durationAPIMs"`
	WaitInput     *HistoryPosition `json:"waitInput,omitempty"`
	WaitMS        *int64           `json:"waitMs,omitempty"`
	ChangeStat    *EventChangeStat `json:"changeStat,omitempty"`
	Cost          EventCost        `json:"cost,omitzero"`
	ContextTokens int              `json:"contextTokens,omitempty"`
}

// HistoryRunningTurn is the root turn in progress: a root prompt or root
// activity started it, and no root result has ended it yet. Number is one past
// the last completed HistoryTurn, and StartedAt follows the HistoryTurn rules,
// so the turn keeps both when it completes. SectionID is the context it runs
// in; compaction can move it to a later one. A turn stops running when the
// harness process exits without ending it: the turn does not complete, and a
// later root prompt or activity resumes it from the same start.
type HistoryRunningTurn struct {
	Number    int       `json:"number"`
	SectionID string    `json:"sectionID"`
	StartedAt time.Time `json:"startedAt"`
}

// HistoryTurnSpanKind names the lane of a turn timeline span.
type HistoryTurnSpanKind string

// Turn timeline span kinds.
const (
	// HistoryTurnSpanKindBackground is a detached command started in the turn.
	HistoryTurnSpanKindBackground HistoryTurnSpanKind = "background"
	// HistoryTurnSpanKindModel is turn time that no root tool call covers. It is
	// inferred: harnesses report a model call only when it ends, so this time
	// also contains harness overhead between calls. In a running turn, the
	// model span after the last root tool call is provisional and has no EndMS.
	HistoryTurnSpanKindModel HistoryTurnSpanKind = "model"
	// HistoryTurnSpanKindSubagent is a native subagent started in the turn.
	HistoryTurnSpanKindSubagent HistoryTurnSpanKind = "subagent"
	// HistoryTurnSpanKindTool is a tool call by the root agent or a child actor.
	HistoryTurnSpanKindTool HistoryTurnSpanKind = "tool"
)

// HistoryTurnSpan is one interval of a turn timeline, in milliseconds from the
// turn start. EndMS is absent when the span has no observed end: a tool call
// still open when the turn ended or still running, a subagent or command whose
// Status is running or unknown, or the provisional model span of a running
// turn. A subagent or command can end after the turn: EndMS then exceeds the
// turn duration.
//
// The identity locates the span's transcript item. A tool span has Source and
// ToolUseID, which scope the call as tool cards do. A command span has
// CommandID, plus the Source and ToolUseID of the call that launched it. A
// subagent span has ActivityID, a HistoryActivity ID; a tool or command span by
// a child actor names its owning activity there when known. A model span has
// only times.
//
// Label is the tool name, subagent label, or command label; Detail is the tool
// card's short detail. Status is completed, failed, or interrupted for a tool;
// subagents and commands use their HistoryActivity and HistoryBackgroundCommand
// status values.
type HistoryTurnSpan struct {
	Kind       HistoryTurnSpanKind `json:"kind"`
	StartMS    int64               `json:"startMs"`
	EndMS      *int64              `json:"endMs,omitempty"`
	Label      string              `json:"label,omitempty"`
	Detail     string              `json:"detail,omitempty"`
	Status     string              `json:"status,omitempty"`
	Source     EventMessageSource  `json:"source,omitzero"`
	ToolUseID  string              `json:"toolUseID,omitempty"`
	ActivityID string              `json:"activityID,omitempty"`
	CommandID  string              `json:"commandID,omitempty"`
}

// HistoryTurnOmission summarizes the spans of one kind that a timeline omits.
// DurationMS sums their durations within the turn, which can overlap: a span
// without an end, or one that outlives the turn, counts up to the turn's end.
type HistoryTurnOmission struct {
	Kind       HistoryTurnSpanKind `json:"kind"`
	Count      int                 `json:"count"`
	DurationMS int64               `json:"durationMs"`
}

// HistoryTurnTokens are disjoint token classes. The three input classes sum to
// a call's input context; ReasoningOutputTokens is a subset of OutputTokens.
type HistoryTurnTokens struct {
	InputTokens              int `json:"inputTokens"`
	CacheCreationInputTokens int `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int `json:"cacheReadInputTokens"`
	OutputTokens             int `json:"outputTokens"`
	ReasoningOutputTokens    int `json:"reasoningOutputTokens"`
}

// HistoryTurnCall is one model API call in a turn, by the root agent or a
// child actor. Number counts the turn's calls from 1 in the order the harness
// reported them, which is the timeline's order. AtMS is when the harness
// reported the call, in milliseconds from the turn start: a call reports its
// usage when it ends, so AtMS is the end of the model time that produced it.
//
// The call's context is its input classes summed. ContextWindow is absent
// when unknown; root calls without their own report use the window the turn
// reports. Cost is absent when the call cannot be priced. It is reported when
// the harness prices each call, and otherwise estimated from model token
// prices, even when the harness reports the turn's total.
type HistoryTurnCall struct {
	HistoryTurnTokens

	Number        int                `json:"number"`
	AtMS          int64              `json:"atMs"`
	Source        EventMessageSource `json:"source,omitzero"`
	ActivityID    string             `json:"activityID,omitempty"`
	Model         string             `json:"model,omitempty"`
	ContextWindow int                `json:"contextWindow,omitempty"`
	Cost          EventCost          `json:"cost,omitzero"`
}

// HistoryTurnCallOmission sums the token classes of the calls a timeline omits.
type HistoryTurnCallOmission struct {
	HistoryTurnTokens

	Count int `json:"count"`
}

// HistoryTurnTool sums one tool name's calls that start in the turn, by the
// root agent and child actors. DurationMS and LongestMS count only time within
// the turn: a call without an end counts up to the turn's end. Concurrent calls
// overlap, so durations can sum to more than the turn.
type HistoryTurnTool struct {
	Name       string `json:"name"`
	Calls      int    `json:"calls"`
	DurationMS int64  `json:"durationMs"`
	LongestMS  int64  `json:"longestMs"`
}

// HistoryTurnTimeline places one root turn's work on one time axis of
// DurationMS from StartedAt. For a completed turn, StartedAt and EndedAt match
// its HistoryTurn's StartedAt and At. For the HistoryRunningTurn, EndedAt is absent and the axis
// ends at the later of the server's clock and the turn's latest observation
// when the server derived the timeline; open spans and model time then grow
// with each request. It includes tool calls
// that start in the turn, from the root agent and from child actors, and the
// subagents and detached commands that start in it, including those that
// outlive it. Model spans fill the turn time outside root tool calls; ModelMS
// is their exact total. Background work and child calls run beside the root
// agent, so they do not reduce model time.
//
// Spans holds at most 500 spans sorted by start: the longest ones, with spans
// lacking an end kept first. Omitted counts the shorter spans left out, per
// kind. The server derives the timeline from its own history index, so archived
// context sections need no transcript download. Durations within the turn count
// up to the axis end.
//
// Calls holds at most 500 API calls sorted by Number: those with the most
// tokens. OmittedCalls sums the rest. Calls is empty when the harness reports
// usage only per turn. Unattributed sums the root agent's usage reports that
// cover no single call, such as cumulative deltas spanning several requests
// after a missed event; they are not priced per call. Tools sums every tool
// call in the turn by name, longest total first, including calls that Spans
// omits.
type HistoryTurnTimeline struct {
	Number       int                     `json:"number"`
	StartedAt    time.Time               `json:"startedAt"`
	EndedAt      time.Time               `json:"endedAt,omitzero"`
	DurationMS   int64                   `json:"durationMs"`
	ModelMS      int64                   `json:"modelMs"`
	Spans        []HistoryTurnSpan       `json:"spans"`
	Omitted      []HistoryTurnOmission   `json:"omitted"`
	Calls        []HistoryTurnCall       `json:"calls"`
	OmittedCalls HistoryTurnCallOmission `json:"omittedCalls,omitzero"`
	Unattributed HistoryTurnTokens       `json:"unattributed,omitzero"`
	Tools        []HistoryTurnTool       `json:"tools"`
}

// HistoryToolTiming aggregates completed tools across root and child actors.
type HistoryToolTiming struct {
	Name       string `json:"name"`
	Calls      int    `json:"calls"`
	DurationMS int64  `json:"durationMs"`
}

// HistoryActivity is one native subagent's payload-free lifecycle and routing
// state. Prompt, result, and child transcripts are fetched through the finite
// activity resource only on disclosure. BodyThrough is a stable finite prefix
// containing all actor payload and ownership observations known at the
// manifest snapshot. Unrelated task records do not change it, allowing
// disclosures to reuse the HTTP cache. OwnershipThrough advances only for
// related routing/namespace observations; clients use it to distinguish
// ownership revisions from new actor output.
type HistoryActivity struct {
	ID string `json:"id"`
	// RunID distinguishes resumed runs and sorts in execution order per ID.
	RunID string `json:"runID,omitempty"`
	// SectionID identifies the body containing the earliest actual lifecycle or
	// resolved transcript origin, including an unfinished turn carried forward.
	SectionID string                   `json:"sectionID"`
	Source    EventMessageSource       `json:"source,omitzero"`
	ToolUseID string                   `json:"toolUseID,omitempty"`
	Scope     EventNativeSubagentScope `json:"scope,omitempty"`
	GroupID   string                   `json:"groupID,omitempty"`
	Label     string                   `json:"label,omitempty"`
	// Kind names the agent definition the harness reports, such as Claude's
	// subagent_type. It is absent when the harness reports none.
	Kind             string          `json:"kind,omitempty"`
	Status           string          `json:"status"`
	Background       bool            `json:"background,omitempty"`
	StartedAt        *time.Time      `json:"startedAt,omitempty"`
	EndedAt          *time.Time      `json:"endedAt,omitempty"`
	AnchorAt         time.Time       `json:"anchorAt"`
	First            HistoryPosition `json:"first"`
	BodyThrough      uint64          `json:"bodyThrough"`
	OwnershipThrough uint64          `json:"ownershipThrough"`
}

// HistoryBackgroundCommand is a detached command's folded, payload-free
// lifecycle. Clients attach it to the tool call identified by Source and
// ToolUseID; OutputRef names the container file holding its output.
type HistoryBackgroundCommand struct {
	ID         string             `json:"id"`
	Source     EventMessageSource `json:"source,omitzero"`
	ToolUseID  string             `json:"toolUseID,omitempty"`
	Label      string             `json:"label,omitempty"`
	Status     string             `json:"status"`
	StartedAt  *time.Time         `json:"startedAt,omitempty"`
	EndedAt    *time.Time         `json:"endedAt,omitempty"`
	ExitCode   *int               `json:"exitCode,omitempty"`
	OutputRef  string             `json:"outputRef,omitempty"`
	Watch      bool               `json:"watch,omitempty"`      // See EventBackgroundCommand.Watch.
	WakesAgent bool               `json:"wakesAgent,omitempty"` // See EventBackgroundCommand.WakesAgent.
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
	RoutingRevision    uint64                     `json:"routingRevision"`
	Routing            []HistoryRoute             `json:"routing"`
	HasInitialPrompt   bool                       `json:"hasInitialPrompt"`
	SetupEndedAt       time.Time                  `json:"setupEndedAt,omitzero"`
	Turns              []HistoryTurn              `json:"turns"`
	RunningTurn        *HistoryRunningTurn        `json:"runningTurn,omitempty"`
	Tools              []HistoryToolTiming        `json:"tools"`
	Activities         []HistoryActivity          `json:"activities"`
	BackgroundCommands []HistoryBackgroundCommand `json:"backgroundCommands"`
	InitialInit        *HistoryEvent              `json:"initialInit,omitempty"`
	InitialInput       *HistoryPosition           `json:"initialInput,omitempty"`
	Setup              *HistorySlice              `json:"setup,omitempty"`
	Todo               *EventTodo                 `json:"todo,omitempty"`
}
