// Defines caic public model context independently of transport routing.

package mcpschema

// TaskContext identifies the current authorized caic task.
type TaskContext struct {
	ID  string `json:"id"`
	URI string `json:"uri"`
}

// ConversationModel is caic's public model context. Its MCP instructions own interpretation.
type ConversationModel struct {
	CurrentTask *TaskContext `json:"currentTask"`
}
