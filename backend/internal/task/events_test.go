// Tests settings-only activity policies across exit failure and pending input folds.

package task

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func TestExitStatus(t *testing.T) {
	t.Parallel()
	t.Run("metadata does not accept input or clear failure", func(t *testing.T) {
		t.Parallel()
		for _, message := range []agent.Message{
			&agent.SessionMetadataMessage{ReportedModel: "updated"},
			&agent.MetaSessionMessage{SessionID: "restored"},
			&agent.SyncActivityMessage{Phase: "end", Status: "pushed"},
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
	t.Run("turn error", func(t *testing.T) {
		t.Parallel()
		var status ExitStatus
		status.Observe(&agent.ResultMessage{IsError: true, Result: "INVALID_ARGUMENT (code 400)"})
		if status.TurnError != "INVALID_ARGUMENT (code 400)" {
			t.Fatalf("failed result: %+v", status)
		}
		status.Observe(&agent.ExitMessage{ExitCode: 0})
		status.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeInputDeliveryStarted})
		status.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeInputDeliveryFailed})
		if status.TurnError == "" {
			t.Fatal("exit or failed delivery cleared the turn error")
		}
		status.Observe(&agent.TextMessage{Text: "next turn"})
		if status.TurnError != "" {
			t.Fatalf("new turn kept the error: %+v", status)
		}
		status.Observe(&agent.ResultMessage{IsError: true})
		if status.TurnError == "" {
			t.Fatal("failed result without text left no error")
		}
		status.Observe(&agent.ResultMessage{Result: "done"})
		if status.TurnError != "" {
			t.Fatalf("successful result kept the error: %+v", status)
		}
		status.Observe(&agent.ResultMessage{IsError: true, Result: "child", Source: v3.MessageSource{AgentID: "child"}})
		if status.TurnError != "" {
			t.Fatalf("child result set the root error: %+v", status)
		}
	})
}
