// Shared record-handler fake and native-input fixtures for ordered recording tests.

package agenttest

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// RecordHandler observes committed events without deriving runtime controls.
type RecordHandler struct {
	Messages chan<- agent.TimedMessage

	mu     sync.Mutex
	events []agent.TimedMessage
}

// RecordAppended accepts physical provenance without maintaining a runtime cursor.
func (*RecordHandler) RecordAppended(agent.PhysicalRecord) {}

// PrepareRecords produces no derived controls.
func (*RecordHandler) PrepareRecords(context.Context, []agent.TimedMessage) ([]agent.Message, error) {
	return nil, nil
}

// ApplyRecords captures committed events and forwards them when Messages is set.
func (h *RecordHandler) ApplyRecords(ctx context.Context, m []agent.TimedMessage) error {
	h.mu.Lock()
	h.events = append(h.events, m...)
	h.mu.Unlock()
	if h.Messages != nil {
		for _, event := range m {
			select {
			case h.Messages <- event:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

// Events returns the accepted events in their publication order.
func (h *RecordHandler) Events() []agent.TimedMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]agent.TimedMessage(nil), h.events...)
}

// AppendInputNativeRecord writes a recorded-input fixture without creating a
// live execution owner. Runtime input must use Records instead.
func AppendInputNativeRecord(log agent.LogSink, data []byte) error {
	data = bytes.TrimSuffix(data, []byte{'\n'})
	record, err := (&v3.NativeRecord{Type: string(taskslog.KindInput), Time: time.Now().UTC(), Message: data}).MarshalJSON()
	if err != nil {
		return err
	}
	return log.AppendRecord(agent.PhysicalRecord{Encoded: append(record, '\n')})
}
