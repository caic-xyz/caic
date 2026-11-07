// Structured task-detail responses for MCP callers, with independent field truncation metadata.

package v1

// MCPTaskDetail is the structured result of the task_get_detail tool.
type MCPTaskDetail struct {
	Result                string           `json:"result"` // Short readable summary, also used as MCP text content.
	TaskNumber            int              `json:"taskNumber" jsonschema_description:"Task-list number, or zero when addressed by stable ID; use taskID for subsequent calls"`
	TaskID                string           `json:"taskID"`
	Title                 string           `json:"title"`
	State                 TaskState        `json:"state"`
	Harness               Harness          `json:"harness"`
	Model                 string           `json:"model,omitempty"`
	Effort                string           `json:"effort,omitempty"`
	RuntimeName           string           `json:"runtimeName,omitempty"`
	NumTurns              int              `json:"numTurns"`
	DurationSeconds       float64          `json:"durationSeconds"`
	CostUSD               float64          `json:"costUSD"`
	CostKnown             bool             `json:"costKnown" jsonschema_description:"Whether any cost is known; costUSD may be a subtotal"`
	CostComplete          bool             `json:"costComplete" jsonschema_description:"Whether costUSD covers all observed billed usage"`
	TaskResult            string           `json:"taskResult,omitempty"`
	Error                 string           `json:"error,omitempty"`
	ChangedFiles          int              `json:"changedFiles"`
	ChangedPaths          []string         `json:"changedPaths"`
	ChangedPathsTruncated bool             `json:"changedPathsTruncated"`
	PendingInput          *MCPPendingInput `json:"pendingInput" jsonschema:"nullable"`
	Diffstat              *MCPTaskDiffstat `json:"diffstat,omitempty"`
	TruncatedFields       []string         `json:"truncatedFields,omitempty"` // Scalar field names shortened or repaired for valid UTF-8.
}

// MCPPendingInputState identifies the current delivery status of a pending prompt.
type MCPPendingInputState string

// Pending input delivery states.
const (
	MCPPendingInputDelivered  MCPPendingInputState = "delivered"
	MCPPendingInputDelivering MCPPendingInputState = "delivering"
	MCPPendingInputQueued     MCPPendingInputState = "queued"
)

// MCPPendingInput exposes a pending prompt's identity, delivery state, and bounded text.
type MCPPendingInput struct {
	ID                string               `json:"id"`
	State             MCPPendingInputState `json:"state" jsonschema:"enum=delivered,enum=delivering,enum=queued"`
	Text              string               `json:"text"`
	OriginalTextBytes int                  `json:"originalTextBytes"`
	TextTruncated     bool                 `json:"textTruncated"`
	ImageCount        int                  `json:"imageCount"`
	Error             string               `json:"error,omitempty"`
	ErrorTruncated    bool                 `json:"errorTruncated"`
}

// MCPTaskDiffstat contains independently bounded repository detail and complete comparison totals.
type MCPTaskDiffstat struct {
	Repositories []DiffIndexRepository `json:"repositories"`
	Totals       MCPDiffTotals         `json:"totals"`
	Truncated    bool                  `json:"truncated"`
}

// MCPDiffTotals counts all compared commits and working trees, including omitted rows.
type MCPDiffTotals struct {
	FileChanges int `json:"fileChanges"`
	Additions   int `json:"additions"`
	Deletions   int `json:"deletions"`
	BinaryFiles int `json:"binaryFiles"`
}

// MCPTaskSummary is one bounded task-list entry exposed through MCP.
type MCPTaskSummary struct {
	ServiceActive

	HasPendingInput bool      `json:"hasPendingInput" jsonschema_description:"Whether a pending prompt record exists; task_get_detail shows its text and delivery status"`
	TaskNumber      int       `json:"taskNumber"              jsonschema_description:"Current session task number used by other task tools"`
	TaskID          string    `json:"taskID"                  jsonschema_description:"Stable task ID"`
	Title           string    `json:"title"                   jsonschema_description:"Task title"`
	State           TaskState `json:"state"                   jsonschema_description:"Current task lifecycle state"`
	Harness         Harness   `json:"harness"                 jsonschema_description:"Coding-agent harness"`
	Model           string    `json:"model,omitempty"         jsonschema_description:"Configured model, or absent for the harness default"`
	Effort          string    `json:"effort,omitempty"        jsonschema_description:"Configured reasoning effort, or absent for the harness default"`
	RuntimeName     string    `json:"runtimeName,omitempty"   jsonschema_description:"Runtime backend name"`
	StatusMessage   string    `json:"statusMessage,omitempty" jsonschema_description:"Bounded result or failure context for terminal tasks"`
	ChangedFiles    int       `json:"changedFiles"            jsonschema_description:"Number of changed files"`
	Additions       int       `json:"additions"               jsonschema_description:"Added lines"`
	Deletions       int       `json:"deletions"               jsonschema_description:"Deleted lines"`
	ForgePR         int       `json:"forgePR,omitempty"       jsonschema_description:"Pull request number"`
	CIStatus        CIStatus  `json:"ciStatus,omitempty"      jsonschema_description:"Continuous integration status"`
	DurationSeconds float64   `json:"durationSeconds"         jsonschema_description:"Task duration in seconds"`
	CostUSD         float64   `json:"costUSD"                 jsonschema_description:"Task cost in US dollars"`
	CostKnown       bool      `json:"costKnown"               jsonschema_description:"Whether any cost is known; costUSD may be a subtotal"`
	CostComplete    bool      `json:"costComplete" jsonschema_description:"Whether costUSD covers all observed billed usage"`
}
