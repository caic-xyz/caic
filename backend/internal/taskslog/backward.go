// Streams task history backward from a seekable decompressed-log spool.

package taskslog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

const (
	backwardReadBlockBytes = 64 << 10
	maxBackwardRecordBytes = 32 << 20
)

// stableBackwardRecord returns record-local messages whose values do not
// depend on earlier native records. Stateful records fall back to ordered
// reconstruction of their turn window.
func stableBackwardRecord(record agent.ParsedRecord, parseErr error) ([]agent.ParsedMessage, bool) {
	if parseErr != nil {
		return nil, false
	}
	messages := record.Messages
	if record.Control {
		messages = slices.DeleteFunc(slices.Clone(messages), func(message agent.ParsedMessage) bool {
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
func findBackwardWindow(lt *LoadedTask, ctx context.Context, spool *backwardHistorySpool, windowEnd int64) (start int64, more bool, err error) {
	native, err := lt.resolver(spool.authority.Harness)
	if err != nil {
		return 0, false, err
	}
	parser, err := agent.NewLogRecordParser(spool.authority.Version, native)
	if err != nil {
		return 0, false, err
	}
	if _, err := parser.ParseRecord(spool.header); err != nil {
		return 0, false, err
	}

	reader := backwardRecordReader{file: spool.file, start: spool.headerEnd, pos: windowEnd}
	haveNewerNative := false
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		line, _, lineEnd, ok, err := reader.previous()
		if err != nil {
			return 0, false, err
		}
		if !ok {
			return spool.headerEnd, false, nil
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

func parseBackwardWindow(lt *LoadedTask, ctx context.Context, spool *backwardHistorySpool, start, end int64) ([]agent.ParsedMessage, error) {
	native, err := lt.resolver(spool.authority.Harness)
	if err != nil {
		return nil, err
	}
	parser, err := agent.NewLogRecordParser(spool.authority.Version, native)
	if err != nil {
		return nil, err
	}
	if _, err := parser.ParseRecord(spool.header); err != nil {
		return nil, err
	}

	scanner := newPhysicalLogScanner(io.NewSectionReader(spool.file, start, end-start), spool.file.Name())
	scanner.headerSet = true
	scanner.authority = spool.authority
	var messages []agent.ParsedMessage
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := parser.ParseRecord(scanner.Bytes())
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

type backwardHistorySpool struct {
	file      *os.File
	authority logAuthority
	header    []byte
	headerEnd int64
	end       int64
}

func newBackwardHistorySpool(ctx context.Context, path string) (*backwardHistorySpool, error) {
	file, err := os.CreateTemp("", "caic-task-history-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("create task history spool: %w", err)
	}
	spool := &backwardHistorySpool{file: file}
	if err := spool.load(ctx, path); err != nil {
		return nil, errors.Join(err, spool.cleanup())
	}
	return spool, nil
}

func (s *backwardHistorySpool) load(ctx context.Context, path string) error {
	writer := bufio.NewWriterSize(s.file, 1<<20)
	writeRecord := func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := writer.Write(line)
		s.end += int64(n)
		if err != nil {
			return err
		}
		if n != len(line) {
			return io.ErrShortWrite
		}
		if err := writer.WriteByte('\n'); err != nil {
			return err
		}
		s.end++
		return nil
	}
	err := scanPhysicalLog(path, true, func(_ os.FileInfo, scanner *physicalLogScanner, _ agent.MetaMessage) error {
		s.authority = scanner.authority
		s.header = bytes.Clone(scanner.headerRaw)
		if err := writeRecord(s.header); err != nil {
			return err
		}
		s.headerEnd = s.end
		for scanner.Scan() {
			if err := writeRecord(scanner.Bytes()); err != nil {
				return err
			}
		}
		return scanner.Err()
	})
	if err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush task history spool: %w", err)
	}
	return nil
}

func (s *backwardHistorySpool) cleanup() error {
	return errors.Join(s.file.Close(), os.Remove(s.file.Name()))
}

type backwardRecordReader struct {
	file  *os.File
	start int64
	pos   int64
	block []byte
	line  []byte
}

func (r *backwardRecordReader) previous() (line []byte, start, end int64, ok bool, err error) {
	if r.pos <= r.start {
		return nil, 0, 0, false, nil
	}
	recordEnd := r.pos
	lineEnd := recordEnd - 1
	var newline [1]byte
	if _, err := r.file.ReadAt(newline[:], lineEnd); err != nil {
		return nil, 0, 0, false, fmt.Errorf("read task history record terminator: %w", err)
	}
	if newline[0] != '\n' {
		return nil, 0, 0, false, errors.New("task history spool record is not newline terminated")
	}

	lineStart := r.start
	for cursor := lineEnd; cursor > r.start; {
		chunkStart := max(r.start, cursor-backwardReadBlockBytes)
		chunkLength := int(cursor - chunkStart)
		if cap(r.block) < chunkLength {
			r.block = make([]byte, chunkLength)
		}
		chunk := r.block[:chunkLength]
		if _, err := r.file.ReadAt(chunk, chunkStart); err != nil {
			return nil, 0, 0, false, fmt.Errorf("scan task history record backward: %w", err)
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			lineStart = chunkStart + int64(i) + 1
			break
		}
		cursor = chunkStart
	}
	lineLength := lineEnd - lineStart
	if lineLength > maxBackwardRecordBytes {
		return nil, 0, 0, false, fmt.Errorf("task history record exceeds %d bytes", maxBackwardRecordBytes)
	}
	length := int(lineLength)
	if cap(r.line) < length {
		r.line = make([]byte, length)
	}
	r.line = r.line[:length]
	if _, err := r.file.ReadAt(r.line, lineStart); err != nil {
		return nil, 0, 0, false, fmt.Errorf("read task history record: %w", err)
	}
	r.pos = lineStart
	return r.line, lineStart, recordEnd, true, nil
}
