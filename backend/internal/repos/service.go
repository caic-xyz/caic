// Service manages repository metadata, task executor registration, and change notifications.

package repos

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// ErrorKind classifies repository service errors for API adapters.
type ErrorKind string

const (
	// ErrorBadRequest is returned for invalid caller input.
	ErrorBadRequest ErrorKind = "bad_request"
	// ErrorConflict is returned for conflicting repository state.
	ErrorConflict ErrorKind = "conflict"
	// ErrorInternal is returned for operational failures.
	ErrorInternal ErrorKind = "internal"
)

// Error is a typed repository service error.
type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string { return e.Message }

// CloneRequest describes a git repository clone request.
type CloneRequest struct {
	URL   string
	Path  string
	Depth int
}

// InitResult holds the outcome of initialising a single newly-discovered
// repository.
type InitResult struct {
	Info     Info
	Executor *task.RepoExecutor
	InitErr  error
}

// Service owns managed repository metadata and task executor wiring.
type Service struct {
	absRoot       string
	logDir        string
	cacheDir      string
	harnessEnv    map[string][]string
	registry      *Registry
	taskMgr       *tasks.Manager
	runtime       runtime.Backend
	agentBackends map[harness.Name]agent.Backend

	mu      sync.Mutex
	changed chan struct{}
}

// NewService creates a repository service.
func NewService(
	absRoot string,
	logDir string,
	cacheDir string,
	harnessEnv map[string][]string,
	registry *Registry,
	taskMgr *tasks.Manager,
	runtimeBackend runtime.Backend,
	agentBackends map[harness.Name]agent.Backend,
) *Service {
	if registry == nil {
		registry = NewRegistry(nil)
	}
	return &Service{
		absRoot:       absRoot,
		logDir:        logDir,
		cacheDir:      cacheDir,
		harnessEnv:    harnessEnv,
		registry:      registry,
		taskMgr:       taskMgr,
		runtime:       runtimeBackend,
		agentBackends: agentBackends,
		changed:       make(chan struct{}),
	}
}

// Registry returns the service repository registry.
func (s *Service) Registry() *Registry {
	return s.registry
}

// Changed returns a channel that is closed when repository metadata or CI state changes.
func (s *Service) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// DiscoverExecutor discovers repo metadata and initializes its task executor.
func (s *Service) DiscoverExecutor(ctx context.Context, abs string) (InitResult, error) {
	rel := s.RelPath(abs)
	checkout := &git.Checkout{Root: abs, Logger: slog.Default()}
	remoteName, err := checkout.DefaultRemote(ctx)
	if err != nil {
		return InitResult{}, fmt.Errorf("cannot determine default remote: %w", err)
	}
	branch, err := checkout.DefaultBranch(ctx, remoteName)
	if err != nil {
		return InitResult{}, fmt.Errorf("cannot determine default branch: %w", err)
	}
	remote := checkout.RemoteOriginURL(ctx)
	forgeKind, forgeOwner, forgeRepo := parseForgeRemote(ctx, remote)
	info := Info{
		RelPath:          rel,
		AbsPath:          abs,
		BaseBranch:       branch,
		BaseBranchRemote: remoteName,
		Remote:           remote,
		ForgeKind:        forgeKind,
		ForgeOwner:       forgeOwner,
		ForgeRepo:        forgeRepo,
	}
	executor, initErr := s.newExecutor(ctx, &info)
	return InitResult{Info: info, Executor: executor, InitErr: initErr}, nil
}

// RelPath returns abs as a path relative to the repository root.
func (s *Service) RelPath(abs string) string {
	rel, err := filepath.Rel(s.absRoot, abs)
	if err != nil {
		return filepath.Base(abs)
	}
	if rel == "." {
		return ""
	}
	return rel
}

// RegisterExecutor adds a discovered repo and registers its executor.
func (s *Service) RegisterExecutor(r *InitResult) {
	if r == nil || r.Executor == nil {
		return
	}
	s.registry.Add(&r.Info)
	s.taskMgr.RegisterExecutor(r.Info.RelPath, r.Executor)
	s.notifyChanged()
}

// DeregisterExecutor removes a repo and unregisters its executor.
func (s *Service) DeregisterExecutor(relPath string) {
	removed := s.registry.RemoveMatching(func(r Info) bool {
		return r.RelPath == relPath
	})
	for _, rel := range removed {
		s.taskMgr.UnregisterExecutor(rel)
	}
	if len(removed) > 0 {
		s.notifyChanged()
	}
}

// Snapshot returns the current managed repository snapshot.
func (s *Service) Snapshot() []Info {
	return s.registry.Snapshot()
}

// SnapshotWithCI returns the current managed repository and CI snapshot.
func (s *Service) SnapshotWithCI() []InfoWithCI {
	return s.registry.SnapshotWithCI()
}

// InfoFor returns the current managed repository for relPath.
func (s *Service) InfoFor(relPath string) (Info, bool) {
	return s.registry.InfoFor(relPath)
}

// ByForge returns the current managed repository for a forge owner/repo pair.
func (s *Service) ByForge(owner, repo string) (Info, bool) {
	return s.registry.ByForge(owner, repo)
}

// ExecutorRegistered reports whether an executor is registered for relPath.
func (s *Service) ExecutorRegistered(relPath string) bool {
	_, ok := s.taskMgr.Executor(relPath)
	return ok
}

// RegisterNoRepoExecutor initializes and registers the no-repo executor.
func (s *Service) RegisterNoRepoExecutor(ctx context.Context) error {
	noRepoExecutor, err := s.newExecutor(ctx, nil)
	if err != nil {
		return fmt.Errorf("init no-repo executor: %w", err)
	}
	s.taskMgr.RegisterExecutor("", noRepoExecutor)
	return nil
}

// AdoptionRepos returns repo metadata in the shape expected by tasks.Manager.
func (s *Service) AdoptionRepos() []tasks.AdoptRepo {
	snap := s.registry.Snapshot()
	adoptRepos := make([]tasks.AdoptRepo, len(snap))
	for i := range snap {
		r := &snap[i]
		adoptRepos[i] = tasks.AdoptRepo{
			RelPath:    r.RelPath,
			AbsPath:    r.AbsPath,
			ForgeKind:  string(r.ForgeKind),
			ForgeOwner: r.ForgeOwner,
			ForgeRepo:  r.ForgeRepo,
		}
	}
	return adoptRepos
}

// CIStatusFor returns the cached CI status for relPath.
func (s *Service) CIStatusFor(relPath string) ci.RepoCIState {
	return s.registry.CIStatusFor(relPath)
}

// ForgePathsAtSHA returns repository paths with matching forge coordinates and CI SHA.
func (s *Service) ForgePathsAtSHA(owner, repo, sha string) []string {
	return s.registry.ForgePathsAtSHA(owner, repo, sha)
}

// SetCIStatusIfChanged stores the CI status for relPath and reports status changes.
func (s *Service) SetCIStatusIfChanged(relPath, sha string, result forgecache.Result) bool {
	checks := make([]forge.Check, len(result.Checks))
	copy(checks, result.Checks)
	next := ci.RepoCIState{Status: result.Status, Checks: checks, HeadSHA: sha}
	changed := s.registry.SetCIStatusIfChanged(relPath, next)
	if changed {
		s.notifyChanged()
	}
	return changed
}

// Clone clones a repository, registers its metadata, and wires its executor.
func (s *Service) Clone(ctx context.Context, req CloneRequest) (Info, error) {
	targetPath := req.Path
	if targetPath == "" {
		base := filepath.Base(req.URL)
		base = strings.TrimSuffix(base, ".git")
		if base == "" || base == "." || base == "/" {
			return Info{}, repoError(ErrorBadRequest, "cannot derive repo name from URL; specify path explicitly")
		}
		targetPath = base
	}

	absTarget := filepath.Join(s.absRoot, targetPath)
	if rel, err := filepath.Rel(s.absRoot, absTarget); err != nil || strings.HasPrefix(rel, "..") {
		return Info{}, repoError(ErrorBadRequest, "path escapes root directory")
	} else {
		targetPath = rel
	}

	if _, err := os.Stat(absTarget); err == nil {
		return Info{}, repoError(ErrorConflict, "directory already exists: "+targetPath)
	}
	if _, ok := s.taskMgr.Executor(targetPath); ok {
		return Info{}, repoError(ErrorConflict, "repo already registered: "+targetPath)
	}

	bn := filepath.Base(targetPath)
	var basenameConflict string
	s.taskMgr.RangeExecutors(func(rel string, _ *task.RepoExecutor) bool {
		if rel != "" && filepath.Base(rel) == bn && rel != targetPath {
			basenameConflict = rel
			return false
		}
		return true
	})
	if basenameConflict != "" {
		return Info{}, repoError(ErrorConflict, "repo basename conflicts with existing: "+basenameConflict)
	}

	depth := req.Depth
	if depth == 0 {
		depth = 1
	}

	cloneCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	args := []string{"clone", "--depth", strconv.Itoa(depth), "--recurse-submodules", "--shallow-submodules", req.URL, absTarget}
	cmd := exec.CommandContext(cloneCtx, "git", args...) //nolint:gosec // args are validated: depth is an int, URL is user-provided input, absTarget is validated above
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(absTarget)
		slog.WarnContext(ctx, "git clone failed", "url", req.URL, "err", err, "out", string(out))
		return Info{}, repoError(ErrorInternal, "git clone failed: "+err.Error())
	}

	checkout := &git.Checkout{Root: absTarget, Logger: slog.Default()}
	remoteName, err := checkout.DefaultRemote(ctx)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return Info{}, repoError(ErrorInternal, "cannot determine default remote: "+err.Error())
	}
	branch, err := checkout.DefaultBranch(ctx, remoteName)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return Info{}, repoError(ErrorInternal, "cannot determine default branch: "+err.Error())
	}
	remote := checkout.RemoteOriginURL(ctx)
	info := Info{RelPath: targetPath, AbsPath: absTarget, BaseBranch: branch, BaseBranchRemote: remoteName, Remote: remote}
	executor, err := s.newExecutor(ctx, &info)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return Info{}, repoError(ErrorInternal, "failed to init executor: "+err.Error())
	}
	info.ForgeKind, info.ForgeOwner, info.ForgeRepo = parseForgeRemote(ctx, remote)
	s.registry.Add(&info)
	s.taskMgr.RegisterExecutor(targetPath, executor)
	s.notifyChanged()
	slog.InfoContext(ctx, "cloned repo", "url", req.URL, "path", targetPath)

	return info, nil
}

func (s *Service) notifyChanged() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) newExecutor(ctx context.Context, info *Info) (*task.RepoExecutor, error) {
	executor := &task.RepoExecutor{
		RepoWorkspace: task.RepoWorkspace{
			Runtime: s.runtime,
		},
		LogDir:     s.logDir,
		CacheDir:   s.cacheDir,
		Backends:   s.agentBackends,
		HarnessEnv: s.harnessEnv,
		EventReplayFactory: func(path string, h harness.Name) task.EventReplayWriter {
			return eventreplay.NewMessageWriter(path, h)
		},
	}
	if info != nil {
		executor.BaseBranch = info.BaseBranch
		executor.Dir = info.AbsPath
		executor.RepoName = info.RelPath
	}
	err := executor.Init(ctx)
	return executor, err
}

func parseForgeRemote(ctx context.Context, remote string) (kind forge.Kind, owner, repo string) {
	if remote == "" {
		return "", "", ""
	}
	kind, owner, repo, err := forge.ParseRemoteURL(remote)
	if err != nil {
		slog.DebugContext(ctx, "unsupported forge remote", "remote", remote, "err", err)
		return "", "", ""
	}
	return kind, owner, repo
}

func repoError(k ErrorKind, msg string) error {
	return &Error{Kind: k, Message: msg}
}
