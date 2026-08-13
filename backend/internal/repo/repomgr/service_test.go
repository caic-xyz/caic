// Tests for repository service notifications.

package repomgr

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/repo/repowork"
)

func TestServiceChanged(t *testing.T) {
	t.Parallel()

	t.Run("registerWorkspace invokes move hook before notifying", func(t *testing.T) {
		t.Parallel()

		workspaces := repowork.NewRegistry(t.Context(), nil)
		s, err := NewService("", repo.New([]repo.Info{{RelPath: "old", AbsPath: "/repo"}}), workspaces)
		if err != nil {
			t.Fatal(err)
		}
		workspace := &repowork.Workspace{
			BaseBranch: "main",
			Dir:        "/repo",
			RepoName:   "repo",
			GitTimeout: time.Minute,
			Log:        logtest.Logger(t),
		}
		ch := s.Changed()
		var sawNotifyBeforeHook bool
		move := s.RegisterWorkspace(&InitResult{Info: repo.Info{RelPath: "new", AbsPath: "/repo"}, Workspace: workspace}, func(repo.Move) {
			select {
			case <-ch:
				sawNotifyBeforeHook = true
			default:
			}
		})
		if !move.Moved() || move.OldRel != "old" || move.NewRel != "new" {
			t.Fatalf("move = %+v, want old -> new", move)
		}
		if sawNotifyBeforeHook {
			t.Fatal("Changed channel closed before move hook ran")
		}
		select {
		case <-ch:
		default:
			t.Fatal("Changed channel was not closed")
		}
	})
}

func TestNewServiceRequiresDependencies(t *testing.T) {
	t.Parallel()

	workspaces := repowork.NewRegistry(t.Context(), nil)
	if _, err := NewService("", nil, workspaces); err == nil || err.Error() != "repository registry is required" {
		t.Fatalf("NewService() error = %v, want repository registry is required", err)
	}
	if _, err := NewService("", repo.New(nil), nil); err == nil || err.Error() != "workspace registry is required" {
		t.Fatalf("NewService() error = %v, want workspace registry is required", err)
	}
}
