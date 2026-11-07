// Decodes incremental journals with adjacent delivery timing, stable identities, and terminal semantic failures.

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
	ready  []agent.ParsedRecord
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
// A semantic decode failure is terminal; valid pending records precede its error.
// Cancellation and plain-log EOF polling leave the decoder resumable.
func (c *RecordCursor) Read(ctx context.Context) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		if c.err != nil && len(c.ready) == 0 {
			yield(Record{}, c.err)
			return
		}
		if err := ctx.Err(); err != nil {
			yield(Record{}, err)
			return
		}
		drain := func() bool {
			for len(c.ready) > 0 {
				if err := ctx.Err(); err != nil {
					yield(Record{}, err)
					return false
				}
				parsed := c.ready[0]
				c.ready[0] = agent.ParsedRecord{}
				c.ready = c.ready[1:]
				if len(c.ready) == 0 {
					c.ready = nil
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
				if !yield(Record{ID: parsed.ID, Messages: messages}, nil) {
					return false
				}
			}
			c.ready = nil
			return true
		}
		if !drain() {
			return
		}
		if c.err != nil {
			yield(Record{}, c.err)
			return
		}
		for raw, err := range c.raw.Read(ctx) {
			if err != nil {
				yield(Record{}, err)
				return
			}
			c.ready, err = c.parser.DecodeHistoryFrame(&raw.Frame, raw.ID)
			if err != nil {
				c.err = err
				c.parser = nil
			}
			if !drain() {
				return
			}
			if c.err != nil {
				yield(Record{}, c.err)
				return
			}
		}
		if c.raw.Exhausted() && c.parser != nil {
			c.ready = c.parser.FlushHistory()
			c.parser = nil
			drain()
		}
	}
}

// Close releases the cursor's physical descriptor.
func (c *RecordCursor) Close() error {
	c.parser = nil
	c.ready = nil
	return c.raw.Close()
}
