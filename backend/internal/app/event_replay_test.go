// Tests production replay-writer attachment uses Reopen proof only at construction.

package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task"
)

func TestNewEventReplayWriterProofCalls(t *testing.T) {
	t.Parallel()
	newProof := func(t *testing.T) (string, task.CacheProof) {
		path := filepath.Join(t.TempDir(), "task.jsonl")
		meta := agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "replay", Harness: harness.Claude}
		data, err := agent.MarshalMessage(&meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		proof, err := task.CacheProofForLog(path)
		if err != nil {
			t.Fatal(err)
		}
		return path, proof
	}

	t.Run("construction uses initial proof", func(t *testing.T) {
		t.Parallel()
		path, initial := newProof(t)
		freshCalls := 0
		writer, err := newEventReplayWriter(path, initial, func(string) (task.CacheProof, error) {
			freshCalls++
			return task.CacheProof{}, errors.New("fresh proof called during construction")
		})
		if err != nil {
			t.Fatal(err)
		}
		if freshCalls != 0 {
			t.Fatalf("fresh proof calls during construction = %d, want 0", freshCalls)
		}
		if err := writer.Commit(t.Context(), path); err == nil {
			t.Fatal("Commit accepted a fresh-proof failure")
		}
		if freshCalls == 0 {
			t.Fatal("Commit did not use the fresh task proof provider")
		}
	})

	t.Run("later cache validation is fresh", func(t *testing.T) {
		t.Parallel()
		path, initial := newProof(t)
		freshCalls := 0
		writer, err := newEventReplayWriter(path, initial, func(proofPath string) (task.CacheProof, error) {
			freshCalls++
			return task.CacheProofForLog(proofPath)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Commit(t.Context(), path); err != nil {
			t.Fatal(err)
		}
		if freshCalls != 2 {
			t.Fatalf("fresh proof calls during cache commit = %d, want 2", freshCalls)
		}
	})
}
