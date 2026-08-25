// Benchmarks V1 task-log adoption and header-validated reopen.

package task_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func mustNewTask(t testing.TB, id ksid.ID, prompt agent.Prompt) *task.Task {
	tk, err := task.NewTask(id, prompt, harness.Claude, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func BenchmarkTaskAdoption(b *testing.B) {
	b.StopTimer()
	dir := b.TempDir()
	id := ksid.NewID()
	name := id.String() + "-org-repo-caic-0.jsonl"
	path := filepath.Join(dir, name)
	writeProductionAdoptionFixture(b, path)
	store := taskslog.NewStore(slog.New(slog.DiscardHandler), dir)

	b.ResetTimer()
	b.StartTimer()
	for range b.N {
		logs, err := store.LoadForTaskIDs([]string{id.String()})
		if err != nil {
			b.Fatal(err)
		}
		if len(logs) != 1 {
			b.Fatalf("Store.LoadForTaskIDs returned %d tasks, want 1", len(logs))
		}
		lt := logs[0]
		lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		})
		if lt.SessionID == "" || lt.AgentVersion == "" {
			if err := lt.LoadSessionMetadata(); err != nil {
				b.Fatal(err)
			}
		}
		if err := lt.LoadMessages(); err != nil {
			b.Fatal(err)
		}
		tk := mustNewTask(b, id, agent.Prompt{Text: "benchmark adoption"})
		tk.Repos = []taskslog.RepoMount{{Name: "org/repo", Branch: "caic-0"}}
		w, _, err := store.Reopen(name, tk.LogHeader())
		if err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSeedTimeline measures the replay pass that turns an adopted log
// into task state. The message mix mirrors a long session: mostly text and
// tool traffic punctuated by turn results, diff stats and context boundaries.
//
// The seeded messages are shared across iterations; SeedTimeline only reads
// the slice, so sharing is safe.
func BenchmarkSeedTimeline(b *testing.B) {
	const turns = 500
	msgs := make([]agent.Message, 0, turns*8)
	for turn := range turns {
		msgs = append(msgs,
			&agent.TextMessage{Text: "Working on the next step."},
			&agent.ToolUseMessage{ToolUseID: fmt.Sprintf("tool-%d", turn), Name: "Read", Input: json.RawMessage(`{"file_path":"/src/main.go"}`)},
			&agent.ToolResultMessage{ToolUseID: fmt.Sprintf("tool-%d", turn)},
			&agent.DiffStatMessage{MessageType: "diff_stat", DiffStat: agent.DiffStat{{Path: "main.go", Added: 3, Deleted: 1}}},
			&agent.UsageMessage{Usage: agent.Usage{InputTokens: 900, OutputTokens: 120, CacheTTLSeconds: 300}, ContextWindow: 200000},
			&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done", TotalCostUSD: 0.02, NumTurns: 1, DurationMs: 1200,
				Usage: agent.Usage{InputTokens: 900, OutputTokens: 120, CacheReadInputTokens: 4000}},
		)
		if turn%50 == 0 {
			msgs = append(msgs, &agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary"})
		}
	}
	id := ksid.NewID()

	for b.Loop() {
		mustNewTask(b, id, agent.Prompt{Text: "benchmark seed"}).SeedTimeline(msgs)
	}
}

// BenchmarkBackwardMessagesLatestMatch measures the live request path when a
// caller searches a long timeline newest first for its latest matching message.
func BenchmarkBackwardMessagesLatestMatch(b *testing.B) {
	const messageCount = 4000
	msgs := make([]agent.Message, 0, messageCount)
	for i := range messageCount - 1 {
		msgs = append(msgs, &agent.TextMessage{Text: fmt.Sprintf("message %d", i)})
	}
	want := &agent.ToolUseMessage{ToolUseID: "target"}
	msgs = append(msgs, want)
	tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark lookup"})
	tk.SeedTimeline(msgs)
	b.ReportAllocs()

	for b.Loop() {
		var got agent.Message
		for message := range tk.BackwardMessages() {
			if toolUse, ok := message.(*agent.ToolUseMessage); ok && toolUse.ToolUseID == "target" {
				got = toolUse
				break
			}
		}
		if got != want {
			b.Fatalf("latest matching message = %#v, want target", got)
		}
	}
}

func writeProductionAdoptionFixture(b *testing.B, path string) {
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	meta, err := json.Marshal(agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Prompt:      "benchmark adoption",
		Repos:       []agent.MetaRepo{{Name: "org/repo", Branch: "caic-0"}},
		Harness:     harness.Claude,
		StartedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		b.Fatal(err)
	}
	session, err := json.Marshal(agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "benchmark-session", AgentVersion: "2.1.0"})
	if err != nil {
		b.Fatal(err)
	}
	for _, line := range [][]byte{meta, session} {
		if _, err := writer.Write(append(line, '\n')); err != nil {
			b.Fatal(err)
		}
	}
	const native = `{"type":"assistant","message":{"content":[{"type":"text","text":"Completed one adoption step."}]}}` + "\n"
	for written := len(meta) + len(session) + 2; written < 128<<20; written += len(native) {
		if _, err := writer.WriteString(native); err != nil {
			b.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		b.Fatal(fmt.Errorf("sync production adoption fixture: %w", err))
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		b.Fatal(err)
	} else if info.Size() < 128<<20 {
		b.Fatalf("fixture size = %d, want at least %d", info.Size(), 128<<20)
	}
}
