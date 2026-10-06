// Benchmarks incremental secret scanning of disk-backed Git diffs.

package repo

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func BenchmarkCheckSafety(b *testing.B) {
	for _, size := range []int{1 << 20, 16 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			dir := b.TempDir()
			git := func(args ...string) {
				cmd := exec.CommandContext(b.Context(), "git", args...) //nolint:gosec // Benchmark-controlled Git arguments.
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					b.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			git("init", "-b", "main")
			git("config", "user.name", "Test")
			git("config", "user.email", "test@example.com")
			git("commit", "--allow-empty", "-m", "base")
			git("update-ref", "refs/remotes/origin/main", "HEAD")
			git("checkout", "-b", "topic")
			f, err := os.Create(filepath.Join(dir, "large.txt")) //nolint:gosec // Fixed fixture name in the benchmark temporary directory.
			if err != nil {
				b.Fatal(err)
			}
			// Generate on disk: fixture memory must not inflate the sampled live heap.
			line := strings.Repeat("x", 127) + "\n"
			for n := 0; n < size; n += len(line) {
				if _, err := f.WriteString(line); err != nil {
					b.Fatal(err)
				}
			}
			if err := f.Close(); err != nil {
				b.Fatal(err)
			}
			git("add", ".")
			git("commit", "-m", "fixture")
			log := slog.New(slog.DiscardHandler)
			runtime.GC()
			var initial runtime.MemStats
			runtime.ReadMemStats(&initial)
			peak := initial.HeapAlloc
			done := make(chan struct{})
			var wg sync.WaitGroup
			wg.Go(func() {
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-done:
						return
					case <-ticker.C:
						var sample runtime.MemStats
						runtime.ReadMemStats(&sample)
						peak = max(peak, sample.HeapAlloc)
					}
				}
			})
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for b.Loop() {
				issues, err := CheckSafety(b.Context(), log, dir, "topic", "origin/main", nil)
				if err != nil || len(issues) != 0 {
					b.Fatalf("CheckSafety: %v, %v", issues, err)
				}
			}
			b.StopTimer()
			close(done)
			wg.Wait()
			b.ReportMetric(float64(peak-initial.HeapAlloc), "peak-heap-B")
		})
	}
}
