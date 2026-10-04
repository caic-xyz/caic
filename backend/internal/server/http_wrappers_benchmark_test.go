// Benchmarks capped JSON request decoding, including large canonical image payloads.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func requestBodyFixture(size int) string {
	return `{"initialPrompt":{"images":[{"mediaType":"image/png","data":"` + strings.Repeat("A", size) + `"}]},"harness":"claude"}`
}

func BenchmarkReadAndDecodeBody(b *testing.B) {
	for _, size := range []int{1024, 1 << 20, 27962024} {
		b.Run(fmt.Sprintf("Bytes%d", size), func(b *testing.B) {
			body := requestBodyFixture(size)
			ctx := context.WithValue(b.Context(), httpLoggerKey{}, slog.New(slog.DiscardHandler))
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for b.Loop() {
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(body))
				var in v1.CreateTaskReq
				if !readAndDecodeBody(httptest.NewRecorder(), req, &in) {
					b.Fatal("decode failed")
				}
			}
		})
	}
}

// TestRequestBodyPeak samples allocated heap for one large decode in an isolated test process.
// Run with CAIC_REQUEST_HEAP_SAMPLE=1 GOGC=off go test ./backend/internal/server -run '^TestRequestBodyPeak$' -v -count=1.
func TestRequestBodyPeak(t *testing.T) {
	t.Parallel()
	if os.Getenv("CAIC_REQUEST_HEAP_SAMPLE") != "1" {
		t.Skip("set CAIC_REQUEST_HEAP_SAMPLE=1 for isolated heap sampling")
	}
	body := requestBodyFixture(27962024)
	req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/", strings.NewReader(body))
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			if n := stats.HeapAlloc; n > peak.Load() {
				peak.Store(n)
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	var in v1.CreateTaskReq
	ok := readAndDecodeBody(httptest.NewRecorder(), req, &in)
	close(stop)
	<-done
	var final runtime.MemStats
	runtime.ReadMemStats(&final)
	if final.HeapAlloc > peak.Load() {
		peak.Store(final.HeapAlloc)
	}
	runtime.KeepAlive(in)
	runtime.KeepAlive(body)
	if !ok {
		t.Fatal("decode failed")
	}
	t.Logf("sampled incremental allocated heap: %d bytes", peak.Load()-before.HeapAlloc)
}
