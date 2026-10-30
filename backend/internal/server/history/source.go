// Package history incrementally projects raw task records into immutable bodies, section summaries, and pricing observations.
package history

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"path/filepath"
	"slices"

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

// RecordEndIndex selects all outputs of a physical record in slice requests.
// Discovery responses always contain actual normalized positions instead.
const RecordEndIndex uint32 = math.MaxUint32

// Event identifies one API projection attributable to a physical raw record.
// Index is zero-based across all outputs from that record, including outputs
// emitted by different semantic messages in the same native record.
type Event = v1.HistoryEvent

// Snapshot identifies the complete raw-record prefix projected at one instant.
// A generation owner must not reuse it with a different Source.
type Snapshot struct {
	Through          uint64
	MetadataRevision uint64
}

// Source owns one generation's native parser, API projection state and a
// private disk spool. Request callers share serialized incremental recovery;
// ranges read the projected spool without re-reading or parsing raw prefixes.
// Only one raw record and one requested range are materialized at a time.
// One integer offset per physical record permits direct indexed reads without
// retaining decoded events. Activity/setup indexes retain record IDs only.
// Compact context, invocation, and activity indexes serve discovery separately.
// The task service owns its lifetime and calls Close on lifecycle replacement
// or shutdown. A projection error is sticky: discard the source to recover.
type Source struct {
	gate       chan struct{}
	cursor     *taskslog.RecordCursor
	tracker    *apiconv.ToolTimingTracker
	spool      *os.File
	writer     *bufio.Writer
	starts     []int64
	through    uint64
	end        int64
	journal    *costJournal
	sections   sectionIndex
	err        error
	exitStatus task.ExitStatus
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
	return &Source{gate: make(chan struct{}, 1), cursor: cursor, tracker: tracker, spool: f, writer: bufio.NewWriterSize(f, 64<<10), journal: journal, sections: newSectionIndex()}, nil
}

// Snapshot returns the already projected prefix without inspecting or parsing
// newly appended raw records. Immutable range requests use it before deciding
// whether cold recovery is needed, so old ranges never project an unrelated tail.
func (s *Source) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := s.lock(ctx); err != nil {
		return Snapshot{}, err
	}
	defer s.unlock()
	return Snapshot{Through: s.through, MetadataRevision: s.sections.revision}, s.err
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
	var encoded bytes.Buffer
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
			if !s.exitStatus.Observe(m) {
				continue
			}
			projected := s.tracker.ConvertMessage(m, parsed.ProducerTime)
			for i := range projected {
				events = append(events, Event{RecordID: record.ID, Index: uint32(len(events)), Message: projected[i]}) //nolint:gosec // A 32 MiB record cannot emit 2^32 projections.
			}
		}
		encoded.Reset()
		for i := range events {
			s.sections.append(&events[i])
		}
		encoded.WriteByte('[')
		encoder := json.NewEncoder(&encoded)
		for i := range events {
			if i != 0 {
				encoded.WriteByte(',')
			}
			if err := encoder.Encode(&events[i]); err != nil {
				s.err = fmt.Errorf("encode projected history: %w", err)
				return Snapshot{}, s.err
			}
		}
		encoded.WriteByte(']')
		payload := encoded.Bytes()
		var frame [8]byte
		binary.LittleEndian.PutUint64(frame[:], uint64(len(payload)))
		if _, err := s.writer.Write(frame[:]); err != nil {
			s.err = err
			return Snapshot{}, err
		}
		if _, err := s.writer.Write(payload); err != nil {
			s.err = err
			return Snapshot{}, err
		}
		s.starts = append(s.starts, s.end)
		s.end += int64(len(frame)) + int64(len(payload))
		s.through = record.ID
	}
	if err := s.writer.Flush(); err != nil {
		s.err = err
		return Snapshot{}, err
	}
	return Snapshot{Through: s.through, MetadataRevision: s.sections.revision}, nil
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

// Sections returns a coherent discovery snapshot. The exact raw prefix and
// summaries are captured together, so clients can subscribe after that cursor
// before fetching bodies. Returned values never alias subsequently mutated state.
func (s *Source) Sections(ctx context.Context, initialPrompt string) (Snapshot, []v1.HistorySection, v1.HistoryMetadata, error) {
	if err := s.lock(ctx); err != nil {
		return Snapshot{}, nil, v1.HistoryMetadata{}, err
	}
	defer s.unlock()
	if s.err != nil {
		return Snapshot{}, nil, v1.HistoryMetadata{}, s.err
	}
	sections, metadata := s.sections.snapshot()
	metadata.HasInitialPrompt = s.sections.initialEligible && s.sections.initialDigest == sha256.Sum256([]byte(initialPrompt))
	return Snapshot{Through: s.through, MetadataRevision: s.sections.revision}, sections, metadata, nil
}

// CheckSlice validates slice bounds before HTTP headers are sent. A zero first
// index starts at a physical record even when it emits no normalized outputs.
func (s *Source) CheckSlice(ctx context.Context, snap Snapshot, body v1.HistorySlice) error {
	if err := validSlice(body, snap.Through); err != nil {
		return err
	}
	var positions []Position
	if body.First.Index != 0 {
		positions = append(positions, body.First)
	}
	if body.Last.Index != RecordEndIndex {
		if len(positions) > 0 && body.Last.RecordID == body.First.RecordID {
			positions[0] = body.Last
		} else {
			positions = append(positions, body.Last)
		}
	}
	for _, p := range positions {
		found := false
		for frame, err := range s.frames(ctx, snap, p.RecordID, p.RecordID) {
			if err != nil {
				return err
			}
			// The spool encoder emits exactly one newline per normalized output.
			if int64(bytes.Count(frame, []byte{'\n'})) > int64(p.Index) {
				found = true
				break
			}
		}
		if !found {
			return errors.New("history slice endpoint does not exist")
		}
	}
	return nil
}

// WriteSlice streams inclusive normalized positions without including adjacent
// outputs from the same physical record. It forwards framed JSON lines without
// decoding messages or replaying native raw history. RecordEndIndex includes all
// outputs of the final physical record, including an outputless control.
func (s *Source) WriteSlice(ctx context.Context, snap Snapshot, body v1.HistorySlice, w io.Writer) error {
	if err := validSlice(body, snap.Through); err != nil {
		return err
	}
	writer := bufio.NewWriterSize(w, 32<<10)
	if _, err := writer.WriteString("[\n"); err != nil {
		return err
	}
	comma := false
	for first := body.First.RecordID; first <= body.Last.RecordID; {
		last := min(body.Last.RecordID, ((first-1)/RecordsPerChunk+1)*RecordsPerChunk)
		record := first
		for frame, err := range s.frames(ctx, snap, first, last) {
			if err != nil {
				return err
			}
			index := uint32(0)
			for line := range bytes.SplitSeq(frame[1:len(frame)-1], []byte{'\n'}) {
				if len(line) == 0 {
					continue
				}
				selected := (record != body.First.RecordID || index >= body.First.Index) && (record != body.Last.RecordID || index <= body.Last.Index)
				index++
				if !selected {
					continue
				}
				if line[0] == ',' {
					line = line[1:]
				}
				if comma {
					if err := writer.WriteByte(','); err != nil {
						return err
					}
				}
				if _, err := writer.Write(line); err != nil {
					return err
				}
				if err := writer.WriteByte('\n'); err != nil {
					return err
				}
				comma = true
			}
			record++
		}
		first = last + 1
	}
	if _, err := writer.WriteString("]\n"); err != nil {
		return err
	}
	return writer.Flush()
}

// WriteSetup streams only root startup log observations and the initial init.
// Later provisioning logs may be interleaved with conversation events; those
// intervening payloads must never leak into the setup disclosure resource.
func (s *Source) WriteSetup(ctx context.Context, snap Snapshot, through uint64, w io.Writer) error {
	if through == 0 || through > snap.Through {
		return errors.New("invalid history setup prefix")
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	if s.err != nil {
		err := s.err
		s.unlock()
		return err
	}
	records := appendRecordPrefix(nil, s.sections.setupRecords, through)
	s.unlock()
	events := func(yield func(Event, error) bool) {
		firstInit := false
		for e, err := range s.selectedEvents(ctx, snap, records) {
			if err != nil {
				yield(Event{}, err)
				return
			}
			if historyChild(&e.Message.Source) {
				continue
			}
			if e.Message.Log != nil || e.Message.Init != nil && !firstInit {
				if e.Message.Init != nil {
					firstInit = true
				}
				if !yield(e, nil) {
					return
				}
			}
		}
	}
	return writeHistoryEvents(w, events)
}

// ActivityStart locates one known activity's earliest observed position. It
// bounds a disclosure scan independently of the task's earlier raw history.
func (s *Source) ActivityStart(ctx context.Context, kind v1.EventKind, id string) (Position, error) {
	if err := s.lock(ctx); err != nil {
		return Position{}, err
	}
	defer s.unlock()
	if s.err != nil {
		return Position{}, s.err
	}
	a, ok := s.sections.activities[string(kind)+":"+id]
	if !ok {
		return Position{}, errors.New("unknown history activity")
	}
	return a.First, nil
}

// WriteActivity streams one actor's lifecycle payloads and transcript through a
// finite record. It reads only indexed direct/delegated actor records from the
// projected spool. After is an exclusive normalized bound; RecordEndIndex skips
// all outputs of its record, including outputless checkpoints. A zero bound
// selects the full prefix. Current status remains manifest metadata.
func (s *Source) WriteActivity(ctx context.Context, snap Snapshot, kind v1.EventKind, id string, through uint64, after Position, w io.Writer) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	if s.err != nil {
		err := s.err
		s.unlock()
		return err
	}
	a, ok := s.sections.activities[string(kind)+":"+id]
	if !ok || through < a.First.RecordID || through > snap.Through || after.RecordID > through {
		s.unlock()
		return errors.New("invalid history activity prefix")
	}
	// Copy compact correlation facts while locked. Later discoveries must not
	// change membership in an already published finite-prefix resource.
	routes := make([]activityRoute, 0, len(s.sections.routing))
	for route, at := range s.sections.routing {
		if at <= through {
			routes = append(routes, route)
		}
	}
	pending := map[string]int{}
	for tool, scopes := range s.sections.pendingScopes {
		for _, at := range scopes {
			if at <= through {
				pending[tool]++
			}
		}
	}
	firstRecord := after.RecordID
	if after.Index == RecordEndIndex {
		firstRecord++
	}
	records := appendRecordRange(nil, s.sections.actorRecords[string(kind)+":"+id], firstRecord, through)
	if kind == v1.EventKindNativeSubagent {
		for key, ids := range s.sections.delegationRecords {
			for _, route := range routes {
				if route.id != id || route.tool != key.tool || route.namespace != "" && key.namespace != "" && route.namespace != key.namespace {
					continue
				}
				records = appendRecordRange(records, ids, firstRecord, through)
				break
			}
		}
	}
	s.unlock()
	slices.Sort(records)
	records = slices.Compact(records)
	events := func(yield func(Event, error) bool) {
		for e, err := range s.selectedEvents(ctx, snap, records) {
			if err != nil {
				yield(Event{}, err)
				return
			}
			if e.RecordID < after.RecordID || e.RecordID == after.RecordID && e.Index <= after.Index {
				continue
			}
			m := &e.Message
			matches := kind == v1.EventKindNativeSubagent && m.NativeSubagent != nil && m.NativeSubagent.ID == id || kind == v1.EventKindBackgroundCommand && m.BackgroundCommand != nil && m.BackgroundCommand.ID == id
			if !matches && kind == v1.EventKindNativeSubagent {
				matches = activityOwner(&m.Source, routes, pending) == id
			}
			if matches && !yield(e, nil) {
				return
			}
		}
	}
	return writeHistoryEvents(w, events)
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

func (s *Source) selectedEvents(ctx context.Context, snap Snapshot, records []uint64) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(Event{}, err)
			return
		}
		for i := 0; i < len(records); {
			first, last := records[i], records[i]
			i++
			for i < len(records) && records[i] == last+1 {
				last = records[i]
				i++
			}
			for e, err := range s.Events(ctx, snap, first, last) {
				if !yield(e, err) || err != nil {
					return
				}
			}
		}
	}
}

// frames reads one record at a time from its direct offset. Refresh only
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
		offset, end := s.starts[first-1], s.end
		s.unlock()
		for id := first; id <= last; id++ {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			var frame [8]byte
			if _, err := s.spool.ReadAt(frame[:], offset); err != nil {
				yield(nil, fmt.Errorf("read projected history frame: %w", err))
				return
			}
			length := binary.LittleEndian.Uint64(frame[:])
			if length > math.MaxInt64 {
				yield(nil, errors.New("invalid projected history frame length"))
				return
			}
			size := int64(length)
			offset += int64(len(frame))
			if size > end-offset || size < 2 {
				yield(nil, errors.New("invalid projected history frame"))
				return
			}
			encoded := make([]byte, size)
			if _, err := s.spool.ReadAt(encoded, offset); err != nil {
				yield(nil, fmt.Errorf("read projected history: %w", err))
				return
			}
			if !yield(encoded, nil) {
				return
			}
			offset += size
		}
	}
}
