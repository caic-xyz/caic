// Benchmarks V1 task-log adoption and bounded reopen validation.
//go:build adoption_benchmark

package task_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task"
)

func BenchmarkTaskAdoption(b *testing.B) {
	b.StopTimer()
	dir := b.TempDir()
	id := ksid.NewID()
	path := filepath.Join(dir, id.String()+"-org-repo-caic-0.jsonl")
	writeProductionAdoptionFixture(b, path)
	store := &task.LogStore{LogDir: dir}

	b.ResetTimer()
	b.StartTimer()
	for range b.N {
		logs, err := task.LoadLogsForTaskIDs(dir, []string{id.String()})
		if err != nil {
			b.Fatal(err)
		}
		if len(logs) != 1 {
			b.Fatalf("LoadLogsForTaskIDs returned %d tasks, want 1", len(logs))
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
		tk := &task.Task{
			ID:            id,
			InitialPrompt: agent.Prompt{Text: "benchmark adoption"},
			Repos:         []task.RepoMount{{Name: "org/repo", Branch: "caic-0"}},
			Harness:       harness.Claude,
		}
		tk.SetLogPath(lt.LogPath())
		tk.SetLogValidationSnapshot(lt.ValidatedSnapshot())
		w, err := store.Reopen(tk)
		if err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
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
