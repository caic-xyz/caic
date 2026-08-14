// Tests terminal replay cache publication from locally created task logs.

package taskmgr

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	"github.com/caic-xyz/caic/backend/internal/task"
)

type replayBuffer struct {
	bytes.Buffer
}

func (*replayBuffer) Flush() {}

func TestPublishTerminalReplayPublishesLocalTaskCache(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		state     task.State
		cancelled bool
	}{
		{name: "stopped", state: task.StateStopped},
		{name: "failed", state: task.StateFailed},
		{name: "purged", state: task.StatePurged},
		{name: "cancelled cache publication", state: task.StateStopped, cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logs := task.LogStore{LogDir: t.TempDir()}
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       harness.Claude,
				StartedAt:     time.Now().UTC(),
			}
			log, err := logs.Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			if err := agent.AppendNativeRecord(log, log.LogVersion(), []byte(`{"type":"assistant","message":{"model":"claude-opus-4-6","id":"msg_01","role":"assistant","content":[{"type":"text","text":"native task"}],"usage":{}},"session_id":"abc","uuid":"u1"}`)); err != nil {
				t.Fatal(err)
			}
			if err := log.AppendMessage(&agent.LogMessage{MessageType: "caic_log", Line: "local task"}); err != nil {
				t.Fatal(err)
			}
			if err := logs.WriteResultTrailer(log, tk.Title(), &task.Result{State: tc.state}); err != nil {
				t.Fatal(err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			tk.SetState(tc.state)
			if tc.state != task.StateStopped {
				terminal, err := task.LoadLogsForTaskIDs(logs.LogDir, []string{tk.ID.String()})
				if err != nil {
					t.Fatal(err)
				}
				if err := task.CompressTerminalLogs(terminal); err != nil {
					t.Fatal(err)
				}
				if len(terminal) != 1 {
					t.Fatalf("compressed logs = %d, want 1", len(terminal))
				}
				tk.SetLogPath(terminal[0].LogPath())
			}

			m := newTestManager(t, Config{
				ServerCtx: t.Context(),
				Backends: map[harness.Name]agent.Backend{
					harness.Claude: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire},
				},
			})
			entry := NewEntry(tk, nil)
			ctx := t.Context()
			if tc.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			m.publishTerminalReplay(ctx, entry)
			loaded := entry.LoadedTask()
			if loaded == nil {
				t.Fatal("local task cache publication did not retain its replay source")
			}
			if tc.state != task.StateStopped && !strings.HasSuffix(loaded.LogPath(), ".zst") {
				t.Fatalf("final replay log = %q, want compressed log", loaded.LogPath())
			}
			replay, ok := eventreplay.OpenReplay(loaded.LogPath(), loaded.CacheProofForLog)
			if tc.cancelled {
				if ok {
					t.Cleanup(replay.Close)
					t.Fatal("cancelled terminal replay cache was published")
				}
				return
			}
			if !ok {
				t.Fatal("local terminal task replay cache was not published")
			}
			t.Cleanup(replay.Close)
			out := &replayBuffer{}
			idx := 0
			if replay.WriteSSE(out, out, &idx) != eventreplay.SSEComplete || !bytes.Contains(out.Bytes(), []byte("local task")) {
				t.Fatalf("terminal replay = %q, index = %d", out.String(), idx)
			}
		})
	}
}
