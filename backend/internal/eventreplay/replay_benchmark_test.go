// Benchmarks cold and trusted replay-sidecar opens with cache construction outside the timer.

package eventreplay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkOpenReplay(b *testing.B) {
	dir := b.TempDir()
	logPath := filepath.Join(dir, "task.jsonl")
	if err := os.WriteFile(logPath, []byte(`{"type":"caic_meta"}`+"\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	cache, err := NewCacheWriter(logPath, filepath.Join(dir, ".replay-tmp"), localCacheProof, testFormat)
	if err != nil {
		b.Fatal(err)
	}
	line := []byte(`{"kind":"` + strings.Repeat("x", 1024) + `"}`)
	for range 1024 {
		cache.WriteData(line)
	}
	if err := cache.CommitContext(b.Context(), logPath); err != nil {
		b.Fatal(err)
	}
	proof, err := localCacheProof(logPath)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("ColdValidation", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			replay, ok := OpenReplayWithProof(logPath, proof, localCacheProof, testFormat)
			if !ok {
				b.Fatal("cold OpenReplayWithProof = false")
			}
			replay.Close()
		}
	})

	cold, ok := OpenReplayWithProof(logPath, proof, localCacheProof, testFormat)
	if !ok {
		b.Fatal("initial OpenReplayWithProof = false")
	}
	identity := cold.CacheIdentity()
	cold.Close()
	b.Run("TrustedHit", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			replay, ok := OpenTrustedReplay(logPath, proof, localCacheProof, testFormat, identity)
			if !ok {
				b.Fatal("warm OpenTrustedReplay = false")
			}
			replay.Close()
		}
	})
}
