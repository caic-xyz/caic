// Verifies the sanitized v2 Claude task-lifecycle protocol evidence.

package claudecode

import (
	"bufio"
	"os"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestTaskLifecycleEvidence(t *testing.T) {
	t.Parallel()

	f, err := os.Open("testdata/evidence/task-lifecycle.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	p, err := agent.NewLogRecordParser(agent.LogVersionV2, New().NewWire().ParseMessage)
	if err != nil {
		t.Fatal(err)
	}
	var messages []agent.Message
	s := bufio.NewScanner(f)
	for s.Scan() {
		record, err := p.ParseRecord(s.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range record.Messages {
			messages = append(messages, message.Message)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	// The fixture's records are the header, task_started, task_notification,
	// and the footer. Only the control records produce messages: local_bash task
	// events are neither transcript messages nor native-subagent observation.
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2 control records", len(messages))
	}
}
