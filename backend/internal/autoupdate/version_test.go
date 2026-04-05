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

func TestFormatVersion(t *testing.T) {
	t.Run("tagged_clean", func(t *testing.T) {
		got := formatVersion("v1.2.3", "abc12345", false)
		if got != "1.2.3" {
			t.Errorf("got %q, want %q", got, "1.2.3")
		}
	})
	t.Run("tagged_dirty", func(t *testing.T) {
		got := formatVersion("v1.2.3", "abc12345", true)
		if got != "1.2.3+dirty" {
			t.Errorf("got %q, want %q", got, "1.2.3+dirty")
		}
	})
	t.Run("pseudo_version_already_dirty", func(t *testing.T) {
		// Go pseudo-version with +dirty suffix — don't double-append.
		got := formatVersion("v0.5.7-0.20260405184006-d7f6fcd91f7a+dirty", "d7f6fcd91f7a", true)
		if got != "0.5.7-0.20260405184006-d7f6fcd91f7a+dirty" {
			t.Errorf("got %q, want %q", got, "0.5.7-0.20260405184006-d7f6fcd91f7a+dirty")
		}
	})
	t.Run("pseudo_version_clean", func(t *testing.T) {
		got := formatVersion("v0.5.7-0.20260405184006-d7f6fcd91f7a", "d7f6fcd91f7a", false)
		if got != "0.5.7-0.20260405184006-d7f6fcd91f7a" {
			t.Errorf("got %q, want %q", got, "0.5.7-0.20260405184006-d7f6fcd91f7a")
		}
	})
	t.Run("devel_clean", func(t *testing.T) {
		got := formatVersion("(devel)", "abc123456789", false)
		if got != "devel-abc12345" {
			t.Errorf("got %q, want %q", got, "devel-abc12345")
		}
	})
	t.Run("devel_dirty", func(t *testing.T) {
		got := formatVersion("(devel)", "abc123456789", true)
		if got != "devel-abc12345+dirty" {
			t.Errorf("got %q, want %q", got, "devel-abc12345+dirty")
		}
	})
	t.Run("devel_no_revision", func(t *testing.T) {
		got := formatVersion("(devel)", "", false)
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}
