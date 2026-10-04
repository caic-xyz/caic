// Usage-day compression and record iteration keep old days compact and avoid whole-day buffers.

package usagedb

import (
	"bufio"
	"context"
	"errors"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	compressedDaySuffix  = ".jsonl.zstd"
	compressionGraceDays = 2
)

// writeCompressedDay stages a complete compressed copy without publishing it.
// The caller checks that the plain source has not changed before publication.
func writeCompressedDay(path string) (stagedPath string, err error) {
	in, err := os.Open(path) //nolint:gosec // validated day filename inside the configured rollup directory.
	if err != nil {
		return "", err
	}
	staged, err := os.CreateTemp(filepath.Dir(path), ".usage-zstd-*.tmp")
	if err != nil {
		return "", errors.Join(err, in.Close())
	}
	pathToClean := staged.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(pathToClean)
		}
	}()
	defer func() { err = errors.Join(err, in.Close()) }()
	enc, err := zstd.NewWriter(staged, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return "", errors.Join(err, staged.Close())
	}
	if _, err := io.Copy(enc, in); err != nil {
		return "", errors.Join(err, enc.Close(), staged.Close())
	}
	if err := enc.Close(); err != nil {
		return "", errors.Join(err, staged.Close())
	}
	if err := staged.Sync(); err != nil {
		return "", errors.Join(err, staged.Close())
	}
	if err := staged.Close(); err != nil {
		return "", err
	}
	return pathToClean, nil
}

// dayRecords yields records with their original newline, including an unterminated
// final record. Working memory is the largest record, a 64 KiB read buffer, and
// zstd decoder state; existing durable rows have no size limit. Records live until the next
// iteration. File and decoder resources are released when iteration stops.
func dayRecords(ctx context.Context, path string) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		f, err := os.Open(path) //nolint:gosec // validated day filename inside the configured rollup directory.
		if err != nil {
			yield(nil, err)
			return
		}
		var in io.Reader = f
		var dec *zstd.Decoder
		if strings.HasSuffix(path, compressedDaySuffix) {
			dec, err = zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
			if err != nil {
				yield(nil, errors.Join(err, f.Close()))
				return
			}
			in = dec
		}
		closed := false
		closeInput := func() error {
			if dec != nil {
				dec.Close()
			}
			closed = true
			return f.Close()
		}
		defer func() {
			if !closed {
				_ = closeInput()
			}
		}()
		r := bufio.NewReaderSize(in, 64<<10)
		for {
			if err := ctx.Err(); err != nil {
				yield(nil, errors.Join(err, closeInput()))
				return
			}
			row, err := r.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				large := append([]byte(nil), row...)
				for errors.Is(err, bufio.ErrBufferFull) {
					if err := ctx.Err(); err != nil {
						yield(nil, errors.Join(err, closeInput()))
						return
					}
					row, err = r.ReadSlice('\n')
					large = append(large, row...)
				}
				row = large
			}
			if err != nil && !errors.Is(err, io.EOF) {
				yield(nil, errors.Join(err, closeInput()))
				return
			}
			if len(row) != 0 && !yield(row, nil) {
				return
			}
			if errors.Is(err, io.EOF) {
				if err := closeInput(); err != nil {
					yield(nil, err)
				}
				return
			}
		}
	}
}

// syncDayDir makes a newly published copy durable before its old copy is
// removed. If directory sync fails, the caller leaves the old copy in place.
func syncDayDir(path string) (err error) {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	return dir.Sync()
}
