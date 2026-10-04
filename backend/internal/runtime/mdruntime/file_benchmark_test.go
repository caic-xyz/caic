// Benchmarks runtime file transfer allocation and throughput through the actual read command.

package mdruntime

import (
	"bytes"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

func BenchmarkReadFile(b *testing.B) {
	if goruntime.GOOS != "linux" {
		b.Skip("executes GNU dd from the Linux container locally")
	}
	for _, tc := range []struct {
		name string
		size int
	}{
		{"1MiB", 1 << 20},
		{"16MiB", 16 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "artifact")
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), tc.size), 0o600); err != nil {
				b.Fatal(err)
			}
			backend := newTestBackend(&fakeMDClient{getResult: &fileCommandContainer{}})
			id := runtime.NewID("docker", "file-benchmark")
			b.ReportAllocs()
			b.SetBytes(int64(tc.size))
			for b.Loop() {
				n := 0
				for data, err := range backend.ReadFile(b.Context(), id, path, 0, -1) {
					if err != nil {
						b.Fatal(err)
					}
					n += len(data)
				}
				if n != tc.size {
					b.Fatalf("read %d bytes", n)
				}
			}
		})
	}
}
