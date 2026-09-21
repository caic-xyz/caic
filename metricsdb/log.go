// Package metricsdb appends operation observations to per-day JSONL files and
// compresses completed days.
//
// Files live in one directory as YYYY-MM-DD.jsonl, named and timestamped in
// UTC, and durations are stored in seconds to the nearest microsecond. Rolling
// past midnight closes the finished day and rewrites it as
// YYYY-MM-DD.jsonl.zstd, and days left behind by a restart are compressed on
// the next start. The log is a durable record rather than a query surface:
// decompress it with zstd and read it with jq, or ship it to an OpenTelemetry
// backend.
//
// A failed write never fails the operation being measured; it is logged and
// dropped, because metrics must not take down the work they describe.
package metricsdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/metrics"
)

const (
	dirMode     = 0o700
	fileMode    = 0o600
	dayLayout   = "2006-01-02"
	jsonlSuffix = ".jsonl"
	zstdSuffix  = ".jsonl.zstd"
	tempSuffix  = ".tmp"
)

// Seconds is a duration in seconds, quantized to the nearest microsecond.
//
// Seconds is the unit OpenTelemetry, Prometheus, and a metrics backend expect,
// so importing the log is a field copy rather than a conversion that can be
// wrong by a factor of a thousand. Microseconds are the precision the measured
// operations carry; nanoseconds would be resolution they do not have.
type Seconds float64

// NewSeconds converts d to seconds, rounded to the nearest microsecond.
func NewSeconds(d time.Duration) Seconds {
	return Seconds(d.Round(time.Microsecond).Microseconds()) / 1e6
}

// Duration returns s as a standard library duration, rounded to the nearest
// microsecond.
func (s Seconds) Duration() time.Duration {
	return time.Duration(math.Round(float64(s)*1e6)) * time.Microsecond
}

// record is one observation as written to the log.
type record struct {
	Time     time.Time         `json:"time"`
	Name     string            `json:"name"`
	Outcome  metrics.Outcome   `json:"outcome"`
	Duration Seconds           `json:"duration"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Resource resource          `json:"resource"`
}

// resource names the process that produced a record so one directory can hold
// more than one service.
type resource struct {
	Service string `json:"service"`
	Version string `json:"version,omitempty"`
	Host    string `json:"host,omitempty"`
}

// Log appends observations to per-day JSONL files. It implements
// metrics.Recorder and is safe for concurrent use.
type Log struct {
	dir      string
	resource resource
	log      *slog.Logger
	now      func() time.Time

	mu     sync.Mutex
	closed bool
	day    string
	file   *os.File
}

// NewLog creates the log directory and compresses any day left unfinishable by
// an earlier run. A day that cannot be compressed is reported as a warning
// rather than an error: losing history is not worth refusing to start.
func NewLog(log *slog.Logger, dir string, res metrics.Resource) (*Log, error) {
	if log == nil {
		return nil, errors.New("logger is required")
	}
	if dir == "" {
		return nil, errors.New("directory is required")
	}
	if res.ServiceName == "" {
		return nil, errors.New("service name is required")
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create metrics directory: %w", err)
	}
	l := &Log{
		dir: dir,
		resource: resource{
			Service: res.ServiceName,
			Version: res.ServiceVersion,
			Host:    res.Host,
		},
		log: log.With("cmp", "metricsdb"),
		now: time.Now,
	}
	if err := l.compressFinished(); err != nil {
		l.log.Warn("compress unfinished metrics days", "err", err)
	}
	return l, nil
}

// Record appends one observation. It implements metrics.Recorder.
func (l *Log) Record(ctx context.Context, name string, outcome metrics.Outcome, d time.Duration, attrs ...metrics.Attr) {
	now := l.now()
	line, err := json.Marshal(record{
		Time:     now,
		Name:     name,
		Outcome:  outcome,
		Duration: NewSeconds(d),
		Attrs:    metrics.DedupAttrs(attrs),
		Resource: l.resource,
	})
	if err != nil {
		l.log.ErrorContext(ctx, "encode metrics observation", "err", err, "name", name)
		return
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rollLocked(ctx, now.UTC().Format(dayLayout)); err != nil {
		l.log.ErrorContext(ctx, "open metrics day", "err", err, "name", name)
		return
	}
	if _, err := l.file.Write(line); err != nil {
		l.log.ErrorContext(ctx, "append metrics observation", "err", err, "name", name)
	}
}

// Close flushes the current file. The unfinished day stays uncompressed so a
// later run can append to it, and Close is safe to call more than once.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return l.detachLocked()
}

// rollLocked opens the file for day, finishing the previous day first.
func (l *Log) rollLocked(ctx context.Context, day string) error {
	if l.closed {
		return errors.New("log is closed")
	}
	if l.file != nil && l.day == day {
		return nil
	}
	if l.file != nil {
		finished := l.day
		if err := l.finishLocked(); err != nil {
			l.log.ErrorContext(ctx, "finish metrics day", "err", err, "day", finished)
		}
	}
	f, err := os.OpenFile(filepath.Join(l.dir, day+jsonlSuffix), os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode) //nolint:gosec // G304: the configured metrics directory plus a UTC date.
	if err != nil {
		return err
	}
	l.day = day
	l.file = f
	return nil
}

// finishLocked closes the current file and compresses its day. A day that
// cannot be flushed is left in place for a later run to compress.
func (l *Log) finishLocked() error {
	day := l.day
	if err := l.detachLocked(); err != nil {
		return err
	}
	return compress(filepath.Join(l.dir, day+jsonlSuffix))
}

// detachLocked flushes the current file and forgets it.
func (l *Log) detachLocked() error {
	f := l.file
	l.file = nil
	l.day = ""
	if f == nil {
		return nil
	}
	return errors.Join(f.Sync(), f.Close())
}

// compressFinished compresses every day except the current one and removes
// leftover temporary files.
func (l *Log) compressFinished() error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("read metrics directory: %w", err)
	}
	today := l.now().UTC().Format(dayLayout)
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(l.dir, name)
		switch {
		case entry.IsDir():
		case strings.HasSuffix(name, tempSuffix):
			if err := os.Remove(path); err != nil {
				errs = append(errs, err)
			}
		case strings.HasSuffix(name, jsonlSuffix) && strings.TrimSuffix(name, jsonlSuffix) != today:
			if err := compress(path); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// compress rewrites the JSONL file at path as <path>.zstd and removes the
// original. The compressed file replaces any earlier copy atomically.
func compress(path string) error {
	temp := path + tempSuffix
	if err := writeCompressed(path, temp); err != nil {
		_ = os.Remove(temp)
		return err
	}
	dst := strings.TrimSuffix(path, jsonlSuffix) + zstdSuffix
	if err := os.Rename(temp, dst); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return os.Remove(path)
}

// writeCompressed writes the JSONL file at src as a zstd stream at dst.
func writeCompressed(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: a metrics log path under the configured directory.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fileMode) //nolint:gosec // G304: a metrics log path under the configured directory.
	if err != nil {
		return err
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		return errors.Join(err, out.Close())
	}
	if _, err := io.Copy(enc, in); err != nil {
		return errors.Join(err, enc.Close(), out.Close())
	}
	if err := enc.Close(); err != nil {
		return errors.Join(err, out.Close())
	}
	if err := out.Sync(); err != nil {
		return errors.Join(err, out.Close())
	}
	return out.Close()
}
