// Tests for the repoRegistry concurrency-safe repository/CI-state store.

package server

import (
	"sync"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
)

func TestRepoRegistry(t *testing.T) {
	t.Parallel()

	t.Run("infoFor returns an independent copy", func(t *testing.T) {
		t.Parallel()
		r := newRepoRegistry([]repoInfo{{RelPath: "a", ForgeOwner: "org"}})
		got, ok := r.infoFor("a")
		if !ok {
			t.Fatal("infoFor(a) not found")
		}
		// Mutating the returned copy must not affect the registry.
		got.ForgeOwner = "mutated"
		again, _ := r.infoFor("a")
		if got.ForgeOwner != "mutated" || again.ForgeOwner != "org" {
			t.Errorf("infoFor leaked an interior reference: copy=%q registry=%q", got.ForgeOwner, again.ForgeOwner)
		}
	})

	t.Run("nil registry reads as empty", func(t *testing.T) {
		t.Parallel()
		var r *repoRegistry
		if _, ok := r.infoFor("a"); ok {
			t.Error("nil registry infoFor returned ok")
		}
		if s := r.snapshot(); s != nil {
			t.Errorf("nil registry snapshot = %v, want nil", s)
		}
		if st := r.ciStatusFor("a"); st.Status != "" {
			t.Errorf("nil registry ciStatusFor = %+v, want zero", st)
		}
	})

	t.Run("setCIStatusIfChanged reports changes", func(t *testing.T) {
		t.Parallel()
		r := newRepoRegistry(nil)
		if !r.setCIStatusIfChanged("a", ci.RepoCIState{Status: forge.CIStatusSuccess}) {
			t.Error("first set should report changed")
		}
		if r.setCIStatusIfChanged("a", ci.RepoCIState{Status: forge.CIStatusSuccess}) {
			t.Error("identical status should report unchanged")
		}
		if !r.setCIStatusIfChanged("a", ci.RepoCIState{Status: forge.CIStatusFailure}) {
			t.Error("differing status should report changed")
		}
	})

	t.Run("concurrent add and read are race-free", func(t *testing.T) {
		t.Parallel()
		r := newRepoRegistry(nil)
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				info := repoInfo{RelPath: "r", ForgeOwner: "o", ForgeRepo: "p"}
				r.add(&info)
			}()
			go func() {
				defer wg.Done()
				_ = r.snapshot()
				_, _ = r.byForge("o", "p")
				_ = r.forgePathsAtSHA("o", "p", "sha")
			}()
			_ = i
		}
		wg.Wait()
		if got := len(r.snapshot()); got != 50 {
			t.Errorf("snapshot len = %d, want 50", got)
		}
	})
}
