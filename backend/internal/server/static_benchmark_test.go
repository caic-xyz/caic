// Benchmarks cold static-asset transcoding with disk-backed Brotli inputs.

package server

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

// BenchmarkDoTranscode measures cold work, including the intended final output.
// peak-heap-B is HeapAlloc growth sampled every millisecond and at completion;
// it includes uncollected garbage and is neither exact peak live memory nor RSS.
func BenchmarkDoTranscode(b *testing.B) {
	for _, kind := range []string{"random", "text"} {
		for _, size := range []int{1 << 20, 16 << 20} {
			b.Run(fmt.Sprintf("%s/%dMiB", kind, size>>20), func(b *testing.B) {
				dir := b.TempDir()
				writeTranscodeFixture(b, dir, size, kind)
				for _, enc := range []string{"gzip", "identity", "zstd"} {
					b.Run(enc, func(b *testing.B) {
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
						var output []byte
						for b.Loop() {
							var err error
							output, err = doTranscode(os.DirFS(dir), "asset.js", enc)
							if err != nil {
								b.Fatal(err)
							}
						}
						close(done)
						wg.Wait()
						var final runtime.MemStats
						runtime.ReadMemStats(&final)
						peak = max(peak, final.HeapAlloc)
						runtime.KeepAlive(output)
						b.ReportMetric(float64(max(peak, initial.HeapAlloc)-initial.HeapAlloc), "peak-heap-B")
						b.ReportMetric(float64(len(output)), "output-B")
					})
				}
			})
		}
	}
}

// writeTranscodeFixture generates on disk, retaining no whole raw payload.
func writeTranscodeFixture(b *testing.B, dir string, size int, kind string) {
	f, err := os.Create(filepath.Join(dir, "asset.js.br")) //nolint:gosec // Fixed fixture name inside the benchmark temporary directory.
	if err != nil {
		b.Fatal(err)
	}
	w := brotli.NewWriterLevel(f, 4)
	chunk := make([]byte, 64<<10)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // Deterministic compression fixture, not security-sensitive.
	text := []byte("function render(value){return document.createElement(value);}/* static bundle */\n")
	for offset := 0; offset < size; offset += len(chunk) {
		for i := range chunk {
			if kind == "random" {
				chunk[i] = byte(rng.Uint32() & 255)
			} else {
				chunk[i] = text[(offset+i)%len(text)]
			}
		}
		if _, err := w.Write(chunk[:min(len(chunk), size-offset)]); err != nil {
			b.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
}
