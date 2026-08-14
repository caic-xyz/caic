// Tests server-owned regeneration of API replay sidecars.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	"github.com/caic-xyz/caic/backend/internal/logproof"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

type bridgeBuffer struct {
	bytes.Buffer
}

func (*bridgeBuffer) Flush() {}

func bridgeProof(path string) (logproof.CacheProof, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return logproof.CacheProof{}, err
	}
	header, _, _ := bytes.Cut(data, []byte("\n"))
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return logproof.CacheProof{}, err
	}
	return logproof.CacheProof{
		Device:    1,
		Inode:     1,
		Size:      info.Size(),
		ModTimeNs: info.ModTime().UnixNano(),
		Version:   agent.LogVersionV1,
		Harness:   harness.Claude,
		RawHeader: string(header),
	}, nil
}

func TestReplayPublisherPrune(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	tempDir := filepath.Join(logDir, ".replay-tmp")
	publisher, err := NewReplayPublisher(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(tempDir, "interrupted.pending")
	if err := os.WriteFile(orphan, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Prune(logDir, bridgeProof); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("temporary replay artifact remains: %v", err)
	}
}

func TestRegenerateReplay(t *testing.T) {
	t.Parallel()
	writeRawLog := func(t *testing.T, path string) {
		if err := os.WriteFile(path, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newPublisher := func(t *testing.T, path string) *ReplayPublisher {
		publisher, err := NewReplayPublisher(filepath.Join(filepath.Dir(path), ".replay-tmp"))
		if err != nil {
			t.Fatal(err)
		}
		return publisher
	}
	assertNoArtifacts := func(t *testing.T, path string) {
		if _, err := os.Stat(eventreplay.CachePath(path)); !os.IsNotExist(err) {
			t.Fatalf("published unsafe cache: %v", err)
		}
		matches, err := filepath.Glob(eventreplay.CachePath(path) + ".*")
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("stale replay artifacts = %v", matches)
		}
		entries, err := os.ReadDir(filepath.Join(filepath.Dir(path), ".replay-tmp"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("stale replay temporary artifacts = %v", entries)
		}
	}

	t.Run("source mutation discards derived artifacts", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		writeRawLog(t, path)
		proof, err := bridgeProof(path)
		if err != nil {
			t.Fatal(err)
		}
		publisher := newPublisher(t, path)
		err = publisher.regenerateSource(t.Context(), path, bridgeProof, func(_ context.Context, yield func(agent.ParsedMessage) error) (logproof.CacheProof, error) {
			if err := yield(agent.ParsedMessage{Message: &agent.TextMessage{Text: "before mutation"}}); err != nil {
				return logproof.CacheProof{}, err
			}
			if err := os.WriteFile(path, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"changed\"}\n"), 0o600); err != nil {
				return logproof.CacheProof{}, err
			}
			return proof, nil
		})
		if err == nil {
			t.Fatal("regenerateReplaySource succeeded after source mutation")
		}
		assertNoArtifacts(t, path)
	})

	t.Run("cancellation removes pending spool", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		writeRawLog(t, path)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		publisher := newPublisher(t, path)
		err := publisher.regenerateSource(ctx, path, bridgeProof, func(_ context.Context, yield func(agent.ParsedMessage) error) (logproof.CacheProof, error) {
			if err := yield(agent.ParsedMessage{Message: &agent.TextDeltaMessage{Text: strings.Repeat("x", maxPendingReplayBytes+1)}}); err != nil {
				return logproof.CacheProof{}, err
			}
			cancel()
			return bridgeProof(path)
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("regenerateReplaySource error = %v, want context cancellation", err)
		}
		assertNoArtifacts(t, path)
	})

	t.Run("source failure after pending spool is restart-safe", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		writeRawLog(t, path)
		publisher := newPublisher(t, path)
		sourceErr := errors.New("source failed")
		err := publisher.regenerateSource(t.Context(), path, bridgeProof, func(_ context.Context, yield func(agent.ParsedMessage) error) (logproof.CacheProof, error) {
			if err := yield(agent.ParsedMessage{Message: &agent.TextDeltaMessage{Text: strings.Repeat("x", maxPendingReplayBytes+1)}}); err != nil {
				return logproof.CacheProof{}, err
			}
			return logproof.CacheProof{}, sourceErr
		})
		if !errors.Is(err, sourceErr) {
			t.Fatalf("regenerateReplaySource error = %v, want source failure", err)
		}
		assertNoArtifacts(t, path)
		// Model an interrupted process after the source failure has cleaned up.
		// Startup pruning owns artifacts left by a later process interruption.
		tempPath := filepath.Join(filepath.Dir(path), ".replay-tmp", "interrupted.pending")
		if err := os.WriteFile(tempPath, []byte("incomplete"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := publisher.Prune(filepath.Dir(path), bridgeProof); err != nil {
			t.Fatal(err)
		}
		assertNoArtifacts(t, path)
	})

	t.Run("compacts superseded deltas", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		writeRawLog(t, path)
		publisher := newPublisher(t, path)
		err := publisher.regenerateSource(t.Context(), path, bridgeProof, func(_ context.Context, yield func(agent.ParsedMessage) error) (logproof.CacheProof, error) {
			if err := yield(agent.ParsedMessage{Message: &agent.TextDeltaMessage{Text: "partial"}}); err != nil {
				return logproof.CacheProof{}, err
			}
			if err := yield(agent.ParsedMessage{Message: &agent.TextMessage{Text: "complete"}}); err != nil {
				return logproof.CacheProof{}, err
			}
			return bridgeProof(path)
		})
		if err != nil {
			t.Fatal(err)
		}
		replay, ok := eventreplay.OpenReplay(path, bridgeProof, replayFormat)
		if !ok {
			t.Fatal("regenerated replay was not publishable")
		}
		t.Cleanup(replay.Close)
		out := &bridgeBuffer{}
		idx := 0
		if replay.WriteSSE(out, out, &idx) != eventreplay.SSEComplete {
			t.Fatal("regenerated replay did not write")
		}
		if bytes.Contains(out.Bytes(), []byte("partial")) || !bytes.Contains(out.Bytes(), []byte("complete")) {
			t.Fatalf("compacted replay = %q", out.String())
		}
		var event v1.EventMessage
		start := bytes.Index(out.Bytes(), []byte("data: "))
		if start < 0 {
			t.Fatalf("replay has no event data: %q", out.String())
		}
		data, _, ok := bytes.Cut(out.Bytes()[start+len("data: "):], []byte("\n"))
		if !ok {
			t.Fatalf("replay event data has no terminator: %q", out.String())
		}
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Text == nil || event.Text.Text != "complete" || idx != 1 {
			t.Fatalf("compacted event = %#v, index = %d", event, idx)
		}
	})
}
