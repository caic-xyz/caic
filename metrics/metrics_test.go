// Tests for measurement aggregation and recorder identity helpers.

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

// atSeconds returns the amount Duration produces for d, so tests compare
// quantized amounts without re-deriving the conversion.
func atSeconds(d time.Duration) float64 {
	return metrics.Duration(d).Amount
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

func TestMeasurement(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  metrics.Measurement
		want metrics.Measurement
	}{
		{
			name: "Bytes",
			got:  metrics.Bytes(4096),
			want: metrics.Measurement{Kind: metrics.KindHistogram, Unit: metrics.UnitBytes, Amount: 4096},
		},
		{
			name: "Count",
			got:  metrics.Count(3),
			want: metrics.Measurement{Kind: metrics.KindCounter, Unit: metrics.UnitCount, Amount: 3},
		},
		{
			name: "UpDownCount",
			got:  metrics.UpDownCount(-2),
			want: metrics.Measurement{Kind: metrics.KindUpDownCounter, Unit: metrics.UnitCount, Amount: -2},
		},
		{
			name: "Gauge",
			got:  metrics.Gauge(0.5, metrics.UnitBytes),
			want: metrics.Measurement{Kind: metrics.KindGauge, Unit: metrics.UnitBytes, Amount: 0.5},
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}
}

func TestDuration(t *testing.T) {
	t.Parallel()

	t.Run("is a histogram in seconds", func(t *testing.T) {
		t.Parallel()
		want := metrics.Measurement{Kind: metrics.KindHistogram, Unit: metrics.UnitSeconds, Amount: 1.5}
		if got := metrics.Duration(1500 * time.Millisecond); got != want {
			t.Fatalf("Duration(1.5s) = %+v, want %+v", got, want)
		}
	})

	t.Run("quantizes to the nearest microsecond", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			in   time.Duration
			want time.Duration
		}{
			{in: 0, want: 0},
			{in: 1500 * time.Millisecond, want: 1500 * time.Millisecond},
			{in: 30 * time.Microsecond, want: 30 * time.Microsecond},
			{in: 999 * time.Nanosecond, want: time.Microsecond},
			{in: 1499 * time.Nanosecond, want: time.Microsecond},
			{in: 1499*time.Nanosecond + 500, want: 2 * time.Microsecond},
		}
		for _, tc := range cases {
			if got, want := atSeconds(tc.in), atSeconds(tc.want); got != want {
				t.Fatalf("Duration(%v).Amount = %v, want %v", tc.in, got, want)
			}
		}
	})
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
		rec.Record(t.Context(), "repo.diff", metrics.OutcomeOK, metrics.Duration(time.Millisecond))
		for _, store := range []*metrics.Store{first, second} {
			if got := store.Snapshot(); len(got) != 1 || got[0].Count != 1 {
				t.Fatalf("snapshot = %+v, want one measurement", got)
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

	t.Run("aggregates a histogram", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		sum := 0.0
		for i := 1; i <= 100; i++ {
			amount := metrics.Duration(time.Duration(i) * time.Millisecond)
			sum += amount.Amount
			store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, amount)
		}

		got := store.Snapshot()
		if len(got) != 1 {
			t.Fatalf("snapshot = %#v, want one series", got)
		}
		s := got[0]
		want := metrics.Series{
			Name:    "repo.diff",
			Outcome: metrics.OutcomeOK,
			Kind:    metrics.KindHistogram,
			Unit:    metrics.UnitSeconds,
			Count:   100,
			Samples: 100,
			Sum:     sum,
			Min:     atSeconds(time.Millisecond),
			P50:     atSeconds(50 * time.Millisecond),
			P95:     atSeconds(95 * time.Millisecond),
			Max:     atSeconds(100 * time.Millisecond),
			Last:    atSeconds(100 * time.Millisecond),
		}
		if !reflect.DeepEqual(s, want) {
			t.Fatalf("series = %+v, want %+v", s, want)
		}
	})

	t.Run("orders series by name", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "repo.fetch", metrics.OutcomeOK, metrics.Duration(time.Second))
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond))

		got := store.Snapshot()
		if len(got) != 2 || got[0].Name != "container.launch" || got[1].Name != "repo.fetch" {
			t.Fatalf("snapshot = %#v, want container.launch before repo.fetch", got)
		}
	})

	t.Run("separates kinds and units of one name", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "container.disk_usage", metrics.OutcomeOK, metrics.Duration(time.Millisecond))
		store.Record(t.Context(), "container.disk_usage", metrics.OutcomeOK, metrics.Bytes(4096))
		store.Record(t.Context(), "container.instances", metrics.OutcomeOK, metrics.Gauge(3, metrics.UnitCount))
		store.Record(t.Context(), "container.instances", metrics.OutcomeOK, metrics.Count(3))

		got := store.Snapshot()
		if len(got) != 4 {
			t.Fatalf("snapshot = %#v, want four series", got)
		}
		want := []struct {
			name string
			kind metrics.Kind
			unit metrics.Unit
		}{
			{"container.disk_usage", metrics.KindHistogram, metrics.UnitBytes},
			{"container.disk_usage", metrics.KindHistogram, metrics.UnitSeconds},
			{"container.instances", metrics.KindCounter, metrics.UnitCount},
			{"container.instances", metrics.KindGauge, metrics.UnitCount},
		}
		for i, w := range want {
			if got[i].Name != w.name || got[i].Kind != w.kind || got[i].Unit != w.unit {
				t.Fatalf("series[%d] = %+v, want %s %s %s", i, got[i], w.name, w.kind, w.unit)
			}
		}
	})

	t.Run("counters sum increments", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "task.purged", metrics.OutcomeOK, metrics.Count(2))
		store.Record(t.Context(), "task.purged", metrics.OutcomeOK, metrics.Count(3))

		got := store.Snapshot()
		if len(got) != 1 || got[0].Kind != metrics.KindCounter || got[0].Sum != 5 || got[0].Count != 2 {
			t.Fatalf("snapshot = %#v, want a counter summing to 5 over two increments", got)
		}
	})

	t.Run("gauges keep the latest amount", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "container.instances", metrics.OutcomeOK, metrics.Gauge(3, metrics.UnitCount))
		store.Record(t.Context(), "container.instances", metrics.OutcomeOK, metrics.Gauge(5, metrics.UnitCount))

		got := store.Snapshot()
		if len(got) != 1 || got[0].Kind != metrics.KindGauge {
			t.Fatalf("snapshot = %#v, want one gauge", got)
		}
		if got[0].Last != 5 || got[0].Min != 3 || got[0].Max != 5 || got[0].Count != 2 {
			t.Fatalf("series = %+v, want a gauge observed at 3 then 5", got[0])
		}
	})

	t.Run("outcomes are separate series", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "task.push", metrics.OutcomeOK, metrics.Duration(time.Millisecond))
		store.Record(t.Context(), "task.push", metrics.OutcomeError, metrics.Duration(2*time.Millisecond))

		got := store.Snapshot()
		if len(got) != 2 {
			t.Fatalf("snapshot = %#v, want two series", got)
		}
		if got[0].Outcome != metrics.OutcomeError || got[1].Outcome != metrics.OutcomeOK {
			t.Fatalf("snapshot = %#v, want error before ok", got)
		}
	})

	t.Run("attributes form separate series", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond),
			metrics.Attr{Key: "container.runtime", Value: "podman"})
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(3*time.Millisecond),
			metrics.Attr{Key: "container.runtime", Value: "docker"})

		got := store.Snapshot()
		if len(got) != 2 {
			t.Fatalf("snapshot = %#v, want two series", got)
		}
		want := map[string]float64{"podman": atSeconds(time.Millisecond), "docker": atSeconds(3 * time.Millisecond)}
		for _, series := range got {
			runtimeName, ok := series.Attrs["container.runtime"]
			if !ok {
				t.Fatalf("series = %+v, want a container.runtime attribute", series)
			}
			if series.Count != 1 || series.P50 != want[runtimeName] {
				t.Fatalf("series = %+v, want one %v call for %s", series, want[runtimeName], runtimeName)
			}
		}
	})

	t.Run("attribute order does not matter", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, metrics.Duration(time.Millisecond),
			metrics.Attr{Key: "forge.name", Value: "github"},
			metrics.Attr{Key: "container.runtime", Value: "podman"})
		store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, metrics.Duration(time.Millisecond),
			metrics.Attr{Key: "container.runtime", Value: "podman"},
			metrics.Attr{Key: "forge.name", Value: "github"})

		if got := store.Snapshot(); len(got) != 1 || got[0].Count != 2 {
			t.Fatalf("snapshot = %#v, want one series with two measurements", got)
		}
	})

	t.Run("duplicate attribute keys keep the last value", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond),
			metrics.Attr{Key: "container.runtime", Value: "podman"},
			metrics.Attr{Key: "container.runtime", Value: "docker"})

		got := store.Snapshot()
		if len(got) != 1 || got[0].Attrs["container.runtime"] != "docker" {
			t.Fatalf("snapshot = %#v, want runtime docker", got)
		}
	})

	t.Run("one attribute and a repeated one form the same series", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		// The single-attribute path skips the map and sort the general path needs,
		// so both must still derive one key for the same attribute set.
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond),
			metrics.Attr{Key: "container.runtime", Value: "podman"})
		store.Record(t.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(time.Millisecond),
			metrics.Attr{Key: "container.runtime", Value: "podman"},
			metrics.Attr{Key: "container.runtime", Value: "podman"})

		got := store.Snapshot()
		if len(got) != 1 || got[0].Count != 2 {
			t.Fatalf("snapshot = %#v, want one series of two calls", got)
		}
	})

	t.Run("retains only the most recent samples", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(testResource())
		// Keep this in step with the package's retention cap.
		const limit = 1024
		total := limit + 10
		for i := range total {
			store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, metrics.Duration(time.Duration(i)*time.Millisecond))
		}

		got := store.Snapshot()
		if len(got) != 1 {
			t.Fatalf("snapshot = %#v, want one series", got)
		}
		if got[0].Count != int64(total) || got[0].Samples != limit {
			t.Fatalf("series = %+v, want %d measurements and %d samples", got[0], total, limit)
		}
		if got[0].Min != atSeconds(10*time.Millisecond) {
			t.Fatalf("Min = %v, want 10ms (oldest samples evicted)", got[0].Min)
		}
		if got[0].Max != atSeconds(time.Duration(total-1)*time.Millisecond) {
			t.Fatalf("Max = %v, want %v", got[0].Max, atSeconds(time.Duration(total-1)*time.Millisecond))
		}
	})
}
