// Spools ordered API projections so disk previews retain authoritative native ownership.

package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// recentDiskPreview parses native history in order before selecting its tail.
// Native ownership and projections can depend on earlier sessions and spawns,
// so individually decoding physical records backward cannot establish them.
// A private, length-framed spool retains projections on disk; memory holds one
// record and the selected preview rather than the complete task history.
func recentDiskPreview(ctx context.Context, lt *taskslog.LoadedTask, tracker *apiconv.ToolTimingTracker) (events []v1.EventMessage, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "caic-task-preview-*")
	if err != nil {
		return nil, fmt.Errorf("create preview spool: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, file.Close(), os.Remove(file.Name())) }()
	writer := bufio.NewWriterSize(file, 64<<10)
	var end int64
	var frame [8]byte
	for parsed, err := range lt.StreamMessages(ctx) {
		if err != nil {
			return nil, fmt.Errorf("parse preview history: %w", err)
		}
		// Canonical history reconciles relay exit errors with successful results.
		if _, ok := parsed.Message.(*agent.ExitMessage); ok {
			continue
		}
		projected := tracker.ConvertMessage(parsed.Message, parsed.ProducerTime)
		for i := range projected {
			event := &projected[i]
			record := projectedPreviewRecord(event)
			if record.kind == "" || record.source.IsChild() {
				continue
			}
			data, err := json.Marshal(event)
			if err != nil {
				return nil, fmt.Errorf("encode preview event: %w", err)
			}
			binary.LittleEndian.PutUint64(frame[:], uint64(len(data)))
			if _, err := writer.Write(data); err != nil {
				return nil, fmt.Errorf("write preview event: %w", err)
			}
			if _, err := writer.Write(frame[:]); err != nil {
				return nil, fmt.Errorf("write preview frame: %w", err)
			}
			end += int64(len(data)) + int64(len(frame))
		}
	}
	if err := writer.Flush(); err != nil {
		return nil, fmt.Errorf("flush preview spool: %w", err)
	}
	preview := newTaskPreview()
	for end > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := file.ReadAt(frame[:], end-int64(len(frame))); err != nil {
			return nil, fmt.Errorf("read preview frame: %w", err)
		}
		size := binary.LittleEndian.Uint64(frame[:])
		if size > uint64(math.MaxInt) || size > uint64(end-int64(len(frame))) {
			return nil, errors.New("invalid preview frame length")
		}
		data := make([]byte, int(size))
		end -= int64(len(frame)) + int64(size)
		if _, err := file.ReadAt(data, end); err != nil {
			return nil, fmt.Errorf("read preview event: %w", err)
		}
		var event v1.EventMessage
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, fmt.Errorf("decode preview event: %w", err)
		}
		record := projectedPreviewRecord(&event)
		if !preview.addRecord(&record) {
			break
		}
	}
	return preview.events(tracker), nil
}
