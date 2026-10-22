// Tests prompt image serving from task logs, including resize, cache, and EXIF rules.

package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"

	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/draw"
)

// attachmentImageLog writes a task log whose input record carries one image and
// returns its path, digest, and bytes.
func attachmentImageLog(t *testing.T, mediaType string, data []byte) (path, digest string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "task.jsonl")
	record := fmt.Sprintf(`{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{"input":[{"type":"image","url":%q}]}}}`,
		"data:"+mediaType+";base64,"+base64.StdEncoding.EncodeToString(data))
	if err := os.WriteFile(path, []byte(record+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

// testImage builds a large image by scaling a two-pixel color tile, so it needs
// no per-pixel channel conversion and still compresses like photographic content.
func testImage(width, height int) *image.RGBA {
	tile := image.NewRGBA(image.Rect(0, 0, 2, 2))
	tile.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	tile.Set(1, 0, color.RGBA{G: 0xff, A: 0xff})
	tile.Set(0, 1, color.RGBA{B: 0xff, A: 0xff})
	tile.Set(1, 1, color.RGBA{R: 0xff, G: 0xff, A: 0xff})
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(img, img.Bounds(), tile, tile.Bounds(), draw.Src, nil)
	return img
}

// testJPEG encodes a synthetic image so scaling produces a different byte count.
func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testImage(width, height), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func attachmentRequest(t *testing.T, s *testRouter, id, digest, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet,
		"/tasks/"+id+"/attachments/"+digest+query, nil)
	testTaskHandlers(s).routes().ServeHTTP(w, req)
	return w
}

func TestTaskAttachment(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		entry, ok := s.taskMgr.GetEntry(id)
		if !ok {
			t.Fatal("task entry missing")
		}
		jpegData := testJPEG(t, 1600, 900)
		logPath, digest := attachmentImageLog(t, "image/jpeg", jpegData)
		entry.LogPath.Set(logPath)

		t.Run("original bytes", func(t *testing.T) {
			t.Parallel()
			w := attachmentRequest(t, s, id.String(), digest, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Content-Type"); got != "image/jpeg" {
				t.Errorf("Content-Type = %q, want image/jpeg", got)
			}
			if got := w.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
				t.Errorf("Cache-Control = %q, want immutable", got)
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if w.Header().Get("ETag") == "" {
				t.Error("missing ETag")
			}
			if !bytes.Equal(w.Body.Bytes(), jpegData) {
				t.Errorf("body = %d bytes, want the stored %d bytes", w.Body.Len(), len(jpegData))
			}
		})

		t.Run("conversation width", func(t *testing.T) {
			t.Parallel()
			w := attachmentRequest(t, s, id.String(), digest, "?w=480")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Width != attachmentConversationWidth {
				t.Errorf("width = %d, want %d", cfg.Width, attachmentConversationWidth)
			}
			if w.Body.Len() >= len(jpegData) {
				t.Errorf("thumbnail = %d bytes, want fewer than the %d stored bytes", w.Body.Len(), len(jpegData))
			}
		})

		t.Run("conditional request", func(t *testing.T) {
			t.Parallel()
			first := attachmentRequest(t, s, id.String(), digest, "?w=480")
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet,
				"/tasks/"+id.String()+"/attachments/"+digest+"?w=480", nil)
			req.Header.Set("If-None-Match", first.Header().Get("ETag"))
			testTaskHandlers(s).routes().ServeHTTP(w, req)
			if w.Code != http.StatusNotModified {
				t.Fatalf("status = %d, want 304", w.Code)
			}
			if w.Body.Len() != 0 {
				t.Errorf("body = %d bytes, want empty", w.Body.Len())
			}
		})
	})

	t.Run("conditional request skips the log", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		_, digest := attachmentImageLog(t, "image/png", []byte("never read"))
		etag := `"` + digest + "-480-" + attachmentVariant + `"`
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet,
			"/tasks/"+id.String()+"/attachments/"+digest+"?w=480", nil)
		req.Header.Set("If-None-Match", etag)
		testTaskHandlers(s).routes().ServeHTTP(w, req)
		if w.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304 without a log", w.Code)
		}
	})

	t.Run("gif is served verbatim", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		entry, _ := s.taskMgr.GetEntry(id)
		var buf bytes.Buffer
		palette := color.Palette{color.Black, color.White}
		img := image.NewPaletted(image.Rect(0, 0, 800, 600), palette)
		if err := gif.Encode(&buf, img, nil); err != nil {
			t.Fatal(err)
		}
		gifData := buf.Bytes()
		logPath, digest := attachmentImageLog(t, "image/gif", gifData)
		entry.LogPath.Set(logPath)
		w := attachmentRequest(t, s, id.String(), digest, "?w=480")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), gifData) {
			t.Error("gif was re-encoded, want the stored bytes")
		}
	})

	t.Run("webp is served verbatim", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		entry, _ := s.taskMgr.GetEntry(id)
		webpData := []byte("RIFF\x1a\x00\x00\x00WEBPVP8L")
		logPath, digest := attachmentImageLog(t, "image/webp", webpData)
		entry.LogPath.Set(logPath)
		w := attachmentRequest(t, s, id.String(), digest, "?w=480")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), webpData) {
			t.Error("webp was re-encoded, want the stored bytes")
		}
	})

	t.Run("oversized frame is served unchanged", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		entry, _ := s.taskMgr.GetEntry(id)
		huge := pngHeader(7000, 7000)
		logPath, digest := attachmentImageLog(t, "image/png", huge)
		entry.LogPath.Set(logPath)
		w := attachmentRequest(t, s, id.String(), digest, "?w=480")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), huge) {
			t.Error("an oversized frame was re-encoded, want the stored bytes")
		}
	})

	t.Run("oriented jpeg keeps its bytes", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		entry, _ := s.taskMgr.GetEntry(id)
		jpegData := orientedJPEG(t, testJPEG(t, 1600, 900), 6)
		logPath, digest := attachmentImageLog(t, "image/jpeg", jpegData)
		entry.LogPath.Set(logPath)
		w := attachmentRequest(t, s, id.String(), digest, "?w=480")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), jpegData) {
			t.Error("oriented jpeg was re-encoded, want the stored bytes")
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		entry, _ := s.taskMgr.GetEntry(id)
		logPath, digest := attachmentImageLog(t, "image/png", []byte("\x89PNG\r\n\x1a\npayload"))
		entry.LogPath.Set(logPath)
		for _, tc := range []struct {
			name   string
			digest string
			query  string
			status int
		}{
			{"unknown digest", hex.EncodeToString(bytes.Repeat([]byte{7}, sha256.Size)), "", http.StatusNotFound},
			{"malformed digest", "abc", "", http.StatusBadRequest},
			{"unsupported width", digest, "?w=481", http.StatusBadRequest},
			{"non-numeric width", digest, "?w=wide", http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				w := attachmentRequest(t, s, id.String(), tc.digest, tc.query)
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
				}
				if got := w.Header().Get("Cache-Control"); got != "no-store" {
					t.Errorf("Cache-Control = %q, want no-store on a failed lookup", got)
				}
				if got := w.Header().Get("ETag"); got != "" {
					t.Errorf("ETag = %q, want none on a failed lookup", got)
				}
			})
		}
		t.Run("no log", func(t *testing.T) {
			t.Parallel()
			absent, _, absentID := newTaskDiffTestRouter(t)
			w := attachmentRequest(t, absent, absentID.String(), digest, "")
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", w.Code)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if got := w.Header().Get("ETag"); got != "" {
				t.Errorf("ETag = %q, want none", got)
			}
		})
	})
}

// pngHeader builds a PNG carrying only its signature and IHDR chunk, which is
// enough to report dimensions without decoding a frame-sized allocation.
func pngHeader(width, height uint32) []byte {
	ihdr := make([]byte, 0, 13)
	ihdr = binary.BigEndian.AppendUint32(ihdr, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr, 8, 2, 0, 0, 0)
	out := make([]byte, 0, 8+12+len(ihdr))
	out = append(out, 0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n')
	out = binary.BigEndian.AppendUint32(out, uint32(len(ihdr))) //nolint:gosec // fixture: IHDR is 13 bytes.
	out = append(out, "IHDR"...)
	out = append(out, ihdr...)
	out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...)))
	return out
}

// orientedJPEG inserts an Exif APP1 segment carrying orientation before the
// start of scan of an encoded JPEG.
func orientedJPEG(t *testing.T, jpegData []byte, orientation uint16) []byte {
	t.Helper()
	tiff := make([]byte, 0, 26)
	tiff = append(tiff,
		'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0,
		0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(orientation), byte(orientation>>8), 0, 0, 0, 0, 0, 0, //nolint:gosec // fixture: the orientation tag is at most 8.
	)
	segment := append([]byte("Exif\x00\x00"), tiff...)
	length := len(segment) + 2
	out := make([]byte, 0, len(jpegData)+length)
	out = append(out, 0xff, 0xd8, 0xff, 0xe1, byte(length>>8), byte(length)) //nolint:gosec // fixture: the Exif segment length fits 16 bits.
	out = append(out, segment...)
	out = append(out, jpegData[2:]...)
	return out
}

func TestDownscaleImage(t *testing.T) {
	t.Parallel()
	jpegData := testJPEG(t, 1600, 900)
	small := testJPEG(t, 400, 200)
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		scaled, ok := downscaleImage(jpegData, "image/jpeg", attachmentConversationWidth)
		if !ok {
			t.Fatal("downscaleImage reported no resize for a large JPEG")
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(scaled))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Width != attachmentConversationWidth {
			t.Errorf("width = %d, want %d", cfg.Width, attachmentConversationWidth)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name      string
			data      []byte
			mediaType string
			width     int
		}{
			{"animated format", jpegData, "image/gif", attachmentConversationWidth},
			{"already narrow", small, "image/jpeg", attachmentConversationWidth},
			{"not an image", []byte("not an image"), "image/jpeg", attachmentConversationWidth},
			{"oriented", orientedJPEG(t, jpegData, 6), "image/jpeg", attachmentConversationWidth},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if scaled, ok := downscaleImage(tc.data, tc.mediaType, tc.width); ok {
					t.Errorf("downscaleImage = %d bytes, want the original", len(scaled))
				}
			})
		}
	})
}

func TestJPEGOrientation(t *testing.T) {
	t.Parallel()
	jpegData := testJPEG(t, 40, 20)
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, orientation := range []uint16{1, 3, 6, 8} {
			if got := jpegOrientation(orientedJPEG(t, jpegData, orientation)); got != int(orientation) {
				t.Errorf("jpegOrientation = %d, want %d", got, orientation)
			}
		}
		if got := jpegOrientation(jpegData); got != 1 {
			t.Errorf("jpegOrientation(no Exif) = %d, want 1", got)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			data []byte
		}{
			{"empty", nil},
			{"truncated marker", []byte{0xff, 0xd8, 0xff}},
			{"bad segment length", []byte{0xff, 0xd8, 0xff, 0xe1, 0xff, 0xff}},
			{"not a jpeg", []byte("not a jpeg")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := jpegOrientation(tc.data); got != 0 {
					t.Errorf("jpegOrientation = %d, want 0 for unreadable EXIF", got)
				}
			})
		}
	})
}
