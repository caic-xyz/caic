// Tests for operation duration aggregation.

package metrics_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/caic-xyz/caic/metrics"
)

func testResource() metrics.Resource {
	return metrics.Resource{ServiceName: "caic-test", ServiceVersion: "1.2.3", Host: "test-host"}
}

func TestOutcomeOf(t *testing.T) {
	t.Parallel()

	if got := metrics.OutcomeOf(nil); got != metrics.OutcomeOK {
		t.Fatalf("OutcomeOf(nil) = %q, want %q", got, metrics.OutcomeOK)
	}
	if got := metrics.OutcomeOf(errors.New("boom")); got != metrics.OutcomeError {
		t.Fatalf("OutcomeOf(err) = %q, want %q", got, metrics.OutcomeError)
	}
}

func TestDedupAttrs(t *testing.T) {
	t.Parallel()

	if got := metrics.DedupAttrs(nil); got != nil {
		t.Fatalf("DedupAttrs(nil) = %v, want nil", got)
	}
	got := metrics.DedupAttrs([]metrics.Attr{
		{Key: "container.runtime", Value: "podman"},
		{Key: "container.runtime", Value: "docker"},
	})
	want := map[string]string{"container.runtime": "docker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupAttrs = %v, want %v", got, want)
	}
}

func TestMulti(t *testing.T) {
	t.Parallel()

	t.Run("requires a recorder", func(t *testing.T) {
		t.Parallel()
		if _, err := metrics.Multi(); err == nil {
			t.Fatal("Multi accepted no recorders")
		}
	})

	t.Run("rejects a nil recorder", func(t *testing.T) {
		t.Parallel()
		if _, err := metrics.Multi(metrics.Nop{}, nil); err == nil {
			t.Fatal("Multi accepted a nil recorder")
		}
	})

	t.Run("forwards to every recorder", func(t *testing.T) {
		t.Parallel()
		first := metrics.NewStore(testResource())
		second := metrics.NewStore(testResource())
		rec, err := metrics.Multi(first, second)
		if err != nil {
			t.Fatalf("Multi: %v", err)
		}
		rec.Record(t.Context(), "repo.diff", metrics.OutcomeOK, time.Millisecond)
		for _, store := range []*metrics.Store{first, second} {
			if got := store.Snapshot(); len(got) != 1 || got[0].Calls != 1 {
				t.Fatalf("snapshot = %+v, want one call", got)
			}
		}
	})
}

func TestStore(t *testing.T) {
	t.Parallel()

	t.Run("empty store has no series", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		if got := store.Snapshot(); len(got) != 0 {
			t.Fatalf("snapshot = %#v, want empty", got)
		}
		if store.Since.IsZero() {
			t.Fatal("Since is zero")
		}
		if store.Resource != testResource() {
			t.Fatalf("Resource = %+v, want %+v", store.Resource, testResource())
		}
	})

	t.Run("percentiles sorted slowest first", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		for i := 1; i <= 100; i++ {
			store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, time.Duration(i)*time.Millisecond)
		}
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, 500*time.Millisecond)

		got := store.Snapshot()
		if len(got) != 2 {
			t.Fatalf("snapshot = %#v, want two series", got)
		}
		if got[0].Name != "container.launch" {
			t.Fatalf("first series = %q, want container.launch (slowest p95)", got[0].Name)
		}
		want := metrics.Series{
			Name:    "repo.diff",
			Outcome: metrics.OutcomeOK,
			Calls:   100,
			Samples: 100,
			Min:     time.Millisecond,
			P50:     50 * time.Millisecond,
			P95:     95 * time.Millisecond,
			Max:     100 * time.Millisecond,
		}
		if !reflect.DeepEqual(got[1], want) {
			t.Fatalf("repo.diff series = %+v, want %+v", got[1], want)
		}
	})

	t.Run("outcomes are separate series", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "task.push", metrics.OutcomeOK, time.Millisecond)
		store.Record(t.Context(), "task.push", metrics.OutcomeError, 2*time.Millisecond)

		got := store.Snapshot()
		if len(got) != 2 {
			t.Fatalf("snapshot = %#v, want two series", got)
		}
		if got[0].Outcome != metrics.OutcomeError || got[1].Outcome != metrics.OutcomeOK {
			t.Fatalf("snapshot = %#v, want error before ok", got)
		}
	})

	t.Run("attrs form separate series", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, time.Millisecond,
			metrics.Attr{Key: "container.runtime", Value: "podman"})
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, 3*time.Millisecond,
			metrics.Attr{Key: "container.runtime", Value: "docker"})

		got := store.Snapshot()
		if len(got) != 2 {
			t.Fatalf("snapshot = %#v, want two series", got)
		}
		want := map[string]time.Duration{"podman": time.Millisecond, "docker": 3 * time.Millisecond}
		for _, series := range got {
			runtimeName, ok := series.Attrs["container.runtime"]
			if !ok {
				t.Fatalf("series = %+v, want a container.runtime attribute", series)
			}
			if series.Calls != 1 || series.P50 != want[runtimeName] {
				t.Fatalf("series = %+v, want one %v call for %s", series, want[runtimeName], runtimeName)
			}
		}
	})

	t.Run("attribute order does not matter", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, time.Millisecond,
			metrics.Attr{Key: "forge.name", Value: "github"},
			metrics.Attr{Key: "container.runtime", Value: "podman"})
		store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, time.Millisecond,
			metrics.Attr{Key: "container.runtime", Value: "podman"},
			metrics.Attr{Key: "forge.name", Value: "github"})

		if got := store.Snapshot(); len(got) != 1 || got[0].Calls != 2 {
			t.Fatalf("snapshot = %#v, want one series with two calls", got)
		}
	})

	t.Run("duplicate attribute keys keep the last value", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, time.Millisecond,
			metrics.Attr{Key: "container.runtime", Value: "podman"},
			metrics.Attr{Key: "container.runtime", Value: "docker"})

		got := store.Snapshot()
		if len(got) != 1 || got[0].Attrs["container.runtime"] != "docker" {
			t.Fatalf("snapshot = %#v, want runtime docker", got)
		}
	})

	t.Run("retains only the most recent samples", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		// Keep this in step with the package's retention cap.
		const limit = 1024
		total := limit + 10
		for i := range total {
			store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, time.Duration(i)*time.Millisecond)
		}

		got := store.Snapshot()
		if len(got) != 1 {
			t.Fatalf("snapshot = %#v, want one series", got)
		}
		if got[0].Calls != int64(total) || got[0].Samples != limit {
			t.Fatalf("series = %+v, want %d calls and %d samples", got[0], total, limit)
		}
		if got[0].Min != 10*time.Millisecond {
			t.Fatalf("Min = %v, want 10ms (oldest samples evicted)", got[0].Min)
		}
		if got[0].Max != time.Duration(total-1)*time.Millisecond {
			t.Fatalf("Max = %v, want %v", got[0].Max, time.Duration(total-1)*time.Millisecond)
		}
	})
}
