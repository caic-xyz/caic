// Tests for the released Streamable HTTP compatibility handler.

package mcp

import "testing"

func TestNegotiateCompatVersion(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"2025-11-25": "2025-11-25",
		"2025-06-18": "2025-06-18",
		"2024-11-05": "2024-11-05",
		"2099-01-01": compatDefaultProtocolVersion,
		"":           compatDefaultProtocolVersion,
		"2026-07-28": compatDefaultProtocolVersion, // caic's native revision is not a released version
	}
	for requested, want := range cases {
		if got := negotiateCompatVersion(requested); got != want {
			t.Errorf("negotiateCompatVersion(%q) = %q, want %q", requested, got, want)
		}
	}
}
