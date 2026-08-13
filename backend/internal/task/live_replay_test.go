// Tests live replay sidecars continue through adopted task-log appends.

package task_test

import (
	"errors"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	"github.com/caic-xyz/caic/backend/internal/task"
)

func TestLiveReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &task.LogStore{LogDir: dir}
	original := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
	log, err := store.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := task.LoadLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded logs = %d, want 1", len(loaded))
	}
	adopted := &task.Task{ID: original.ID, Harness: harness.Codex}
	adopted.SetLogPath(loaded[0].LogPath())
	adopted.SetLogValidationSnapshot(loaded[0].ValidatedSnapshot())

	var replayWriter task.EventReplayWriter
	store.EventReplayFactory = func(path string, proof task.CacheProof, provider task.CacheProofProvider) (task.EventReplayWriter, error) {
		writer, err := eventreplay.NewMessageWriter(path, proof, eventreplay.ProofProvider(provider))
		if err != nil {
			return nil, err
		}
		replayWriter = writer
		return writer, nil
	}
	log, err = store.Reopen(adopted)
	if err != nil {
		t.Fatal(err)
	}
	message := &agent.LogMessage{MessageType: "caic_log", Line: "live output"}
	if err := log.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	if err := replayWriter.WriteMessage(t.Context(), agent.ParsedMessage{Message: message}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := adopted.CommitEventReplay(t.Context()); err != nil {
		t.Fatal(err)
	}

	replay, ok := eventreplay.OpenReplay(adopted.LogPath(), eventreplay.ProofProvider(task.CacheProofForLog))
	if !ok {
		t.Fatal("live replay cache was not published")
	}
	replay.Close()
}

func TestAdoptedTaskWithoutReplayCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &task.LogStore{LogDir: dir}
	original := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
	log, err := store.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := task.LoadLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	adopted := &task.Task{ID: original.ID, Harness: harness.Codex}
	adopted.SetLogPath(loaded[0].LogPath())
	adopted.SetLogValidationSnapshot(loaded[0].ValidatedSnapshot())

	message := &agent.LogMessage{MessageType: "caic_log", Line: "output while offline"}
	log, err = store.Reopen(adopted)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	store.EventReplayFactory = func(path string, proof task.CacheProof, provider task.CacheProofProvider) (task.EventReplayWriter, error) {
		writer, err := eventreplay.NewMessageWriter(path, proof, eventreplay.ProofProvider(provider))
		if errors.Is(err, eventreplay.ErrNoCompleteCache) {
			return nil, task.ErrNoLiveReplayCache
		}
		return writer, err
	}
	log, err = store.Reopen(adopted)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := adopted.CommitEventReplay(t.Context()); err != nil {
		t.Fatal(err)
	}
}
