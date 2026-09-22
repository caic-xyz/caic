// Tests agent backend and wire doubles.

package agenttest

import (
	"errors"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestWire(t *testing.T) {
	t.Parallel()

	t.Run("zero_value", func(t *testing.T) {
		t.Parallel()
		messages, err := (&Wire{}).ParseMessage([]byte("native message"))
		if err != nil {
			t.Fatal(err)
		}
		if messages != nil {
			t.Errorf("ParseMessage = %#v, want nil", messages)
		}
	})
	t.Run("parse", func(t *testing.T) {
		t.Parallel()
		want := errors.New("parse failed")
		wire := Wire{Parse: func([]byte) ([]agent.Message, error) { return nil, want }}
		if _, err := wire.ParseMessage([]byte("native message")); !errors.Is(err, want) {
			t.Fatalf("ParseMessage error = %v, want %v", err, want)
		}
	})
}
