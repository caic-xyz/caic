// Tests and benchmarks coordinate-based prompt image lookup in plain and compressed task logs.

package taskslog

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	logv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func TestFindAttachment(t *testing.T) {
	t.Parallel()
	resolver := backends.Default("", nil)
	data := []byte("prompt image bytes")
	encoded := base64.StdEncoding.EncodeToString(data)
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("compressed=%v", compressed), func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed,
					`{"t":"caic_meta","version":3,"harness":"claude","prompt":"original"}`,
					"", // Blank lines do not consume a physical record ID.
					fmt.Sprintf(`{"t":"user_input","text":"look","images":[{"media_type":"image/jpeg","data":"b3RoZXI="},{"media_type":"image/png","data":%q}]}`, encoded))
				media, actual, err := FindAttachment(t.Context(), path, resolver, 2, 0, 1)
				if err != nil {
					t.Fatal(err)
				}
				if media != "image/png" || !bytes.Equal(actual, data) {
					t.Fatalf("image = (%q, %q)", media, actual)
				}
			})
		}
		for _, tc := range []struct{ harness, record string }{
			{"codex", fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"t1","turnId":"turn_1","item":{"id":"u1","type":"userMessage","content":[{"type":"image","url":"data:image/png;base64,%s"}]}}}}`, encoded)},
			{"pi", fmt.Sprintf(`{"t":"input","ts":1.000,"msg":{"id":"p1","type":"prompt","message":"look","images":[{"type":"image","data":%q,"mimeType":"image/png"}]}}`, encoded)},
			{"opencode", fmt.Sprintf(`{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"image","data":%q,"mimeType":"image/png"}]}}}`, encoded)},
		} {
			t.Run(tc.harness, func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, false, fmt.Sprintf(`{"t":"caic_meta","version":3,"harness":%q,"prompt":"original"}`, tc.harness), tc.record)
				media, actual, err := FindAttachment(t.Context(), path, resolver, 2, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				if media != "image/png" || !bytes.Equal(actual, data) {
					t.Fatalf("image = (%q, %q)", media, actual)
				}
			})
		}
	})
	t.Run("message index and parser state", func(t *testing.T) {
		t.Parallel()
		resolver := newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
			seen := false
			return &agenttest.Wire{Output: func(raw []byte) ([]agent.Message, error) {
				if string(raw) == `{"setup":true}` {
					seen = true
					return nil, nil
				}
				if !seen {
					return nil, errors.New("missing setup record")
				}
				return []agent.Message{&agent.TextMessage{Text: "before input"}, &agent.UserInputMessage{Images: []logv3.ImageData{{MediaType: "image/jpeg", Data: "b3RoZXI="}, {MediaType: "image/png", Data: encoded}}}}, nil
			}}, nil
		})
		path := writePhysicalTestLog(t, false, `{"t":"caic_meta","version":3,"harness":"claude","prompt":"original"}`, `{"t":"agent","ts":1.000,"msg":{"setup":true}}`, `{"t":"agent","ts":2.000,"msg":{"prompt":true}}`)
		media, actual, err := FindAttachment(t.Context(), path, resolver, 3, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if media != "image/png" || !bytes.Equal(actual, data) {
			t.Fatalf("image = (%q, %q)", media, actual)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		path := writePhysicalTestLog(t, false, `{"t":"caic_meta","version":3,"harness":"claude","prompt":"original"}`, fmt.Sprintf(`{"t":"user_input","images":[{"media_type":"image/png","data":%q}]}`, encoded), `{"t":"user_input","images":[{"media_type":"image/png","data":"invalid base64"}]}`)
		for _, tc := range []struct {
			name                   string
			record, message, image uint64
		}{
			{"missing record", 99, 0, 0}, {"missing message", 2, 1, 0}, {"missing image", 2, 0, 1}, {"metadata record", 1, 0, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				_, _, err := FindAttachment(t.Context(), path, resolver, tc.record, tc.message, tc.image)
				if !errors.Is(err, ErrAttachmentNotFound) {
					t.Fatalf("err = %v, want ErrAttachmentNotFound", err)
				}
			})
		}
		t.Run("corrupt payload", func(t *testing.T) {
			t.Parallel()
			if _, _, err := FindAttachment(t.Context(), path, resolver, 3, 0, 0); err == nil || errors.Is(err, ErrAttachmentNotFound) {
				t.Fatalf("err = %v, want decode failure", err)
			}
		})
		t.Run("missing log", func(t *testing.T) {
			t.Parallel()
			if _, _, err := FindAttachment(t.Context(), filepath.Join(t.TempDir(), "absent.jsonl"), resolver, 2, 0, 0); err == nil {
				t.Fatal("missing log succeeded")
			}
		})
		t.Run("canceled", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, _, err := FindAttachment(ctx, path, resolver, 2, 0, 0); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want cancellation", err)
			}
		})
	})
}

// BenchmarkFindAttachment measures a browser cache miss near the end of a task
// with 1,000 progress records and a 1 MiB prompt image. Each lookup opens and
// reads the log anew, as immutable HTTP caching normally avoids repeat work.
func BenchmarkFindAttachment(b *testing.B) {
	data := bytes.Repeat([]byte{7}, 1<<20)
	encoded := base64.StdEncoding.EncodeToString(data)
	var log strings.Builder
	log.WriteString(`{"t":"caic_meta","version":3,"harness":"claude","prompt":"original"}` + "\n")
	for range 1000 {
		log.WriteString(`{"t":"log","line":"working on the task"}` + "\n")
	}
	fmt.Fprintf(&log, `{"t":"user_input","text":"look","images":[{"media_type":"image/png","data":%q}]}`+"\n", encoded)
	path := filepath.Join(b.TempDir(), "task.jsonl")
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		b.Fatal(err)
	}
	resolver := backends.Default("", nil)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := FindAttachment(b.Context(), path, resolver, 1002, 0, 0); err != nil {
			b.Fatal(err)
		}
	}
}
