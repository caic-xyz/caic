// Tests streamed asset transcoding, failure atomicity, and concurrent variant caching.

package server

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"sync"
	"testing"
	"testing/fstest"
)

// transcodeFS wraps the natural source boundary to observe opens and inject I/O errors.
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
	for _, fail := range []bool{false, true} {
		name := "success"
		var readErr error
		if fail {
			name = "failure"
			readErr = errors.New("source failure")
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
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
					if fail {
						if outputs[i] != nil || !errors.Is(errs[i], readErr) {
							t.Fatal("concurrent failure was inconsistent")
						}
					} else {
						if errs[i] != nil || !bytes.Equal(outputs[i], outputs[0]) || len(outputs[i]) == 0 {
							t.Fatal("concurrent successful output was inconsistent")
						}
					}
				}
			}
			if dist.opens != 3 || dist.closes != 3 {
				t.Fatalf("opens=%d closes=%d; expected one computation per encoding", dist.opens, dist.closes)
			}
		})
	}
}
