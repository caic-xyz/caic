// Tests and benchmarks streamed asset transcoding, failure atomicity, and concurrent variant caching.

package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"sync"
	"testing"
	"testing/fstest"

	// transcodeFS wraps the natural source boundary to observe opens and inject I/O errors.
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/andybalholm/brotli"
)

type transcodeFS struct {
	fs.FS

	readErr  error
	closeErr error
	mu       sync.Mutex
	opens    int
	closes   int
}

func (f *transcodeFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	return &transcodeFile{File: file, source: f}, nil
}

type transcodeFile struct {
	fs.File

	source *transcodeFS
}

func (f *transcodeFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if errors.Is(err, io.EOF) && f.source.readErr != nil {
		return n, f.source.readErr
	}
	return n, err
}

func (f *transcodeFile) Close() error {
	f.source.mu.Lock()
	f.source.closes++
	f.source.mu.Unlock()
	return errors.Join(f.File.Close(), f.source.closeErr)
}

func TestDoTranscode(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 512<<10)
	rng := rand.New(rand.NewPCG(2, 3)) //nolint:gosec // Deterministic compression fixture, not security-sensitive.
	for i := range payload {
		payload[i] = byte(rng.Uint32() & 255)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{{"empty", nil}, {"small", []byte("small asset")}, {"multipleBlocks", payload}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dist := fstest.MapFS{"asset.js.br": {Data: brCompress(t, tc.data)}}
			for _, enc := range []string{"gzip", "identity", "zstd"} {
				t.Run(enc, func(t *testing.T) {
					t.Parallel()
					output, err := doTranscode(dist, "asset.js", enc)
					if err != nil {
						t.Fatal(err)
					}
					decoded := output
					if enc == "gzip" {
						decoded = decompressGzip(t, output)
					}
					if enc == "zstd" {
						decoded = decompressZstd(t, output)
					}
					if !bytes.Equal(decoded, tc.data) {
						t.Fatal("decoded asset differs from source")
					}
				})
			}
		})
	}
	t.Run("FailureAtomicity", func(t *testing.T) {
		t.Parallel()
		valid := brCompress(t, payload)
		sourceErr := errors.New("source read failed")
		closeErr := errors.New("source close failed")
		for _, tc := range []struct {
			name              string
			data              []byte
			readErr, closeErr error
		}{
			{"corrupt", []byte{0xff, 0xff, 0xff}, nil, nil},
			{"truncated", valid[:len(valid)-4], nil, nil},
			{"lateRead", valid, sourceErr, nil},
			{"close", valid, nil, closeErr},
			{"readAndClose", valid, sourceErr, closeErr},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				for _, enc := range []string{"gzip", "identity", "zstd"} {
					t.Run(enc, func(t *testing.T) {
						t.Parallel()
						dist := &transcodeFS{FS: fstest.MapFS{"asset.js.br": {Data: tc.data}}, readErr: tc.readErr, closeErr: tc.closeErr}
						var cache sync.Map
						for range 2 {
							output, err := transcode(&cache, dist, "asset.js", enc)
							if err == nil || output != nil {
								t.Fatal("failed transcode cached partial or successful output")
							}
							if tc.readErr != nil && !errors.Is(err, tc.readErr) {
								t.Fatal("source read error lost")
							}
							if tc.closeErr != nil && !errors.Is(err, tc.closeErr) {
								t.Fatal("source close error lost")
							}
						}
						if dist.opens != 1 || dist.closes != 1 {
							t.Fatalf("source opens=%d closes=%d; expected one cached computation", dist.opens, dist.closes)
						}
					})
				}
			})
		}
	})
}

func TestTranscode(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("cached static asset\n"), 20_000)

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		dist := &transcodeFS{FS: fstest.MapFS{"asset.js.br": {Data: brCompress(t, data)}}}
		var cache sync.Map
		for _, enc := range []string{"gzip", "identity", "zstd"} {
			const callers = 12
			outputs := make([][]byte, callers)
			errs := make([]error, callers)
			var wg sync.WaitGroup
			for i := range callers {
				wg.Go(func() { outputs[i], errs[i] = transcode(&cache, dist, "asset.js", enc) })
			}
			wg.Wait()
			for i := range callers {
				if errs[i] != nil || !bytes.Equal(outputs[i], outputs[0]) || len(outputs[i]) == 0 {
					t.Fatal("concurrent successful output was inconsistent")
				}
			}
		}
		if dist.opens != 3 || dist.closes != 3 {
			t.Fatalf("opens=%d closes=%d; expected one computation per encoding", dist.opens, dist.closes)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		readErr := errors.New("source failure")
		dist := &transcodeFS{FS: fstest.MapFS{"asset.js.br": {Data: brCompress(t, data)}}, readErr: readErr}
		var cache sync.Map
		for _, enc := range []string{"gzip", "identity", "zstd"} {
			const callers = 12
			outputs := make([][]byte, callers)
			errs := make([]error, callers)
			var wg sync.WaitGroup
			for i := range callers {
				wg.Go(func() { outputs[i], errs[i] = transcode(&cache, dist, "asset.js", enc) })
			}
			wg.Wait()
			for i := range callers {
				if outputs[i] != nil || !errors.Is(errs[i], readErr) {
					t.Fatal("concurrent failure was inconsistent")
				}
			}
		}
		if dist.opens != 3 || dist.closes != 3 {
			t.Fatalf("opens=%d closes=%d; expected one computation per encoding", dist.opens, dist.closes)
		}
	})
}

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
