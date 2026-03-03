// Record probe type used by Record.UnmarshalJSON.
package claude

// typeProbe extracts the type discriminator from a Claude Code JSONL record.
type typeProbe struct {
	Type string `json:"type"`
}
