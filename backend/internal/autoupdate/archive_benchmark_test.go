// Benchmarks update ZIP extraction with disk-backed fixtures and sampled live heap.

package autoupdate

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"
)

// BenchmarkExtractZip uses stored payloads so compressed archive size grows with
// input size. Fixture generation is outside timing; destinations are disk-backed.
func BenchmarkExtractZip(b *testing.B) {
	for _, size := range []int64{1 << 20, 64 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			root := zipBenchmarkFixture(b, size)
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				src, err := root.Open("archive.zip")
				if err != nil {
					b.Fatal(err)
				}
				dst, err := os.CreateTemp(b.TempDir(), "binary-*")
				if err != nil {
					b.Fatal(err)
				}
				err = extractZipToFile(src, "caic.exe", dst)
				if e := src.Close(); err == nil {
					err = e
				}
				if e := dst.Close(); err == nil {
					err = e
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkExtractZipLiveHeap reports post-GC heap at the first destination write.
// This samples retained archive storage, not exact peak heap or process RSS.
func BenchmarkExtractZipLiveHeap(b *testing.B) {
	for _, size := range []int64{1 << 20, 64 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			root := zipBenchmarkFixture(b, size)
			for range b.N {
				src, err := root.Open("archive.zip")
				if err != nil {
					b.Fatal(err)
				}
				dst, err := os.CreateTemp(b.TempDir(), "binary-*")
				if err != nil {
					b.Fatal(err)
				}
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				w := &heapSampleWriter{dst: dst, sample: &after}
				err = extractZipToFile(src, "caic.exe", w)
				if e := src.Close(); err == nil {
					err = e
				}
				if e := dst.Close(); err == nil {
					err = e
				}
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(max(after.HeapAlloc, before.HeapAlloc)-before.HeapAlloc), "sampled-live-heap-B")
			}
		})
	}
}

func zipBenchmarkFixture(b *testing.B, size int64) *os.Root {
	root, err := os.OpenRoot(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := root.Close(); err != nil {
			b.Error(err)
		}
	})
	f, err := root.Create("archive.zip")
	if err != nil {
		b.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "caic.exe", Method: zip.Store})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.CopyN(w, zeroReader{}, size); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return root
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// heapSampleWriter samples the archive's live retained memory before forwarding
// the first extracted chunk. It intentionally changes GC behavior and is used
// only in the separate memory benchmark.
type heapSampleWriter struct {
	dst     io.Writer
	sample  *runtime.MemStats
	sampled bool
}

func (w *heapSampleWriter) Write(p []byte) (int, error) {
	if !w.sampled {
		runtime.GC()
		runtime.ReadMemStats(w.sample)
		w.sampled = true
	}
	return w.dst.Write(p)
}
