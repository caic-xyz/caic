// Tests for the canonical model attribution labels.

package usagedb

import "testing"

func TestModelLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		{"bare model", "gpt-6.1-sol", "", "gpt-6.1-sol"},
		{"provider prefix", "openai-codex/gpt-6.1-sol", "", "gpt-6.1-sol"},
		{"last provider segment", "openai/openai-codex/gpt-6.1-sol", "", "gpt-6.1-sol"},
		{"effort suffix", "claude-sonnet-5-5-medium", "", "claude-sonnet-5-5 (medium)"},
		{"reported effort", "gpt-6.1-sol", "high", "gpt-6.1-sol (high)"},
		{"suffix and reported effort agree", "claude-sonnet-5-5-medium", "medium", "claude-sonnet-5-5 (medium)"},
		{"prefix and suffix", "anthropic/claude-sonnet-5-5-high", "", "claude-sonnet-5-5 (high)"},
		{"effort word inside the name", "claude-sonnet-5-5-lowspeed", "", "claude-sonnet-5-5-lowspeed"},
		{"effort without a model", "", "high", ""},
		{"no attribution", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ModelLabel(tt.model, tt.effort); got != tt.want {
				t.Errorf("ModelLabel(%q, %q) = %q, want %q", tt.model, tt.effort, got, tt.want)
			}
		})
	}
}
