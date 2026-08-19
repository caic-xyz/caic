// Provides Linux page-cache eviction for task adoption benchmarks.
//go:build linux

package taskslog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func prepareColdAdoptionFixture(path string) (retErr error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, f.Close())
	}()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync fixture: %w", err)
	}
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		return fmt.Errorf("%w: fadvise DONTNEED: %w", errAdoptionColdUnsupported, err)
	}
	return nil
}
