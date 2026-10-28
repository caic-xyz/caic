// Package history incrementally projects raw task records into immutable chunks and compact pricing observations.
package history

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"path/filepath"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/server/history/data"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// RecordsPerChunk partitions one-based physical record IDs independently of
// normalized output cardinality. Every nonempty record, including the header
// and zero-output controls, occupies one position.
const RecordsPerChunk uint64 = 256

// Event identifies one API projection attributable to a physical raw record.
// Index is zero-based across all outputs from that record, including outputs
// emitted by different semantic messages in the same native record.
type Event = v1.HistoryEvent

// Snapshot identifies the complete raw-record prefix projected at one instant.
// A generation owner must not reuse it with a different Source.
type Snapshot struct {
	Through uint64
}

// Source owns one generation's native parser, API projection state and a
// private disk spool. Request callers share serialized incremental recovery;
// ranges read the projected spool without re-reading or parsing raw prefixes.
// Only one raw record and one requested range are materialized at a time.
// Sparse chunk offsets, rather than decoded events, remain in memory.
// The task service owns its lifetime and calls Close on lifecycle replacement
// or shutdown. A projection error is sticky: discard the source to recover.
type Source struct {
	gate    chan struct{}
	cursor  *taskslog.RecordCursor
	tracker *apiconv.ToolTimingTracker
	spool   *os.File
	writer  *bufio.Writer
	starts  []int64
	through uint64
	end     int64
	journal *costJournal
	err     error
	clean   bool
}

// NewSource creates a lazy projection source. No task-history replay occurs
// until Refresh. Projection is a pure function of the ordered physical log:
// native costs and usage are retained, while live pricing estimates and the
// latest-visible-plan lookup belong to revalidated endpoints.
func NewSource(ctx context.Context, path string, resolver taskslog.WireResolver) (*Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cursor, err := taskslog.OpenRecordCursor(path, resolver)
	if err != nil {
		return nil, err
	}
	f, err := openProjectionSpool(filepath.Dir(path))
	if err != nil {
		return nil, errors.Join(err, cursor.Close())
	}
	tracker := apiconv.NewToolTimingTracker(cursor.Harness, cursor.RequestedModel, nil)
	journal, err := newCostJournal(filepath.Dir(path))
	if err != nil {
		return nil, errors.Join(err, cursor.Close(), f.Close())
	}
	return &Source{gate: make(chan struct{}, 1), cursor: cursor, tracker: tracker, spool: f, writer: bufio.NewWriterSize(f, 64<<10), journal: journal}, nil
}

// Snapshot returns the already projected prefix without inspecting or parsing
// newly appended raw records. Immutable range requests use it before deciding
// whether cold recovery is needed, so old ranges never project an unrelated tail.
func (s *Source) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := s.lock(ctx); err != nil {
		return Snapshot{}, err
	}
	defer s.unlock()
	return Snapshot{Through: s.through}, s.err
}

// Refresh projects newly appended complete records and returns their snapshot.
// Concurrent refreshes share parser state; followers consume only a suffix
// appended after the preceding caller's captured file size. Cancellation leaves
// completed record projections usable and does not discard preceding work.
func (s *Source) Refresh(ctx context.Context) (Snapshot, error) {
	if err := s.lock(ctx); err != nil {
		return Snapshot{}, err
	}
	defer s.unlock()
	if s.err != nil {
		return Snapshot{}, s.err
	}
	for record, err := range s.cursor.Read(ctx) {
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.err = err
			}
			return Snapshot{}, err
		}
		var events []Event
		for _, parsed := range record.Messages {
			m := parsed.Message
			index := uint32(len(events)) //nolint:gosec // A 32 MiB record cannot emit 2^32 projections.
			if fact := costFactFor(parsed, Position{RecordID: record.ID, Index: index}); fact != nil {
				if err := s.journal.append(fact); err != nil {
					s.err = err
					return Snapshot{}, err
				}
			}
			// Successful turn results supersede a trailing relay exit failure,
			// matching the canonical task history's exit reconciliation.
			if exit, ok := m.(*agent.ExitMessage); ok && exit.ExitCode != 0 && s.clean {
				continue
			}
			if task.ClearsExitError(m) {
				s.clean = false
			}
			if result, ok := m.(*agent.ResultMessage); ok && !result.Source.IsChild() {
				s.clean = !result.IsError
			}
			projected := s.tracker.ConvertMessage(m, parsed.ProducerTime)
			for i := range projected {
				events = append(events, Event{RecordID: record.ID, Index: uint32(len(events)), Message: projected[i]}) //nolint:gosec // A 32 MiB record cannot emit 2^32 projections.
			}
		}
		encoded := []byte("[]")
		if len(events) != 0 {
			var err error
			encoded, err = json.Marshal(events)
			if err != nil {
				s.err = fmt.Errorf("encode projected history: %w", err)
				return Snapshot{}, s.err
			}
		}
		var frame [8]byte
		binary.LittleEndian.PutUint64(frame[:], uint64(len(encoded)))
		if _, err := s.writer.Write(frame[:]); err != nil {
			s.err = err
			return Snapshot{}, err
		}
		if _, err := s.writer.Write(encoded); err != nil {
			s.err = err
			return Snapshot{}, err
		}
		if (record.ID-1)%RecordsPerChunk == 0 {
			s.starts = append(s.starts, s.end)
		}
		s.end += int64(len(frame)) + int64(len(encoded))
		s.through = record.ID
	}
	if err := s.writer.Flush(); err != nil {
		s.err = err
		return Snapshot{}, err
	}
	return Snapshot{Through: s.through}, nil
}

// WriteRange streams compact JSON Event[] for one inclusive chunk range. Its
// memory use is bounded by one physical record rather than the chunk's total
// payload. HTTP consumers should use it instead of materializing a large range.
func (s *Source) WriteRange(ctx context.Context, snapshot Snapshot, first, last uint64, w io.Writer) error {
	writer := bufio.NewWriterSize(w, 32<<10)
	if err := writer.WriteByte('['); err != nil {
		return err
	}
	comma := false
	for encoded, err := range s.frames(ctx, snapshot, first, last) {
		if err != nil {
			return err
		}
		if len(encoded) > 2 {
			if comma {
				if err := writer.WriteByte(','); err != nil {
					return err
				}
			}
			if _, err := writer.Write(encoded[1 : len(encoded)-1]); err != nil {
				return err
			}
			comma = true
		}
	}
	if err := writer.WriteByte(']'); err != nil {
		return err
	}
	return writer.Flush()
}

// Events yields normalized outputs in an inclusive raw-record range, reading
// one physical record at a time. Stopping releases its decoded working set.
// It never reads or reparses raw task records.
func (s *Source) Events(ctx context.Context, snapshot Snapshot, first, last uint64) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if err := s.checkState(ctx); err != nil {
			yield(Event{}, err)
			return
		}
		for first <= last {
			end := min(last, ((first-1)/RecordsPerChunk+1)*RecordsPerChunk)
			for encoded, err := range s.frames(ctx, snapshot, first, end) {
				if err != nil {
					yield(Event{}, err)
					return
				}
				dec := json.NewDecoder(bytes.NewReader(encoded))
				if _, err := dec.Token(); err != nil {
					yield(Event{}, err)
					return
				}
				for dec.More() {
					if err := ctx.Err(); err != nil {
						yield(Event{}, err)
						return
					}
					var event Event
					if err := dec.Decode(&event); err != nil {
						yield(Event{}, err)
						return
					}
					if !yield(event, nil) {
						return
					}
				}
			}
			first = end + 1
		}
	}
}

// Close releases the source's parser and removes its private projection spool.
// Its owner calls it once, after withdrawing the generation from discovery.
func (s *Source) Close() error {
	s.gate <- struct{}{}
	defer s.unlock()
	s.err = os.ErrClosed
	return errors.Join(s.cursor.Close(), s.spool.Close(), s.journal.file.Close())
}

// TurnCosts prices the requested raw-record prefix through last. Only compact
// observations are read; the tracker owns all harness and pricing semantics.
func (s *Source) TurnCosts(ctx context.Context, snapshot Snapshot, last uint64, pricer usage.ModelPricer) iter.Seq2[TurnEstimate, error] {
	return func(yield func(TurnEstimate, error) bool) {
		if err := s.lock(ctx); err != nil {
			yield(TurnEstimate{}, err)
			return
		}
		if s.err != nil {
			err := s.err
			s.unlock()
			yield(TurnEstimate{}, err)
			return
		}
		if last == 0 || last > snapshot.Through || snapshot.Through > s.through {
			s.unlock()
			yield(TurnEstimate{}, errors.New("invalid history cost prefix"))
			return
		}
		if err := s.journal.writer.Flush(); err != nil {
			s.err = err
			s.unlock()
			yield(TurnEstimate{}, err)
			return
		}
		end := s.journal.end
		s.unlock()
		tracker := task.CostTracker{Harness: s.cursor.Harness, Model: s.cursor.RequestedModel}
		for fact, err := range s.journal.facts(ctx, end, last) {
			if err != nil {
				yield(TurnEstimate{}, err)
				return
			}
			message, err := factMessage(&fact)
			if err != nil {
				yield(TurnEstimate{}, err)
				return
			}
			cost, known := tracker.Observe(message, fact.At, pricer)
			source := agent.MessageSource(fact.Source)
			if fact.Kind != data.KindResult || source.IsChild() {
				continue
			}
			if !known {
				cost = task.TurnCost{}
			}
			if !yield(TurnEstimate{RecordID: fact.RecordID, Index: fact.Index, Model: tracker.Model, Cost: cost, TotalUSD: tracker.TotalUSD}, nil) {
				return
			}
		}
	}
}

func (s *Source) lock(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Source) unlock() { <-s.gate }

func (s *Source) checkState(ctx context.Context) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	return s.err
}

// frames reads one record at a time from a sparse chunk offset. Refresh only
// appends, so the captured prefix remains immutable after the lock is released.
func (s *Source) frames(ctx context.Context, snapshot Snapshot, first, last uint64) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		if err := s.lock(ctx); err != nil {
			yield(nil, err)
			return
		}
		if s.err != nil {
			err := s.err
			s.unlock()
			yield(nil, err)
			return
		}
		if first == 0 || last < first || last > snapshot.Through || snapshot.Through > s.through || (first-1)/RecordsPerChunk != (last-1)/RecordsPerChunk {
			s.unlock()
			yield(nil, errors.New("invalid projected history range"))
			return
		}
		if err := s.writer.Flush(); err != nil {
			s.err = err
			s.unlock()
			yield(nil, err)
			return
		}
		index := (first - 1) / RecordsPerChunk
		offset, end := s.starts[index], s.end
		s.unlock()
		for id := index*RecordsPerChunk + 1; id <= last; id++ {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			var frame [8]byte
			if _, err := s.spool.ReadAt(frame[:], offset); err != nil {
				yield(nil, fmt.Errorf("read projected history frame: %w", err))
				return
			}
			encoded := binary.LittleEndian.Uint64(frame[:])
			if encoded > math.MaxInt64 {
				yield(nil, errors.New("invalid projected history frame length"))
				return
			}
			size := int64(encoded)
			offset += int64(len(frame))
			if size > end-offset || size < 2 {
				yield(nil, errors.New("invalid projected history frame"))
				return
			}
			if id >= first {
				encoded := make([]byte, size)
				if _, err := s.spool.ReadAt(encoded, offset); err != nil {
					yield(nil, fmt.Errorf("read projected history: %w", err))
					return
				}
				if !yield(encoded, nil) {
					return
				}
			}
			offset += size
		}
	}
}
