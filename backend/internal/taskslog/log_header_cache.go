// Caches parsed task-log headers beside each log so startup skips re-decoding
// immutable historical logs.
//
// Building the startup inventory scans every task log. For a zstd-compressed
// log that means decompressing the whole stream just to read the header, which
// dominated startup CPU. This cache lets a completed log's header come from a
// few kilobytes instead.
//
// The design is deliberately minimal so it stays a pure, safe optimization:
//
//   - One file per log, in the same directory and keyed by base name, so a log's
//     plain and compressed forms share a single entry and the entry goes away
//     with the log.
//   - Invalidation is size + mtime + schema version only. The backend is the
//     sole writer of task logs, so no inode, proof, or authority tracking is
//     used; a same-size/mtime collision is not treated as a threat.
//   - A miss, mismatch, oversize, or corrupt entry transparently falls back to a
//     full scan, which rewrites the entry. The cache is best-effort and is never
//     required for correctness.
//   - The entry is written atomically (temp + rename) so a crash cannot leave a
//     torn file behind.
//
// This is intentionally NOT the removed event replay cache. It caches only the
// small header projection, not the message stream, and has no coupling to any
// replay or proof machinery.

package taskslog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	v6 "github.com/caic-xyz/caic/backend/internal/taskslog/data/headercache/v6"
)

const (
	// headerCacheExt is the per-log header cache extension. It is a compact JSON
	// document, not .jsonl, so it never matches IsLogName and is never mistaken
	// for a task log.
	headerCacheExt = ".header.json"

	// currentVersion guards the marshaled shape. Bump it whenever the
	// persisted v6 schema changes, so entries written
	// by older binaries fall back to a full scan instead of misreading.
	currentVersion = 6

	// headerCacheMaxBytes is the largest entry the reader will accept. A valid
	// entry is a few kilobytes; anything larger is treated as corrupt.
	headerCacheMaxBytes = 1 << 20
)

// reapHeaderCaches removes header cache entries whose log no longer exists in
// either form. Caches are keyed beside their log, but a log can leave the
// directory without its cache (manual removal, retention cleanup elsewhere),
// so the scan prunes orphans. Removal is best-effort: a failure only leaves a
// harmless stale file behind. Orphaned .tmp leftovers from an interrupted
// atomic write are reaped the same way.
func reapHeaderCaches(logDir string, entries []os.DirEntry, logBases map[string]struct{}) {
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base, ok := strings.CutSuffix(e.Name(), headerCacheExt)
		if !ok {
			// Temp names are <log-base><headerCacheExt>.<random>.tmp.
			stem, isTmp := strings.CutSuffix(e.Name(), ".tmp")
			before, after, ok := strings.CutLast(stem, headerCacheExt)
			if !isTmp || !ok {
				continue
			}
			if after != "" && !strings.HasPrefix(after, ".") {
				continue
			}
			base = before
		}
		if _, valid := logBases[base]; valid {
			continue
		}
		_ = os.Remove(filepath.Join(logDir, e.Name()))
	}
}

// logHeaderCachePath returns the cache path for a task log. It is keyed by the
// log's base name (extension stripped) so the plain and compressed forms of the
// same log share one entry.
func logHeaderCachePath(path string) string {
	return filepath.Join(filepath.Dir(path), trimLogExt(filepath.Base(path))+headerCacheExt)
}

// readHeaderCache returns the cached header when the entry matches the log's
// current size, mtime, and schema version. Any mismatch, unreadable file,
// oversize entry, or corrupt payload reports ok=false so the caller falls back
// to a full scan.
func readHeaderCache(path string) (*LoadedTask, bool) {
	info, err := os.Stat(filepath.Clean(path))
	if err != nil || info.Size() == 0 {
		return nil, false
	}
	entryPath := logHeaderCachePath(path)
	entryInfo, err := os.Stat(filepath.Clean(entryPath))
	if err != nil || entryInfo.Size() > headerCacheMaxBytes {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Clean(entryPath))
	if err != nil {
		return nil, false
	}
	// Override state fields with the behavioral type during decoding. This
	// preserves its rejection of explicit null/unknown states, while omitted
	// states retain their historical zero value.
	var entry struct {
		v6.Entry

		Task *struct {
			v6.Task

			State  State `json:"state"`
			Result *struct {
				v6.Result

				State State `json:"state"`
			} `json:"result"`
		} `json:"task"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	if entry.Task == nil || entry.Version != currentVersion {
		return nil, false
	}
	if entry.LogSize != info.Size() || entry.LogMTimeNano != info.ModTime().UnixNano() {
		return nil, false
	}
	entry.Task.Task.State = string(entry.Task.State)
	if entry.Task.Result != nil {
		entry.Task.Result.Result.State = string(entry.Task.Result.State)
		entry.Task.LastTrailer = &entry.Task.Result.Result
	}
	task := headerTaskFromData(&entry.Task.Task)
	task.path = path
	task.LogSize = info.Size()
	return task, true
}

// writeHeaderCache stores the header beside the log so the next load skips the
// scan. It reports any I/O failure; the caller treats it as best-effort because
// the header is already in hand. The write is atomic (temp + rename) so a crash
// never leaves a torn entry behind.
func writeHeaderCache(path string, lt *LoadedTask) error {
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return err
	}
	data, err := json.Marshal(v6.Entry{
		Version:      currentVersion,
		LogSize:      info.Size(),
		LogMTimeNano: info.ModTime().UnixNano(),
		Task:         headerTaskToData(lt),
	})
	if err != nil {
		return err
	}
	entryPath := logHeaderCachePath(path)
	// A unique temp name keeps concurrent writers of the same log from
	// clobbering each other's in-flight entry; the reaper collects any
	// orphaned leftover by pattern.
	tmp, err := os.CreateTemp(filepath.Dir(entryPath), filepath.Base(entryPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = errors.Join(tmp.Close(), os.Remove(tmpName))
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, entryPath); err != nil {
		// The temp lives in the entry's directory, so the rename is atomic and a
		// failure is rare. Remove the leftover temp and join any removal error so
		// neither is dropped; a surviving temp is harmless because the reaper
		// collects it.
		return errors.Join(err, os.Remove(tmpName))
	}
	return nil
}
