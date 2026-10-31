// Decodes incremental physical journal cursors with ordered provider state and terminal semantic failures.

package history

import (
	"context"
	"errors"
	"iter"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// Record carries the stable physical identity and all its visible events.
type Record struct {
	ID       uint64
	Messages []agent.TimedMessage
}

// RecordCursor owns the raw descriptor and one ordered semantic decoder.
type RecordCursor struct {
	Harness        harness.Name
	RequestedModel string

	raw    *taskslog.RawCursor
	parser *agent.LogRecordParser
	err    error
}

// OpenRecordCursor validates header authority before resolving its fresh parser.
func OpenRecordCursor(path string, resolver ParserResolver) (*RecordCursor, error) {
	raw, err := taskslog.OpenRawCursor(path)
	if err != nil {
		return nil, err
	}
	native, err := resolver.ResolveParser(harness.Name(raw.Harness))
	if err != nil {
		return nil, errors.Join(err, raw.Close())
	}
	parser, err := agent.NewLogRecordParser(native)
	if err != nil {
		return nil, errors.Join(err, raw.Close())
	}
	return &RecordCursor{Harness: harness.Name(raw.Harness), RequestedModel: raw.RequestedModel, raw: raw, parser: parser}, nil
}

// Read decodes each yielded physical record once; stopping retains ordered state.
// A semantic decode failure is terminal and subsequent reads yield that error.
// Cancellation and plain-log EOF polling leave the decoder resumable.
func (c *RecordCursor) Read(ctx context.Context) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		if c.err != nil {
			yield(Record{}, c.err)
			return
		}
		defer func() {
			if c.raw.Exhausted() {
				c.parser = nil
			}
		}()
		for raw, err := range c.raw.Read(ctx) {
			if err != nil {
				yield(Record{}, err)
				return
			}
			parsed, err := c.parser.DecodeFrame(&raw.Frame)
			if err != nil {
				c.err = err
				c.parser = nil
				yield(Record{}, err)
				return
			}
			messages := parsed.Messages
			if parsed.Control {
				messages = nil
				for _, m := range parsed.Messages {
					if !isInventoryOnlyMessage(m.Message) {
						messages = append(messages, m)
					}
				}
			}
			if !yield(Record{ID: raw.ID, Messages: messages}, nil) {
				return
			}
		}
	}
}

// Close releases the cursor's physical descriptor.
func (c *RecordCursor) Close() error { return c.raw.Close() }
