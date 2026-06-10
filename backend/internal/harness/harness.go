// Package harness defines coding agent harness identifiers shared across backend domains.
package harness

// Name identifies a coding agent harness.
type Name string

// Supported agent harnesses.
const (
	Claude   Name = "claude"
	Codex    Name = "codex"
	Gemini   Name = "gemini"
	Kilo     Name = "kilo"
	OpenCode Name = "opencode"
	Pi       Name = "pi"
)

// RequiresResumeSessionID reports whether h needs a persisted session ID to
// reconnect to an existing stateful agent session.
func RequiresResumeSessionID(h Name) bool {
	switch h {
	case Codex, OpenCode:
		return true
	default:
		return false
	}
}
