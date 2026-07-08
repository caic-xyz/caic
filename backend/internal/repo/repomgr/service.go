// Service manages repository metadata, workspace registration, and change notifications.

package repomgr

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

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/repo/repowork"
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
	Info      repo.Info
	Workspace *repowork.Workspace
}

// Service owns managed repository metadata and workspace registration.
type Service struct {
	absRoot    string
	registry   *repo.Registry
	workspaces *repowork.Registry

	mu      sync.Mutex
	changed chan struct{}
}

// NewService creates a repository service.
func NewService(ctx context.Context, absRoot string, registry *repo.Registry, workspaces *repowork.Registry) *Service {
	if registry == nil {
		registry = repo.New(nil)
	}
	if workspaces == nil {
		workspaces = repowork.NewRegistry(ctx, nil)
	}
	return &Service{
		absRoot:    absRoot,
		registry:   registry,
		workspaces: workspaces,
		changed:    make(chan struct{}),
	}
}

// Registry returns the service repository registry.
func (s *Service) Registry() *repo.Registry {
	return s.registry
}

// Changed returns a channel that is closed when repository metadata changes.
func (s *Service) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// DiscoverWorkspace discovers repo metadata and creates its task workspace.
func (s *Service) DiscoverWorkspace(ctx context.Context, abs string) (InitResult, error) {
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
	info := repo.Info{
		RelPath:          rel,
		AbsPath:          abs,
		BaseBranch:       branch,
		BaseBranchRemote: remoteName,
		Remote:           remote,
		ForgeKind:        forgeKind,
		ForgeOwner:       forgeOwner,
		ForgeRepo:        forgeRepo,
	}
	workspace := &repowork.Workspace{
		BaseBranch: info.BaseBranch,
		Dir:        info.AbsPath,
		RepoName:   info.RelPath,
		GitTimeout: time.Minute,
		Log:        slog.With("repo", filepath.Base(info.AbsPath)),
	}
	return InitResult{Info: info, Workspace: workspace}, nil
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

// RegisterWorkspace adds a discovered repo and registers its workspace.
func (s *Service) RegisterWorkspace(r *InitResult, onMove func(repo.Move)) repo.Move {
	if r == nil || r.Workspace == nil {
		return repo.Move{}
	}
	move := s.registry.Add(&r.Info)
	s.workspaces.RegisterWorkspace(r.Info.RelPath, r.Workspace)
	if move.Moved() && onMove != nil {
		onMove(move)
	}
	s.notifyChanged()
	return move
}

// DeregisterWorkspace removes a repo and unregisters its workspace.
func (s *Service) DeregisterWorkspace(relPath string) {
	removed := s.registry.RemoveMatching(func(r repo.Info) bool {
		return r.RelPath == relPath
	})
	for _, rel := range removed {
		s.workspaces.UnregisterWorkspace(rel)
	}
	if len(removed) > 0 {
		s.notifyChanged()
	}
}

// Snapshot returns the current managed repository snapshot.
func (s *Service) Snapshot() []repo.Info {
	return s.registry.Snapshot()
}

// InfoFor returns the current managed repository for relPath.
func (s *Service) InfoFor(relPath string) (repo.Info, bool) {
	return s.registry.InfoFor(relPath)
}

// ByForge returns the current managed repository for a forge owner/repo pair.
func (s *Service) ByForge(owner, repoName string) (repo.Info, bool) {
	return s.registry.ByForge(owner, repoName)
}

// WorkspaceRegistered reports whether a workspace is registered for relPath.
func (s *Service) WorkspaceRegistered(relPath string) bool {
	_, ok := s.workspaces.Workspace(relPath)
	return ok
}

// Clone clones a repository, registers its metadata, and wires its workspace.
func (s *Service) Clone(ctx context.Context, req CloneRequest) (repo.Info, error) {
	targetPath := req.Path
	if targetPath == "" {
		base := filepath.Base(req.URL)
		base = strings.TrimSuffix(base, ".git")
		if base == "" || base == "." || base == "/" {
			return repo.Info{}, repoError(ErrorBadRequest, "cannot derive repo name from URL; specify path explicitly")
		}
		targetPath = base
	}

	absTarget := filepath.Join(s.absRoot, targetPath)
	if rel, err := filepath.Rel(s.absRoot, absTarget); err != nil || strings.HasPrefix(rel, "..") {
		return repo.Info{}, repoError(ErrorBadRequest, "path escapes root directory")
	} else {
		targetPath = rel
	}

	if _, err := os.Stat(absTarget); err == nil {
		return repo.Info{}, repoError(ErrorConflict, "directory already exists: "+targetPath)
	}
	if _, ok := s.workspaces.Workspace(targetPath); ok {
		return repo.Info{}, repoError(ErrorConflict, "repo already registered: "+targetPath)
	}

	bn := filepath.Base(targetPath)
	var basenameConflict string
	s.workspaces.RangeWorkspaces(func(rel string, _ *repowork.Workspace) bool {
		if rel != "" && filepath.Base(rel) == bn && rel != targetPath {
			basenameConflict = rel
			return false
		}
		return true
	})
	if basenameConflict != "" {
		return repo.Info{}, repoError(ErrorConflict, "repo basename conflicts with existing: "+basenameConflict)
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
		return repo.Info{}, repoError(ErrorInternal, "git clone failed: "+err.Error())
	}

	checkout := &git.Checkout{Root: absTarget, Logger: slog.Default()}
	remoteName, err := checkout.DefaultRemote(ctx)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return repo.Info{}, repoError(ErrorInternal, "cannot determine default remote: "+err.Error())
	}
	branch, err := checkout.DefaultBranch(ctx, remoteName)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return repo.Info{}, repoError(ErrorInternal, "cannot determine default branch: "+err.Error())
	}
	remote := checkout.RemoteOriginURL(ctx)
	info := repo.Info{RelPath: targetPath, AbsPath: absTarget, BaseBranch: branch, BaseBranchRemote: remoteName, Remote: remote}
	workspace := &repowork.Workspace{
		BaseBranch: info.BaseBranch,
		Dir:        info.AbsPath,
		RepoName:   info.RelPath,
		GitTimeout: time.Minute,
		Log:        slog.With("repo", filepath.Base(info.AbsPath)),
	}
	info.ForgeKind, info.ForgeOwner, info.ForgeRepo = parseForgeRemote(ctx, remote)
	s.registry.Add(&info)
	s.workspaces.RegisterWorkspace(targetPath, workspace)
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

func parseForgeRemote(ctx context.Context, remote string) (kind forge.Kind, owner, repoName string) {
	if remote == "" {
		return "", "", ""
	}
	kind, owner, repoName, err := forge.ParseRemoteURL(remote)
	if err != nil {
		slog.DebugContext(ctx, "unsupported forge remote", "remote", remote, "err", err)
		return "", "", ""
	}
	return kind, owner, repoName
}

func repoError(k ErrorKind, msg string) error {
	return &Error{Kind: k, Message: msg}
}
