// Tests for repository service notifications.

package repos

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
)

func TestServiceChanged(t *testing.T) {
	t.Parallel()

	t.Run("setCIStatusIfChanged", func(t *testing.T) {
		t.Parallel()

		s := NewService("", "", "", nil, NewRegistry(nil), nil, nil, nil)
		ch := s.Changed()
		if !s.SetCIStatusIfChanged("repo", "sha", forgecache.Result{Status: forge.CIStatusSuccess}) {
			t.Fatal("SetCIStatusIfChanged() = false, want true")
		}
		select {
		case <-ch:
		default:
			t.Fatal("Changed channel was not closed")
		}

		ch = s.Changed()
		if s.SetCIStatusIfChanged("repo", "sha", forgecache.Result{Status: forge.CIStatusSuccess}) {
			t.Fatal("SetCIStatusIfChanged() = true, want false")
		}
		select {
		case <-ch:
			t.Fatal("Changed channel closed for unchanged status")
		default:
		}
	})
}
