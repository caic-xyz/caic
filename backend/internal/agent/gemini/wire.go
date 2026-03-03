// Record probe type used by Record.UnmarshalJSON.
package gemini

// typeProbe extracts the type discriminator from a Gemini CLI record.
type typeProbe struct {
	Type string `json:"type"`
}
