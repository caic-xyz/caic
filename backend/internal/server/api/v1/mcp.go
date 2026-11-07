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
