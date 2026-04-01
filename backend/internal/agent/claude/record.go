package claude

import (
	"encoding/json"

	"fmt"

	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// Record type constants.
const (
	TypeQueueOperation      = "queue-operation"
	TypeUser                = "user"
	TypeAssistant           = "assistant"
	TypeSystem              = "system"
	TypeProgress            = "progress"
	TypeSummary             = "summary"
	TypeFileHistorySnapshot = "file-history-snapshot"
)

// Record is a single line from a Claude Code JSONL session log.
// Use the typed accessor methods to get the concrete record after checking Type.
type Record struct {
	// Type discriminates the record kind.
	Type string `json:"type"`

	raw json.RawMessage
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *Record) UnmarshalJSON(data []byte) error {
	var probe outputTypeProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("Record: %w", err)
	}
	r.Type = string(probe.Type)
	r.raw = append(r.raw[:0], data...)
	return nil
}

// AsQueueOperation decodes the record as a QueueOperation.
func (r *Record) AsQueueOperation() (*QueueOperation, error) {
	var v QueueOperation
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsUser decodes the record as a UserRecord.
func (r *Record) AsUser() (*UserRecord, error) {
	var v UserRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsAssistant decodes the record as an AssistantRecord.
func (r *Record) AsAssistant() (*AssistantRecord, error) {
	var v AssistantRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsSystem decodes the record as a SystemRecord.
func (r *Record) AsSystem() (*SystemRecord, error) {
	var v SystemRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsProgress decodes the record as a ProgressRecord.
func (r *Record) AsProgress() (*ProgressRecord, error) {
	var v ProgressRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsSummary decodes the record as a SummaryRecord.
func (r *Record) AsSummary() (*SummaryRecord, error) {
	var v SummaryRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsFileHistorySnapshot decodes the record as a FileHistorySnapshotRecord.
func (r *Record) AsFileHistorySnapshot() (*FileHistorySnapshotRecord, error) {
	var v FileHistorySnapshotRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Raw returns the original JSON bytes for this record.
func (r *Record) Raw() json.RawMessage { return r.raw }

// QueueOperation represents a session queue event.
type QueueOperation struct {
	Type      string `json:"type"`
	Operation string `json:"operation"` // "enqueue" or "dequeue"
	Timestamp string `json:"timestamp"`
	SessionID string `json:"sessionId"`
	Content   string `json:"content,omitempty"`

	jsonutil.Overflow
}

var queueOperationKnown = jsonutil.KnownFields(QueueOperation{})

// UnmarshalJSON implements json.Unmarshaler.
func (q *QueueOperation) UnmarshalJSON(data []byte) error {
	type Alias QueueOperation
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("QueueOperation: %w", err)
	}
	alias := (*Alias)(q)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("QueueOperation: %w", err)
	}
	q.Extra = jsonutil.CollectUnknown(raw, queueOperationKnown)
	jsonutil.WarnUnknown("QueueOperation", q.Extra)
	return nil
}

// MessageFields are common fields across user/assistant/progress records.
type MessageFields struct {
	Type        string  `json:"type"`
	ParentUUID  *string `json:"parentUuid"`
	IsSidechain bool    `json:"isSidechain,omitempty"`
	UserType    string  `json:"userType,omitempty"`
	CWD         string  `json:"cwd,omitempty"`
	SessionID   string  `json:"sessionId"`
	Version     string  `json:"version,omitempty"`
	GitBranch   string  `json:"gitBranch,omitempty"`
	AgentID     string  `json:"agentId,omitempty"`
	Slug        string  `json:"slug,omitempty"`
	UUID        string  `json:"uuid"`
	Timestamp   string  `json:"timestamp"`
}

// UserRecord is a user message in the conversation.
type UserRecord struct {
	MessageFields

	Message                   *UserMessage      `json:"message"`
	PermissionMode            string            `json:"permissionMode,omitempty"`
	ThinkingMetadata          *ThinkingMetadata `json:"thinkingMetadata,omitempty"`
	Todos                     []Todo            `json:"todos,omitempty"`
	ToolUseResult             json.RawMessage   `json:"toolUseResult,omitempty"`
	SourceToolAssistantUUID   string            `json:"sourceToolAssistantUUID,omitempty"`
	IsCompactSummary          bool              `json:"isCompactSummary,omitempty"`
	IsMeta                    bool              `json:"isMeta,omitempty"`
	IsVisibleInTranscriptOnly bool              `json:"isVisibleInTranscriptOnly,omitempty"`
	PlanContent               string            `json:"planContent,omitempty"`
	SourceToolUseID           string            `json:"sourceToolUseID,omitempty"`

	jsonutil.Overflow
}

var userRecordKnown = jsonutil.KnownFields(UserRecord{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *UserRecord) UnmarshalJSON(data []byte) error {
	type Alias UserRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("UserRecord: %w", err)
	}
	alias := (*Alias)(u)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("UserRecord: %w", err)
	}
	u.Extra = jsonutil.CollectUnknown(raw, userRecordKnown)
	jsonutil.WarnUnknown("UserRecord", u.Extra)
	return nil
}

// ToolUseResultText returns the tool use result as a plain string (e.g. Bash output).
// Returns ("", false) if it's not a string.
func (u *UserRecord) ToolUseResultText() (string, bool) {
	if u.ToolUseResult == nil {
		return "", false
	}
	var s string
	if err := json.Unmarshal(u.ToolUseResult, &s); err == nil {
		return s, true
	}
	return "", false
}

// ToolUseResultObject returns the tool use result as a typed ToolUseResult object.
// Returns (nil, false) if it's a plain string or absent.
func (u *UserRecord) ToolUseResultObject() (*ToolUseResult, bool) {
	if u.ToolUseResult == nil {
		return nil, false
	}
	var v ToolUseResult
	if err := json.Unmarshal(u.ToolUseResult, &v); err == nil {
		return &v, true
	}
	return nil, false
}

// AssistantRecord is an assistant response in the conversation.
type AssistantRecord struct {
	MessageFields

	Message           *APIMessage `json:"message"`
	RequestID         string      `json:"requestId,omitempty"`
	Error             string      `json:"error,omitempty"`
	IsAPIErrorMessage bool        `json:"isApiErrorMessage,omitempty"`

	jsonutil.Overflow
}

var assistantRecordKnown = jsonutil.KnownFields(AssistantRecord{})

// UnmarshalJSON implements json.Unmarshaler.
func (a *AssistantRecord) UnmarshalJSON(data []byte) error {
	type Alias AssistantRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("AssistantRecord: %w", err)
	}
	alias := (*Alias)(a)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("AssistantRecord: %w", err)
	}
	a.Extra = jsonutil.CollectUnknown(raw, assistantRecordKnown)
	jsonutil.WarnUnknown("AssistantRecord", a.Extra)
	return nil
}

// SystemRecord represents system-level events (compaction, errors, commands).
type SystemRecord struct {
	MessageFields

	Subtype               string           `json:"subtype,omitempty"`
	Content               string           `json:"content,omitempty"`
	Level                 string           `json:"level,omitempty"`
	IsMeta                bool             `json:"isMeta,omitempty"`
	LogicalParentUUID     string           `json:"logicalParentUuid,omitempty"`
	DurationMs            int64            `json:"durationMs,omitempty"`
	CompactMetadata       *CompactMetadata `json:"compactMetadata,omitempty"`
	MicrocompactMetadata  json.RawMessage  `json:"microcompactMetadata,omitempty"`
	Error                 json.RawMessage  `json:"error,omitempty"`
	RetryInMs             float64          `json:"retryInMs,omitempty"`
	RetryAttempt          int              `json:"retryAttempt,omitempty"`
	MaxRetries            int              `json:"maxRetries,omitempty"`
	HasOutput             bool             `json:"hasOutput,omitempty"`
	HookCount             int              `json:"hookCount,omitempty"`
	HookErrors            json.RawMessage  `json:"hookErrors,omitempty"`
	HookInfos             json.RawMessage  `json:"hookInfos,omitempty"`
	PreventedContinuation bool             `json:"preventedContinuation,omitempty"`
	StopReason            string           `json:"stopReason,omitempty"`
	ToolUseID             string           `json:"toolUseID,omitempty"`

	jsonutil.Overflow
}

var systemRecordKnown = jsonutil.KnownFields(SystemRecord{})

// UnmarshalJSON implements json.Unmarshaler.
func (s *SystemRecord) UnmarshalJSON(data []byte) error {
	type Alias SystemRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("SystemRecord: %w", err)
	}
	alias := (*Alias)(s)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("SystemRecord: %w", err)
	}
	s.Extra = jsonutil.CollectUnknown(raw, systemRecordKnown)
	jsonutil.WarnUnknown("SystemRecord", s.Extra)
	return nil
}

// CompactMetadata appears on compact_boundary system records.
type CompactMetadata struct {
	Trigger   string `json:"trigger"`
	PreTokens int    `json:"preTokens"`

	jsonutil.Overflow
}

var compactMetadataKnown = jsonutil.KnownFields(CompactMetadata{})

// UnmarshalJSON implements json.Unmarshaler.
func (c *CompactMetadata) UnmarshalJSON(data []byte) error {
	type Alias CompactMetadata
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("CompactMetadata: %w", err)
	}
	alias := (*Alias)(c)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("CompactMetadata: %w", err)
	}
	c.Extra = jsonutil.CollectUnknown(raw, compactMetadataKnown)
	jsonutil.WarnUnknown("CompactMetadata", c.Extra)
	return nil
}

// SummaryRecord is a conversation summary marker.
type SummaryRecord struct {
	Type     string `json:"type"`
	Summary  string `json:"summary"`
	LeafUUID string `json:"leafUuid"`

	jsonutil.Overflow
}

var summaryRecordKnown = jsonutil.KnownFields(SummaryRecord{})

// UnmarshalJSON implements json.Unmarshaler.
func (s *SummaryRecord) UnmarshalJSON(data []byte) error {
	type Alias SummaryRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("SummaryRecord: %w", err)
	}
	alias := (*Alias)(s)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("SummaryRecord: %w", err)
	}
	s.Extra = jsonutil.CollectUnknown(raw, summaryRecordKnown)
	jsonutil.WarnUnknown("SummaryRecord", s.Extra)
	return nil
}

// FileHistorySnapshotRecord tracks file changes for undo.
type FileHistorySnapshotRecord struct {
	Type             string    `json:"type"`
	MessageID        string    `json:"messageId"`
	Snapshot         *Snapshot `json:"snapshot"`
	IsSnapshotUpdate bool      `json:"isSnapshotUpdate"`

	jsonutil.Overflow
}

var fileHistorySnapshotKnown = jsonutil.KnownFields(FileHistorySnapshotRecord{})

// UnmarshalJSON implements json.Unmarshaler.
func (f *FileHistorySnapshotRecord) UnmarshalJSON(data []byte) error {
	type Alias FileHistorySnapshotRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("FileHistorySnapshotRecord: %w", err)
	}
	alias := (*Alias)(f)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("FileHistorySnapshotRecord: %w", err)
	}
	f.Extra = jsonutil.CollectUnknown(raw, fileHistorySnapshotKnown)
	jsonutil.WarnUnknown("FileHistorySnapshotRecord", f.Extra)
	return nil
}

// Snapshot captures file states at a point in time.
type Snapshot struct {
	MessageID          string                `json:"messageId"`
	TrackedFileBackups map[string]FileBackup `json:"trackedFileBackups"`
	Timestamp          string                `json:"timestamp"`

	jsonutil.Overflow
}

var snapshotKnown = jsonutil.KnownFields(Snapshot{})

// UnmarshalJSON implements json.Unmarshaler.
func (s *Snapshot) UnmarshalJSON(data []byte) error {
	type Alias Snapshot
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("Snapshot: %w", err)
	}
	alias := (*Alias)(s)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("Snapshot: %w", err)
	}
	s.Extra = jsonutil.CollectUnknown(raw, snapshotKnown)
	jsonutil.WarnUnknown("Snapshot", s.Extra)
	return nil
}

// FileBackup describes a single file's backup state.
type FileBackup struct {
	BackupFileName string `json:"backupFileName"`
	Version        int    `json:"version"`
	BackupTime     string `json:"backupTime"`

	jsonutil.Overflow
}

var fileBackupKnown = jsonutil.KnownFields(FileBackup{})

// UnmarshalJSON implements json.Unmarshaler.
func (f *FileBackup) UnmarshalJSON(data []byte) error {
	type Alias FileBackup
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("FileBackup: %w", err)
	}
	alias := (*Alias)(f)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("FileBackup: %w", err)
	}
	f.Extra = jsonutil.CollectUnknown(raw, fileBackupKnown)
	jsonutil.WarnUnknown("FileBackup", f.Extra)
	return nil
}
