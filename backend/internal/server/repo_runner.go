// Repository route adapters for managed repositories.

package server

import (
	"context"
	"errors"

	"github.com/caic-xyz/md/gitutil"

	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// DiscoverRepoRunner discovers repo metadata and initializes its task runner.
func (s *Server) DiscoverRepoRunner(ctx context.Context, abs string) (repos.InitResult, error) {
	s.initConcernAdapters()
	return s.repos.DiscoverRunner(ctx, abs)
}

// RepoRelPath returns abs as a path relative to the server repo root.
func (s *Server) RepoRelPath(abs string) string {
	s.initConcernAdapters()
	return s.repos.RelPath(abs)
}

// RegisterRepoRunner adds a discovered repo and registers its runner.
func (s *Server) RegisterRepoRunner(r *repos.InitResult) {
	s.initConcernAdapters()
	s.repos.RegisterRunner(r)
}

// DeregisterRepoRunner removes a repo and unregisters its runner.
func (s *Server) DeregisterRepoRunner(relPath string) {
	s.initConcernAdapters()
	s.repos.DeregisterRunner(relPath)
}

// RepoSnapshot returns the current managed repository snapshot.
func (s *Server) RepoSnapshot() []repos.Info {
	s.initConcernAdapters()
	return s.repos.Snapshot()
}

// RunnerRegistered reports whether a runner is registered for relPath.
func (s *Server) RunnerRegistered(relPath string) bool {
	s.initConcernAdapters()
	return s.repos.RunnerRegistered(relPath)
}

// RegisterNoRepoRunner initializes and registers the no-repo runner.
func (s *Server) RegisterNoRepoRunner(ctx context.Context) {
	s.initConcernAdapters()
	s.repos.RegisterNoRepoRunner(ctx)
}

// AdoptionRepos returns repo metadata in the shape expected by tasks.Manager.
func (s *Server) AdoptionRepos() []tasks.AdoptRepo {
	s.initConcernAdapters()
	return s.repos.AdoptionRepos()
}

// WarnRepoBasenameCollisions logs repos whose basenames may confuse users.
func (s *Server) WarnRepoBasenameCollisions() {
	s.initConcernAdapters()
	s.repos.WarnBasenameCollisions()
}

func repoListFromSnapshot(snap []repos.InfoWithCI) *[]v1.Repo {
	out := make([]v1.Repo, len(snap))
	for i := range snap {
		out[i] = repoDTO(&snap[i])
	}
	return &out
}

func repoDTO(status *repos.InfoWithCI) v1.Repo {
	info := &status.Info
	repo := v1.Repo{
		Path:       info.RelPath,
		Branch:     info.BaseBranch,
		BaseBranch: v1.BranchInfo{Name: info.BaseBranch, Remote: info.BaseBranchRemote},
		RemoteURL:  gitutil.RemoteToHTTPS(info.Remote),
		Forge:      v1.Forge(info.ForgeKind),
	}
	if status.HasCI {
		repo.CI = v1.CIStatus(status.CI.Status)
		repo.CIChecks = make([]v1.ForgeCheck, len(status.CI.Checks))
		for i := range status.CI.Checks {
			repo.CIChecks[i] = v1conv.ForgeCheck(&status.CI.Checks[i])
		}
	}
	return repo
}

func cloneRepoDTO(ctx context.Context, s *repos.Service, req *v1.CloneRepoReq) (*v1.Repo, error) {
	info, err := s.Clone(ctx, repos.CloneRequest{URL: req.URL, Path: req.Path, Depth: req.Depth})
	if err != nil {
		return nil, repoServiceError(err)
	}
	return &v1.Repo{
		Path:       info.RelPath,
		Branch:     info.BaseBranch,
		BaseBranch: v1.BranchInfo{Name: info.BaseBranch, Remote: info.BaseBranchRemote},
		RemoteURL:  gitutil.RemoteToHTTPS(info.Remote),
		Forge:      v1.Forge(info.ForgeKind),
	}, nil
}

func repoServiceError(err error) error {
	var repoErr *repos.Error
	if !errors.As(err, &repoErr) {
		return api.InternalError(err.Error())
	}
	switch repoErr.Kind {
	case repos.ErrorBadRequest:
		return api.BadRequest(repoErr.Message)
	case repos.ErrorConflict:
		return api.Conflict(repoErr.Message)
	default:
		return api.InternalError(repoErr.Message)
	}
}
