// Package eventreplay stores the compact replay format served by task SSE.
//
// Task persistence has two files with different jobs:
//   - the raw task log (<task>.jsonl[.zst]) is the source of truth. It contains
//     harness-native NDJSON interleaved with caic control records and is kept so
//     parser and converter bugs can be fixed by regenerating derived data.
//   - the replay sidecar (<task>.events.zst) is a rebuildable cache of
//     v1.EventMessage JSONL. Once its header matches the raw log identity, the
//     body is validated as EventMessage JSONL through EOF, then streamed as
//     "data: <line>" SSE frames without harness parsing or DTO conversion.
//
// Harnesses still write their native wire format. Replay generation deliberately
// keeps the pipeline as harness wire -> agent.Message -> v1.EventMessage so API
// DTO churn cannot corrupt the raw record and stale sidecars can be regenerated.
package eventreplay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/logproof"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
)

// CacheVersion is the schema version of the cached EventMessage JSONL.
// Bump it whenever event conversion or DTO semantics change so stale caches are
// ignored rather than served.
const CacheVersion = 5

// maxReplayRecordBytes bounds memory consumed by one untrusted cache JSONL
// record. Oversized records invalidate the entire derived cache.
const maxReplayRecordBytes = 32 << 20

var errReplayRecordTooLarge = errors.New("replay cache record exceeds size limit")

// ProofProvider returns a raw-header and identity observation. It is injected
// by the task layer so eventreplay can compare cache identity without importing
// or reimplementing task-log validation.
type ProofProvider func(string) (logproof.CacheProof, error)

// ReplaySource scans one raw log through EOF, passing parsed conversation
// messages to yield, and returns the completed scan's proof. A replay body is
// committed only when that exact proof still names the raw log.
type ReplaySource func(context.Context, func(agent.ParsedMessage) error) (logproof.CacheProof, error)

// CacheHeader is the first JSONL record of a replay cache. It binds the cache
// to a specific raw log file so a changed or replaced log invalidates it.
type CacheHeader struct {
	Version          int              `json:"v"`
	LogDevice        uint64           `json:"logDevice"`
	LogInode         uint64           `json:"logInode"`
	LogSize          int64            `json:"logSize"`
	LogModNs         int64            `json:"logModNs"`
	AuthorityVersion agent.LogVersion `json:"authorityVersion"`
	AuthorityHarness harness.Name     `json:"authorityHarness"`
	RawHeader        string           `json:"rawHeader"`
	Empty            bool             `json:"empty,omitempty"`
}

// CachePath returns the sidecar cache path for a task log path. The
// ".events.zst" suffix is deliberately not a task-log name, so the log scanner
// never mistakes a cache for a log.
func CachePath(logPath string) string {
	return cachePathForBase(trimLogExt(logPath))
}

// PruneStaleCaches removes replay sidecars that no longer match a raw task log.
// It also removes orphaned temp files left by interrupted cache writes.
func PruneStaleCaches(logDir string, prove ProofProvider) (int, error) {
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
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if isReplayTempName(name) {
			if err := removeFile(filepath.Join(logDir, name)); err != nil {
				errs = append(errs, err)
				continue
			}
			removed++
			continue
		}
		if !strings.HasSuffix(name, ".events.zst") {
			continue
		}
		cachePath := filepath.Join(logDir, name)
		logPath, logPathErr := logPathForCache(cachePath)
		if logPathErr != nil {
			// Do not turn a transient raw-path observation failure into cache
			// deletion. A future bounded sweep can retry the proof.
			continue
		}
		if logPath != "" {
			_, closeFn, freshness := freshCacheBody(logPath, prove)
			if closeFn != nil {
				closeFn()
			}
			switch freshness {
			case cacheFresh:
				continue
			case cacheUnverifiable:
				// A cache-proof failure can be a transient permission, I/O, or
				// replacement race. Retain the derived cache until raw authority
				// can be observed conclusively.
				continue
			case cacheStale:
				// A completed proof established a definite cache mismatch.
			}
		}
		if err := removeFile(cachePath); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// Replay is one validated, open EventMessage JSONL replay sidecar.
type Replay struct {
	logPath string
	proof   logproof.CacheProof
	prove   ProofProvider
	file    *os.File
	decoder *zstd.Decoder
	br      *replayRecordReader
	empty   bool
}

// SSEWriteResult distinguishes an untouched writer from a completed replay and
// a failed write after at least one history byte was published. Callers must not
// regenerate from index zero after SSEPartial or they would duplicate history.
type SSEWriteResult uint8

const (
	// SSEUnpublished means WriteSSE did not write any history byte.
	SSEUnpublished SSEWriteResult = iota
	// SSEComplete means WriteSSE delivered every validated replay record.
	SSEComplete
	// SSEPartial means WriteSSE failed after writing history bytes.
	SSEPartial
)

// OpenReplay opens, validates through EOF, and rewinds one replay file identity
// for publication. It intentionally never validates one sidecar file and serves
// a separately opened replacement.
func OpenReplay(logPath string, prove ProofProvider) (*Replay, bool) {
	proof, err := resolveProof(logPath, prove)
	if err != nil {
		return nil, false
	}
	file, decoder, br, err := openReplayBody(logPath)
	if err != nil {
		return nil, false
	}
	header, ok := readCacheHeader(br, proof)
	r := &Replay{logPath: logPath, proof: proof, prove: prove, file: file, decoder: decoder, br: br, empty: header.Empty}
	if !ok || !r.validateForPublication() {
		r.Close()
		return nil, false
	}
	return r, true
}

// Close releases the replay file and decoder.
func (r *Replay) Close() {
	if r == nil {
		return
	}
	if r.decoder != nil {
		r.decoder.Close()
		r.decoder = nil
	}
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
}

// WriteSSE streams cached EventMessage JSONL as SSE, starting at *idx and
// updating it for callers that append stats or ready events afterward. The body
// is trusted after OpenReplay's same-file EOF EventMessage validation, so each
// line is copied directly into a data frame without JSON decoding.
func (r *Replay) WriteSSE(w io.Writer, flusher http.Flusher, idx *int) SSEWriteResult {
	if r == nil || r.br == nil {
		return SSEUnpublished
	}
	bytesSinceFlush := 0
	published := false
	for {
		line, rerr := r.br.ReadRecord()
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) || len(line) != 0 {
				slog.Warn("replay cache: changed during read", "path", CachePath(r.logPath), "err", rerr)
				if published {
					return SSEPartial
				}
				return SSEUnpublished
			}
			flusher.Flush()
			return SSEComplete
		}
		line = trimLineEnding(line)
		if len(line) == 0 {
			continue
		}
		n, err := fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", line, *idx)
		if err != nil {
			if published || n > 0 {
				return SSEPartial
			}
			return SSEUnpublished
		}
		published = true
		(*idx)++
		bytesSinceFlush += n
		if bytesSinceFlush >= 65536 {
			flusher.Flush()
			bytesSinceFlush = 0
		}
	}
}

// validateForPublication verifies the complete derived stream before the first
// SSE byte, then rewinds that same descriptor for publication. This prevents a
// corrupt/truncated sidecar from publishing a prefix or a replacement from
// slipping between validation and output.
func (r *Replay) validateForPublication() bool {
	hasBody := false
	for {
		line, err := r.br.ReadRecord()
		if err != nil {
			if !errors.Is(err, io.EOF) || len(line) != 0 {
				slog.Warn("replay cache: invalid body", "path", CachePath(r.logPath), "err", err)
				return false
			}
			break
		}
		line = trimLineEnding(line)
		if len(line) == 0 {
			continue
		}
		if err := validateReplayEventLine(line); err != nil {
			slog.Warn("replay cache: invalid EventMessage", "path", CachePath(r.logPath), "err", err)
			return false
		}
		hasBody = true
	}
	if !hasBody && !r.empty {
		return false
	}
	finalProof, err := resolveProof(r.logPath, r.prove)
	if err != nil || finalProof != r.proof {
		slog.Warn("replay cache: raw log changed before publication", "path", CachePath(r.logPath), "err", err)
		return false
	}
	r.decoder.Close()
	r.decoder = nil
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return false
	}
	decoder, err := zstd.NewReader(r.file)
	if err != nil {
		return false
	}
	r.decoder = decoder
	r.br = newReplayRecordReader(decoder)
	header, ok := readCacheHeader(r.br, r.proof)
	return ok && header.Empty == r.empty
}

// CacheWriter accumulates EventMessage JSONL body bytes and commits them with a
// final header bound to the final raw log identity.
type CacheWriter struct {
	bodyName   string
	dst        string
	seedProof  logproof.CacheProof
	prove      ProofProvider
	allowEmpty bool

	mu   sync.Mutex
	body *os.File
	werr error
}

// NewCacheWriter opens an empty temp EventMessage body file for a complete
// regeneration. Full rebuilds never seed from a prior derived cache.
func NewCacheWriter(logPath string, prove ProofProvider) (*CacheWriter, error) {
	return newCacheWriter(logPath, logproof.CacheProof{}, prove)
}

func newAppendCacheWriter(logPath string, proof logproof.CacheProof, prove ProofProvider) (*CacheWriter, error) {
	w, err := newCacheWriter(logPath, proof, prove)
	if err != nil {
		return nil, err
	}
	// A live append writer observes its raw log through its final append proof.
	// It may validly emit no EventMessages, unlike a partially built cache.
	w.allowEmpty = true
	seeded, err := w.seedFromFreshCache(logPath)
	if err != nil {
		w.Abort()
		return nil, err
	}
	info, err := os.Stat(filepath.Clean(logPath))
	if err != nil {
		w.Abort()
		return nil, err
	}
	if info.Size() > 0 && !seeded {
		// A just-created log contains exactly its authoritative header and has no
		// history to seed. Any additional raw record requires a complete cache so
		// reopening cannot publish a partial replay.
		if info.Size() != int64(len(proof.RawHeader)+1) {
			w.werr = errors.New("existing log has no complete replay cache to append to")
		}
	}
	return w, nil
}

func newCacheWriter(logPath string, proof logproof.CacheProof, prove ProofProvider) (*CacheWriter, error) {
	dst := CachePath(logPath)
	body, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.body")
	if err != nil {
		return nil, fmt.Errorf("create replay cache temp: %w", err)
	}
	return &CacheWriter{body: body, bodyName: body.Name(), dst: dst, seedProof: proof, prove: prove}, nil
}

// WriteEventData appends a marshaled EventMessage JSON object to the cache,
// recording the first error so Commit can discard a partial cache.
func (w *CacheWriter) WriteEventData(data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeLineLocked(data)
}

// WriteEventDataBytes appends already newline-delimited event records from the
// bounded compactor buffer. The bytes were marshaled before buffering and are
// never used for an untrusted cache read.
func (w *CacheWriter) WriteEventDataBytes(data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.werr != nil || w.body == nil || len(data) == 0 {
		return
	}
	if _, err := w.body.Write(data); err != nil {
		w.werr = err
	}
}

// Commit finalizes an independently constructed cache with the current raw
// observation. Replay regeneration uses CommitExact instead.
func (w *CacheWriter) Commit(logPath string) error {
	return w.CommitContext(context.Background(), logPath)
}

// CommitContext finalizes an independently constructed cache unless ctx is
// cancelled while its body is copied, compressed, or published.
func (w *CacheWriter) CommitContext(ctx context.Context, logPath string) error {
	proof, err := resolveProof(logPath, w.prove)
	if err != nil {
		w.Abort()
		return err
	}
	return w.CommitExactContext(ctx, logPath, proof)
}

// CommitExact finalizes a regenerated cache only if the raw log still exactly
// matches the proof returned by the completed semantic scan.
func (w *CacheWriter) CommitExact(logPath string, proof logproof.CacheProof) error {
	return w.CommitExactContext(context.Background(), logPath, proof)
}

// CommitExactContext finalizes a regenerated cache only while ctx remains
// active and the raw log exactly matches the completed semantic scan.
func (w *CacheWriter) CommitExactContext(ctx context.Context, logPath string, proof logproof.CacheProof) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.commitLocked(ctx, logPath, proof, false)
}

// Abort discards an uncommitted cache body.
func (w *CacheWriter) Abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.abortLocked()
}

// commitAppend permits only append growth after the raw observation from which
// the seeded replay cache was validated. Identity and header authority remain
// immutable; a same-size mtime mutation is rejected.
func (w *CacheWriter) commitAppend(logPath string, proof logproof.CacheProof) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.commitLocked(context.Background(), logPath, proof, true)
}

func (w *CacheWriter) seedFromFreshCache(logPath string) (bool, error) {
	br, closeFn, freshness := freshCacheBodyWithProof(logPath, w.seedProof)
	ok := freshness == cacheFresh
	if !ok {
		return false, nil
	}
	defer closeFn()
	if _, err := io.Copy(w.body, br); err != nil {
		w.werr = err
		return false, fmt.Errorf("seed replay cache: %w", err)
	}
	return true, nil
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

// appendPending atomically moves a completed compaction run from its temporary
// disk spool into the cache body. It is called only after the run can no longer
// be superseded by its matching final message.
func (w *CacheWriter) appendPending(ctx context.Context, pending *os.File) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.werr != nil || w.body == nil {
		return w.werr
	}
	if err := ctx.Err(); err != nil {
		w.werr = errors.Join(w.werr, err)
		return err
	}
	if _, err := pending.Seek(0, io.SeekStart); err != nil {
		w.werr = err
		return err
	}
	if err := copyWithContext(ctx, w.body, pending); err != nil {
		w.werr = errors.Join(w.werr, err)
		return err
	}
	return nil
}

func (w *CacheWriter) recordError(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.werr = errors.Join(w.werr, err)
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

func (w *CacheWriter) commitLocked(ctx context.Context, logPath string, expectedProof logproof.CacheProof, allowAppendGrowth bool) error {
	if err := ctx.Err(); err != nil {
		w.abortLocked()
		return err
	}
	if w.body == nil {
		return nil
	}
	body := w.body
	w.body = nil
	closeBody := body.Close
	defer func() {
		_ = os.Remove(w.bodyName)
	}()

	if err := ctx.Err(); err != nil {
		w.werr = errors.Join(w.werr, err)
	}
	publicationProof, proofErr := resolveProof(logPath, w.prove)
	switch {
	case proofErr != nil:
		w.werr = errors.Join(w.werr, proofErr)
	case allowAppendGrowth && !appendProofMatches(expectedProof, publicationProof):
		w.werr = errors.Join(w.werr, errors.New("raw task log changed outside live append growth"))
	case !allowAppendGrowth && publicationProof != expectedProof:
		w.werr = errors.Join(w.werr, errors.New("raw task log changed since completed semantic scan"))
	}
	bodyInfo, err := body.Stat()
	if err != nil {
		w.werr = errors.Join(w.werr, err)
	}
	empty := err == nil && bodyInfo.Size() == 0
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		w.werr = errors.Join(w.werr, err)
	}
	if w.werr != nil {
		closeErr := closeBody()
		return fmt.Errorf("discard partial replay cache %s: %w", w.dst, errors.Join(w.werr, closeErr))
	}

	if err := ctx.Err(); err != nil {
		closeErr := closeBody()
		return fmt.Errorf("discard partial replay cache %s: %w", w.dst, errors.Join(err, closeErr))
	}
	tmp, err := os.CreateTemp(filepath.Dir(w.dst), filepath.Base(w.dst)+".*.tmp")
	if err != nil {
		closeErr := closeBody()
		return fmt.Errorf("create replay cache commit temp: %w", errors.Join(err, closeErr))
	}
	tmpName := tmp.Name()
	enc, err := zstd.NewWriter(tmp, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(64<<10))
	if err != nil {
		closeErr := errors.Join(closeBody(), tmp.Close(), os.Remove(tmpName))
		return fmt.Errorf("create replay cache zstd writer: %w", errors.Join(err, closeErr))
	}
	header, err := json.Marshal(CacheHeader{
		Version:          CacheVersion,
		LogDevice:        publicationProof.Device,
		LogInode:         publicationProof.Inode,
		LogSize:          publicationProof.Size,
		LogModNs:         publicationProof.ModTimeNs,
		AuthorityVersion: publicationProof.Version,
		AuthorityHarness: publicationProof.Harness,
		RawHeader:        publicationProof.RawHeader,
		Empty:            empty && w.allowEmpty,
	})
	if err == nil {
		_, err = enc.Write(append(header, '\n'))
	}
	if err == nil {
		err = copyWithContext(ctx, enc, body)
	}
	closeErr := errors.Join(enc.Close(), tmp.Close(), closeBody())
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(err, ctxErr)
	}
	if err = errors.Join(err, closeErr); err != nil {
		removeErr := os.Remove(tmpName)
		return fmt.Errorf("discard partial replay cache %s: %w", w.dst, errors.Join(err, removeErr))
	}
	// Reprove after all cache bytes have been written and closed, immediately
	// before rename. A regeneration must still exactly name its semantic scan;
	// live append may only have grown the observed file.
	if err := ctx.Err(); err != nil {
		removeErr := os.Remove(tmpName)
		return fmt.Errorf("discard replay cache %s: %w", w.dst, errors.Join(err, removeErr))
	}
	finalProof, proofErr := resolveProof(logPath, w.prove)
	matches := proofErr == nil && finalProof == publicationProof
	if allowAppendGrowth {
		matches = proofErr == nil && appendProofMatches(expectedProof, finalProof) && finalProof == publicationProof
	}
	if !matches {
		removeErr := os.Remove(tmpName)
		if proofErr == nil {
			proofErr = errors.New("raw task log changed while publishing replay cache")
		}
		return fmt.Errorf("discard replay cache %s: %w", w.dst, errors.Join(proofErr, removeErr))
	}
	if err := ctx.Err(); err != nil {
		removeErr := os.Remove(tmpName)
		return fmt.Errorf("discard replay cache %s: %w", w.dst, errors.Join(err, removeErr))
	}
	tmpInfo, err := os.Stat(tmpName)
	if err != nil {
		return fmt.Errorf("stat replay cache %s before rename: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, w.dst); err != nil {
		removeErr := os.Remove(tmpName)
		return fmt.Errorf("rename replay cache %s: %w", w.dst, errors.Join(err, removeErr))
	}
	if err := ctx.Err(); err != nil {
		if dstInfo, statErr := os.Stat(w.dst); statErr == nil && os.SameFile(tmpInfo, dstInfo) {
			_ = os.Remove(w.dst)
		}
		return err
	}
	return nil
}

// RegenerateReplay rebuilds the DTO replay sidecar from one completed semantic
// raw-log scan. Cancellation leaves neither a replay cache nor a disk spool.
func RegenerateReplay(ctx context.Context, logPath string, prove ProofProvider, source ReplaySource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	initialProof, err := resolveProof(logPath, prove)
	if err != nil {
		return fmt.Errorf("prove replay log authority: %w", err)
	}
	cache, err := NewCacheWriter(logPath, prove)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			cache.Abort()
		}
	}()
	filter := newReplayDiskFilter(cache, initialProof.Harness, time.Now, true)
	defer filter.close()
	proof, err := source(ctx, func(msg agent.ParsedMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return filter.pushContext(ctx, msg)
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if initialProof.Version != proof.Version || initialProof.Harness != proof.Harness || initialProof.RawHeader != proof.RawHeader {
		return errors.New("raw task log authority changed during replay scan")
	}
	if err := filter.flushContext(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cache.allowEmpty = true
	if err := cache.CommitExactContext(ctx, logPath, proof); err != nil {
		return err
	}
	committed = true
	return nil
}

// MessageWriter converts agent messages into compacted EventMessage JSONL.
type MessageWriter struct {
	mu     sync.Mutex
	cache  *CacheWriter
	filter *replayDiskFilter
}

// NewMessageWriter creates a live EventMessage replay writer for logPath. The
// physical header is the only harness authority for timing conversion.
func NewMessageWriter(logPath string, prove ProofProvider) (*MessageWriter, error) {
	if strings.HasSuffix(logPath, ".jsonl.zst") {
		return nil, errors.New("live replay append does not support compressed task logs")
	}
	proof, err := resolveProof(logPath, prove)
	if err != nil {
		return nil, fmt.Errorf("prove replay log authority: %w", err)
	}
	cache, err := newAppendCacheWriter(logPath, proof, prove)
	if err != nil {
		return nil, err
	}
	return &MessageWriter{cache: cache, filter: newReplayDiskFilter(cache, proof.Harness, time.Now, false)}, nil
}

// WriteMessage appends m to the live replay stream after write-time compaction.
func (w *MessageWriter) WriteMessage(m agent.ParsedMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.filter.push(m)
}

// Commit flushes any buffered delta tail and commits the underlying cache.
func (w *MessageWriter) Commit(logPath string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.filter.flush()
	w.filter.close()
	return w.cache.commitAppend(logPath, w.cache.seedProof)
}

const maxPendingReplayBytes = 64 << 10

// replayDiskFilter is the replay compactor used by both cache writers. It keeps
// small live delta runs in a fixed-size memory buffer and spills only a run that
// exceeds maxPendingReplayBytes. A matching completed message discards that
// pending output; every other boundary copies it to the cache in event order.
type replayDiskFilter struct {
	cache             *CacheWriter
	tracker           *v1conv.ToolTimingTracker
	now               func() time.Time
	fixedObservation  bool
	fixedNow          time.Time
	pendingBuffer     bytes.Buffer
	pending           *os.File
	pendingName       string
	pendingKind       int
	pendingToolUseID  string
	cleanTurnComplete bool
}

func newReplayDiskFilter(cache *CacheWriter, h harness.Name, now func() time.Time, fixedObservation bool) *replayDiskFilter {
	return &replayDiskFilter{
		cache:            cache,
		tracker:          v1conv.NewToolTimingTracker(h, FormatToolOutput),
		now:              now,
		fixedObservation: fixedObservation,
		fixedNow:         now(),
	}
}

// Write buffers a small delta run and creates a disk spool only once the fixed
// memory limit is exceeded. It is called while MessageWriter.mu is held.
func (f *replayDiskFilter) Write(data []byte) (int, error) {
	if f.pending != nil {
		return f.pending.Write(data)
	}
	if f.pendingBuffer.Len()+len(data) <= maxPendingReplayBytes {
		return f.pendingBuffer.Write(data)
	}
	pending, err := os.CreateTemp(filepath.Dir(f.cache.dst), filepath.Base(f.cache.dst)+".*.pending")
	if err != nil {
		f.cache.recordError(fmt.Errorf("create replay pending spool: %w", err))
		return 0, err
	}
	f.pending, f.pendingName = pending, pending.Name()
	if _, err := pending.Write(f.pendingBuffer.Bytes()); err != nil {
		f.cache.recordError(fmt.Errorf("spill replay pending buffer: %w", err))
		return 0, err
	}
	f.pendingBuffer.Reset()
	return pending.Write(data)
}

func (f *replayDiskFilter) observation(parsed agent.ParsedMessage) time.Time {
	if !parsed.ProducerTime.IsZero() {
		return parsed.ProducerTime
	}
	if f.fixedObservation {
		return f.fixedNow
	}
	return f.now()
}

func (f *replayDiskFilter) writeConverted(dst io.Writer, parsed agent.ParsedMessage) {
	evs := f.tracker.ConvertMessage(parsed.Message, f.observation(parsed))
	for i := range evs {
		data, err := v1conv.MarshalEvent(&evs[i])
		if err != nil {
			f.cache.recordError(fmt.Errorf("marshal replay event: %w", err))
			return
		}
		if _, err := dst.Write(data); err != nil {
			f.cache.recordError(fmt.Errorf("write replay event: %w", err))
			return
		}
		if _, err := dst.Write([]byte{'\n'}); err != nil {
			f.cache.recordError(fmt.Errorf("write replay event terminator: %w", err))
			return
		}
	}
}

func (f *replayDiskFilter) emit(parsed agent.ParsedMessage) {
	// CacheWriter owns its body synchronization. Convert one bounded semantic
	// record at a time to retain the scanner's record-size memory ceiling.
	evs := f.tracker.ConvertMessage(parsed.Message, f.observation(parsed))
	for i := range evs {
		data, err := v1conv.MarshalEvent(&evs[i])
		if err != nil {
			f.cache.recordError(fmt.Errorf("marshal replay event: %w", err))
			return
		}
		f.cache.WriteEventData(data)
	}
}

func (f *replayDiskFilter) startPending(kind int, toolUseID string) {
	if f.pendingKind == 0 {
		f.pendingKind = kind
		f.pendingToolUseID = toolUseID
	}
}

func (f *replayDiskFilter) discardPending() {
	if f.pending != nil {
		if err := f.pending.Close(); err != nil {
			f.cache.recordError(err)
		}
		if err := os.Remove(f.pendingName); err != nil && !os.IsNotExist(err) {
			f.cache.recordError(err)
		}
	}
	f.pendingBuffer.Reset()
	f.pending, f.pendingName, f.pendingKind, f.pendingToolUseID = nil, "", 0, ""
}

func (f *replayDiskFilter) flushPendingContext(ctx context.Context) error {
	if f.pendingKind == 0 {
		return nil
	}
	defer f.discardPending()
	if f.pending != nil {
		return f.cache.appendPending(ctx, f.pending)
	}
	if err := ctx.Err(); err != nil {
		f.cache.recordError(err)
		return err
	}
	if f.pendingBuffer.Len() > 0 {
		f.cache.WriteEventDataBytes(f.pendingBuffer.Bytes())
	}
	return ctx.Err()
}

func (f *replayDiskFilter) flushPending() {
	_ = f.flushPendingContext(context.Background())
}

func (f *replayDiskFilter) push(parsed agent.ParsedMessage) {
	_ = f.pushContext(context.Background(), parsed)
}

func (f *replayDiskFilter) pushContext(ctx context.Context, parsed agent.ParsedMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m := parsed.Message
	if exit, ok := m.(*agent.ExitMessage); ok && exit.ExitCode != 0 && f.cleanTurnComplete {
		return nil
	}
	if kind := deltaKind(m); kind != 0 {
		toolUseID := deltaToolUseID(m)
		if f.pendingKind != 0 && (f.pendingKind != kind || f.pendingToolUseID != toolUseID) {
			if err := f.flushPendingContext(ctx); err != nil {
				return err
			}
		}
		f.startPending(kind, toolUseID)
		f.writeConverted(f, parsed)
		return ctx.Err()
	}
	if finalKind(m) != 0 && f.pendingKind != 0 && f.pendingKind == finalKind(m) && f.pendingToolUseID == finalToolUseID(m) {
		f.discardPending()
	} else if err := f.flushPendingContext(ctx); err != nil {
		return err
	}
	if clearsExit(m) {
		f.cleanTurnComplete = false
	}
	if rm, ok := m.(*agent.ResultMessage); ok {
		f.cleanTurnComplete = !rm.IsError
	}
	f.emit(parsed)
	return ctx.Err()
}

func (f *replayDiskFilter) flush() {
	f.flushPending()
}

func (f *replayDiskFilter) flushContext(ctx context.Context) error {
	return f.flushPendingContext(ctx)
}

func (f *replayDiskFilter) close() {
	f.discardPending()
}

// copyWithContext copies in bounded chunks so cancellation stops cache seeding,
// compaction, and compression before a derived file can be published.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			wn, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if wn != n {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func cachePathForBase(base string) string {
	return base + ".events.zst"
}

func isReplayTempName(name string) bool {
	return strings.Contains(name, ".events.zst.") && (strings.HasSuffix(name, ".body") || strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".pending"))
}

func removeFile(path string) error {
	if err := os.Remove(filepath.Clean(path)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func trimLogExt(path string) string {
	for _, ext := range []string{".jsonl.zst", ".jsonl"} {
		if trimmed, ok := strings.CutSuffix(path, ext); ok {
			return trimmed
		}
	}
	return path
}

func appendProofMatches(initial, current logproof.CacheProof) bool {
	if initial.Device != current.Device || initial.Inode != current.Inode ||
		initial.Version != current.Version || initial.Harness != current.Harness ||
		initial.RawHeader != current.RawHeader || current.Size < initial.Size {
		return false
	}
	return (current.Size > initial.Size && current.ModTimeNs >= initial.ModTimeNs) ||
		(current.Size == initial.Size && current.ModTimeNs == initial.ModTimeNs)
}

// replayRecordReader reads one newline-delimited derived-cache record without
// allowing a corrupt line to allocate beyond maxReplayRecordBytes.
type replayRecordReader struct {
	reader *bufio.Reader
}

// Read preserves buffered bytes for callers that stream a body already
// validated record-by-record through this reader.
func (r *replayRecordReader) Read(data []byte) (int, error) {
	return r.reader.Read(data)
}

func newReplayRecordReader(r io.Reader) *replayRecordReader {
	return &replayRecordReader{reader: bufio.NewReaderSize(r, 64<<10)}
}

func (r *replayRecordReader) ReadRecord() ([]byte, error) {
	var line []byte
	for {
		part, err := r.reader.ReadSlice('\n')
		if len(line)+len(part) > maxReplayRecordBytes {
			return nil, errReplayRecordTooLarge
		}
		line = append(line, part...)
		switch {
		case err == nil:
			return trimLineEnding(line), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) == 0:
			return nil, io.EOF
		case errors.Is(err, io.EOF):
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
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

func logPathForCache(cachePath string) (string, error) {
	base, ok := strings.CutSuffix(cachePath, ".events.zst")
	if !ok {
		return "", nil
	}
	compressed := base + ".jsonl.zst"
	if _, err := os.Stat(filepath.Clean(compressed)); err == nil {
		return compressed, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	plain := base + ".jsonl"
	if _, err := os.Stat(filepath.Clean(plain)); err == nil {
		return plain, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return "", nil
}

type cacheFreshness uint8

const (
	cacheStale cacheFreshness = iota
	cacheFresh
	cacheUnverifiable
)

func resolveProof(logPath string, prove ProofProvider) (logproof.CacheProof, error) {
	if prove == nil {
		return logproof.CacheProof{}, errors.New("replay cache proof provider is nil")
	}
	return prove(logPath)
}

// freshCacheBody distinguishes a definitive stale sidecar from an observation
// that could not establish raw authority. Callers may rebuild on either, but
// pruning must never delete the latter.
func freshCacheBody(logPath string, prove ProofProvider) (*replayRecordReader, func(), cacheFreshness) {
	proof, err := resolveProof(logPath, prove)
	if err != nil {
		return nil, nil, cacheUnverifiable
	}
	return freshCacheBodyWithProof(logPath, proof)
}

func freshCacheBodyWithProof(logPath string, proof logproof.CacheProof) (*replayRecordReader, func(), cacheFreshness) {
	file, decoder, br, err := openReplayBody(logPath)
	if err != nil {
		return nil, nil, cacheStale
	}
	closeFn := func() {
		decoder.Close()
		_ = file.Close()
	}
	header, ok := readCacheHeader(br, proof)
	if !ok || !cacheBodyEOFValid(br, header.Empty) {
		closeFn()
		return nil, nil, cacheStale
	}
	decoder.Close()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, nil, cacheStale
	}
	decoder, err = zstd.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, nil, cacheStale
	}
	br = newReplayRecordReader(decoder)
	if _, ok := readCacheHeader(br, proof); !ok {
		closeFn()
		return nil, nil, cacheStale
	}
	return br, closeFn, cacheFresh
}

// cacheBodyEOFValid verifies decompression and every EventMessage line through
// EOF before a cache body can seed a live append or remain fresh for pruning.
// A valid header alone is never sufficient authority for an eventful cache.
func cacheBodyEOFValid(br *replayRecordReader, empty bool) bool {
	hasBody := false
	for {
		line, err := br.ReadRecord()
		if err != nil {
			return errors.Is(err, io.EOF) && len(line) == 0 && (hasBody || empty)
		}
		line = trimLineEnding(line)
		if len(line) == 0 {
			continue
		}
		if validateReplayEventLine(line) != nil {
			return false
		}
		hasBody = true
	}
}

// validateReplayEventLine accepts exactly one recognized EventMessage payload
// matching its kind. Cache readers must reject JSON values that Unmarshal can
// otherwise silently coerce into a zero-value EventMessage.
func validateReplayEventLine(line []byte) error {
	if err := validateReplayJSON(line, reflect.TypeFor[v1.EventMessage]()); err != nil {
		return fmt.Errorf("invalid EventMessage schema: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var event v1.EventMessage
	if err := decoder.Decode(&event); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	payloads := 0
	for _, payload := range []bool{
		event.Init != nil, event.Text != nil, event.TextDelta != nil,
		event.ToolUse != nil, event.ToolResult != nil, event.Ask != nil,
		event.Usage != nil, event.Result != nil, event.System != nil,
		event.UserInput != nil, event.Todo != nil, event.DiffStat != nil,
		event.Error != nil, event.Thinking != nil, event.ThinkingDelta != nil,
		event.SubagentStart != nil, event.SubagentEnd != nil, event.Log != nil,
		event.ToolOutputDelta != nil, event.Widget != nil, event.WidgetDelta != nil,
		event.RateLimit != nil, event.Stats != nil,
	} {
		if payload {
			payloads++
		}
	}
	if payloads != 1 {
		return errors.New("EventMessage must contain exactly one payload")
	}
	var validPayload bool
	switch event.Kind {
	case v1.EventKindInit:
		validPayload = event.Init != nil
	case v1.EventKindText:
		validPayload = event.Text != nil
	case v1.EventKindTextDelta:
		validPayload = event.TextDelta != nil
	case v1.EventKindToolUse:
		validPayload = event.ToolUse != nil
	case v1.EventKindToolResult:
		validPayload = event.ToolResult != nil
	case v1.EventKindAsk:
		validPayload = event.Ask != nil
	case v1.EventKindUsage:
		validPayload = event.Usage != nil
	case v1.EventKindResult:
		validPayload = event.Result != nil
	case v1.EventKindSystem:
		validPayload = event.System != nil
	case v1.EventKindUserInput:
		validPayload = event.UserInput != nil
	case v1.EventKindTodo:
		validPayload = event.Todo != nil
	case v1.EventKindDiffStat:
		validPayload = event.DiffStat != nil
	case v1.EventKindError:
		validPayload = event.Error != nil
	case v1.EventKindThinking:
		validPayload = event.Thinking != nil
	case v1.EventKindThinkingDelta:
		validPayload = event.ThinkingDelta != nil
	case v1.EventKindSubagentStart:
		validPayload = event.SubagentStart != nil
	case v1.EventKindSubagentEnd:
		validPayload = event.SubagentEnd != nil
	case v1.EventKindLog:
		validPayload = event.Log != nil
	case v1.EventKindToolOutputDelta:
		validPayload = event.ToolOutputDelta != nil
	case v1.EventKindWidget:
		validPayload = event.Widget != nil
	case v1.EventKindWidgetDelta:
		validPayload = event.WidgetDelta != nil
	case v1.EventKindRateLimit:
		validPayload = event.RateLimit != nil
	case v1.EventKindStats:
		validPayload = event.Stats != nil
	default:
		return fmt.Errorf("unknown EventMessage kind %q", event.Kind)
	}
	if !validPayload {
		return fmt.Errorf("EventMessage kind %q has mismatched payload", event.Kind)
	}
	return nil
}

// validateReplayJSON recursively enforces the emitted EventMessage schema.
// encoding/json accepts unknown and duplicate object keys, so cache validation
// must inspect every nested object before decoding it into the public DTO.
var (
	jsonRawMessageType  = reflect.TypeFor[json.RawMessage]()
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
)

func validateReplayJSON(data []byte, typ reflect.Type) error {
	if !json.Valid(data) {
		return errors.New("invalid JSON")
	}
	typ = indirectJSONType(typ)
	if typ == jsonRawMessageType || reflect.PointerTo(typ).Implements(jsonUnmarshalerType) {
		return validateReplayJSONUnknown(data)
	}
	if isJSONScalarType(typ) {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return errors.New("scalar field must not be JSON null")
		}
		if err := json.Unmarshal(data, reflect.New(typ).Interface()); err != nil {
			return err
		}
		return nil
	}
	if typ.Kind() == reflect.Struct {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			return errors.New("must be a JSON object")
		}
		if object == nil {
			return errors.New("must be a JSON object")
		}
		fields := jsonSchemaFields(typ)
		seen, err := jsonObjectKeys(data)
		if err != nil {
			return err
		}
		for _, key := range seen {
			field, ok := fields[key]
			if !ok {
				return fmt.Errorf("unknown field %q", key)
			}
			if err := validateReplayJSON(object[key], field.typ); err != nil {
				return fmt.Errorf("field %q: %w", key, err)
			}
		}
		for name, field := range fields {
			if field.required {
				if _, ok := object[name]; !ok {
					return fmt.Errorf("missing required field %q", name)
				}
			}
		}
		return nil
	}
	switch typ.Kind() { //nolint:exhaustive // primitives have no nested keys.
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := validateReplayJSON(value, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map, reflect.Interface:
		return validateReplayJSONUnknown(data)
	}
	return nil
}

type replayJSONField struct {
	typ      reflect.Type
	required bool
}

func jsonSchemaFields(typ reflect.Type) map[string]replayJSONField {
	fields := make(map[string]replayJSONField)
	for field := range typ.Fields() {
		if field.Anonymous {
			maps.Copy(fields, jsonSchemaFields(indirectJSONType(field.Type)))
			continue
		}
		tag := field.Tag.Get("json")
		name, options, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = replayJSONField{
			typ:      field.Type,
			required: !strings.Contains(options, "omitempty") && !strings.Contains(options, "omitzero"),
		}
	}
	return fields
}

func indirectJSONType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

func isJSONScalarType(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.String:
		return true
	default:
		return false
	}
}

// jsonObjectKeys returns object keys while detecting duplicates. Values remain
// raw so their own nested schemas can be checked by validateReplayJSON.
func jsonObjectKeys(data []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("must be a JSON object")
	}
	keys := make([]string, 0)
	present := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid JSON object key")
		}
		if _, duplicate := present[key]; duplicate {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		present[key] = struct{}{}
		keys = append(keys, key)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return keys, requireJSONEOF(decoder)
}

// validateReplayJSONUnknown permits arbitrary raw input values while still
// rejecting duplicate keys at every nested object level.
func validateReplayJSONUnknown(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateReplayJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func validateReplayJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		present := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := present[key]; duplicate {
				return fmt.Errorf("duplicate field %q", key)
			}
			present[key] = struct{}{}
			if err := validateReplayJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := validateReplayJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func readCacheHeader(br *replayRecordReader, proof logproof.CacheProof) (CacheHeader, bool) {
	headerLine, err := br.ReadRecord()
	if err != nil {
		return CacheHeader{}, false
	}
	if err := validateReplayJSON(headerLine, reflect.TypeFor[CacheHeader]()); err != nil {
		return CacheHeader{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(headerLine))
	decoder.DisallowUnknownFields()
	var h CacheHeader
	if err := decoder.Decode(&h); err != nil || requireJSONEOF(decoder) != nil {
		return CacheHeader{}, false
	}
	ok := h.Version == CacheVersion && h.LogDevice == proof.Device && h.LogInode == proof.Inode && h.LogSize == proof.Size &&
		h.LogModNs == proof.ModTimeNs && h.AuthorityVersion == proof.Version &&
		h.AuthorityHarness == proof.Harness && h.RawHeader == proof.RawHeader
	return h, ok
}

// openReplayBody opens one sidecar descriptor and decoder at its header.
func openReplayBody(logPath string) (*os.File, *zstd.Decoder, *replayRecordReader, error) {
	file, err := os.Open(filepath.Clean(CachePath(logPath)))
	if err != nil {
		return nil, nil, nil, err
	}
	decoder, err := zstd.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, nil, nil, err
	}
	return file, decoder, newReplayRecordReader(decoder), nil
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

func deltaToolUseID(m agent.Message) string {
	if delta, ok := m.(*agent.ToolOutputDeltaMessage); ok {
		return delta.ToolUseID
	}
	return ""
}

func finalToolUseID(m agent.Message) string {
	if result, ok := m.(*agent.ToolResultMessage); ok {
		return result.ToolUseID
	}
	return ""
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
