// Tests for the opus stub (no CGo).

//go:build !cgo || windows

package opus

import (
	"errors"
	"testing"
)

func TestOpusStub(t *testing.T) {
	t.Run("NewEncoder", func(t *testing.T) {
		enc, err := NewEncoder(16000, 1, AppVoIP)
		if !errors.Is(err, ErrNotAvailable) {
			t.Fatalf("got err=%v, want ErrNotAvailable", err)
		}
		if enc != nil {
			t.Fatal("expected nil encoder")
		}
	})

	t.Run("NewDecoder", func(t *testing.T) {
		dec, err := NewDecoder(16000, 1)
		if !errors.Is(err, ErrNotAvailable) {
			t.Fatalf("got err=%v, want ErrNotAvailable", err)
		}
		if dec != nil {
			t.Fatal("expected nil decoder")
		}
	})
}
