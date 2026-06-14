// Package eventreplay stores the compact replay format served by task SSE.
//
// Task persistence has two files with different jobs:
//   - the raw task log (<task>.jsonl[.zst]) is the source of truth. It contains
//     harness-native NDJSON interleaved with caic control records and is kept so
//     parser and converter bugs can be fixed by regenerating derived data.
//   - the replay sidecar (<task>.events.zst) is a rebuildable cache of
//     v1.EventMessage JSONL. Once its header matches the raw log identity, the
//     body is trusted and streamed as "data: <line>" SSE frames without
//     harness parsing, DTO conversion, or per-line JSON validation.
//
// Harnesses still write their native wire format. Replay generation deliberately
// keeps the pipeline as harness wire -> agent.Message -> v1.EventMessage so API
// DTO churn cannot corrupt the raw record and stale sidecars can be regenerated.
package eventreplay

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
)

// CacheVersion is the schema version of the cached EventMessage JSONL.
// Bump it whenever event conversion or DTO semantics change so stale caches are
// ignored rather than served.
const CacheVersion = 2

// CacheHeader is the first JSONL record of a replay cache. It binds the cache
// to a specific raw log file so a changed or replaced log invalidates it.
type CacheHeader struct {
	Version  int   `json:"v"`
	LogSize  int64 `json:"logSize"`
	LogModNs int64 `json:"logModNs"`
}

// CachePath returns the sidecar cache path for a task log path. The
// ".events.zst" suffix is deliberately not a task-log name, so the log scanner
// never mistakes a cache for a log.
func CachePath(logPath string) string {
	return cachePathForBase(trimLogExt(logPath))
}

// PruneStaleCaches removes replay sidecars that no longer match a raw task log.
func PruneStaleCaches(logDir string) (int, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".events.zst") {
			continue
		}
		cachePath := filepath.Join(logDir, e.Name())
		logPath := logPathForCache(cachePath)
		if logPath != "" {
			if _, closeFn, ok := openFreshCacheBody(logPath); ok {
				closeFn()
				continue
			}
		}
		if err := os.Remove(filepath.Clean(cachePath)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// Replay is an open EventMessage JSONL replay sidecar.
type Replay struct {
	logPath string
	br      *bufio.Reader
	closeFn func()
}

// OpenReplay opens a valid DTO replay sidecar for logPath.
func OpenReplay(logPath string) (*Replay, bool) {
	br, closeFn, ok := openFreshCacheBody(logPath)
	if !ok {
		return nil, false
	}
	return &Replay{logPath: logPath, br: br, closeFn: closeFn}, true
}

// Close releases the replay reader.
func (r *Replay) Close() {
	if r == nil || r.closeFn == nil {
		return
	}
	r.closeFn()
	r.closeFn = nil
}

// WriteSSE streams cached EventMessage JSONL as SSE, starting at *idx and
// updating it for callers that append stats or ready events afterward. The body
// is trusted after the header validates: each line is copied directly into a
// data frame without JSON decoding.
func (r *Replay) WriteSSE(w io.Writer, flusher http.Flusher, idx *int) bool {
	if r == nil {
		return false
	}
	bytesSinceFlush := 0
	wrote := false
	for {
		line, rerr := r.br.ReadBytes('\n')
		line = trimLineEnding(line)
		if len(line) > 0 {
			n, _ := fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", line, *idx)
			(*idx)++
			bytesSinceFlush += n
			wrote = true
			if bytesSinceFlush >= 65536 {
				flusher.Flush()
				bytesSinceFlush = 0
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) && !wrote {
				return false
			}
			if !errors.Is(rerr, io.EOF) {
				slog.Warn("replay cache: truncated read", "path", CachePath(r.logPath), "err", rerr)
			}
			flusher.Flush()
			return true
		}
	}
}

// ServeCacheFromDisk streams a fresh cached EventMessage JSONL for logPath to w
// as SSE, flushing periodically. It reports whether a valid cache was found and
// served. A miss leaves w untouched so the caller can render normally.
func ServeCacheFromDisk(w io.Writer, flusher http.Flusher, logPath string) bool {
	replay, ok := OpenReplay(logPath)
	if !ok {
		return false
	}
	defer replay.Close()
	idx := 0
	if !replay.WriteSSE(w, flusher, &idx) {
		return false
	}
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	return true
}

// CacheWriter accumulates EventMessage JSONL body bytes and commits them with a
// final header bound to the final raw log identity.
type CacheWriter struct {
	mu       sync.Mutex
	body     *os.File
	bodyName string
	dst      string
	werr     error
}

// NewCacheWriter opens a temp EventMessage body file for logPath. If a valid
// cache already exists for the current log identity, its body is copied into the
// new writer so later sessions can append without rebuilding from raw.
func NewCacheWriter(logPath string) *CacheWriter {
	dst := CachePath(logPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		slog.Warn("replay cache: create dir", "err", err)
		return nil
	}
	body, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.body")
	if err != nil {
		slog.Warn("replay cache: create temp", "err", err)
		return nil
	}
	w := &CacheWriter{body: body, bodyName: body.Name(), dst: dst}
	w.seedFromFreshCache(logPath)
	return w
}

// WriteEventData appends a marshaled EventMessage JSON object to the cache,
// recording the first error so Commit can discard a partial cache.
func (w *CacheWriter) WriteEventData(data []byte) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeLineLocked(data)
}

// Commit finalizes the cache via atomic rename using logPath's current size and
// mtime, or discards it on any error.
func (w *CacheWriter) Commit(logPath string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commitLocked(logPath)
}

// Abort discards an uncommitted cache body.
func (w *CacheWriter) Abort() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.abortLocked()
}

func (w *CacheWriter) seedFromFreshCache(logPath string) {
	br, closeFn, ok := openFreshCacheBody(logPath)
	if !ok {
		return
	}
	defer closeFn()
	if _, err := io.Copy(w.body, br); err != nil {
		w.werr = err
	}
}

func (w *CacheWriter) writeLineLocked(data []byte) {
	if w.werr != nil || w.body == nil {
		return
	}
	if _, err := w.body.Write(data); err != nil {
		w.werr = err
		return
	}
	if _, err := w.body.Write([]byte{'\n'}); err != nil {
		w.werr = err
	}
}

func (w *CacheWriter) abortLocked() {
	if w.body == nil {
		return
	}
	body := w.body
	w.body = nil
	_ = body.Close()
	_ = os.Remove(w.bodyName)
}

func (w *CacheWriter) commitLocked(logPath string) {
	if w.body == nil {
		return
	}
	body := w.body
	w.body = nil
	closeBody := body.Close
	defer func() {
		_ = os.Remove(w.bodyName)
	}()

	info, statErr := os.Stat(filepath.Clean(logPath))
	if statErr != nil {
		w.werr = errors.Join(w.werr, statErr)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		w.werr = errors.Join(w.werr, err)
	}
	if w.werr != nil {
		_ = closeBody()
		slog.Warn("replay cache: discarding partial cache", "path", w.dst, "err", w.werr)
		return
	}

	tmp, err := os.CreateTemp(filepath.Dir(w.dst), filepath.Base(w.dst)+".*.tmp")
	if err != nil {
		_ = closeBody()
		slog.Warn("replay cache: create commit temp", "err", err)
		return
	}
	tmpName := tmp.Name()
	enc, err := zstd.NewWriter(tmp, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(64<<10))
	if err != nil {
		_ = closeBody()
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	header, err := json.Marshal(CacheHeader{
		Version:  CacheVersion,
		LogSize:  info.Size(),
		LogModNs: info.ModTime().UnixNano(),
	})
	if err == nil {
		_, err = enc.Write(append(header, '\n'))
	}
	if err == nil {
		_, err = io.Copy(enc, body)
	}
	closeErr := errors.Join(enc.Close(), tmp.Close(), closeBody())
	if err = errors.Join(err, closeErr); err != nil {
		_ = os.Remove(tmpName)
		slog.Warn("replay cache: discarding partial cache", "path", w.dst, "err", err)
		return
	}
	if err := os.Rename(tmpName, w.dst); err != nil {
		_ = os.Remove(tmpName)
		slog.Warn("replay cache: rename", "path", w.dst, "err", err)
	}
}

// RegenerateReplay rebuilds the DTO replay sidecar from parsed raw-log messages.
func RegenerateReplay(logPath string, h harness.Name, msgs iter.Seq2[agent.Message, error]) error {
	cache := NewCacheWriter(logPath)
	if cache == nil {
		return errors.New("create replay cache writer")
	}
	committed := false
	defer func() {
		if !committed {
			cache.Abort()
		}
	}()

	tracker := v1conv.NewToolTimingTracker(h, FormatToolOutput)
	now := time.Now()
	emit := func(m agent.Message) {
		evs := tracker.ConvertMessage(m, now)
		for i := range evs {
			data, err := v1conv.MarshalEvent(&evs[i])
			if err != nil {
				slog.Warn("marshal replay event", "err", err)
				continue
			}
			cache.WriteEventData(data)
		}
	}
	push, flush := NewFilter(emit)
	for msg, err := range msgs {
		if err != nil {
			return err
		}
		push(msg)
	}
	flush()
	cache.Commit(logPath)
	committed = true
	return nil
}

// MessageWriter converts agent messages into compacted EventMessage JSONL.
type MessageWriter struct {
	mu      sync.Mutex
	cache   *CacheWriter
	tracker *v1conv.ToolTimingTracker
	push    func(agent.Message)
	flush   func()
}

// NewMessageWriter creates a live EventMessage replay writer for logPath.
func NewMessageWriter(logPath string, h harness.Name) *MessageWriter {
	cache := NewCacheWriter(logPath)
	if cache == nil {
		return nil
	}
	w := &MessageWriter{
		cache:   cache,
		tracker: v1conv.NewToolTimingTracker(h, FormatToolOutput),
	}
	emit := func(m agent.Message) {
		evs := w.tracker.ConvertMessage(m, time.Now())
		for i := range evs {
			data, err := v1conv.MarshalEvent(&evs[i])
			if err != nil {
				slog.Warn("marshal replay event", "err", err)
				continue
			}
			w.cache.WriteEventData(data)
		}
	}
	w.push, w.flush = NewFilter(emit)
	return w
}

// WriteMessage appends m to the live replay stream after write-time compaction.
func (w *MessageWriter) WriteMessage(m agent.Message) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.push(m)
}

// Commit flushes any buffered delta tail and commits the underlying cache.
func (w *MessageWriter) Commit(logPath string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flush()
	w.cache.Commit(logPath)
}

// NewFilter is the streaming replay compactor. It collapses a contiguous run of
// streaming-delta messages when the matching consolidated message immediately
// follows, emitting surviving messages in order via emit.
func NewFilter(emit func(agent.Message)) (push func(agent.Message), flush func()) {
	var pending []agent.Message
	cleanTurnComplete := false
	flush = func() {
		for _, m := range pending {
			emit(m)
		}
		pending = pending[:0]
	}
	push = func(m agent.Message) {
		if exit, ok := m.(*agent.ExitMessage); ok && exit.ExitCode != 0 && cleanTurnComplete {
			return
		}
		if k := deltaKind(m); k != 0 {
			if len(pending) > 0 && deltaKind(pending[0]) != k {
				flush()
			}
			pending = append(pending, m)
			return
		}
		if finalKind(m) != 0 && len(pending) > 0 && deltaKind(pending[0]) == finalKind(m) {
			pending = pending[:0]
		} else {
			flush()
		}
		if clearsExit(m) {
			cleanTurnComplete = false
		}
		if rm, ok := m.(*agent.ResultMessage); ok {
			cleanTurnComplete = !rm.IsError
		}
		emit(m)
	}
	return push, flush
}

func cachePathForBase(base string) string {
	return base + ".events.zst"
}

func trimLogExt(path string) string {
	for _, ext := range []string{".jsonl.zst", ".jsonl"} {
		if trimmed, ok := strings.CutSuffix(path, ext); ok {
			return trimmed
		}
	}
	return path
}

func trimLineEnding(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}

func logPathForCache(cachePath string) string {
	base, ok := strings.CutSuffix(cachePath, ".events.zst")
	if !ok {
		return ""
	}
	compressed := base + ".jsonl.zst"
	if _, err := os.Stat(filepath.Clean(compressed)); err == nil {
		return compressed
	}
	plain := base + ".jsonl"
	if _, err := os.Stat(filepath.Clean(plain)); err == nil {
		return plain
	}
	return ""
}

func openFreshCacheBody(logPath string) (*bufio.Reader, func(), bool) {
	info, err := os.Stat(filepath.Clean(logPath))
	if err != nil {
		return nil, nil, false
	}
	f, err := os.Open(filepath.Clean(CachePath(logPath)))
	if err != nil {
		return nil, nil, false
	}
	dec, err := zstd.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, false
	}
	br := bufio.NewReaderSize(dec, 64<<10)
	headerLine, err := br.ReadBytes('\n')
	if err != nil {
		dec.Close()
		_ = f.Close()
		return nil, nil, false
	}
	var h CacheHeader
	if json.Unmarshal(headerLine, &h) != nil {
		dec.Close()
		_ = f.Close()
		return nil, nil, false
	}
	if h.Version != CacheVersion || h.LogSize != info.Size() || h.LogModNs != info.ModTime().UnixNano() {
		dec.Close()
		_ = f.Close()
		return nil, nil, false
	}
	return br, func() { dec.Close(); _ = f.Close() }, true
}

func deltaKind(m agent.Message) int {
	switch m.(type) {
	case *agent.TextDeltaMessage:
		return 1
	case *agent.ThinkingDeltaMessage:
		return 2
	case *agent.WidgetDeltaMessage:
		return 3
	case *agent.ToolOutputDeltaMessage:
		return 4
	}
	return 0
}

func finalKind(m agent.Message) int {
	switch m.(type) {
	case *agent.TextMessage:
		return 1
	case *agent.ThinkingMessage:
		return 2
	case *agent.WidgetMessage:
		return 3
	case *agent.ToolResultMessage:
		return 4
	}
	return 0
}

func clearsExit(m agent.Message) bool {
	switch m := m.(type) {
	case *agent.ExitMessage, *agent.DiffStatMessage, *agent.RawMessage,
		*agent.ParseErrorMessage, *agent.LogMessage, *agent.StrippedEnvMessage:
		return false
	case *agent.ResultMessage:
		return !m.IsError
	default:
		return true
	}
}

// FormatToolOutput analyzes a tool output string and returns its content type
// along with an optional formatted version.
func FormatToolOutput(raw string) (contentType v1.ToolOutputContentType, formatted string) {
	if raw == "" {
		return v1.ToolOutputText, ""
	}
	if ct, formatted := formatAsJSON(raw); ct != "" {
		return ct, formatted
	}
	if looksLikeMarkdown(raw) {
		return v1.ToolOutputMarkdown, ""
	}
	return v1.ToolOutputText, ""
}

func formatAsJSON(raw string) (ct v1.ToolOutputContentType, formatted string) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
			return "", ""
		}
	}

	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", ""
	}
	formattedBytes, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", ""
	}
	return v1.ToolOutputJSON, string(formattedBytes)
}

func looksLikeMarkdown(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "- ") ||
		strings.HasPrefix(trimmed, "* ") ||
		strings.HasPrefix(trimmed, "+ ") ||
		strings.Contains(trimmed, "```") ||
		strings.Contains(trimmed, "\n")
}
