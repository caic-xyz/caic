// Tests for the Backend interface's default Base implementation.

package agent

import (
	"sync"
	"testing"
)

func TestBase(t *testing.T) {
	t.Parallel()

	t.Run("ModelInventoryDefaultsToZeroValue", func(t *testing.T) {
		t.Parallel()
		var b Base
		if got := b.ModelInventory(); len(got.Models) != 0 {
			t.Fatalf("ModelInventory() = %+v, want zero value", got)
		}
	})

	t.Run("SetModelInventoryRoundTrips", func(t *testing.T) {
		t.Parallel()
		var b Base
		want := ModelInventory{Models: []Model{{ID: "m1"}, {ID: "m2"}}}
		b.SetModelInventory(want)
		if got := b.ModelInventory(); len(got.Models) != 2 || got.Models[0].ID != "m1" || got.Models[1].ID != "m2" {
			t.Fatalf("ModelInventory() = %+v, want %+v", got, want)
		}
	})

	// Base is registered once per process and shared across every concurrent
	// task for its harness (see backends.Default): a background refresh calls
	// SetModelInventory while request handlers call ModelInventory
	// concurrently. This must not race.
	t.Run("ConcurrentAccessDoesNotRace", func(t *testing.T) {
		t.Parallel()
		var b Base
		b.SetModelInventory(ModelInventory{Models: []Model{{ID: "initial"}}})

		var wg sync.WaitGroup
		for range 50 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				b.SetModelInventory(ModelInventory{Models: []Model{{ID: "refreshed"}}})
			}()
			go func() {
				defer wg.Done()
				inv := b.ModelInventory()
				if len(inv.Models) == 0 {
					t.Error("ModelInventory() returned no models mid-refresh")
				}
			}()
		}
		wg.Wait()
	})
}
