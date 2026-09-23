// Usage-day compression keeps old JSONL days compact and restorable for late writes.

package usagedb

import (
	"errors"
	"io"
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

func readDayFile(path string) (data []byte, err error) {
	f, err := os.Open(path) //nolint:gosec // validated day filename inside the configured rollup directory.
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if !strings.HasSuffix(path, compressedDaySuffix) {
		return io.ReadAll(f)
	}
	dec, err := zstd.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return io.ReadAll(dec)
}

func writeDayBytes(f *os.File, path string, data []byte) error {
	if !strings.HasSuffix(path, compressedDaySuffix) {
		_, err := f.Write(data)
		return err
	}
	enc, err := zstd.NewWriter(f, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return err
	}
	if _, err := enc.Write(data); err != nil {
		return errors.Join(err, enc.Close())
	}
	return enc.Close()
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
