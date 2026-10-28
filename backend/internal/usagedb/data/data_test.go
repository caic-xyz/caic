// Tests persisted usage timestamps and JSON zero-versus-empty field semantics.

package data

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestTime(t *testing.T) {
	t.Parallel()

	t.Run("encodes as int64", func(t *testing.T) {
		t.Parallel()
		data, err := json.Marshal(map[string]Time{"ts": NewTime(time.UnixMilli(1700000000000))})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(data), `{"ts":1700000000000}`; got != want {
			t.Errorf("json = %s, want %s", got, want)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		t.Parallel()
		want := time.UnixMilli(1700000000123)
		if got := NewTime(want).AsTime(); !got.Equal(want) {
			t.Errorf("AsTime() = %v, want %v", got, want)
		}
		if NewTime(time.Time{}) == 0 {
			t.Errorf("zero time.Time must not map to the zero Time (guard with IsZero before converting)")
		}
	})
}

func TestUsageRow(t *testing.T) {
	t.Parallel()

	t.Run("zero and empty distinctions", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			row  UsageRow
			want string
		}{
			{"nil", UsageRow{}, `{"kind":"","day":"","ts":0,"task_id":""}`},
			{"empty", UsageRow{Repos: []string{}, SkillReads: map[string]int{}, ToolCalls: map[string]int{}, ToolTimings: map[string]ToolTiming{}}, `{"skill_reads":{},"tool_calls":{},"tool_timings":{},"kind":"","day":"","ts":0,"task_id":"","repos":[]}`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				encoded, err := json.Marshal(&c.row)
				if err != nil {
					t.Fatal(err)
				}
				if string(encoded) != c.want {
					t.Fatalf("JSON = %s, want %s", encoded, c.want)
				}
				var disk UsageRow
				if err := json.Unmarshal(encoded, &disk); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(disk, c.row) {
					t.Fatalf("recovered = %+v, want %+v", disk, c.row)
				}
			})
		}
	})
}

func TestTokenBuckets(t *testing.T) {
	t.Parallel()
	t.Run("arbitrary and stored durations", func(t *testing.T) {
		t.Parallel()
		var row UsageRow
		if err := json.Unmarshal([]byte(`{"cache_write_5m":10,"cache_write_1h":20,"cache_writes":[{"ttl_seconds":7200,"tokens":30}],"input_tokens":40}`), &row); err != nil {
			t.Fatal(err)
		}
		if row.TotalInput() != 100 || row.CacheWriteCount(2*time.Hour) != 30 {
			t.Fatalf("counts = %+v", row)
		}
		snapshot := row.Clone()
		snapshot.CacheWrites[0].Tokens = 999
		if row.CacheWrites[0].Tokens != 30 {
			t.Fatal("snapshot aliases source")
		}
		row.Add(&Delta{CacheWrites: []CacheWrite{{TTLSeconds: 7200, Tokens: 5}}})
		if row.CacheWriteCount(2*time.Hour) != 35 || row.TotalInput() != 105 {
			t.Fatalf("aggregate = %+v", row)
		}
	})
}
