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

// DiscoverCheckout discovers one local checkout.
func (s *Service) DiscoverCheckout(ctx context.Context, abs string) (*Checkout, error) {
	rel := s.RelPath(abs)
	gitCheckout := &git.Checkout{Root: abs, Logger: s.log}
	remoteName, err := gitCheckout.DefaultRemote(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot determine default remote: %w", err)
	}
	branch, err := gitCheckout.DefaultBranch(ctx, remoteName)
	if err != nil {
		return nil, fmt.Errorf("cannot determine default branch: %w", err)
	}
	remote := gitCheckout.RemoteOriginURL(ctx)
	forgeKind, forgeOwner, forgeRepo := parseForgeRemote(ctx, s.log, remote)
	checkout, err := NewCheckout(ctx, s.log.With(slog.String("repo", rel)), abs, branch)
	if err != nil {
		return nil, fmt.Errorf("initialize checkout: %w", err)
	}
	if remote != "" {
		checkout.Repository = s.Repositories.RegisterRepository(Repository{Remote: remote, ForgeKind: forgeKind, ForgeOwner: forgeOwner, ForgeRepo: forgeRepo})
	}
	checkout.RelPath = rel
	checkout.BaseBranchRemote = remoteName
	return checkout, nil
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

// RegisterCheckout records a discovered local checkout.
func (s *Service) RegisterCheckout(checkout *Checkout) error {
	if err := s.Repositories.RegisterCheckout(checkout); err != nil {
		return err
	}
	s.notifyChanged()
	return nil
}

// UnregisterCheckout removes a current local checkout from the registry.
func (s *Service) UnregisterCheckout(relPath string) {
	if s.Repositories.UnregisterCheckout(relPath) {
		s.notifyChanged()
	}
}

// Clone clones and registers a local checkout.
func (s *Service) Clone(ctx context.Context, req CloneRequest) (*Checkout, error) {
	targetPath := req.Path
	if targetPath == "" {
		base := filepath.Base(req.URL)
		base = strings.TrimSuffix(base, ".git")
		if base == "" || base == "." || base == "/" {
			return nil, repoError(ErrorBadRequest, "cannot derive repo name from URL; specify path explicitly")
		}
		targetPath = base
	}

	absTarget := filepath.Join(s.absRoot, targetPath)
	if rel, err := filepath.Rel(s.absRoot, absTarget); err != nil || strings.HasPrefix(rel, "..") {
		return nil, repoError(ErrorBadRequest, "path escapes root directory")
	} else {
		targetPath = rel
	}

	if _, err := os.Stat(absTarget); err == nil {
		return nil, repoError(ErrorConflict, "directory already exists: "+targetPath)
	}
	if _, ok := s.Repositories.Checkout(targetPath); ok {
		return nil, repoError(ErrorConflict, "repo already registered: "+targetPath)
	}

	bn := filepath.Base(targetPath)
	var basenameConflict string
	for checkout := range s.Repositories.Checkouts() {
		if checkout.RelPath != "" && filepath.Base(checkout.RelPath) == bn && checkout.RelPath != targetPath {
			basenameConflict = checkout.RelPath
			break
		}
	}
	if basenameConflict != "" {
		return nil, repoError(ErrorConflict, "repo basename conflicts with existing: "+basenameConflict)
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
		return nil, repoError(ErrorInternal, "git clone failed: "+err.Error())
	}

	gitCheckout := &git.Checkout{Root: absTarget, Logger: s.log}
	remoteName, err := gitCheckout.DefaultRemote(ctx)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, repoError(ErrorInternal, "cannot determine default remote: "+err.Error())
	}
	branch, err := gitCheckout.DefaultBranch(ctx, remoteName)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, repoError(ErrorInternal, "cannot determine default branch: "+err.Error())
	}
	remote := gitCheckout.RemoteOriginURL(ctx)
	checkout, err := NewCheckout(ctx, s.log.With(slog.String("repo", targetPath)), absTarget, branch)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, repoError(ErrorInternal, "initialize checkout: "+err.Error())
	}
	forgeKind, forgeOwner, forgeRepo := parseForgeRemote(ctx, s.log, remote)
	if remote != "" {
		checkout.Repository = s.Repositories.RegisterRepository(Repository{Remote: remote, ForgeKind: forgeKind, ForgeOwner: forgeOwner, ForgeRepo: forgeRepo})
	}
	checkout.RelPath = targetPath
	checkout.BaseBranchRemote = remoteName
	if err := s.RegisterCheckout(checkout); err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, repoError(ErrorConflict, err.Error())
	}
	s.log.InfoContext(ctx, "cloned repo", "url", req.URL, "path", targetPath)

	return checkout, nil
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
