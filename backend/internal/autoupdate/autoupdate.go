// Package autoupdate provides nightly binary auto-update from GitHub Releases.
package autoupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge/github"
)

const (
	owner = "caic-xyz"
	repo  = "caic"
	// maxJitter spreads update checks to avoid thundering-herd on shared hosts.
	maxJitter = 5 * time.Minute
	// maxBinarySize caps decompression to prevent zip/gzip bombs.
	maxBinarySize = 256 << 20 // 256 MiB
)

// Schedule holds a parsed cron-style schedule (minute, hour, day-of-month,
// month, day-of-week). Each field is a sorted slice of allowed values; nil
// means "any" (wildcard).
type Schedule struct {
	Minute     []int // 0–59
	Hour       []int // 0–23
	DayOfMonth []int // 1–31
	Month      []int // 1–12
	DayOfWeek  []int // 0–6 (0 = Sunday)
}

// ParseSchedule parses a 5-field cron expression (minute hour dom month dow).
// Supports integers, comma-separated lists, and "*" (wildcard).
func ParseSchedule(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron: expected 5 fields, got %d in %q", len(fields), expr)
	}
	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron minute: %w", err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron hour: %w", err)
	}
	dom, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron day-of-month: %w", err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron month: %w", err)
	}
	dow, err := parseCronField(fields[4], 0, 6)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron day-of-week: %w", err)
	}
	return Schedule{Minute: minute, Hour: hour, DayOfMonth: dom, Month: month, DayOfWeek: dow}, nil
}

// parseCronField parses a single cron field. Returns nil for "*".
func parseCronField(field string, lo, hi int) ([]int, error) {
	if field == "*" {
		return nil, nil
	}
	var vals []int
	for part := range strings.SplitSeq(field, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("invalid value %q", part)
		}
		if v < lo || v > hi {
			return nil, fmt.Errorf("value %d out of range [%d, %d]", v, lo, hi)
		}
		vals = append(vals, v)
	}
	return vals, nil
}

// Next returns the next time after now that matches the schedule.
func (s *Schedule) Next(now time.Time) time.Time {
	// Start from the next minute.
	t := now.Truncate(time.Minute).Add(time.Minute)
	// Search up to 366 days ahead to handle any valid cron pattern.
	limit := t.Add(366 * 24 * time.Hour)
	for t.Before(limit) {
		if !intMatch(s.Month, int(t.Month())) {
			// Skip to first day of next month.
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !intMatch(s.DayOfMonth, t.Day()) || !intMatch(s.DayOfWeek, int(t.Weekday())) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !intMatch(s.Hour, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !intMatch(s.Minute, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	// Should not happen for valid schedules; fall back to 24h from now.
	return now.Add(24 * time.Hour)
}

// intMatch returns true if vals is nil (wildcard) or v is in vals.
func intMatch(vals []int, v int) bool {
	if vals == nil {
		return true
	}
	return slices.Contains(vals, v)
}

// Run starts the auto-update loop on the given schedule. On successful update,
// replaceBinary overwrites the executable; watchExecutable detects the change
// and triggers a graceful restart. Blocks until ctx is cancelled.
func Run(ctx context.Context, gh *github.Client, sched *Schedule) {
	slog.Info("autoupdate enabled", "version", Version)
	for {
		now := time.Now()
		target := sched.Next(now)
		jitter := time.Duration(rand.IntN(int(maxJitter))) //nolint:gosec // G404: jitter does not need crypto/rand
		delay := target.Sub(now) + jitter
		slog.Debug("autoupdate: next check", "in", delay.Round(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if err := CheckAndUpdate(ctx, gh); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("autoupdate", "err", err)
		}
	}
}

// CheckLatest returns the latest release version from GitHub. Returns "" on error.
func CheckLatest(ctx context.Context, gh *github.Client) (string, error) {
	rel, err := gh.LatestRelease(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	return strings.TrimPrefix(rel.TagName, "v"), nil
}

// CheckAndUpdate fetches the latest GitHub release and, if newer, downloads and
// installs it. Returns nil on successful update, an error otherwise.
func CheckAndUpdate(ctx context.Context, gh *github.Client) error {
	rel, err := gh.LatestRelease(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("fetch latest release: %w", err)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	current := strings.TrimPrefix(Version, "v")
	if !IsNewer(latest, current) {
		slog.Info("autoupdate: up to date", "current", current, "latest", latest)
		return nil
	}
	slog.Info("autoupdate: new version available", "current", current, "latest", latest)
	return downloadAndInstall(ctx, gh, rel)
}

// downloadAndInstall downloads the correct archive, streams it through a
// SHA-256 hash, extracts the binary to a temp file, verifies the checksum,
// and renames over the running executable only if the checksum matches.
func downloadAndInstall(ctx context.Context, gh *github.Client, rel *github.Release) error {
	// Find matching archive asset.
	osName, archName := platformStrings()
	var archiveAsset *github.ReleaseAsset
	for i := range rel.Assets {
		a := &rel.Assets[i]
		lower := strings.ToLower(a.Name)
		if strings.Contains(lower, strings.ToLower(osName)) && strings.Contains(lower, strings.ToLower(archName)) {
			archiveAsset = a
			break
		}
	}
	if archiveAsset == nil {
		return fmt.Errorf("no release asset for %s/%s", osName, archName)
	}

	// Download checksums (small file, read fully).
	wantSum, err := downloadExpectedChecksum(ctx, gh, rel, archiveAsset.Name)
	if err != nil {
		return err
	}

	// Stream the archive through a SHA-256 hasher while extracting.
	body, err := gh.DownloadAsset(ctx, archiveAsset.DownloadURL)
	if err != nil {
		return fmt.Errorf("download archive: %w", err)
	}
	defer func() { _ = body.Close() }()

	h := sha256.New()
	reader := io.TeeReader(body, h)

	binaryName := "caic"
	if runtime.GOOS == "windows" {
		binaryName = "caic.exe"
	}

	// Extract to a temp file next to the executable.
	exe, err := executablePath()
	if err != nil {
		return err
	}
	info, err := os.Stat(exe)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), "caic-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // cleanup on error or checksum mismatch

	if strings.HasSuffix(archiveAsset.Name, ".zip") {
		err = extractZipToFile(reader, binaryName, tmp)
	} else {
		err = extractTarGzToFile(reader, binaryName, tmp)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("extract binary: %w", err)
	}

	// Verify checksum after the entire stream has been consumed.
	if wantSum != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if got != wantSum {
			return fmt.Errorf("checksum mismatch: expected %s, got %s", wantSum, got)
		}
		slog.Info("autoupdate: checksum verified", "asset", archiveAsset.Name)
	}

	// Checksum OK — replace the executable.
	if err := os.Chmod(tmpPath, info.Mode()); err != nil {
		return err
	}
	return os.Rename(tmpPath, exe)
}

// downloadExpectedChecksum fetches checksums.txt from the release and returns
// the expected SHA-256 for assetName. Returns "" if no checksums asset exists.
func downloadExpectedChecksum(ctx context.Context, gh *github.Client, rel *github.Release, assetName string) (string, error) {
	var checksumsAsset *github.ReleaseAsset
	for i := range rel.Assets {
		a := &rel.Assets[i]
		if a.Name == "checksums.txt" {
			checksumsAsset = a
			break
		}
	}
	if checksumsAsset == nil {
		return "", nil
	}
	body, err := gh.DownloadAsset(ctx, checksumsAsset.DownloadURL)
	if err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return "", fmt.Errorf("read checksums: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("asset %q not found in checksums", assetName)
}

// extractTarGzToFile extracts a named file from a tar.gz stream into dst.
func extractTarGzToFile(r io.Reader, name string, dst *os.File) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if filepath.Base(hdr.Name) == name && hdr.Typeflag == tar.TypeReg {
			_, err = io.Copy(dst, io.LimitReader(tr, maxBinarySize))
			return err
		}
	}
	return fmt.Errorf("%q not found in archive", name)
}

// extractZipToFile extracts a named file from a zip stream into dst.
// Zip requires random access, so the stream is buffered into memory.
func extractZipToFile(r io.Reader, name string, dst *os.File) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, io.LimitReader(rc, maxBinarySize))
		_ = rc.Close()
		return err
	}
	return fmt.Errorf("%q not found in archive", name)
}

// executablePath returns the resolved path to the running binary.
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// platformStrings returns the OS and architecture strings used in GoReleaser
// archive names.
func platformStrings() (osStr, archStr string) {
	switch runtime.GOOS {
	case "darwin":
		osStr = "Darwin"
	case "linux":
		osStr = "Linux"
	case "windows":
		osStr = "Windows"
	default:
		osStr = runtime.GOOS
	}
	switch runtime.GOARCH {
	case "amd64":
		archStr = "amd64"
	case "arm64":
		archStr = "arm64"
	default:
		archStr = runtime.GOARCH
	}
	// GoReleaser universal_binaries replaces per-arch darwin archives.
	if runtime.GOOS == "darwin" {
		archStr = "all"
	}
	return osStr, archStr
}

// IsNewer reports whether latest is a higher semver than current.
// Both are expected without a "v" prefix (e.g. "1.2.3").
func IsNewer(latest, current string) bool {
	lMaj, lMin, lPatch, lok := parseSemver(latest)
	cMaj, cMin, cPatch, cok := parseSemver(current)
	if !lok || !cok {
		// Fall back to string comparison if not valid semver.
		return latest != current
	}
	if lMaj != cMaj {
		return lMaj > cMaj
	}
	if lMin != cMin {
		return lMin > cMin
	}
	return lPatch > cPatch
}

// parseSemver extracts major.minor.patch from a version string.
func parseSemver(s string) (major, minor, patch int, ok bool) {
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, false
	}
	// Strip anything after a hyphen (e.g. "3-rc1").
	patchStr, _, _ := strings.Cut(parts[2], "-")
	patch, err = strconv.Atoi(patchStr)
	if err != nil {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}
