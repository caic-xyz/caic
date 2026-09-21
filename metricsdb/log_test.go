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
	log, err := NewLog(slog.New(slog.DiscardHandler), dir, testResource())
	if err != nil {
		t.Fatalf("NewLog: %v", err)
	}
	if !now.IsZero() {
		log.now = func() time.Time { return now }
	}
	return log
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

	t.Run("appends one json line per observation", func(t *testing.T) {
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

		lines := readLines(t, filepath.Join(dir, "2026-09-21.jsonl"))
		if len(lines) != 2 {
			t.Fatalf("lines = %d, want 2", len(lines))
		}
		wantLine := `{"time":"2026-09-21T10:30:00Z","name":"container.launch","outcome":"ok",` +
			`"kind":"histogram","unit":"s","amount":1.5,"attrs":{"container.runtime":"podman"},` +
			`"resource":{"service":"caic","version":"1.2.3","host":"host-1"}}`
		if lines[0] != wantLine {
			t.Fatalf("first line = %s, want %s", lines[0], wantLine)
		}
		var got record
		if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
			t.Fatalf("decode first line: %v", err)
		}
		want := record{
			Time:     day,
			Name:     "container.launch",
			Outcome:  metrics.OutcomeOK,
			Kind:     metrics.KindHistogram,
			Unit:     metrics.UnitSeconds,
			Amount:   metrics.Duration(1500 * time.Millisecond).Amount,
			Attrs:    map[string]string{"container.runtime": "podman"},
			Resource: resource{Service: "caic", Version: "1.2.3", Host: "host-1"},
		}
		if !got.Time.Equal(want.Time) {
			t.Fatalf("time = %v, want %v", got.Time, want.Time)
		}
		got.Time, want.Time = time.Time{}, time.Time{}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("record = %+v, want %+v", got, want)
		}

		var second record
		if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
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

		lines := readLines(t, filepath.Join(dir, "2026-09-21.jsonl"))
		if len(lines) != 2 {
			t.Fatalf("lines = %d, want 2", len(lines))
		}
		var size record
		if err := json.Unmarshal([]byte(lines[0]), &size); err != nil {
			t.Fatalf("decode size: %v", err)
		}
		if size.Kind != metrics.KindHistogram || size.Unit != metrics.UnitBytes || size.Amount != 4096 {
			t.Fatalf("size record = %+v, want a 4096 byte histogram", size)
		}
		var gauge record
		if err := json.Unmarshal([]byte(lines[1]), &gauge); err != nil {
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

		if _, err := os.Stat(filepath.Join(dir, "2026-09-21.jsonl")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("finished day is still plain: %v", err)
		}
		body := readZstd(t, filepath.Join(dir, "2026-09-21.jsonl.zstd"))
		if got := strings.Count(body, "\n"); got != 1 {
			t.Fatalf("compressed day holds %d lines, want 1: %q", got, body)
		}
		if _, err := os.Stat(filepath.Join(dir, "2026-09-22.jsonl")); err != nil {
			t.Fatalf("current day is missing: %v", err)
		}
	})

	t.Run("compresses days left by an earlier run", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		stale := filepath.Join(dir, "2000-01-01.jsonl")
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
		if got := readZstd(t, filepath.Join(dir, "2000-01-01.jsonl.zstd")); got != "{\"name\":\"repo.diff\"}\n" {
			t.Fatalf("compressed day = %q", got)
		}
	})

	t.Run("appends to the current day after a restart", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, time.Now().UTC().Format(dayLayout)+jsonlSuffix)
		if err := os.WriteFile(path, []byte("{\"name\":\"repo.diff\"}\n"), fileMode); err != nil {
			t.Fatalf("seed current day: %v", err)
		}
		log := newTestLog(t, dir, time.Time{})
		log.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond))
		if err := log.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := len(readLines(t, path)); got != 2 {
			t.Fatalf("current day holds %d lines, want 2", got)
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

		entries, err := os.ReadDir(dir)
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
		if got := len(readLines(t, filepath.Join(dir, "2026-09-21.jsonl"))); got != writers*each {
			t.Fatalf("lines = %d, want %d", got, writers*each)
		}
	})
}
