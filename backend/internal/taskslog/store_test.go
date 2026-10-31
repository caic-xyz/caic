// Tests byte-preserving append batches and initialized physical header publication.

package taskslog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func TestEncodeHeader(t *testing.T) {
	t.Parallel()
	meta := &v3.MetaMessage{Prompt: "record", Harness: "claude", ParentTaskID: "parent", CaicMCP: true,
		Tailscale: true, USB: true, Display: true, Sudo: true, GitHubToken: true,
		RuntimeName: "remote", BaseImage: "image", ContainerPlatform: "linux/arm64", MaxCPUs: 4,
		CacheMounts: []v3.MetaCacheMount{{Name: "cache"}}, Mounts: []v3.MetaMount{{HostPath: "/host", ContainerPath: "/container"}}}
	encoded, err := EncodeHeader(meta)
	if err != nil {
		t.Fatal(err)
	}
	var got v3.MetaMessage
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	want := *meta
	want.MessageType, want.Version = "caic_meta", 3
	if !reflect.DeepEqual(got, want) || bytes.ContainsRune(encoded, '\n') {
		t.Fatalf("header = %s, want %+v without LF", encoded, want)
	}
	if _, err := DecodeFrame(encoded); err != nil {
		t.Fatalf("encoded header is unreadable: %v", err)
	}
	canonical, err := EncodeHeader(&v3.MetaMessage{Prompt: "p", Harness: "pi", RequestedModel: "gpt-5.6", RequestedEffort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != `{"effort":"high","harness":"pi","model":"gpt-5.6","prompt":"p","repos":null,"started_at":"0001-01-01T00:00:00Z","t":"caic_meta","version":3}` {
		t.Fatalf("canonical header bytes = %s", canonical)
	}
}

func TestStore(t *testing.T) {
	t.Parallel()
	t.Run("Initialize", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			store := NewStore(t.TempDir())
			prepared, err := store.Initialize("prepared.jsonl", &v3.MetaMessage{Prompt: "prepared", Harness: "future-provider"})
			if err != nil {
				t.Fatal(err)
			}
			meta, err := ReadHeader(prepared.Path)
			if err != nil || meta.Version != 3 || meta.Prompt != "prepared" || meta.Harness != "future-provider" {
				t.Fatalf("initialized header = %+v, %v", meta, err)
			}
			if err := prepared.Rollback(); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct{ name, prompt, harness string }{
				{"missing prompt", "", "claude"},
				{"missing harness", "prepared", ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					store := NewStore(t.TempDir())
					if _, err := store.Initialize("prepared.jsonl", &v3.MetaMessage{Prompt: tc.prompt, Harness: tc.harness}); err == nil {
						t.Fatal("unreadable header was published")
					}
					if _, err := os.Stat(filepath.Join(store.LogDir, "prepared.jsonl")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("failed initialization retained a log: %v", err)
					}
				})
			}
		})
	})
}

func TestWriter(t *testing.T) {
	t.Parallel()
	writer, path, err := NewStore(t.TempDir()).Open("writer.jsonl", &v3.MetaMessage{Prompt: "record", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	header, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{nil, []byte(`{"t":"text","text":"unfinished"}`)} {
		if err := writer.Append(invalid); err == nil {
			t.Fatal("incomplete batch accepted")
		}
	}
	batch := []byte("{\"t\":\"agent\",\"ts\":1.000,\"msg\":{\"content\":\"exact  bytes\"}}\n{\"t\":\"relay_generation\",\"generation\":\"new\"}\n")
	if err := writer.Append(batch); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Clean(path))
	if err != nil || !bytes.Equal(got, append(header, batch...)) {
		t.Fatalf("append changed physical bytes: %q, %v", got, err)
	}
	if err := writer.Append(batch); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("append after close = %v", err)
	}
}
