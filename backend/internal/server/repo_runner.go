// Repository runner construction for managed repositories.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/caic-xyz/md/gitutil"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// RepoInitResult holds the outcome of initialising a single newly-discovered
// repository.
type RepoInitResult struct {
	Info    RepoInfo
	Runner  *task.Runner
	InitErr error
}

// DiscoverRepoRunner discovers repo metadata and initializes its task runner.
func (s *Server) DiscoverRepoRunner(ctx context.Context, abs string) (RepoInitResult, error) {
	rel := s.repoRelPath(abs)
	remoteName, err := gitutil.DefaultRemote(ctx, abs)
	if err != nil {
		return RepoInitResult{}, fmt.Errorf("cannot determine default remote: %w", err)
	}
	branch, err := gitutil.DefaultBranch(ctx, abs, remoteName)
	if err != nil {
		return RepoInitResult{}, fmt.Errorf("cannot determine default branch: %w", err)
	}
	remote := gitutil.RemoteOriginURL(ctx, abs)
	var forgeKind forge.Kind
	var forgeOwner, forgeRepo string
	if rawURL, err := forge.RemoteURL(ctx, abs); err == nil {
		forgeKind, forgeOwner, forgeRepo, _ = forge.ParseRemoteURL(rawURL)
	}
	info := RepoInfo{
		RelPath:          rel,
		AbsPath:          abs,
		BaseBranch:       branch,
		BaseBranchRemote: remoteName,
		Remote:           remote,
		ForgeKind:        forgeKind,
		ForgeOwner:       forgeOwner,
		ForgeRepo:        forgeRepo,
	}
	runner, initErr := s.newRunner(ctx, &info)
	return RepoInitResult{Info: info, Runner: runner, InitErr: initErr}, nil
}

func (s *Server) repoRelPath(abs string) string {
	rel, err := filepath.Rel(s.absRoot, abs)
	if err != nil {
		return filepath.Base(abs)
	}
	if rel == "." {
		return ""
	}
	return rel
}

// RepoRelPath returns abs as a path relative to the server repo root.
func (s *Server) RepoRelPath(abs string) string {
	return s.repoRelPath(abs)
}

func (s *Server) newRunner(ctx context.Context, info *RepoInfo) (*task.Runner, error) {
	runner := &task.Runner{
		LogDir:     s.logDir,
		CacheDir:   s.cacheDir,
		Backends:   s.agentBackends,
		HarnessEnv: s.harnessEnv,
		Runtime:    s.runtimeBackend,
	}
	if info != nil {
		runner.BaseBranch = info.BaseBranch
		runner.Dir = info.AbsPath
		runner.RepoName = info.RelPath
	}
	err := runner.Init(ctx)
	return runner, err
}

// RegisterRepoRunner adds a discovered repo and registers its runner.
func (s *Server) RegisterRepoRunner(r *RepoInitResult) {
	if r == nil || r.Runner == nil {
		return
	}
	s.repoReg.add(&r.Info)
	s.taskMgr.RegisterRunner(r.Info.RelPath, r.Runner)
}

// DeregisterRepoRunner removes a repo and unregisters its runner.
func (s *Server) DeregisterRepoRunner(relPath string) {
	removed := s.repoReg.removeMatching(func(r RepoInfo) bool {
		return r.RelPath == relPath
	})
	for _, rel := range removed {
		s.taskMgr.UnregisterRunner(rel)
	}
}

// RepoSnapshot returns the current managed repository snapshot.
func (s *Server) RepoSnapshot() []RepoInfo {
	return s.repoReg.snapshot()
}

// RunnerRegistered reports whether a runner is registered for relPath.
func (s *Server) RunnerRegistered(relPath string) bool {
	_, ok := s.taskMgr.Runner(relPath)
	return ok
}

// RegisterNoRepoRunner initializes and registers the no-repo runner.
func (s *Server) RegisterNoRepoRunner(ctx context.Context) {
	noRepoRunner, _ := s.newRunner(ctx, nil)
	s.taskMgr.RegisterRunner("", noRepoRunner)
}

// AdoptionRepos returns repo metadata in the shape expected by tasks.Manager.
func (s *Server) AdoptionRepos() []tasks.AdoptRepo {
	snap := s.repoReg.snapshot()
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

// WarnRepoBasenameCollisions logs repos whose basenames may confuse users.
func (s *Server) WarnRepoBasenameCollisions() {
	seen := make(map[string]string)
	snap := s.repoReg.snapshot()
	for i := range snap {
		ri := &snap[i]
		bn := filepath.Base(ri.AbsPath)
		if first, exists := seen[bn]; exists {
			slog.Warn("repo basename collision; containers will use qualified names",
				"a", first, "b", ri.RelPath, "basename", bn)
		} else {
			seen[bn] = ri.RelPath
		}
	}
}
