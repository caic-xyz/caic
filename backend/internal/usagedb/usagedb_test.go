// Time tests: JSON encoding as int64 and the wall-clock round trip.

package usagedb

import (
	"encoding/json"
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
