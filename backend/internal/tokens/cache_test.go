// Tests cache lifetime normalization, immutable aggregation, and partial accounting coverage.

package tokens

import (
	"reflect"
	"testing"
	"time"
)

func TestNewCacheWrites(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		total    int64
		reported CacheWrites
		want     CacheWrites
	}{
		{"excess", 20, CacheWrites{{TTL: time.Hour, Tokens: 100}, {TTL: 5 * time.Minute, Tokens: 10}}, CacheWrites{{TTL: time.Hour, Tokens: 20}}},
		{"negative", 20, CacheWrites{{TTL: time.Hour, Tokens: -10}}, CacheWrites{{Tokens: 20}}},
		{"missing", 20, CacheWrites{{TTL: time.Hour, Tokens: 10}}, CacheWrites{{Tokens: 10}, {TTL: time.Hour, Tokens: 10}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NewCacheWrites(tc.total, 0, tc.reported...); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("partition = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCacheWrites(t *testing.T) {
	t.Parallel()
	t.Run("Add", func(t *testing.T) {
		t.Parallel()
		a := CacheWrites{{TTL: 2 * time.Hour, Tokens: 10}, {Tokens: 3}}
		b := CacheWrites{{TTL: 5 * time.Minute, Tokens: 20}, {TTL: 2 * time.Hour, Tokens: 30}}
		sum := a.Add(b)
		want := CacheWrites{{Tokens: 3}, {TTL: 5 * time.Minute, Tokens: 20}, {TTL: 2 * time.Hour, Tokens: 40}}
		if !reflect.DeepEqual(sum, want) || sum.Total() != 63 {
			t.Fatalf("sum = %+v", sum)
		}
		sum[0].Tokens = 999
		if a[1].Tokens != 3 || b[0].Tokens != 20 {
			t.Fatal("aggregation mutated input ownership")
		}
	})
	t.Run("Subtract", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name                 string
			total, covered, want CacheWrites
		}{
			{"exact durations", CacheWrites{{TTL: 5 * time.Minute, Tokens: 100}, {TTL: 2 * time.Hour, Tokens: 200}}, CacheWrites{{TTL: 2 * time.Hour, Tokens: 100}}, CacheWrites{{TTL: 5 * time.Minute, Tokens: 100}, {TTL: 2 * time.Hour, Tokens: 100}}},
			{"repeated lifetimes", CacheWrites{{TTL: time.Hour, Tokens: 100}, {TTL: time.Hour, Tokens: 100}}, CacheWrites{{TTL: time.Hour, Tokens: 50}}, CacheWrites{{TTL: time.Hour, Tokens: 150}}},
			{"aggregate lacks lifetime", CacheWrites{{Tokens: 100}}, CacheWrites{{TTL: 2 * time.Hour, Tokens: 100}}, nil},
			{"partial unknown aggregate", CacheWrites{{Tokens: 150}}, CacheWrites{{TTL: 2 * time.Hour, Tokens: 100}}, CacheWrites{{Tokens: 50}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := tc.total.Subtract(tc.covered); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("remaining = %+v, want %+v", got, tc.want)
				}
			})
		}
	})
}
