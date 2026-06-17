// Logging utility for masking credential strings.

package auth

import (
	"log/slog"
	"strings"
)

// MaskedToken is a credential string that logs as "xxx...1234" (last 4 chars
// visible, remainder replaced with "x"). Implements [slog.LogValuer].
type MaskedToken string

// LogValue implements [slog.LogValuer].
func (m MaskedToken) LogValue() slog.Value {
	s := string(m)
	if s == "" {
		return slog.StringValue("")
	}
	if len(s) <= 4 {
		return slog.StringValue(s)
	}
	return slog.StringValue(strings.Repeat("x", len(s)-4) + s[len(s)-4:])
}
