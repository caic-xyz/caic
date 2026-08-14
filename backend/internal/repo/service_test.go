// Tests for repository service notifications.

package repo

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/logtest"
)

func TestServiceChanged(t *testing.T) {
	t.Parallel()

	t.Run("registerCheckout invokes move hook before notifying", func(t *testing.T) {
		t.Parallel()

		repositories := NewRegistry(t.Context(), nil)
		repositories.Register(&Repository{RelPath: "old", AbsPath: "/repo"}, &Checkout{Dir: t.TempDir(), GitTimeout: time.Minute, Log: logtest.Logger(t)})
		s, err := NewService("", repositories)
		if err != nil {
			t.Fatal(err)
		}
		checkout := &Checkout{
			BaseBranch: "main",
			Dir:        "/repo",
			RepoName:   "repo",
			GitTimeout: time.Minute,
			Log:        logtest.Logger(t),
		}
		ch := s.Changed()
		var sawNotifyBeforeHook bool
		move := s.RegisterCheckout(&InitResult{Repository: Repository{RelPath: "new", AbsPath: "/repo"}, Checkout: checkout}, func(Move) {
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

	if _, err := NewService("", nil); err == nil || err.Error() != "repository registry is required" {
		t.Fatalf("NewService() error = %v, want repository registry is required", err)
	}
}
