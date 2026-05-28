// Tests for the automatic update download and installation logic.

package autoupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIsNewer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		latest, current string
		want            bool
	}{
		{"1.2.3", "1.2.3", false},
		{"1.2.4", "1.2.3", true},
		{"1.3.0", "1.2.9", true},
		{"2.0.0", "1.9.9", true},
		{"1.2.3", "1.2.4", false},
		{"1.2.3", "2.0.0", false},
		// Pre-release suffix.
		{"1.2.4-rc1", "1.2.3", true},
		// Non-semver falls back to string comparison.
		{"abc", "abc", false},
		{"abc", "def", true},
	} {
		t.Run(tc.latest+"_vs_"+tc.current, func(t *testing.T) {
			t.Parallel()
			got := IsNewer(tc.latest, tc.current)
			if got != tc.want {
				t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
			}
		})
	}
}

func TestParseSemver(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input               string
		major, minor, patch int
		ok                  bool
	}{
		{"1.2.3", 1, 2, 3, true},
		{"0.0.1", 0, 0, 1, true},
		{"10.20.30", 10, 20, 30, true},
		{"1.2.3-rc1", 1, 2, 3, true},
		{"1.2", 0, 0, 0, false},
		{"abc", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			maj, mnr, pat, ok := parseSemver(tc.input)
			if ok != tc.ok || maj != tc.major || mnr != tc.minor || pat != tc.patch {
				t.Errorf("parseSemver(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
					tc.input, maj, mnr, pat, ok, tc.major, tc.minor, tc.patch, tc.ok)
			}
		})
	}
}

func TestParseSchedule(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		s, err := ParseSchedule("50 4 * * *")
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Minute) != 1 || s.Minute[0] != 50 {
			t.Errorf("Minute = %v, want [50]", s.Minute)
		}
		if len(s.Hour) != 1 || s.Hour[0] != 4 {
			t.Errorf("Hour = %v, want [4]", s.Hour)
		}
		if s.DayOfMonth != nil || s.Month != nil || s.DayOfWeek != nil {
			t.Error("wildcards should be nil")
		}
	})

	t.Run("comma list", func(t *testing.T) {
		t.Parallel()
		s, err := ParseSchedule("0,30 * * * *")
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Minute) != 2 || s.Minute[0] != 0 || s.Minute[1] != 30 {
			t.Errorf("Minute = %v, want [0 30]", s.Minute)
		}
	})

	t.Run("wrong field count", func(t *testing.T) {
		t.Parallel()
		_, err := ParseSchedule("50 4 *")
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("out of range", func(t *testing.T) {
		t.Parallel()
		_, err := ParseSchedule("60 4 * * *")
		if err == nil {
			t.Error("expected error for minute=60")
		}
	})
}

func TestScheduleNext(t *testing.T) {
	t.Parallel()
	s, err := ParseSchedule("50 4 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-04-09 03:00 → next should be 2026-04-09 04:50.
	now := time.Date(2026, 4, 9, 3, 0, 0, 0, time.UTC)
	next := s.Next(now)
	want := time.Date(2026, 4, 9, 4, 50, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("Next(%v) = %v, want %v", now, next, want)
	}

	// 2026-04-09 05:00 → next should be 2026-04-10 04:50.
	now = time.Date(2026, 4, 9, 5, 0, 0, 0, time.UTC)
	next = s.Next(now)
	want = time.Date(2026, 4, 10, 4, 50, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("Next(%v) = %v, want %v", now, next, want)
	}
}

func TestPlatformStrings(t *testing.T) {
	t.Parallel()
	osStr, archStr := platformStrings()
	if osStr == "" || archStr == "" {
		t.Fatalf("platformStrings() = (%q, %q), both should be non-empty", osStr, archStr)
	}
	// Verify that archStr matches GoReleaser's default archive name convention,
	// which uses Go architecture names (amd64, arm64), not Debian-style (x86_64).
	// Test against known release assets.
	knownAssets := []string{
		"caic_0.8.1_linux_amd64.tar.gz",
		"caic_0.8.1_linux_arm64.tar.gz",
		"caic_0.8.1_darwin_all.tar.gz",
		"caic_0.8.1_windows_amd64.zip",
	}
	matched := false
	for _, a := range knownAssets {
		if strings.Contains(strings.ToLower(a), strings.ToLower(osStr)) &&
			strings.Contains(strings.ToLower(a), strings.ToLower(archStr)) {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("platformStrings() = (%q, %q) does not match any known release asset: %v", osStr, archStr, knownAssets)
	}
}

func TestExtractTarGzToFile(t *testing.T) {
	t.Parallel()
	content := []byte("binary content here")
	data := makeTarGz(t, "caic", content)

	t.Run("found", func(t *testing.T) {
		t.Parallel()
		tmp, err := os.CreateTemp(t.TempDir(), "test-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := extractTarGzToFile(bytes.NewReader(data), "caic", tmp); err != nil {
			t.Fatal(err)
		}
		_ = tmp.Close()
		got, err := os.ReadFile(tmp.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("got %q, want %q", got, content)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		t.Parallel()
		tmp, err := os.CreateTemp(t.TempDir(), "test-*")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tmp.Close() }()
		if err := extractTarGzToFile(bytes.NewReader(data), "missing", tmp); err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}

// makeTarGz creates a tar.gz archive containing a single file.
func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Size: int64(len(content)),
		Mode: 0o755,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
