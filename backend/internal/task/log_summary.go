// Compressed task log metadata summary cache for fast startup reloads.

package task

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/logproof"
)

const (
	logSummaryVersion = 4
	logSummaryExt     = ".taskmeta.json"
)

// logSummary is the on-disk sidecar for compressed task-log metadata.
//
// Corpus-scale task loading depends on these sidecars: without a valid summary,
// inventory must scan and decompress the entire log. Proof is the same
// physical-log contract used by replay sidecars. Task is a direct LoadedTask
// projection rather than a separately maintained mirror; LoadedTask's JSON form
// excludes message history and runtime-only state.
type logSummary struct {
	Version int                 `json:"v"`
	Proof   logproof.CacheProof `json:"proof"`
	Task    *LoadedTask         `json:"task"`
}

func logSummaryPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), trimLogExt(filepath.Base(logPath))+logSummaryExt)
}

// loadLogSummary returns a LoadedTask reconstructed from a sidecar bound to
// the already-open physical log. The summary's completed EOF proof, current
// identity match, and freshly decoded raw header together re-establish a
// stat-only ValidatedLogSnapshot for replay-cache proof reuse.
func loadLogSummary(logPath string, file *os.File, info os.FileInfo, authority logAuthority, rawHeader []byte, meta *agent.MetaMessage) (*LoadedTask, bool) {
	identity := physicalFileIdentityFromFile(file, info)
	if !identity.Valid {
		return nil, false
	}
	if _, err := verifyPhysicalLog(logPath, file, info); err != nil {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Clean(logSummaryPath(logPath)))
	if err != nil {
		return nil, false
	}
	var summary logSummary
	if err := json.Unmarshal(data, &summary); err != nil { //nolint:musttag // LoadedTask is intentionally the direct sidecar projection.
		slog.Warn("task log summary: invalid cache", "path", logSummaryPath(logPath), "err", err)
		return nil, false
	}
	proof := CacheProof{
		Device:    identity.Device,
		Inode:     identity.Inode,
		Size:      info.Size(),
		ModTimeNs: info.ModTime().UnixNano(),
		Version:   authority.Version,
		Harness:   authority.Harness,
		RawHeader: string(rawHeader),
	}
	if summary.Version != logSummaryVersion || summary.Proof != proof || summary.Task == nil {
		return nil, false
	}
	if err := summary.Task.LogVersion.Validate(); err != nil || summary.Task.Harness == "" ||
		summary.Task.LogVersion != authority.Version || summary.Task.Harness != authority.Harness {
		return nil, false
	}
	header := loadedTaskFromMeta(logPath, taskIDFromLogBase(trimLogExt(filepath.Base(logPath))), meta, info.ModTime().UTC(), info.Size())
	cached := summary.Task
	if !header.headerMatches(cached) {
		return nil, false
	}
	validatedInfo, err := verifyPhysicalLog(logPath, file, info)
	if err != nil {
		return nil, false
	}
	cached.path = logPath
	cached.resolver = nil
	cached.Msgs = nil
	cached.messagesLoaded = false
	cached.LogSize = validatedInfo.Size()
	snapshot, err := newValidatedLogSnapshot(logPath, file, validatedInfo, authority, rawHeader, true)
	if err != nil {
		return nil, false
	}
	cached.setValidatedSnapshot(snapshot)
	return cached, true
}

func storeLogSummary(lt *LoadedTask) (retErr error) {
	if lt == nil || lt.path == "" {
		return nil
	}
	file, err := os.Open(filepath.Clean(lt.path))
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, file.Close())
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return storeLogSummaryForFile(lt, file, info)
}

func storeLogSummaryForFile(lt *LoadedTask, file *os.File, info os.FileInfo) error {
	if lt == nil || lt.path == "" {
		return nil
	}
	if err := lt.LogVersion.Validate(); err != nil {
		return err
	}
	if lt.Harness == "" {
		return errors.New("task log summary: missing harness")
	}
	stableInfo, err := verifyPhysicalLog(lt.path, file, info)
	if err != nil {
		return err
	}
	identity := physicalFileIdentityFromFile(file, stableInfo)
	if !identity.Valid {
		return errors.New("task log summary: missing physical identity")
	}
	snapshot := lt.ValidatedSnapshot()
	if snapshot == nil || !snapshot.EOFValidated || snapshot.Path != filepath.Clean(lt.path) || snapshot.Size != stableInfo.Size() ||
		snapshot.Device != identity.Device || snapshot.Inode != identity.Inode ||
		snapshot.ModTimeNs != stableInfo.ModTime().UnixNano() || snapshot.Authority.Version != lt.LogVersion ||
		snapshot.Authority.Harness != lt.Harness {
		return errors.New("task log summary: missing validated snapshot")
	}
	// LoadedTask's JSON form excludes Msgs and all unexported runtime state, so
	// it is the complete persisted projection without copying its mutexes.
	summary := logSummary{Version: logSummaryVersion, Proof: snapshot.cacheProof(), Task: lt}
	data, err := json.Marshal(summary) //nolint:musttag // LoadedTask is intentionally the direct sidecar projection.
	if err != nil {
		return err
	}
	path := logSummaryPath(lt.path)
	tmp := path + ".tmp"
	if err := os.WriteFile(filepath.Clean(tmp), data, 0o600); err != nil {
		return err
	}
	if _, err := verifyPhysicalLog(lt.path, file, stableInfo); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if _, err := verifyPhysicalLog(lt.path, file, stableInfo); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}
