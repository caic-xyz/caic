// Opens anonymous projection spools that the operating system releases after process exit.
//go:build !windows

package history

import (
	"errors"
	"os"
)

func openProjectionSpool(dir string) (*os.File, error) {
	f, err := os.CreateTemp(dir, ".caic-history-projection-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil { //nolint:gosec // The path is owned by this os.CreateTemp call.
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
