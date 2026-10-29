// Reads plain and compressed physical task-log records with stable ordinals and retained native parser state.

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

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// HistoryRecord is one nonempty physical log record. ID is one-based, includes
// metadata and controls, and is independent of how many messages parsing emits.
type HistoryRecord struct {
	ID       uint64
	Messages []agent.TimedMessage
}

// RecordCursor incrementally parses physical task-log records. Its owner
// serializes calls and closes it when the task's history generation changes.
// An incomplete trailing line is left for the next read. Existing task-log
// schemas are unchanged. Compressed streams release their decoder and parsing
// buffers at EOF; their already projected records remain available to the owner.
type RecordCursor struct {
	// Harness and RequestedModel are immutable values from the physical header.
	Harness        harness.Name
	RequestedModel string

	file       *os.File
	compressed io.ReadCloser
	scanner    *physicalLogScanner
	pending    []byte
	parser     *agent.LogRecordParser
	reader     *bufio.Reader
	authority  logAuthority
	header     [32]byte
	offset     int64
	count      uint64
}

// OpenRecordCursor validates a plain or compressed task log and retains its parser.
func OpenRecordCursor(path string, resolver WireResolver) (_ *RecordCursor, retErr error) {
	if isLogCompressed(path) {
		r, err := openCompressedLogReader(path)
		if err != nil {
			return nil, err
		}
		scanner := newPhysicalLogScanner(r, path)
		meta, err := scanner.ReadHeader()
		if err != nil {
			return nil, errors.Join(err, r.Close())
		}
		wire, err := resolver.ResolveWire(scanner.authority.Harness)
		if err != nil {
			return nil, errors.Join(err, r.Close())
		}
		parser, err := agent.NewLogRecordParser(scanner.authority.Version, wire.ParseMessage)
		if err != nil {
			return nil, errors.Join(err, r.Close())
		}
		return &RecordCursor{Harness: scanner.authority.Harness, RequestedModel: meta.RequestedModel, compressed: r, scanner: scanner, pending: scanner.headerRaw, parser: parser, authority: scanner.authority, header: sha256.Sum256(scanner.headerRaw)}, nil
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
	scanner := newPhysicalLogScanner(io.NewSectionReader(f, 0, info.Size()), path)
	meta, err := scanner.ReadHeader()
	if err != nil {
		return nil, err
	}
	wire, err := resolver.ResolveWire(scanner.authority.Harness)
	if err != nil {
		return nil, err
	}
	parser, err := agent.NewLogRecordParser(scanner.authority.Version, wire.ParseMessage)
	if err != nil {
		return nil, err
	}
	return &RecordCursor{Harness: scanner.authority.Harness, RequestedModel: meta.RequestedModel, file: f, parser: parser, reader: bufio.NewReaderSize(nil, 64<<10), authority: scanner.authority, header: sha256.Sum256(scanner.headerRaw)}, nil
}

// Read yields complete records appended since the preceding read, up to the
// file size observed at entry. Stopping the iterator retains the parser at the
// last yielded record. Errors require the owner to discard this cursor.
func (c *RecordCursor) Read(ctx context.Context) iter.Seq2[HistoryRecord, error] {
	return func(yield func(HistoryRecord, error) bool) {
		if c.file != nil {
			info, err := c.file.Stat()
			if err != nil {
				yield(HistoryRecord{}, err)
				return
			}
			if info.Size() < c.offset {
				yield(HistoryRecord{}, errors.New("task log truncated during incremental history"))
				return
			}
			if info.Size() == c.offset {
				return
			}
			c.reader.Reset(io.NewSectionReader(c.file, c.offset, info.Size()-c.offset))
		}
		for {
			if err := ctx.Err(); err != nil {
				yield(HistoryRecord{}, err)
				return
			}
			line, err := c.nextLine()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(HistoryRecord{}, err)
				return
			}
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			if err := c.validateAuthority(line); err != nil {
				yield(HistoryRecord{}, err)
				return
			}
			parsed, err := c.parser.ParseRecord(line)
			if err != nil {
				yield(HistoryRecord{}, fmt.Errorf("parse physical history record: %w", err))
				return
			}
			c.count++
			messages := parsed.Messages
			if parsed.Control {
				messages = nil
				for _, message := range parsed.Messages {
					// Whole-task loading folds session init into metadata. A
					// raw-addressed history must expose that context at its
					// original record while retaining the loader's other
					// non-inventory conversation/control messages.
					_, init := message.Message.(*agent.InitMessage)
					if init || !isInventoryMetadataMessage(message.Message) {
						messages = append(messages, message)
					}
				}
			}
			if !yield(HistoryRecord{ID: c.count, Messages: messages}, nil) {
				return
			}
		}
	}
}

// Close releases the underlying log file.
func (c *RecordCursor) Close() error {
	if c.compressed != nil {
		return c.closeCompressed()
	}
	if c.file != nil {
		return c.file.Close()
	}
	return nil
}

func (c *RecordCursor) nextLine() ([]byte, error) {
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

func (c *RecordCursor) closeCompressed() error {
	r := c.compressed
	c.compressed = nil
	c.scanner = nil
	c.parser = nil
	c.pending = nil
	return r.Close()
}

func (c *RecordCursor) validateAuthority(line []byte) error {
	if c.count == 0 {
		_, authority, err := decodeAuthorityMeta(line)
		if err != nil {
			return fmt.Errorf("invalid leading physical history header: %w", err)
		}
		if authority != c.authority || sha256.Sum256(line) != c.header {
			return errors.New("leading physical history header changed after validation")
		}
		return nil
	}
	if c.authority.Version != agent.LogVersionV1 && bytes.HasPrefix(line, []byte(`{"t":"agent","ts":`)) {
		return nil
	}
	_, meta, err := decodeSegmentMeta(line, c.authority.Version)
	if errors.Is(err, errNotMetaRecord) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("invalid physical history segment header: %w", err)
	}
	if agent.LogVersion(meta.Version) != c.authority.Version || harness.Name(meta.Harness) != c.authority.Harness {
		return errors.New("physical history segment authority changed")
	}
	return nil
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
