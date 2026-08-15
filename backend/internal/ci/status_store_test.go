// Tests for repository CI status cache behavior.

package ci

import (
	"fmt"
	"sync"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
)

func TestRepoStatusStore(t *testing.T) {
	t.Parallel()

	t.Run("set reports only status changes", func(t *testing.T) {
		t.Parallel()
		s := NewRepoStatusStore()
		ch := s.Changed()
		checks := []forge.Check{{Name: "test"}}
		result := forgecache.Result{Status: forge.CIStatusSuccess, Checks: checks}
		if !s.SetResultIfChanged("a", "sha1", result) {
			t.Fatal("first set should report changed")
		}
		select {
		case <-ch:
		default:
			t.Fatal("Changed channel was not closed")
		}

		checks[0].Name = "mutated"
		st, ok := s.StatusFor("a")
		if !ok || st.Status != forge.CIStatusSuccess || st.HeadSHA != "sha1" || st.Checks[0].Name != "test" {
			t.Fatalf("stored status = %+v, ok=%v", st, ok)
		}
		st.Checks[0].Name = "returned-copy-mutated"
		st, _ = s.StatusFor("a")
		if st.Checks[0].Name != "test" {
			t.Fatalf("StatusFor leaked checks slice: %+v", st.Checks)
		}

		ch = s.Changed()
		if s.SetResultIfChanged("a", "sha2", forgecache.Result{Status: forge.CIStatusSuccess}) {
			t.Fatal("same status should report unchanged")
		}
		select {
		case <-ch:
			t.Fatal("Changed channel closed for unchanged status")
		default:
		}
		st, _ = s.StatusFor("a")
		if st.HeadSHA != "sha2" || len(st.Checks) != 0 {
			t.Fatalf("unchanged status did not update payload: %+v", st)
		}

		if !s.SetResultIfChanged("a", "sha3", forgecache.Result{Status: forge.CIStatusFailure}) {
			t.Fatal("differing status should report changed")
		}
	})

	t.Run("paths at SHA matches repo refs", func(t *testing.T) {
		t.Parallel()
		s := NewRepoStatusStore()
		s.SetResultIfChanged("a", "sha", forgecache.Result{Status: forge.CIStatusSuccess})
		s.SetResultIfChanged("b", "other", forgecache.Result{Status: forge.CIStatusSuccess})
		refs := []RepoRef{
			{RelPath: "a", ForgeOwner: "org", ForgeRepo: "repo"},
			{RelPath: "b", ForgeOwner: "org", ForgeRepo: "repo"},
			{RelPath: "c", ForgeOwner: "org", ForgeRepo: "repo"},
		}
		got := s.PathsAtSHA(refs, "org", "repo", "sha")
		if len(got) != 1 || got[0] != "a" {
			t.Fatalf("PathsAtSHA = %v, want [a]", got)
		}
	})

	t.Run("concurrent set and read are race-free", func(t *testing.T) {
		t.Parallel()
		s := NewRepoStatusStore()
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				rel := fmt.Sprintf("r-%d", i)
				s.SetResultIfChanged(rel, "sha", forgecache.Result{Status: forge.CIStatusSuccess})
			}()
			go func() {
				defer wg.Done()
				_, _ = s.StatusFor("r-0")
				_ = s.PathsAtSHA([]RepoRef{{RelPath: "r-0", ForgeOwner: "o", ForgeRepo: "p"}}, "o", "p", "sha")
			}()
			_ = i
		}
		wg.Wait()
	})
}
