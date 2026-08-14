// Replay-cache regeneration bridges validated task-log scans to API v1 replay sidecars.

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	"github.com/caic-xyz/caic/backend/internal/logproof"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/task"
)

const replayCacheVersion = 6

var replayFormat = eventreplay.Format{Version: replayCacheVersion, ValidateLine: apiconv.ValidateEventJSON}

func resolveReplayProof(logPath string, prove eventreplay.ProofProvider) (logproof.CacheProof, error) {
	if prove == nil {
		return logproof.CacheProof{}, errors.New("replay cache proof provider is nil")
	}
	return prove(logPath)
}

// ReplayPublisher bridges validated task-log scans to v1 replay-sidecar
// storage. Its dedicated temporary directory must share a filesystem with task
// logs so publication remains an atomic rename.
type ReplayPublisher struct {
	tempDir string
}

// NewReplayPublisher creates a replay publisher with an owned temporary
// directory.
func NewReplayPublisher(tempDir string) (*ReplayPublisher, error) {
	if tempDir == "" {
		return nil, errors.New("replay temporary directory is required")
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create replay temporary directory: %w", err)
	}
	return &ReplayPublisher{tempDir: tempDir}, nil
}

// Publish publishes the derived replay cache for a closed, EOF-validated task
// log. The raw task log remains authoritative when it fails.
func (p *ReplayPublisher) Publish(ctx context.Context, lt *task.LoadedTask) error {
	return p.regenerate(ctx, lt)
}

// Prune removes stale replay caches and interrupted artifacts from this
// publisher's dedicated temporary directory.
func (p *ReplayPublisher) Prune(logDir string, prove eventreplay.ProofProvider) (int, error) {
	tempErr := eventreplay.PruneTemporaryArtifacts(p.tempDir)
	removed, cacheErr := eventreplay.PruneStaleCaches(logDir, prove, replayFormat)
	return removed, errors.Join(tempErr, cacheErr)
}

// regenerate rebuilds the v1 replay sidecar from one completed task-log scan.
// Cancellation leaves neither a replay cache nor a disk spool.
func (p *ReplayPublisher) regenerate(ctx context.Context, lt *task.LoadedTask) error {
	return p.regenerateSource(ctx, lt.LogPath(), lt.CacheProofForLog, lt.ScanMessagesWithContext)
}

func (p *ReplayPublisher) regenerateSource(ctx context.Context, logPath string, prove eventreplay.ProofProvider, source replaySource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	initialProof, err := resolveReplayProof(logPath, prove)
	if err != nil {
		return fmt.Errorf("prove replay log authority: %w", err)
	}
	cache, err := eventreplay.NewCacheWriter(logPath, p.tempDir, prove, replayFormat)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			cache.Abort()
		}
	}()
	filter := newReplayDiskFilter(cache, initialProof.Harness, time.Now())
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
	cache.AllowEmpty()
	if err := cache.CommitExactContext(ctx, logPath, proof); err != nil {
		return err
	}
	committed = true
	return nil
}

// replaySource scans one raw log through EOF, passing parsed conversation
// messages to yield, and returns the completed scan's exact authority proof.
type replaySource func(context.Context, func(agent.ParsedMessage) error) (logproof.CacheProof, error)

const maxPendingReplayBytes = 64 << 10

// replayDiskFilter compacts a regeneration scan. It keeps small delta runs in
// a fixed-size memory buffer and spills only a run that exceeds
// maxPendingReplayBytes. A matching completed message discards pending output;
// every other boundary copies it to the cache in event order.
type replayDiskFilter struct {
	cache             *eventreplay.CacheWriter
	tracker           *apiconv.ToolTimingTracker
	fixedNow          time.Time
	pendingBuffer     bytes.Buffer
	pending           *eventreplay.Spool
	pendingKind       int
	pendingToolUseID  string
	cleanTurnComplete bool
}

func newReplayDiskFilter(cache *eventreplay.CacheWriter, h harness.Name, observed time.Time) *replayDiskFilter {
	return &replayDiskFilter{
		cache:    cache,
		tracker:  apiconv.NewToolTimingTracker(h, apiconv.FormatToolOutput),
		fixedNow: observed,
	}
}

// Write buffers a small delta run and creates a disk spool only once the fixed
// memory limit is exceeded.
func (f *replayDiskFilter) Write(data []byte) (int, error) {
	if f.pending != nil {
		return f.pending.Write(data)
	}
	if f.pendingBuffer.Len()+len(data) <= maxPendingReplayBytes {
		return f.pendingBuffer.Write(data)
	}
	pending, err := f.cache.NewSpool()
	if err != nil {
		f.cache.RecordError(err)
		return 0, err
	}
	f.pending = pending
	if _, err := pending.Write(f.pendingBuffer.Bytes()); err != nil {
		f.cache.RecordError(fmt.Errorf("spill replay pending buffer: %w", err))
		return 0, err
	}
	f.pendingBuffer.Reset()
	return pending.Write(data)
}

func (f *replayDiskFilter) observation(parsed agent.ParsedMessage) time.Time {
	if !parsed.ProducerTime.IsZero() {
		return parsed.ProducerTime
	}
	return f.fixedNow
}

func (f *replayDiskFilter) writeConverted(dst io.Writer, parsed agent.ParsedMessage) {
	evs := f.tracker.ConvertMessage(parsed.Message, f.observation(parsed))
	for i := range evs {
		data, err := apiconv.MarshalEvent(&evs[i])
		if err != nil {
			f.cache.RecordError(fmt.Errorf("marshal replay event: %w", err))
			return
		}
		if _, err := dst.Write(data); err != nil {
			f.cache.RecordError(fmt.Errorf("write replay event: %w", err))
			return
		}
		if _, err := dst.Write([]byte{'\n'}); err != nil {
			f.cache.RecordError(fmt.Errorf("write replay event terminator: %w", err))
			return
		}
	}
}

func (f *replayDiskFilter) emit(parsed agent.ParsedMessage) {
	// CacheWriter owns its body synchronization. Convert one bounded semantic
	// record at a time to retain the scanner's record-size memory ceiling.
	evs := f.tracker.ConvertMessage(parsed.Message, f.observation(parsed))
	for i := range evs {
		data, err := apiconv.MarshalEvent(&evs[i])
		if err != nil {
			f.cache.RecordError(fmt.Errorf("marshal replay event: %w", err))
			return
		}
		f.cache.WriteData(data)
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
		f.cache.RecordError(f.pending.Discard())
	}
	f.pendingBuffer.Reset()
	f.pending, f.pendingKind, f.pendingToolUseID = nil, 0, ""
}

func (f *replayDiskFilter) flushPendingContext(ctx context.Context) error {
	if f.pendingKind == 0 {
		return nil
	}
	defer f.discardPending()
	if f.pending != nil {
		return f.cache.AppendSpool(ctx, f.pending)
	}
	if err := ctx.Err(); err != nil {
		f.cache.RecordError(err)
		return err
	}
	if f.pendingBuffer.Len() > 0 {
		f.cache.WriteDataBytes(f.pendingBuffer.Bytes())
	}
	return ctx.Err()
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

func (f *replayDiskFilter) flushContext(ctx context.Context) error {
	return f.flushPendingContext(ctx)
}

func (f *replayDiskFilter) close() {
	f.discardPending()
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
