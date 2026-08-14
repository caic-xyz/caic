// Package eventreplay stores proof-bound replay sidecars served through SSE.
//
// A sidecar (<task>.events.zst) is replaceable derived data: its header binds a
// caller-provided line format to the authoritative raw-log identity. Storage
// validates every record through EOF before streaming it as "data: <line>" SSE
// frames, without knowing the raw-log parser or API schema.
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

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/logproof"
)

// Format defines the versioned line schema of one replay sidecar.
//
// ValidateLine must reject every line that cannot safely be served as an SSE
// data frame. Version must be positive.
type Format struct {
	Version      int
	ValidateLine func([]byte) error
}

const maxReplayRecordBytes = 32 << 20

var errReplayRecordTooLarge = errors.New("replay cache record exceeds size limit")

func (f Format) valid() bool {
	return f.Version > 0 && f.ValidateLine != nil
}

// ProofProvider returns a raw-header and identity observation. It is injected
// by the task layer so eventreplay can compare cache identity without importing
// or reimplementing task-log validation.
type ProofProvider func(string) (logproof.CacheProof, error)

// CacheHeader is the first JSONL record of a replay cache. It binds the cache
// to a specific raw log file so a changed or replaced log invalidates it.
type CacheHeader struct {
	Version int                 `json:"v"`
	Proof   logproof.CacheProof `json:"proof"`
	Empty   bool                `json:"empty,omitempty"`
}

// CachePath returns the sidecar cache path for a task log path. The
// ".events.zst" suffix is deliberately not a task-log name, so the log scanner
// never mistakes a cache for a log.
func CachePath(logPath string) string {
	return cachePathForBase(trimLogExt(logPath))
}

// PruneStaleCaches removes replay sidecars that no longer match a raw task log.
// It also removes orphaned temp files left by interrupted cache regeneration.
func PruneStaleCaches(logDir string, prove ProofProvider, format Format) (int, error) {
	if !format.valid() {
		return 0, errors.New("replay cache format is invalid")
	}
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
			_, closeFn, freshness := freshCacheBody(logPath, prove, format)
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

// PruneTemporaryArtifacts removes interrupted build artifacts from a dedicated
// replay temporary directory.
func PruneTemporaryArtifacts(tempDir string) error {
	entries, err := os.ReadDir(tempDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(tempDir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Replay is one validated, open replay sidecar.
type Replay struct {
	logPath string
	proof   logproof.CacheProof
	prove   ProofProvider
	format  Format
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
func OpenReplay(logPath string, prove ProofProvider, format Format) (*Replay, bool) {
	if !format.valid() {
		return nil, false
	}
	proof, err := resolveProof(logPath, prove)
	if err != nil {
		return nil, false
	}
	file, decoder, br, err := openReplayBody(logPath)
	if err != nil {
		return nil, false
	}
	header, ok := readCacheHeader(br, proof, format)
	r := &Replay{logPath: logPath, proof: proof, prove: prove, format: format, file: file, decoder: decoder, br: br, empty: header.Empty}
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

// WriteSSE streams cached records as SSE, starting at *idx and updating it for
// callers that append stats or ready events afterward. The body is trusted
// after OpenReplay's same-file EOF validation, so each line is copied directly
// into a data frame without decoding.
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
		if err := r.format.ValidateLine(line); err != nil {
			slog.Warn("replay cache: invalid body", "path", CachePath(r.logPath), "err", err)
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
	header, ok := readCacheHeader(r.br, r.proof, r.format)
	return ok && header.Empty == r.empty
}

// Spool is a cache-owned temporary file for a bridge that needs to defer
// writing a bounded sequence of cache records.
type Spool struct {
	file *os.File
}

// Write appends data to the spool.
func (s *Spool) Write(data []byte) (int, error) {
	return s.file.Write(data)
}

// Discard closes and removes the spool.
func (s *Spool) Discard() error {
	return errors.Join(s.file.Close(), os.Remove(s.file.Name()))
}

// CacheWriter accumulates validated-format body bytes and commits them with a
// final header bound to the final raw log identity.
type CacheWriter struct {
	bodyName   string
	dst        string
	tempDir    string
	prove      ProofProvider
	format     Format
	allowEmpty bool

	mu   sync.Mutex
	body *os.File
	werr error
}

// NewCacheWriter opens an empty temp body file for a complete regeneration.
// Full rebuilds never seed from a prior derived cache.
func NewCacheWriter(logPath, tempDir string, prove ProofProvider, format Format) (*CacheWriter, error) {
	if !format.valid() {
		return nil, errors.New("replay cache format is invalid")
	}
	if tempDir == "" {
		return nil, errors.New("replay cache temporary directory is required")
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create replay cache temporary directory: %w", err)
	}
	dst := CachePath(logPath)
	body, err := os.CreateTemp(tempDir, filepath.Base(dst)+".*.body")
	if err != nil {
		return nil, fmt.Errorf("create replay cache temp: %w", err)
	}
	return &CacheWriter{body: body, bodyName: body.Name(), dst: dst, tempDir: tempDir, prove: prove, format: format}, nil
}

// WriteData appends one validated line to the cache, recording the first error
// so Commit can discard a partial cache.
func (w *CacheWriter) WriteData(data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeLineLocked(data)
}

// WriteDataBytes appends already newline-delimited validated records from a
// bounded bridge buffer. They are never used for an untrusted cache read.
func (w *CacheWriter) WriteDataBytes(data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.werr != nil || w.body == nil || len(data) == 0 {
		return
	}
	if _, err := w.body.Write(data); err != nil {
		w.werr = err
	}
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

// CommitExactContext finalizes a regenerated cache only while ctx remains
// active and the raw log exactly matches the completed semantic scan.
func (w *CacheWriter) CommitExactContext(ctx context.Context, logPath string, proof logproof.CacheProof) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.commitLocked(ctx, logPath, proof)
}

// AllowEmpty permits committing a completed scan that emitted no body lines.
func (w *CacheWriter) AllowEmpty() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.allowEmpty = true
}

// Abort discards an uncommitted cache body.
func (w *CacheWriter) Abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.abortLocked()
}

// NewSpool creates a cache-owned temporary spool.
func (w *CacheWriter) NewSpool() (*Spool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.body == nil {
		return nil, errors.New("replay cache writer is closed")
	}
	file, err := os.CreateTemp(w.tempDir, filepath.Base(w.dst)+".*.pending")
	if err != nil {
		return nil, fmt.Errorf("create replay pending spool: %w", err)
	}
	return &Spool{file: file}, nil
}

// AppendSpool copies a completed spool into the cache body.
func (w *CacheWriter) AppendSpool(ctx context.Context, spool *Spool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.werr != nil || w.body == nil {
		return w.werr
	}
	if err := ctx.Err(); err != nil {
		w.werr = errors.Join(w.werr, err)
		return err
	}
	if _, err := spool.file.Seek(0, io.SeekStart); err != nil {
		w.werr = err
		return err
	}
	if err := copyWithContext(ctx, w.body, spool.file); err != nil {
		w.werr = errors.Join(w.werr, err)
		return err
	}
	return nil
}

// RecordError prevents publication after a bridge write failure.
func (w *CacheWriter) RecordError(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.werr = errors.Join(w.werr, err)
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

func (w *CacheWriter) commitLocked(ctx context.Context, logPath string, expectedProof logproof.CacheProof) error {
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
	case publicationProof != expectedProof:
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
	tmp, err := os.CreateTemp(w.tempDir, filepath.Base(w.dst)+".*.tmp")
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
		Version: w.format.Version,
		Proof:   publicationProof,
		Empty:   empty && w.allowEmpty,
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
	// before rename. The raw log must still exactly name the semantic scan.
	if err := ctx.Err(); err != nil {
		removeErr := os.Remove(tmpName)
		return fmt.Errorf("discard replay cache %s: %w", w.dst, errors.Join(err, removeErr))
	}
	finalProof, proofErr := resolveProof(logPath, w.prove)
	matches := proofErr == nil && finalProof == publicationProof
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
func freshCacheBody(logPath string, prove ProofProvider, format Format) (*replayRecordReader, func(), cacheFreshness) {
	proof, err := resolveProof(logPath, prove)
	if err != nil {
		return nil, nil, cacheUnverifiable
	}
	return freshCacheBodyWithProof(logPath, proof, format)
}

func freshCacheBodyWithProof(logPath string, proof logproof.CacheProof, format Format) (*replayRecordReader, func(), cacheFreshness) {
	file, decoder, br, err := openReplayBody(logPath)
	if err != nil {
		return nil, nil, cacheStale
	}
	closeFn := func() {
		decoder.Close()
		_ = file.Close()
	}
	header, ok := readCacheHeader(br, proof, format)
	if !ok || !cacheBodyEOFValid(br, header.Empty, format) {
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
	if _, ok := readCacheHeader(br, proof, format); !ok {
		closeFn()
		return nil, nil, cacheStale
	}
	return br, closeFn, cacheFresh
}

// cacheBodyEOFValid verifies decompression and every formatted record through
// EOF before a cache can remain fresh for pruning. A valid header alone is
// never sufficient authority for a nonempty cache.
func cacheBodyEOFValid(br *replayRecordReader, empty bool, format Format) bool {
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
		if format.ValidateLine(line) != nil {
			return false
		}
		hasBody = true
	}
}

// validateReplayJSON recursively enforces the strict replay-header schema.
// encoding/json accepts unknown and duplicate object keys, so cache validation
// inspects every nested object before decoding it into the header type.
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

func readCacheHeader(br *replayRecordReader, proof logproof.CacheProof, format Format) (CacheHeader, bool) {
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
	return h, h.Version == format.Version && h.Proof == proof
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
