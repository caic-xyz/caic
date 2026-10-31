// Lists physical journal paths with plain files authoritative over compressed siblings.

package taskslog

import (
	"os"
	"path/filepath"
	"slices"
)

// Paths lists one storage form in lexical order without modifying the directory.
func Paths(logDir string, compressed bool) ([]string, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	plainBases := make(map[string]struct{})
	for _, e := range entries {
		if !e.IsDir() && IsLogName(e.Name()) && !IsCompressed(e.Name()) {
			plainBases[TrimExt(e.Name())] = struct{}{}
		}
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !IsLogName(e.Name()) || IsCompressed(e.Name()) != compressed {
			continue
		}
		if compressed {
			if _, ok := plainBases[TrimExt(e.Name())]; ok {
				continue
			}
		}
		paths = append(paths, filepath.Join(logDir, e.Name()))
	}
	slices.Sort(paths)
	return paths, nil
}
