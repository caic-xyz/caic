// Tests for the durable per-day metrics log.

package metricsdb

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/metrics"
)

func testResource() metrics.Resource {
	return metrics.Resource{ServiceName: "caic", ServiceVersion: "1.2.3", Host: "host-1"}
}

// newTestLog opens a log in dir and pins its clock when now is set.
func newTestLog(t *testing.T, dir string, now time.Time) *Log {
	return newTestLogResource(t, dir, now, testResource())
}

func newTestLogResource(t *testing.T, dir string, now time.Time, res metrics.Resource) *Log {
	log, err := NewLog(slog.New(slog.DiscardHandler), dir, res)
	if err != nil {
		t.Fatalf("NewLog: %v", err)
	}
	if !now.IsZero() {
		log.now = func() time.Time { return now }
	}
	return log
}

func dayPath(log *Log, day string) string {
	return filepath.Join(log.dir, day+jsonlSuffix)
}

func readLines(t *testing.T, path string) []string {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a test path under t.TempDir.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := strings.TrimSuffix(string(raw), "\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}

func readZstd(t *testing.T, path string) string {
	f, err := os.Open(path) //nolint:gosec // G304: a test path under t.TempDir.
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatalf("zstd reader for %s: %v", path, err)
	}
	defer dec.Close()
	body, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decompress %s: %v", path, err)
	}
	return string(body)
}

func TestNewLog(t *testing.T) {
	t.Parallel()

	t.Run("requires a logger", func(t *testing.T) {
		t.Parallel()
		if _, err := NewLog(nil, t.TempDir(), testResource()); err == nil {
			t.Fatal("NewLog accepted a nil logger")
		}
	})

	t.Run("requires a directory", func(t *testing.T) {
		t.Parallel()
		if _, err := NewLog(slog.New(slog.DiscardHandler), "", testResource()); err == nil {
			t.Fatal("NewLog accepted an empty directory")
		}
	})

	t.Run("requires a service name", func(t *testing.T) {
		t.Parallel()
		if _, err := NewLog(slog.New(slog.DiscardHandler), t.TempDir(), metrics.Resource{}); err == nil {
			t.Fatal("NewLog accepted an unnamed service")
		}
	})
}

func TestLog(t *testing.T) {
	t.Parallel()

	t.Run("writes one metadata header followed by each observation", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		day := time.Date(2026, 9, 21, 10, 30, 0, 0, time.UTC)
		log := newTestLog(t, dir, day)
		log.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(1500*time.Millisecond),
			metrics.Attr{Key: "container.runtime", Value: "podman"})
		log.Record(t.Context(), "repo.diff", metrics.OutcomeError, metrics.Duration(30*time.Millisecond))
		if err := log.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		lines := readLines(t, dayPath(log, "2026-09-21"))
		if len(lines) != 3 {
			t.Fatalf("lines = %d, want 3", len(lines))
		}
		wantHeader := `{"type":"metrics","version":2,"resource":{"service":"caic","version":"1.2.3","host":"host-1"}}`
		if lines[0] != wantHeader {
			t.Fatalf("header = %s, want %s", lines[0], wantHeader)
		}
		var header fileHeader
		if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
			t.Fatalf("decode header: %v", err)
		}
		if header != log.header() {
			t.Fatalf("header = %+v, want %+v", header, log.header())
		}
		wantLine := `{"time":"2026-09-21T10:30:00Z","name":"container.launch","outcome":"ok",` +
			`"kind":"histogram","unit":"s","amount":1.5,"attrs":{"container.runtime":"podman"}}`
		if lines[1] != wantLine {
			t.Fatalf("first observation = %s, want %s", lines[1], wantLine)
		}
		var got record
		if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
			t.Fatalf("decode first observation: %v", err)
		}
		want := record{
			Time:    day,
			Name:    "container.launch",
			Outcome: metrics.OutcomeOK,
			Kind:    metrics.KindHistogram,
			Unit:    metrics.UnitSeconds,
			Amount:  metrics.Duration(1500 * time.Millisecond).Amount,
			Attrs:   map[string]string{"container.runtime": "podman"},
		}
		if !got.Time.Equal(want.Time) {
			t.Fatalf("time = %v, want %v", got.Time, want.Time)
		}
		got.Time, want.Time = time.Time{}, time.Time{}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("record = %+v, want %+v", got, want)
		}

		var second record
		if err := json.Unmarshal([]byte(lines[2]), &second); err != nil {
			t.Fatalf("decode second line: %v", err)
		}
		if second.Name != "repo.diff" || second.Outcome != metrics.OutcomeError || second.Attrs != nil {
			t.Fatalf("second record = %+v, want an attribute-free repo.diff error", second)
		}
	})

	t.Run("records sizes and gauges", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		day := time.Date(2026, 9, 21, 10, 30, 0, 0, time.UTC)
		log := newTestLog(t, dir, day)
		log.Record(t.Context(), "container.disk_size", metrics.OutcomeOK, metrics.Bytes(4096))
		log.Record(t.Context(), "container.instances", metrics.OutcomeOK, metrics.Gauge(3, metrics.UnitCount))
		if err := log.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		lines := readLines(t, dayPath(log, "2026-09-21"))
		if len(lines) != 3 {
			t.Fatalf("lines = %d, want 3", len(lines))
		}
		var size record
		if err := json.Unmarshal([]byte(lines[1]), &size); err != nil {
			t.Fatalf("decode size: %v", err)
		}
		if size.Kind != metrics.KindHistogram || size.Unit != metrics.UnitBytes || size.Amount != 4096 {
			t.Fatalf("size record = %+v, want a 4096 byte histogram", size)
		}
		var gauge record
		if err := json.Unmarshal([]byte(lines[2]), &gauge); err != nil {
			t.Fatalf("decode gauge: %v", err)
		}
		if gauge.Kind != metrics.KindGauge || gauge.Unit != metrics.UnitCount || gauge.Amount != 3 {
			t.Fatalf("gauge record = %+v, want a gauge at 3", gauge)
		}
	})

	t.Run("compresses the finished day on rollover", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		day := time.Date(2026, 9, 21, 23, 59, 0, 0, time.UTC)
		log := newTestLog(t, dir, day)
		log.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond))

		log.now = func() time.Time { return day.Add(2 * time.Minute) }
		log.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond))
		if err := log.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		if _, err := os.Stat(dayPath(log, "2026-09-21")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("finished day is still plain: %v", err)
		}
		body := readZstd(t, dayPath(log, "2026-09-21")+".zstd")
		if got := strings.Count(body, "\n"); got != 2 {
			t.Fatalf("compressed day holds %d lines, want 2: %q", got, body)
		}
		if _, err := os.Stat(dayPath(log, "2026-09-22")); err != nil {
			t.Fatalf("current day is missing: %v", err)
		}
	})

	t.Run("compresses days left by an earlier run", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		day := time.Now().UTC().AddDate(0, 0, -10).Format(dayLayout)
		resourceDir := filepath.Join(dir, (resource{Service: "caic", Version: "1.2.3", Host: "host-1"}).id())
		if err := os.MkdirAll(resourceDir, dirMode); err != nil {
			t.Fatalf("create resource directory: %v", err)
		}
		stale := filepath.Join(resourceDir, day+jsonlSuffix)
		if err := os.WriteFile(stale, []byte("{\"name\":\"repo.diff\"}\n"), fileMode); err != nil {
			t.Fatalf("seed stale day: %v", err)
		}
		if err := os.WriteFile(stale+tempSuffix, []byte("partial"), fileMode); err != nil {
			t.Fatalf("seed temporary file: %v", err)
		}
		log := newTestLog(t, dir, time.Time{})
		if err := log.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stale day is still plain: %v", err)
		}
		if _, err := os.Stat(stale + tempSuffix); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("temporary file survived: %v", err)
		}
		if got := readZstd(t, filepath.Join(resourceDir, day+zstdSuffix)); got != "{\"name\":\"repo.diff\"}\n" {
			t.Fatalf("compressed day = %q", got)
		}
	})

	t.Run("discards days past the retention window", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		expired := time.Now().UTC().AddDate(0, 0, -100).Format(dayLayout)
		recent := time.Now().UTC().AddDate(0, 0, -10).Format(dayLayout)
		resourceDir := filepath.Join(dir, (resource{Service: "caic", Version: "1.2.3", Host: "host-1"}).id())
		if err := os.MkdirAll(resourceDir, dirMode); err != nil {
			t.Fatalf("create resource directory: %v", err)
		}
		for _, name := range []string{expired + jsonlSuffix, expired + zstdSuffix, recent + jsonlSuffix} {
			if err := os.WriteFile(filepath.Join(resourceDir, name), []byte("{\"name\":\"repo.diff\"}\n"), fileMode); err != nil {
				t.Fatalf("seed %s: %v", name, err)
			}
		}

		log := newTestLog(t, dir, time.Time{})
		if err := log.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		entries, err := os.ReadDir(log.dir)
		if err != nil {
			t.Fatalf("read directory: %v", err)
		}
		got := make([]string, 0, len(entries))
		for _, entry := range entries {
			got = append(got, entry.Name())
		}
		want := []string{recent + zstdSuffix}
		if !slices.Equal(got, want) {
			t.Fatalf("directory = %v, want %v: an aged-out day is discarded, a recent one is only compressed", got, want)
		}
	})

	t.Run("discards days past the retention window on rollover", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		now := time.Now().UTC()
		expired := now.AddDate(0, 0, -100).Format(dayLayout)
		resourceDir := filepath.Join(dir, (resource{Service: "caic", Version: "1.2.3", Host: "host-1"}).id())
		if err := os.MkdirAll(resourceDir, dirMode); err != nil {
			t.Fatalf("create resource directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(resourceDir, expired+zstdSuffix), []byte("seeded"), fileMode); err != nil {
			t.Fatalf("seed expired day: %v", err)
		}
		// A long-running process only sweeps at rollover, never at startup.
		log := newTestLog(t, dir, now)
		log.now = func() time.Time { return now.Add(24 * time.Hour) }
		log.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond))
		if err := log.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := os.Stat(filepath.Join(log.dir, expired+zstdSuffix)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("expired day survived the rollover: %v", err)
		}
	})

	t.Run("appends to the current day after a restart", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		first := newTestLog(t, dir, time.Time{})
		first.Record(t.Context(), "repo.diff", metrics.OutcomeOK, metrics.Duration(time.Millisecond))
		if err := first.Close(); err != nil {
			t.Fatalf("close first log: %v", err)
		}
		second := newTestLog(t, dir, time.Time{})
		second.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond))
		if err := second.Close(); err != nil {
			t.Fatalf("close second log: %v", err)
		}
		if got := len(readLines(t, dayPath(second, time.Now().UTC().Format(dayLayout)))); got != 3 {
			t.Fatalf("current day holds %d lines, want one header and two observations", got)
		}
	})

	t.Run("restores retained observations after a restart", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		history := time.Now().UTC().AddDate(0, 0, -1).Truncate(time.Second)
		first := newTestLog(t, dir, history)
		first.Record(t.Context(), "repo.diff", metrics.OutcomeOK, metrics.Duration(time.Millisecond),
			metrics.Attr{Key: "forge.name", Value: "github"})
		if err := first.Close(); err != nil {
			t.Fatalf("close first log: %v", err)
		}

		second := newTestLog(t, dir, time.Time{})
		t.Cleanup(func() { _ = second.Close() })
		store := metrics.NewStore(testResource())
		if err := second.Restore(t.Context(), store); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, metrics.Duration(2*time.Millisecond),
			metrics.Attr{Key: "forge.name", Value: "github"})

		if !store.Since.Equal(history) {
			t.Fatalf("Since = %s, want %s", store.Since, history)
		}
		got := store.Snapshot()
		if len(got) != 1 {
			t.Fatalf("snapshot = %#v, want one series", got)
		}
		if got[0].Count != 2 || got[0].Sum != 0.003 || got[0].Last != 0.002 {
			t.Fatalf("series = %+v, want two restored and live observations", got[0])
		}
		if forgeName := got[0].Attrs["forge.name"]; forgeName != "github" {
			t.Fatalf("attributes = %#v, want forge.name github", got[0].Attrs)
		}
	})

	t.Run("rejects a mismatched metadata header", func(t *testing.T) {
		t.Parallel()
		log := newTestLog(t, t.TempDir(), time.Time{})
		header, err := json.Marshal(fileHeader{
			Type:     "metrics",
			Version:  formatVersion,
			Resource: resource{Service: "caic-voice-gateway"},
		})
		if err != nil {
			t.Fatalf("marshal header: %v", err)
		}
		path := dayPath(log, time.Now().UTC().Format(dayLayout))
		if err := os.WriteFile(path, append(header, '\n'), fileMode); err != nil {
			t.Fatalf("write mismatched header: %v", err)
		}

		store := metrics.NewStore(testResource())
		err = log.Restore(t.Context(), store)
		if err == nil || !strings.Contains(err.Error(), "unexpected metric resource or format") {
			t.Fatalf("Restore error = %v, want resource or format mismatch", err)
		}
		if got := store.Snapshot(); len(got) != 0 {
			t.Fatalf("snapshot = %#v, want no observations", got)
		}
	})

	t.Run("separates services into distinct resource directories", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		foreign := newTestLogResource(t, dir, time.Now().UTC(), metrics.Resource{ServiceName: "caic-voice-gateway"})
		foreign.Record(t.Context(), "voice.session", metrics.OutcomeOK, metrics.Count(1))
		if err := foreign.Close(); err != nil {
			t.Fatalf("close foreign log: %v", err)
		}

		log := newTestLog(t, dir, time.Time{})
		t.Cleanup(func() { _ = log.Close() })
		store := metrics.NewStore(testResource())
		if err := log.Restore(t.Context(), store); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if foreign.dir == log.dir {
			t.Fatal("foreign and caic resources share a directory")
		}
		if got := store.Snapshot(); len(got) != 0 {
			t.Fatalf("snapshot = %#v, want no observations from another resource", got)
		}
	})

	t.Run("drops observations after close", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		log := newTestLog(t, dir, time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC))
		if err := log.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		log.now = func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
		log.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond))

		entries, err := os.ReadDir(log.dir)
		if err != nil {
			t.Fatalf("read directory: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("entries = %v, want an empty directory", entries)
		}
	})

	t.Run("records concurrently", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		log := newTestLog(t, dir, time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC))
		const writers, each = 8, 25
		var wg sync.WaitGroup
		for range writers {
			wg.Go(func() {
				for range each {
					log.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond))
				}
			})
		}
		wg.Wait()
		if err := log.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := len(readLines(t, dayPath(log, "2026-09-21"))); got != writers*each+1 {
			t.Fatalf("lines = %d, want one header and %d observations", got, writers*each)
		}
	})
}
