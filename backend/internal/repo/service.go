// Service discovers, clones, and registers repositories and their checkouts.

package repo

import (
	"context"
	"errors"
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

// InitResult holds one discovered repository and its initialized checkout.
type InitResult struct {
	Repository Repository
	Checkout   *Checkout
}

// Service discovers and clones repositories into one checkout registry.
type Service struct {
	// Immutable.
	Repositories *Registry

	log     *slog.Logger
	absRoot string

	// Guarded by mu.
	mu      sync.Mutex
	changed chan struct{}
}

// NewService creates a repository service.
func NewService(absRoot string, repositories *Registry) (*Service, error) {
	if repositories == nil {
		return nil, errors.New("repository registry is required")
	}
	return &Service{
		Repositories: repositories,
		log:          slog.Default().With(slog.String("cmp", "reposvc")),
		absRoot:      absRoot,
		changed:      make(chan struct{}),
	}, nil
}

// Changed returns a channel that is closed when repository metadata changes.
func (s *Service) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// DiscoverCheckout discovers repo metadata and creates its task checkout.
func (s *Service) DiscoverCheckout(ctx context.Context, abs string) (InitResult, error) {
	rel := s.RelPath(abs)
	gitCheckout := &git.Checkout{Root: abs, Logger: s.log}
	remoteName, err := gitCheckout.DefaultRemote(ctx)
	if err != nil {
		return InitResult{}, fmt.Errorf("cannot determine default remote: %w", err)
	}
	branch, err := gitCheckout.DefaultBranch(ctx, remoteName)
	if err != nil {
		return InitResult{}, fmt.Errorf("cannot determine default branch: %w", err)
	}
	remote := gitCheckout.RemoteOriginURL(ctx)
	forgeKind, forgeOwner, forgeRepo := parseForgeRemote(ctx, s.log, remote)
	info := Repository{
		RelPath:          rel,
		AbsPath:          abs,
		BaseBranch:       branch,
		BaseBranchRemote: remoteName,
		Remote:           remote,
		ForgeKind:        forgeKind,
		ForgeOwner:       forgeOwner,
		ForgeRepo:        forgeRepo,
	}
	checkout, err := NewCheckout(ctx, s.log.With(slog.String("repo", info.RelPath)), info.AbsPath, info.BaseBranch)
	if err != nil {
		return InitResult{}, fmt.Errorf("initialize checkout: %w", err)
	}
	return InitResult{Repository: info, Checkout: checkout}, nil
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

// RegisterCheckout adds a discovered repository and checkout atomically.
func (s *Service) RegisterCheckout(r *InitResult, onMove func(Move)) Move {
	move := s.Repositories.Register(&r.Repository, r.Checkout)
	if move.Moved() && onMove != nil {
		onMove(move)
	}
	s.notifyChanged()
	return move
}

// DeregisterCheckout removes a repository and its current checkout.
func (s *Service) DeregisterCheckout(relPath string) {
	if s.Repositories.Remove(relPath) {
		s.notifyChanged()
	}
}

// Clone clones a repository, registers its metadata, and wires its checkout.
func (s *Service) Clone(ctx context.Context, req CloneRequest) (Repository, error) {
	targetPath := req.Path
	if targetPath == "" {
		base := filepath.Base(req.URL)
		base = strings.TrimSuffix(base, ".git")
		if base == "" || base == "." || base == "/" {
			return Repository{}, repoError(ErrorBadRequest, "cannot derive repo name from URL; specify path explicitly")
		}
		targetPath = base
	}

	absTarget := filepath.Join(s.absRoot, targetPath)
	if rel, err := filepath.Rel(s.absRoot, absTarget); err != nil || strings.HasPrefix(rel, "..") {
		return Repository{}, repoError(ErrorBadRequest, "path escapes root directory")
	} else {
		targetPath = rel
	}

	if _, err := os.Stat(absTarget); err == nil {
		return Repository{}, repoError(ErrorConflict, "directory already exists: "+targetPath)
	}
	if _, ok := s.Repositories.Checkout(targetPath); ok {
		return Repository{}, repoError(ErrorConflict, "repo already registered: "+targetPath)
	}

	bn := filepath.Base(targetPath)
	var basenameConflict string
	for checkout := range s.Repositories.Checkouts() {
		if checkout.RepoName != "" && filepath.Base(checkout.RepoName) == bn && checkout.RepoName != targetPath {
			basenameConflict = checkout.RepoName
			break
		}
	}
	if basenameConflict != "" {
		return Repository{}, repoError(ErrorConflict, "repo basename conflicts with existing: "+basenameConflict)
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
		s.log.WarnContext(ctx, "git clone failed", "url", req.URL, "err", err, "out", string(out))
		return Repository{}, repoError(ErrorInternal, "git clone failed: "+err.Error())
	}

	gitCheckout := &git.Checkout{Root: absTarget, Logger: s.log}
	remoteName, err := gitCheckout.DefaultRemote(ctx)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return Repository{}, repoError(ErrorInternal, "cannot determine default remote: "+err.Error())
	}
	branch, err := gitCheckout.DefaultBranch(ctx, remoteName)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return Repository{}, repoError(ErrorInternal, "cannot determine default branch: "+err.Error())
	}
	remote := gitCheckout.RemoteOriginURL(ctx)
	info := Repository{RelPath: targetPath, AbsPath: absTarget, BaseBranch: branch, BaseBranchRemote: remoteName, Remote: remote}
	checkout, err := NewCheckout(ctx, s.log.With(slog.String("repo", info.RelPath)), info.AbsPath, info.BaseBranch)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return Repository{}, repoError(ErrorInternal, "initialize checkout: "+err.Error())
	}
	info.ForgeKind, info.ForgeOwner, info.ForgeRepo = parseForgeRemote(ctx, s.log, remote)
	s.Repositories.Register(&info, checkout)
	s.notifyChanged()
	s.log.InfoContext(ctx, "cloned repo", "url", req.URL, "path", targetPath)

	return info, nil
}

func (s *Service) notifyChanged() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func parseForgeRemote(ctx context.Context, log *slog.Logger, remote string) (kind forge.Kind, owner, repoName string) {
	if remote == "" {
		return "", "", ""
	}
	kind, owner, repoName, err := forge.ParseRemoteURL(remote)
	if err != nil {
		log.DebugContext(ctx, "unsupported forge remote", "remote", remote, "err", err)
		return "", "", ""
	}
	return kind, owner, repoName
}

func repoError(k ErrorKind, msg string) error {
	return &Error{Kind: k, Message: msg}
}
