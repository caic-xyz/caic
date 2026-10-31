// Spools journal bytes under validated header authority and reads bounded records backward.

package taskslog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

const (
	backwardReadBlockBytes = 64 << 10
	maxBackwardRecordBytes = 32 << 20
)

// Spool owns one seekable temporary file and its validated physical header.
// The owner closes it after semantic backward queries finish.
type Spool struct {
	File      *os.File
	Harness   string
	Header    Frame
	HeaderEnd int64
	End       int64
}

// NewSpool copies a journal under immutable header authority with bounded memory.
// Native frames are validated when selected records are decoded, avoiding a
// redundant payload scan during the byte-copy pass. Header/control framing and
// compressed EOF are checked while copying. A backward query that stops early
// does not promise to detect corruption in older native frames it never reads.
func NewSpool(ctx context.Context, path string) (*Spool, error) {
	file, err := os.CreateTemp("", "caic-task-history-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("create task history spool: %w", err)
	}
	spool := &Spool{File: file}
	if err := spool.load(ctx, path); err != nil {
		return nil, errors.Join(err, spool.Close())
	}
	return spool, nil
}

// Close releases the spool descriptor and removes its temporary file.
func (s *Spool) Close() error {
	return errors.Join(s.File.Close(), os.Remove(s.File.Name()))
}

func (s *Spool) load(ctx context.Context, path string) (retErr error) {
	writer := bufio.NewWriterSize(s.File, 1<<20)
	writeRecord := func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := writer.Write(line)
		s.End += int64(n)
		if err != nil {
			return err
		}
		if n != len(line) {
			return io.ErrShortWrite
		}
		if err := writer.WriteByte('\n'); err != nil {
			return err
		}
		s.End++
		return nil
	}
	reader, err := OpenReader(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	scanner := NewScanner(reader, path)
	if _, err := scanner.ReadHeader(); err != nil {
		return err
	}
	s.Harness = scanner.Harness
	s.Header = scanner.HeaderFrame
	if err := writeRecord(s.Header.Encoded); err != nil {
		return err
	}
	s.HeaderEnd = s.End
	for scanner.scanner.Scan() {
		line := scanner.scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// Canonical native prefixes establish that this is not a segment
		// header. Their payloads remain opaque until semantic selection.
		if !bytes.HasPrefix(line, []byte(v3AgentRecordPrefix)) && !bytes.HasPrefix(line, []byte(v3InputRecordPrefix)) {
			frame, err := DecodeFrame(line)
			if err != nil {
				return fmt.Errorf("%s: invalid physical record: %w", path, err)
			}
			if meta, ok := frame.Control.(*v3.MetaMessage); ok && meta.Harness != s.Harness {
				return fmt.Errorf("%s: log segment harness changed from %q to %q", path, s.Harness, meta.Harness)
			}
		}
		if err := writeRecord(line); err != nil {
			return err
		}
	}
	if err := scanner.scanner.Err(); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush task history spool: %w", err)
	}
	return nil
}

// ReverseReader borrows a spool file and reads bounded physical lines backward.
type ReverseReader struct {
	file  *os.File
	start int64
	pos   int64
	block []byte
	line  []byte
}

// NewReverseReader reads bounded records backward from a seekable journal spool.
func NewReverseReader(file *os.File, start, end int64) *ReverseReader {
	return &ReverseReader{file: file, start: start, pos: end}
}

// Previous returns the preceding encoded record and its byte boundaries.
func (r *ReverseReader) Previous() (line []byte, start, end int64, ok bool, err error) {
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
