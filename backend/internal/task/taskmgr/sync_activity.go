// Records one sync of a task in its log as a start, bounded hook output chunks and an end.

package taskmgr

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

const (
	// maxSyncOutputBytes bounds the hook output persisted for one sync. A
	// verbose hook such as a dependency install fits in the head; the failure
	// that matters is also carried, bounded, by the end record's error.
	maxSyncOutputBytes = 256 << 10
	// maxSyncChunkBytes bounds one output record.
	maxSyncChunkBytes = 16 << 10
	// syncFlushInterval is the minimum delay between two output flushes. The
	// first output flushes at once; a PushTimeout of 10 minutes therefore
	// bounds a push to about 600 output records however slowly it trickles.
	syncFlushInterval = time.Second
	// maxSyncErrorBytes bounds the error text of an end record.
	maxSyncErrorBytes = 8 << 10
)

// syncInterruptedError is the end-record error of a sync whose server stopped.
const syncInterruptedError = "interrupted: the server stopped during the push"

// endInterruptedSync writes the failed end record of the sync syncID, whose
// start record a previous server process wrote without an end. It runs once at
// restore, before the task can start a sync in this process, and names the
// sync so it never closes another one.
func endInterruptedSync(ctx context.Context, log *slog.Logger, e *Entry, syncID string) {
	end := &agent.SyncActivityMessage{
		SyncID: syncID,
		Phase:  v3.SyncPhaseEnd,
		Ts:     float64(time.Now().UnixMilli()) / 1000,
		Status: v3.SyncStatusFailed,
		Error:  syncInterruptedError,
	}
	if err := e.RecordHost(ctx, end); err != nil {
		log.WarnContext(ctx, "recording interrupted sync failed", "sync", syncID, "err", err)
	}
}

// syncRecorder writes the log records of one sync attempt. Its Write method is
// the hook output sink: it only buffers, so a slow log append never stalls the
// hook, and a background flusher turns the buffer into output records.
type syncRecorder struct {
	record  func(context.Context, agent.Message) error
	log     *slog.Logger
	ctx     context.Context
	id      string
	started time.Time

	wake chan struct{} // Capacity 1: a pending signal covers any number of writes.
	done chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	pending  []byte
	accepted int
	dropped  int
	failed   error
}

// startSyncRecorder appends the start record and begins flushing output.
// ctx must outlive the request: the end record is written even after the
// caller gave up. A failing record function stops the output records and is
// reported once to the sink's writer, so the hook runtime stops sending.
func startSyncRecorder(ctx context.Context, log *slog.Logger, record func(context.Context, agent.Message) error, target SyncTarget, branch string) *syncRecorder {
	s := &syncRecorder{
		record:  record,
		log:     log,
		ctx:     ctx,
		id:      ksid.NewID().String(),
		started: time.Now(),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	s.append(&agent.SyncActivityMessage{Phase: v3.SyncPhaseStart, Target: string(target), Branch: branch})
	s.wg.Go(s.flushLoop)
	return s
}

// Write buffers hook output without blocking on the log. Output past the
// per-sync bound is counted and dropped.
func (s *syncRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return 0, s.failed
	}
	n := min(len(p), maxSyncOutputBytes-s.accepted)
	s.pending = append(s.pending, p[:n]...)
	s.accepted += n
	s.dropped += len(p) - n
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return len(p), nil
}

// finish flushes the remaining output, marks a truncation, and appends the end
// record with the status and, when non-nil, the bounded error text.
func (s *syncRecorder) finish(status string, err error) {
	close(s.done)
	s.wg.Wait()
	s.flush(true)
	s.mu.Lock()
	dropped := s.dropped
	s.mu.Unlock()
	if dropped > 0 {
		s.append(&agent.SyncActivityMessage{Phase: v3.SyncPhaseOutput, Truncated: true, Output: fmt.Sprintf("[output truncated: %d bytes omitted after the first %d]\n", dropped, maxSyncOutputBytes)})
	}
	end := &agent.SyncActivityMessage{Phase: v3.SyncPhaseEnd, Status: status, DurationMS: time.Since(s.started).Milliseconds()}
	if err != nil {
		end.Error = truncateUTF8(err.Error(), maxSyncErrorBytes)
	}
	s.append(end)
}

func (s *syncRecorder) flushLoop() {
	for {
		select {
		case <-s.wake:
		case <-s.done:
			return
		}
		s.flush(false)
		select {
		case <-time.After(syncFlushInterval):
		case <-s.done:
			return
		}
	}
}

// flush appends the buffered output as chunked records. Unless final, an
// incomplete trailing rune stays buffered for the next flush.
func (s *syncRecorder) flush(final bool) {
	s.mu.Lock()
	n := len(s.pending)
	if !final {
		n = completeUTF8Prefix(s.pending)
	}
	chunk := strings.ToValidUTF8(string(s.pending[:n]), "�")
	s.pending = append(s.pending[:0], s.pending[n:]...)
	s.mu.Unlock()
	for chunk != "" {
		end := min(len(chunk), maxSyncChunkBytes)
		for end < len(chunk) && !utf8.RuneStart(chunk[end]) {
			end--
		}
		if !s.append(&agent.SyncActivityMessage{Phase: v3.SyncPhaseOutput, Output: chunk[:end]}) {
			return
		}
		chunk = chunk[end:]
	}
}

// append stamps and records m. A failure is logged and stops further output.
func (s *syncRecorder) append(m *agent.SyncActivityMessage) bool {
	m.SyncID = s.id
	m.Ts = float64(time.Now().UnixMilli()) / 1000
	if err := s.record(s.ctx, m); err != nil {
		s.log.WarnContext(s.ctx, "recording sync activity failed", "sync", s.id, "phase", m.Phase, "err", err)
		s.mu.Lock()
		s.failed = err
		s.pending = nil
		s.mu.Unlock()
		return false
	}
	return true
}

// completeUTF8Prefix returns the length of b without a trailing incomplete rune.
func completeUTF8Prefix(b []byte) int {
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return len(b) - i
			}
			break
		}
	}
	return len(b)
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary, marking the cut.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…[truncated]"
}
