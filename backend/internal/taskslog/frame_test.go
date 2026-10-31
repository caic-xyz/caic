// Tests exact header authority across framing, plain/compressed readers, and incremental cursors.

package taskslog

import (
	"fmt"
	"os"
	"testing"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func TestDecodeFrame(t *testing.T) {
	t.Parallel()
	t.Run("header authority", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, raw := range []string{
				`{"t":"caic_meta","version":3,"prompt":"p","harness":"future-provider"}`,
				`{"t":"caic_meta","Version":2,"version":3,"prompt":"p","harness":"future-provider"}`,
				`{"t":"caic_meta","version":3,"Version":3,"prompt":"p","harness":"future-provider"}`,
				`{"t":"caic_meta","version":3,"Version":null,"prompt":"p","harness":"future-provider"}`,
				`{"T":"other","t":"caic_meta","version":3,"prompt":"p","harness":"future-provider"}`,
				`{"t":"caic_meta","T":null,"version":3,"prompt":"p","harness":"future-provider"}`,
				`{"t":"caic_meta","version":3,"Prompt":"p","Harness":"future-provider"}`,
				`{"t":"caic_meta","Version":2,"ver\u0073ion":3,"prompt":"p","harness":"future-provider"}`,
			} {
				t.Run(raw, func(t *testing.T) {
					t.Parallel()
					frame, err := DecodeFrame([]byte(raw))
					if err != nil {
						t.Fatal(err)
					}
					meta, ok := frame.Control.(*v3.MetaMessage)
					if !ok || meta.MessageType != "caic_meta" || meta.Version != 3 || meta.Prompt != "p" || meta.Harness != "future-provider" {
						t.Fatalf("header = %+v", frame)
					}
					for _, compressed := range []bool{false, true} {
						t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
							t.Parallel()
							path := writeRawFixture(t, compressed, raw+"\n")
							if _, err := ReadHeader(path); err != nil {
								t.Fatal(err)
							}
							cursor, err := OpenRawCursor(path)
							if err != nil {
								t.Fatal(err)
							}
							t.Cleanup(func() {
								if err := cursor.Close(); err != nil {
									t.Error(err)
								}
							})
							var records int
							for _, err := range cursor.Read(t.Context()) {
								if err != nil {
									t.Fatal(err)
								}
								records++
							}
							if records != 1 {
								t.Fatalf("header records = %d", records)
							}
						})
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, raw := range []string{
				`{"t":"caic_meta","version":2,"Version":3,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","Version":3,"version":2,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","version":3,"Version":2,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","Version":3,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","version":null,"Version":3,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","version":3.0,"Version":3,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","version":"3","Version":3,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","version":999999999999999999999,"Version":3,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","version":2,"version":3,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","version":3,"version":2,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","version":3,"ver\u0073ion":3,"prompt":"p","harness":"claude"}`,
				`{"t":"caic_meta","T":"other","version":3,"prompt":"p","harness":"claude"}`,
				`{"T":"caic_meta","version":3,"prompt":"p","harness":"claude"}`,
				`{"t":null,"T":"caic_meta","version":3,"prompt":"p","harness":"claude"}`,
			} {
				t.Run(raw, func(t *testing.T) {
					t.Parallel()
					if _, err := DecodeFrame([]byte(raw)); err == nil {
						t.Error("invalid header accepted as a frame")
					}
					for _, compressed := range []bool{false, true} {
						t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
							t.Parallel()
							path := writeRawFixture(t, compressed, raw+"\n")
							if _, err := ReadHeader(path); err == nil {
								t.Error("invalid first header accepted")
							}
							cursor, err := OpenRawCursor(path)
							if err == nil {
								if err := cursor.Close(); err != nil {
									t.Error(err)
								}
								t.Error("invalid cursor authority accepted")
							}
							segmentPath := writeRawFixture(t, compressed, testHeader+"\n"+raw+"\n")
							err = ScanFile(segmentPath, true, func(_ os.FileInfo, scanner *Scanner, _ v3.MetaMessage) error {
								for scanner.Scan() {
								}
								return scanner.Err()
							})
							if err == nil {
								t.Error("invalid segment header accepted")
							}
						})
					}
				})
			}
		})
	})
}
