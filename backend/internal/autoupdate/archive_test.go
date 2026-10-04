// Tests bounded update archive extraction, full-download hashing, and cleanup.

package autoupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/gzip"

	"github.com/caic-xyz/caic/backend/internal/forge/github"
)

func TestExtractZipToFile(t *testing.T) {
	content := []byte("windows binary payload")
	stored := testZip(t, "bin/caic.exe", content, zip.Store)
	deflated := testZip(t, "caic.exe", content, zip.Deflate)
	corrupt := bytes.Clone(stored)
	i := bytes.Index(corrupt, content)
	if i < 0 {
		t.Fatal("stored payload absent")
	}
	corrupt[i] ^= 1
	t.Run("valid", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			data []byte
		}{{"stored_nested", stored}, {"deflated", deflated}, {"trailing_download_bytes", append(bytes.Clone(stored), []byte("trailing bytes")...)}} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("TMPDIR", dir)
				var dst bytes.Buffer
				if err := extractZipToFile(bytes.NewReader(tc.data), "caic.exe", &dst); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(dst.Bytes(), content) {
					t.Fatalf("extracted %q", dst.Bytes())
				}
				assertNoSpools(t, dir)
			})
		}
	})
	readErr := errors.New("archive read failed")
	writeErr := errors.New("binary write failed")
	oversized := testOversizedZip(t)
	t.Run("error", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			src  io.Reader
			dst  io.Writer
			want error
			text string
		}{
			{"invalid", strings.NewReader("invalid archive"), io.Discard, nil, "zip"},
			{"missing", bytes.NewReader(stored), io.Discard, nil, "not found"},
			{"crc", bytes.NewReader(corrupt), io.Discard, zip.ErrChecksum, ""},
			{"truncated", bytes.NewReader(stored[:len(stored)-12]), io.Discard, nil, "zip"},
			{"read", io.MultiReader(bytes.NewReader(stored[:20]), errorReader{readErr}), io.Discard, readErr, ""},
			{"write", bytes.NewReader(stored), errorWriter{writeErr}, writeErr, ""},
			{"oversized", bytes.NewReader(oversized), io.Discard, nil, "exceeds"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("TMPDIR", dir)
				name := "caic.exe"
				if tc.name == "missing" {
					name = "absent"
				}
				err := extractZipToFile(tc.src, name, tc.dst)
				if err == nil || tc.want != nil && !errors.Is(err, tc.want) || tc.text != "" && !strings.Contains(err.Error(), tc.text) {
					t.Fatalf("unexpected error: %v", err)
				}
				assertNoSpools(t, dir)
			})
		}
	})
}

func TestCopyBinary(t *testing.T) {
	t.Parallel()
	t.Run("exact_limit", func(t *testing.T) {
		t.Parallel()
		if err := copyBinary(io.Discard, io.LimitReader(zeroReader{}, 256<<20)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("overflow", func(t *testing.T) {
		t.Parallel()
		err := copyBinary(io.Discard, io.LimitReader(zeroReader{}, (256<<20)+1))
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversize result: %v", err)
		}
	})
}

func TestExtractDownloadedArchive(t *testing.T) {
	t.Parallel()
	t.Run("hash_entire_tar", func(t *testing.T) {
		t.Parallel()
		data := testTarTrailing(t)
		h := sha256.New()
		var dst bytes.Buffer
		if err := extractDownloadedArchive(io.NopCloser(bytes.NewReader(data)), h, "update.tar.gz", "caic", &dst); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if !bytes.Equal(h.Sum(nil), sum[:]) {
			t.Fatal("checksum omitted downloaded bytes")
		}
		if dst.String() != "binary" {
			t.Fatalf("extracted %q", dst.String())
		}
	})
	t.Run("hash_zip_trailing_bytes", func(t *testing.T) {
		t.Parallel()
		data := append(testZip(t, "caic.exe", []byte("binary"), zip.Store), []byte("trailing bytes")...)
		h := sha256.New()
		if err := extractDownloadedArchive(io.NopCloser(bytes.NewReader(data)), h, "update.zip", "caic.exe", io.Discard); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if !bytes.Equal(h.Sum(nil), sum[:]) {
			t.Fatal("checksum omitted downloaded bytes")
		}
	})
	t.Run("gzip_footer", func(t *testing.T) {
		t.Parallel()
		data := testTarTrailing(t)
		data[len(data)-8] ^= 1
		err := extractTarGzToFile(bytes.NewReader(data), "caic", io.Discard)
		if !errors.Is(err, gzip.ErrChecksum) {
			t.Fatalf("footer error: %v", err)
		}
	})
	t.Run("gzip_truncated", func(t *testing.T) {
		t.Parallel()
		data := testTarTrailing(t)
		err := extractTarGzToFile(bytes.NewReader(data[:len(data)-4]), "caic", io.Discard)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated error: %v", err)
		}
	})
	t.Run("tar_oversized", func(t *testing.T) {
		t.Parallel()
		var data bytes.Buffer
		gz := gzip.NewWriter(&data)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "caic", Size: (256 << 20) + 1, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		// Flush only the oversized header. No huge body is needed to reject it.
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		err := extractTarGzToFile(bytes.NewReader(data.Bytes()), "caic", io.Discard)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("limit result: %v", err)
		}
	})
	t.Run("close_failure", func(t *testing.T) {
		t.Parallel()
		closeErr := errors.New("close download failed")
		body := errorCloseBody{Reader: bytes.NewReader(testZip(t, "caic.exe", []byte("binary"), zip.Store)), err: closeErr}
		err := extractDownloadedArchive(body, io.Discard, "update.zip", "caic.exe", io.Discard)
		if !errors.Is(err, closeErr) {
			t.Fatalf("close error: %v", err)
		}
	})
	t.Run("read_and_close_failure", func(t *testing.T) {
		t.Parallel()
		readErr := errors.New("read download failed")
		closeErr := errors.New("close download failed")
		body := errorCloseBody{Reader: errorReader{readErr}, err: closeErr}
		err := extractDownloadedArchive(body, io.Discard, "update.zip", "caic.exe", io.Discard)
		if !errors.Is(err, readErr) || !errors.Is(err, closeErr) {
			t.Fatalf("joined errors: %v", err)
		}
	})
	t.Run("canceled_download", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("PK"))
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("HTTP writer lacks flush support")
				return
			}
			flusher.Flush()
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		gh := github.NewClient("", http.DefaultTransport)
		body, err := gh.DownloadAsset(ctx, srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		err = extractDownloadedArchive(body, io.Discard, "update.zip", "caic.exe", io.Discard)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error: %v", err)
		}
	})
}

func TestExpectedChecksum(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		sum, err := expectedChecksum(strings.NewReader("other other.zip\nfirst update.zip\nsecond update.zip\n"), "update.zip")
		if err != nil || sum != "first" {
			t.Fatalf("checksum=%q, err=%v", sum, err)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		lateErr := errors.New("late read failed")
		for _, tc := range []struct {
			name string
			src  io.Reader
			want error
		}{
			{"missing", strings.NewReader("hash other.zip\n"), nil},
			{"oversized_line", strings.NewReader(strings.Repeat("x", 64<<10)), nil},
			{"late_read", io.MultiReader(strings.NewReader("hash update.zip\n"), errorReader{lateErr}), lateErr},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				_, err := expectedChecksum(tc.src, "update.zip")
				if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
					t.Fatalf("checksum error: %v", err)
				}
			})
		}
	})
}

func TestDownloadExpectedChecksum(t *testing.T) {
	t.Parallel()
	rel := &github.Release{Assets: []github.ReleaseAsset{{Name: "checksums.txt", DownloadURL: "https://example.com/checksums.txt"}}}
	closeErr := errors.New("checksum close failed")
	gh := github.NewClient("", checksumTransport{body: errorCloseBody{Reader: strings.NewReader("hash update.zip\n"), err: closeErr}})
	_, err := downloadExpectedChecksum(t.Context(), gh, rel, "update.zip")
	if !errors.Is(err, closeErr) {
		t.Fatalf("close error: %v", err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type errorCloseBody struct {
	io.Reader

	err error
}

func (r errorCloseBody) Close() error { return r.err }

type checksumTransport struct{ body io.ReadCloser }

func (tr checksumTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: tr.body}, nil
}

func assertNoSpools(t *testing.T, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("archive spool leaked: %v", entries)
	}
}

func testZip(t *testing.T, name string, content []byte, method uint16) []byte {
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func testOversizedZip(t *testing.T) []byte {
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	if _, err := zw.CreateRaw(&zip.FileHeader{Name: "caic.exe", Method: zip.Store, UncompressedSize64: (256 << 20) + 1}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func testTarTrailing(t *testing.T) []byte {
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	trailing := make([]byte, 1<<20)
	if _, err := rand.NewChaCha8([32]byte{}).Read(trailing); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{"caic", []byte("binary")}, {"trailing", trailing}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Size: int64(len(f.data)), Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
