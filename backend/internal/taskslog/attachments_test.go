// Tests locating prompt images inside plain and compressed task logs.

package taskslog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func writeAttachmentLog(t *testing.T, dir, name string, records ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var buf bytes.Buffer
	for _, record := range records {
		buf.WriteString(record)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func imageRecord(images ...string) string {
	record := `{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{"input":[{"type":"text","text":"hi"}]}}}`
	if len(images) == 0 {
		return record
	}
	var urls bytes.Buffer
	for _, image := range images {
		fmt.Fprintf(&urls, `,{"type":"image","url":%q,"detail":null}`, image)
	}
	return `{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{"input":[{"type":"text","text":"hi"}` +
		urls.String() + `]}}}`
}

func TestFindAttachment(t *testing.T) {
	t.Parallel()
	pngBytes := []byte("\x89PNG\r\n\x1a\nfirst-image")
	jpegBytes := []byte("\xff\xd8\xff\xe0second-image")
	first := sha256.Sum256(pngBytes)
	second := sha256.Sum256(jpegBytes)
	piBytes := []byte("pi-prompt-image")
	pi := sha256.Sum256(piBytes)
	opencodeBytes := []byte("opencode-prompt-image")
	openCode := sha256.Sum256(opencodeBytes)
	controlBytes := []byte("control-record-image")
	control := sha256.Sum256(controlBytes)
	records := []string{
		imageRecord(),
		imageRecord(dataURL("image/png", pngBytes), dataURL("image/jpeg", jpegBytes)),
		`{"t":"log","line":"no image here"}`,
		fmt.Sprintf(`{"t":"user_input","ts":2.000,"text":"control","images":[{"media_type":"image/png","data":%q}]}`,
			base64.StdEncoding.EncodeToString(controlBytes)),
		fmt.Sprintf(`{"t":"input","ts":3.000,"msg":{"id":"p1","type":"prompt","message":"look","images":[{"type":"image","data":%q,"mimeType":"image/jpeg"}]}}`,
			base64.StdEncoding.EncodeToString(piBytes)),
		fmt.Sprintf(`{"t":"input","ts":4.000,"msg":{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"look"},{"type":"image","data":%q,"mimeType":"image/png"}]}}}`,
			base64.StdEncoding.EncodeToString(opencodeBytes)),
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		plain := writeAttachmentLog(t, t.TempDir(), "task.jsonl", records...)
		compressed := plain + ".zst"
		var buf bytes.Buffer
		enc, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(plain) //nolint:gosec // path is an owned test fixture.
		if err != nil {
			t.Fatal(err)
		}
		if _, err := enc.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := enc.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(compressed, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name      string
			path      string
			digest    [sha256.Size]byte
			mediaType string
			data      []byte
		}{
			{"plain png", plain, first, "image/png", pngBytes},
			{"plain jpeg", plain, second, "image/jpeg", jpegBytes},
			{"compressed png", compressed, first, "image/png", pngBytes},
			{"compressed jpeg", compressed, second, "image/jpeg", jpegBytes},
			{"control record", plain, control, "image/png", controlBytes},
			{"pi prompt", plain, pi, "image/jpeg", piBytes},
			{"opencode prompt", plain, openCode, "image/png", opencodeBytes},
			{"compressed pi prompt", compressed, pi, "image/jpeg", piBytes},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				mediaType, data, err := FindAttachment(t.Context(), tc.path, tc.digest)
				if err != nil {
					t.Fatal(err)
				}
				if mediaType != tc.mediaType || !bytes.Equal(data, tc.data) {
					t.Errorf("FindAttachment(%s) = (%q, %d bytes), want (%q, %d bytes)", tc.name, mediaType, len(data), tc.mediaType, len(tc.data))
				}
			})
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		plain := writeAttachmentLog(t, t.TempDir(), "task.jsonl", records...)
		t.Run("unknown digest", func(t *testing.T) {
			t.Parallel()
			_, _, err := FindAttachment(t.Context(), plain, sha256.Sum256([]byte("absent")))
			if !errors.Is(err, ErrAttachmentNotFound) {
				t.Fatalf("err = %v, want ErrAttachmentNotFound", err)
			}
		})
		t.Run("missing log", func(t *testing.T) {
			t.Parallel()
			_, _, err := FindAttachment(t.Context(), filepath.Join(t.TempDir(), "absent.jsonl"), first)
			if err == nil || errors.Is(err, ErrAttachmentNotFound) {
				t.Fatalf("err = %v, want an open failure", err)
			}
		})
		t.Run("canceled", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, _, err := FindAttachment(ctx, plain, first); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		})
	})
}
