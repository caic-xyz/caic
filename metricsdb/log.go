// Package metricsdb manages durable, restart-restored operation metrics.
//
// Each resource owns a SHA-256-named directory of UTC daily JSONL files. A
// versioned metadata header is the first line in every file, followed by its
// metric observations. Rolling past midnight closes the finished day and
// rewrites it as YYYY-MM-DD.jsonl.zstd, and days left behind by a restart are
// compressed on the next start. Days older than 90 days are discarded, at
// startup and on each rollover. At startup, the caic server restores retained
// observations into its bounded in-memory aggregate. The log can also be
// decompressed with zstd and read with jq, or shipped to an OpenTelemetry
// backend.
//
// A failed write never fails the operation being measured; it is logged and
// dropped, because metrics must not take down the work they describe.
package metricsdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/metrics"
)

const (
	dirMode       = 0o700
	fileMode      = 0o600
	dayLayout     = "2006-01-02"
	jsonlSuffix   = ".jsonl"
	zstdSuffix    = ".jsonl.zstd"
	tempSuffix    = ".tmp"
	retentionDays = 90
	formatVersion = 2
)

// record is one measurement as written to the log.
type record struct {
	Time    time.Time         `json:"time"`
	Name    string            `json:"name"`
	Outcome metrics.Outcome   `json:"outcome"`
	Kind    metrics.Kind      `json:"kind"`
	Unit    metrics.Unit      `json:"unit"`
	Amount  float64           `json:"amount"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// resource names the process that owns a directory of metric files.
type resource struct {
	Service string `json:"service"`
	Version string `json:"version,omitempty"`
	Host    string `json:"host,omitempty"`
}

func (r resource) id() string {
	sum := sha256.Sum256([]byte(r.Service + "\x00" + r.Version + "\x00" + r.Host))
	return hex.EncodeToString(sum[:])
}

// fileHeader identifies the resource and format of one daily metric file.
type fileHeader struct {
	Type     string   `json:"type"`
	Version  int      `json:"version"`
	Resource resource `json:"resource"`
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

// NewLog creates the log directory, compresses any day left unfinished by an
// earlier run, and discards days past the retention window. A directory that
// cannot be swept is reported as a warning rather than an error: losing history
// is not worth refusing to start.
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
	r := resource{Service: res.ServiceName, Version: res.ServiceVersion, Host: res.Host}
	if err := os.MkdirAll(filepath.Join(dir, r.id()), dirMode); err != nil {
		return nil, fmt.Errorf("create metrics directory: %w", err)
	}
	l := &Log{
		dir:      filepath.Join(dir, r.id()),
		resource: r,
		log:      log.With("cmp", "metricsdb"),
		now:      time.Now,
	}
	if err := l.sweep(); err != nil {
		l.log.Warn("sweep metrics directory", "err", err)
	}
	return l, nil
}

// Record appends one measurement. It implements metrics.Recorder.
func (l *Log) Record(ctx context.Context, name string, outcome metrics.Outcome, m metrics.Measurement, attrs ...metrics.Attr) {
	now := l.now()
	line, err := json.Marshal(record{
		Time:    now,
		Name:    name,
		Outcome: outcome,
		Kind:    m.Kind,
		Unit:    m.Unit,
		Amount:  m.Amount,
		Attrs:   metrics.DedupAttrs(attrs),
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

// Restore loads retained observations into dst in timestamp order.
//
// A damaged file does not prevent other days from being restored. Callers get
// the combined errors and can surface them without losing valid history.
func (l *Log) Restore(ctx context.Context, dst *metrics.Store) error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("read metrics directory: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, ok := dayOf(entry.Name()); ok {
			paths = append(paths, filepath.Join(l.dir, entry.Name()))
		}
	}
	slices.Sort(paths)
	var errs []error
	for _, path := range paths {
		if err := l.restoreFile(ctx, dst, path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (l *Log) restoreFile(ctx context.Context, dst *metrics.Store, path string) (err error) {
	f, err := os.Open(path) //nolint:gosec // G304: a metrics log path under the configured directory.
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	var in io.Reader = f
	var dec *zstd.Decoder
	if strings.HasSuffix(path, zstdSuffix) {
		dec, err = zstd.NewReader(f)
		if err != nil {
			return err
		}
		defer dec.Close()
		in = dec
	}
	jsonDec := json.NewDecoder(in)
	var h fileHeader
	if err := jsonDec.Decode(&h); err != nil {
		return fmt.Errorf("decode header %s: %w", filepath.Base(path), err)
	}
	if h != l.header() {
		return fmt.Errorf("decode header %s: unexpected metric resource or format", filepath.Base(path))
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var r record
		if err := jsonDec.Decode(&r); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
		}
		if r.Time.IsZero() {
			return fmt.Errorf("decode %s: metric observation has no timestamp", filepath.Base(path))
		}
		attrs := make([]metrics.Attr, 0, len(r.Attrs))
		for key, value := range r.Attrs {
			attrs = append(attrs, metrics.Attr{Key: key, Value: value})
		}
		slices.SortFunc(attrs, func(a, b metrics.Attr) int { return strings.Compare(a.Key, b.Key) })
		dst.Restore(r.Time, r.Name, r.Outcome, metrics.Measurement{Kind: r.Kind, Unit: r.Unit, Amount: r.Amount}, attrs...)
	}
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
		if err := l.pruneExpired(); err != nil {
			l.log.WarnContext(ctx, "discard expired metrics days", "err", err)
		}
	}
	path := filepath.Join(l.dir, day+jsonlSuffix)
	info, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	empty := errors.Is(err, fs.ErrNotExist)
	if !empty {
		empty = info.Size() == 0
	}
	if !empty {
		if err := l.validateHeader(path); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode) //nolint:gosec // G304: the configured metrics directory plus a UTC date.
	if err != nil {
		return err
	}
	if empty {
		line, err := json.Marshal(l.header())
		if err != nil {
			return errors.Join(err, f.Close())
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	l.day = day
	l.file = f
	return nil
}

func (l *Log) header() fileHeader {
	return fileHeader{Type: "metrics", Version: formatVersion, Resource: l.resource}
}

func (l *Log) validateHeader(path string) (err error) {
	f, err := os.Open(path) //nolint:gosec // G304: a metrics log path under the configured directory.
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	var h fileHeader
	if err := json.NewDecoder(f).Decode(&h); err != nil {
		return err
	}
	if h != l.header() {
		return errors.New("unexpected metric resource or format")
	}
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

// sweep compresses days left behind by an earlier run and discards the days that
// have aged past the retention window.
func (l *Log) sweep() error {
	return errors.Join(l.compressLeftovers(), l.pruneExpired())
}

// compressLeftovers compresses every finished day and removes leftover temporary
// files. The current day stays plain so a later run can append to it.
func (l *Log) compressLeftovers() error {
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

// pruneExpired discards logged days that have aged out of the retention window.
// Only names this package writes are considered, and each removal is reported so
// a discarded day is never silent.
func (l *Log) pruneExpired() error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("read metrics directory: %w", err)
	}
	cutoff := l.now().UTC().AddDate(0, 0, -retentionDays)
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		day, ok := dayOf(entry.Name())
		if !ok || !day.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(l.dir, entry.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		l.log.Info("discarded expired metrics day", "day", day.Format(dayLayout), "file", entry.Name())
	}
	return errors.Join(errs...)
}

// dayOf returns the day a logged file covers, or false when the name is not one
// this package writes.
func dayOf(name string) (time.Time, bool) {
	stem, found := strings.CutSuffix(name, zstdSuffix)
	if !found {
		stem, found = strings.CutSuffix(name, jsonlSuffix)
		if !found {
			return time.Time{}, false
		}
	}
	day, err := time.Parse(dayLayout, stem)
	if err != nil {
		return time.Time{}, false
	}
	return day, true
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
