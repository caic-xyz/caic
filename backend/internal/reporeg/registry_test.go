// Tests for the Registry concurrency-safe repository identity store.

package reporeg

import (
	"fmt"
	"sync"
	"testing"
)

func TestRepoRegistry(t *testing.T) {
	t.Parallel()

	t.Run("infoFor returns an independent copy", func(t *testing.T) {
		t.Parallel()
		r := New([]Info{{RelPath: "a", ForgeOwner: "org"}})
		got, ok := r.InfoFor("a")
		if !ok {
			t.Fatal("infoFor(a) not found")
		}
		// Mutating the returned copy must not affect the registry.
		got.ForgeOwner = "mutated"
		again, _ := r.InfoFor("a")
		if got.ForgeOwner != "mutated" || again.ForgeOwner != "org" {
			t.Errorf("infoFor leaked an interior reference: copy=%q registry=%q", got.ForgeOwner, again.ForgeOwner)
		}
	})

	t.Run("add is idempotent by rel path and abs path", func(t *testing.T) {
		t.Parallel()
		r := New(nil)
		if move := r.Add(&Info{RelPath: "a", AbsPath: "/repo/a", BaseBranch: "main"}); move.Moved() {
			t.Fatalf("first add move = %+v, want none", move)
		}
		move := r.Add(&Info{RelPath: "a", AbsPath: "/repo/a", BaseBranch: "trunk"})
		if move.Moved() {
			t.Fatalf("same rel+abs move = %+v, want none", move)
		}
		if got := r.Snapshot(); len(got) != 1 || got[0].BaseBranch != "trunk" {
			t.Fatalf("same rel+abs add = %+v, want one updated repo", got)
		}

		move = r.Add(&Info{RelPath: "nested/a", AbsPath: "/repo/a", BaseBranch: "main"})
		if !move.Moved() || move.OldRel != "a" || move.NewRel != "nested/a" {
			t.Fatalf("same abs move = %+v, want a -> nested/a", move)
		}
		got := r.Snapshot()
		if len(got) != 1 || got[0].RelPath != "nested/a" {
			t.Fatalf("same abs add = %+v, want one repo with updated rel path", got)
		}

		move = r.Add(&Info{RelPath: "nested/a", AbsPath: "/repo/other", BaseBranch: "develop"})
		if move.Moved() {
			t.Fatalf("same rel move = %+v, want none", move)
		}
		got = r.Snapshot()
		if len(got) != 1 || got[0].AbsPath != "/repo/other" {
			t.Fatalf("same rel add = %+v, want one repo with updated abs path", got)
		}
	})

	t.Run("add reports root relpath move", func(t *testing.T) {
		t.Parallel()
		r := New([]Info{{RelPath: "", AbsPath: "/repo"}})
		move := r.Add(&Info{RelPath: "nested", AbsPath: "/repo"})
		if !move.Moved() || move.OldRel != "" || move.NewRel != "nested" {
			t.Fatalf("root relpath move = %+v, want empty -> nested", move)
		}
	})

	t.Run("concurrent add and read are race-free", func(t *testing.T) {
		t.Parallel()
		r := New(nil)
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				info := Info{RelPath: fmt.Sprintf("r-%d", i), AbsPath: fmt.Sprintf("/repo/%d", i), ForgeOwner: "o", ForgeRepo: "p"}
				r.Add(&info)
			}()
			go func() {
				defer wg.Done()
				_ = r.Snapshot()
				_, _ = r.ByForge("o", "p")
			}()
			_ = i
		}
		wg.Wait()
		if got := len(r.Snapshot()); got != 50 {
			t.Errorf("snapshot len = %d, want 50", got)
		}
	})
}
