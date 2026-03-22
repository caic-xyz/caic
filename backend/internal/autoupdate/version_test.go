package autoupdate

import "testing"

func TestVersion(t *testing.T) {
	// In test context, build info is "(devel)" so Version is "devel-<hash>"
	// or empty (no VCS info).
	v := Version
	if v != "" && v != "devel-" {
		// Either empty or starts with "devel-".
		if len(v) < 7 || v[:6] != "devel-" {
			t.Errorf("unexpected dev version: %q", v)
		}
	}
}
