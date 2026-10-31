// Tests settings-only activity policies across exit failure and pending input folds.

package task

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestExitStatus(t *testing.T) {
	t.Parallel()
	t.Run("metadata does not accept input or clear failure", func(t *testing.T) {
		t.Parallel()
		for _, message := range []agent.Message{
			&agent.SessionMetadataMessage{ReportedModel: "updated"},
			&agent.MetaSessionMessage{SessionID: "restored"},
		} {
			var status ExitStatus
			status.Observe(&agent.ExitMessage{ExitCode: 1, Error: "failed"})
			status.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeInputDeliveryStarted})
			if !status.Observe(message) || status.Error == "" || ClearsExitError(message) || AcceptedInputOutput(message) {
				t.Fatalf("%T changed failed turn or accepted pending input: %+v", message, status)
			}
			status.Observe(&agent.UserInputMessage{Text: "pending"})
			if status.Error == "" {
				t.Fatalf("%T retired pending delivery", message)
			}
			status.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeInputDeliveryCompleted})
			if status.Error != "" {
				t.Fatalf("successful delivery retained failure: %+v", status)
			}
		}
	})
}
