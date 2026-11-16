// Tests harness version parsing against each CLI's --version output.

package agent

import "testing"

func TestParseVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		out  string
		want string
	}{
		{"2.1.325 (Claude Code)\n", "2.1.325"},
		{"codex-cli 0.169.0\n", "0.169.0"},
		{"1.5.2\n", "1.5.2"},
		{"0.2.2-rc.1\n", "0.2.2-rc.1"},
		{"\n1.5.9\nnotice: update available\n", "1.5.9"},
	} {
		got, err := parseVersion(tc.out)
		if err != nil || got != tc.want {
			t.Errorf("parseVersion(%q) = %q, %v; want %q", tc.out, got, err, tc.want)
		}
	}
	for _, out := range []string{"", "pi: command not found\n"} {
		if got, err := parseVersion(out); err == nil {
			t.Errorf("parseVersion(%q) = %q, want error", out, got)
		}
	}
}
