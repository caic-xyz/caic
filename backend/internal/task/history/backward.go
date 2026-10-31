// Streams task history backward from a seekable decompressed-log spool.

package history

import (
	"context"
	"io"
	"slices"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/taskslog"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// stableBackwardRecord returns record-local messages whose values do not
// depend on earlier native records. Stateful records fall back to ordered
// reconstruction of their turn window.
func stableBackwardRecord(record agent.ParsedRecord, parseErr error) ([]agent.TimedMessage, bool) {
	if parseErr != nil {
		return nil, false
	}
	messages := record.Messages
	if record.Control {
		messages = slices.DeleteFunc(slices.Clone(messages), func(message agent.TimedMessage) bool {
			return !isHistoryStreamControlMessage(message.Message)
		})
		return messages, true
	}
	if len(messages) == 0 {
		return nil, false
	}
	for _, parsed := range messages {
		switch parsed.Message.(type) {
		case *agent.ResultMessage, *agent.UsageMessage, *agent.ToolOutputDeltaMessage, *agent.WidgetDeltaMessage:
			return nil, false
		}
	}
	return messages, true
}

// findBackwardWindow returns the start of the newest independently replayable
// suffix ending at windowEnd. Native result records reset every harness's
// per-turn parsing state. The newest native result belongs to the suffix; an
// earlier result, or the first result preceded in reverse order by newer native
// records, is the boundary immediately before it.
func findBackwardWindow(lt *LoadedTask, ctx context.Context, spool *taskslog.Spool, windowEnd int64) (start int64, more bool, err error) {
	wire, err := lt.resolver.ResolveParser(harness.Name(spool.Harness))
	if err != nil {
		return 0, false, err
	}
	parser, err := agent.NewLogRecordParser(wire)
	if err != nil {
		return 0, false, err
	}
	if _, err := parser.DecodeFrame(&spool.Header); err != nil {
		return 0, false, err
	}

	reader := taskslog.NewReverseReader(spool.File, spool.HeaderEnd, windowEnd)
	haveNewerNative := false
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		line, _, lineEnd, ok, err := reader.Previous()
		if err != nil {
			return 0, false, err
		}
		if !ok {
			return spool.HeaderEnd, false, nil
		}
		record, parseErr := parser.ParseRecord(line)
		if parseErr != nil {
			// Reverse probing intentionally does not promise ordered parser state.
			// The bounded forward replay below reports real parse failures. Treat an
			// unprobeable physical record as native so it cannot create a false cut.
			haveNewerNative = true
			continue
		}
		if record.Control {
			continue
		}
		hasResult := false
		for _, parsed := range record.Messages {
			if _, ok := parsed.Message.(*agent.ResultMessage); ok {
				hasResult = true
				break
			}
		}
		if hasResult && haveNewerNative {
			return lineEnd, true, nil
		}
		haveNewerNative = true
	}
}

func parseBackwardWindow(lt *LoadedTask, ctx context.Context, spool *taskslog.Spool, start, end int64) ([]agent.TimedMessage, error) {
	wire, err := lt.resolver.ResolveParser(harness.Name(spool.Harness))
	if err != nil {
		return nil, err
	}
	parser, err := agent.NewLogRecordParser(wire)
	if err != nil {
		return nil, err
	}
	if _, err := parser.DecodeFrame(&spool.Header); err != nil {
		return nil, err
	}

	scanner := taskslog.NewSegmentScanner(io.NewSectionReader(spool.File, start, end-start), spool.File.Name(), spool.Harness)
	var messages []agent.TimedMessage
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := parser.DecodeFrame(scanner.Frame())
		if err != nil {
			return nil, err
		}
		for _, message := range record.Messages {
			if record.Control && !isHistoryStreamControlMessage(message.Message) {
				continue
			}
			messages = append(messages, message)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}
