// Reads bounded physical records and validates v3 header authority before iteration.

package taskslog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// Scanner validates a header and each subsequent frame. Returned byte slices
// remain valid until Scan is called again. Final EOF records need no LF.
type Scanner struct {
	HeaderFrame Frame
	Harness     string

	scanner   *bufio.Scanner
	src       string
	frame     Frame
	err       error
	headerSet bool
	eof       bool
}

// NewScanner constructs a bounded physical reader.
func NewScanner(r io.Reader, src string) *Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 1<<20), 32<<20)
	return &Scanner{scanner: s, src: src}
}

// NewSegmentScanner reads a journal section under already validated authority.
func NewSegmentScanner(r io.Reader, src, harness string) *Scanner {
	s := NewScanner(r, src)
	s.headerSet = true
	s.Harness = harness
	return s
}

// ReadHeader validates the first nonblank record as the immutable v3 authority.
func (s *Scanner) ReadHeader() (v3.MetaMessage, error) {
	if s.headerSet {
		return v3.MetaMessage{}, errors.New("task log header already read")
	}
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		frame, err := DecodeFrame(line)
		if err != nil {
			return v3.MetaMessage{}, fmt.Errorf("%s: invalid first log header: %w", s.src, err)
		}
		meta, ok := frame.Control.(*v3.MetaMessage)
		if !ok {
			return v3.MetaMessage{}, fmt.Errorf("%s: invalid first log header: not a caic log file", s.src)
		}
		frame.Encoded = bytes.Clone(frame.Encoded)
		s.HeaderFrame = frame
		s.Harness = meta.Harness
		s.headerSet = true
		return *meta, nil
	}
	if err := s.scanner.Err(); err != nil {
		return v3.MetaMessage{}, err
	}
	return v3.MetaMessage{}, fmt.Errorf("%s: invalid first log header: not a caic log file", s.src)
}

// Scan advances to the next nonblank validated frame.
func (s *Scanner) Scan() bool {
	if !s.headerSet {
		s.err = errors.New("task log header not read")
		return false
	}
	if s.err != nil {
		return false
	}
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		frame, err := DecodeFrame(line)
		if err != nil {
			s.err = fmt.Errorf("%s: invalid physical record: %w", s.src, err)
			return false
		}
		if meta, ok := frame.Control.(*v3.MetaMessage); ok && meta.Harness != s.Harness {
			s.err = fmt.Errorf("%s: log segment harness changed from %q to %q", s.src, s.Harness, meta.Harness)
			return false
		}
		s.frame = frame
		return true
	}
	s.eof = s.scanner.Err() == nil
	return false
}

// Frame returns the current validated record.
func (s *Scanner) Frame() *Frame { return &s.frame }

// Bytes returns the current encoded record without its LF.
func (s *Scanner) Bytes() []byte { return s.frame.Encoded }

// Err reports framing and reader failures.
func (s *Scanner) Err() error { return errors.Join(s.err, s.scanner.Err()) }

// EOFValidated reports successful exhaustion of the entire source.
func (s *Scanner) EOFValidated() bool { return s.eof && s.Err() == nil }

// ScanFile opens one journal and closes it after visit. A full scan must consume
// EOF so decompression failures are observed. Header-only scans may stop early.
func ScanFile(path string, validateEOF bool, visit func(os.FileInfo, *Scanner, v3.MetaMessage) error) (retErr error) {
	r, err := OpenReader(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, r.Close()) }()
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	s := NewScanner(r, path)
	meta, err := s.ReadHeader()
	if err != nil {
		return err
	}
	if err := visit(info, s, meta); err != nil {
		return err
	}
	if validateEOF && !s.EOFValidated() {
		return errors.New("task log did not reach EOF")
	}
	return nil
}

// ReadHeader opens only the bounded schema header and releases its reader.
func ReadHeader(path string) (meta v3.MetaMessage, retErr error) {
	r, err := OpenReader(path)
	if err != nil {
		return meta, err
	}
	defer func() { retErr = errors.Join(retErr, r.Close()) }()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4<<10), 32<<20)
	s := &Scanner{scanner: scanner, src: path}
	return s.ReadHeader()
}
