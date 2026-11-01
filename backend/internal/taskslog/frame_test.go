// Tests exact header authority across framing, plain/compressed readers, and incremental cursors.

package taskslog

import (
	"fmt"
	"os"
	"reflect"
	"strings"
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

// TestV3ControlFieldAllowed pins the strict frame field allowlist to the v3
// schemas it guards, in both directions.
func TestV3ControlFieldAllowed(t *testing.T) {
	t.Parallel()
	tags := map[logControlKind]map[string]bool{}
	for kind := logControlMeta; kind <= logControlRelayGeneration; kind++ {
		control := newControl(kind)
		if control == nil {
			t.Fatalf("kind %d has no schema", kind)
		}
		tags[kind] = v3JSONFields(reflect.TypeOf(control).Elem())
	}
	t.Run("schema field rejected", func(t *testing.T) {
		t.Parallel()
		for kind, names := range tags {
			// caic_meta fields are validated by the strict owned-schema decoder.
			if kind == logControlMeta {
				continue
			}
			for name := range names {
				if !v3ControlFieldAllowed(kind, name) {
					t.Errorf("kind %d rejects schema field %q", kind, name)
				}
			}
		}
	})
	t.Run("unmodelled field accepted", func(t *testing.T) {
		t.Parallel()
		for kind, names := range tags {
			if kind == logControlMeta {
				continue
			}
			fields, ok := v3ControlFields[kind]
			if !ok {
				t.Fatalf("kind %d has no field table", kind)
			}
			for _, name := range fields {
				if !names[name] {
					t.Errorf("kind %d accepts %q, which no schema models", kind, name)
				}
			}
		}
	})
}

// v3JSONFields returns the JSON names of a v3 control schema, including
// embedded provenance.
func v3JSONFields(t reflect.Type) map[string]bool {
	out := make(map[string]bool, t.NumField())
	for f := range t.Fields() {
		if f.Anonymous {
			for name := range v3JSONFields(f.Type) {
				out[name] = true
			}
			continue
		}
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}
