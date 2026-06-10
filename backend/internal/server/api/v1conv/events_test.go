// Tests for agent event to API event conversion.

package v1conv

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestToolTimingTrackerConvertMessage(t *testing.T) {
	t.Parallel()

	t.Run("nonzero exit becomes error event", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Pi, nil)
		events := tracker.ConvertMessage(&agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}, time.Unix(1, 0))
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if events[0].Kind != v1.EventKindError {
			t.Fatalf("kind = %q, want %q", events[0].Kind, v1.EventKindError)
		}
		if events[0].Error == nil || events[0].Error.Err != "Unknown option: --approve" {
			t.Fatalf("error event = %+v, want relay stderr", events[0].Error)
		}
	})

	t.Run("zero exit is hidden", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Pi, nil)
		events := tracker.ConvertMessage(&agent.ExitMessage{ExitCode: 0}, time.Unix(1, 0))
		if len(events) != 0 {
			t.Fatalf("got %d events, want 0", len(events))
		}
	})
}
