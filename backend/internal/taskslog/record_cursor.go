// Reads bounded plain and compressed physical records with stable ordinals and descriptor-owned authority.

package taskslog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// RawRecord is one nonempty physical log record. ID is one-based, includes
// metadata and controls, and is independent of how many messages parsing emits.
type RawRecord struct {
	ID    uint64
	Frame Frame
}

// RawCursor incrementally parses physical task-log records. Its owner
// serializes calls and closes it when the task's history generation changes.
// An incomplete trailing line is left for the next read. Existing task-log
// schemas are unchanged. Compressed streams release their decoder and parsing
// buffers at EOF; their already projected records remain available to the owner.
type RawCursor struct {
	// Harness and RequestedModel are immutable values from the physical header.
	Harness        string
	RequestedModel string

	file       *os.File
	compressed io.ReadCloser
	scanner    *Scanner
	pending    []byte
	reader     *bufio.Reader
	authority  string
	header     [32]byte
	offset     int64
	count      uint64
}

// OpenRawCursor validates a plain or compressed task log and retains its parser.
func OpenRawCursor(path string) (_ *RawCursor, retErr error) {
	if IsCompressed(path) {
		r, err := openCompressedLogReader(path)
		if err != nil {
			return nil, err
		}
		scanner := NewScanner(r, path)
		meta, err := scanner.ReadHeader()
		if err != nil {
			return nil, errors.Join(err, r.Close())
		}
		return &RawCursor{Harness: scanner.Harness, RequestedModel: meta.RequestedModel, compressed: r, scanner: scanner, pending: scanner.HeaderFrame.Encoded, authority: scanner.Harness, header: sha256.Sum256(scanner.HeaderFrame.Encoded)}, nil
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, f.Close())
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	scanner := NewScanner(io.NewSectionReader(f, 0, info.Size()), path)
	meta, err := scanner.ReadHeader()
	if err != nil {
		return nil, err
	}
	return &RawCursor{Harness: scanner.Harness, RequestedModel: meta.RequestedModel, file: f, reader: bufio.NewReaderSize(nil, 64<<10), authority: scanner.Harness, header: sha256.Sum256(scanner.HeaderFrame.Encoded)}, nil
}

// Read yields complete records appended since the preceding read, up to the
// file size observed at entry. Stopping the iterator retains the parser at the
// last yielded record. Errors require the owner to discard this cursor.
func (c *RawCursor) Read(ctx context.Context) iter.Seq2[RawRecord, error] {
	return func(yield func(RawRecord, error) bool) {
		if c.file != nil {
			info, err := c.file.Stat()
			if err != nil {
				yield(RawRecord{}, err)
				return
			}
			if info.Size() < c.offset {
				yield(RawRecord{}, errors.New("task log truncated during incremental history"))
				return
			}
			if info.Size() == c.offset {
				return
			}
			c.reader.Reset(io.NewSectionReader(c.file, c.offset, info.Size()-c.offset))
		}
		for {
			if err := ctx.Err(); err != nil {
				yield(RawRecord{}, err)
				return
			}
			line, err := c.nextLine()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(RawRecord{}, err)
				return
			}
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var frame Frame
			if c.compressed != nil {
				if c.count == 0 {
					frame = c.scanner.HeaderFrame
				} else {
					frame = *c.scanner.Frame()
				}
			} else {
				frame, err = DecodeFrame(line)
			}
			if err != nil {
				yield(RawRecord{}, fmt.Errorf("parse physical history record: %w", err))
				return
			}
			if meta, ok := frame.Control.(*v3.MetaMessage); ok && meta.Harness != c.authority {
				yield(RawRecord{}, errors.New("physical history segment authority changed"))
				return
			}
			if c.count == 0 && sha256.Sum256(line) != c.header {
				yield(RawRecord{}, errors.New("leading physical history header changed after validation"))
				return
			}
			c.count++
			if !yield(RawRecord{ID: c.count, Frame: frame}, nil) {
				return
			}
		}
	}
}

// Close releases the underlying log file.
func (c *RawCursor) Close() error {
	if c.compressed != nil {
		return c.closeCompressed()
	}
	if c.file != nil {
		return c.file.Close()
	}
	return nil
}

// Exhausted reports that an immutable compressed stream released its descriptor.
func (c *RawCursor) Exhausted() bool { return c.file == nil && c.compressed == nil }

func (c *RawCursor) nextLine() ([]byte, error) {
	if c.scanner != nil {
		if c.pending != nil {
			line := c.pending
			c.pending = nil
			return line, nil
		}
		if c.scanner.Scan() {
			return c.scanner.Bytes(), nil
		}
		if err := errors.Join(c.scanner.Err(), c.closeCompressed()); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	if c.file == nil {
		return nil, io.EOF
	}
	line, err := readCursorLine(c.reader)
	if err != nil {
		return nil, err
	}
	c.offset += int64(len(line))
	return line[:len(line)-1], nil
}

func (c *RawCursor) closeCompressed() error {
	r := c.compressed
	c.compressed = nil
	c.scanner = nil

	c.pending = nil
	return r.Close()
}

func readCursorLine(r *bufio.Reader) ([]byte, error) {
	const maxRecordBytes = 32 << 20
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(line)+len(part) > maxRecordBytes {
			return nil, errors.New("physical history record exceeds 32 MiB")
		}
		if len(line) == 0 && err == nil {
			return part, nil
		}
		line = append(line, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}
