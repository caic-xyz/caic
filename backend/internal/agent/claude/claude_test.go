package claude

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestWritePrompt(t *testing.T) {
	t.Run("TextOnly", func(t *testing.T) {
		var buf bytes.Buffer
		var logBuf bytes.Buffer
		var b Backend
		if err := b.WritePrompt(&buf, agent.Prompt{Text: "hello"}, &logBuf); err != nil {
			t.Fatal(err)
		}
		if buf.String() != logBuf.String() {
			t.Errorf("stdin and log differ:\nstdin: %q\nlog:   %q", buf.String(), logBuf.String())
		}
		if !strings.Contains(buf.String(), `"content":"hello"`) {
			t.Errorf("unexpected output: %s", buf.String())
		}
	})

	t.Run("WithImages", func(t *testing.T) {
		var buf bytes.Buffer
		var b Backend
		images := []agent.ImageData{
			{MediaType: "image/png", Data: "iVBOR..."},
		}
		if err := b.WritePrompt(&buf, agent.Prompt{Text: "describe this", Images: images}, nil); err != nil {
			t.Fatal(err)
		}
		// Content must be an array of content blocks, not a string.
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &msg); err != nil {
			t.Fatal(err)
		}
		var blocks []struct {
			Type   string `json:"type"`
			Text   string `json:"text,omitempty"`
			Source *struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source,omitempty"`
		}
		if err := json.Unmarshal(msg.Message.Content, &blocks); err != nil {
			t.Fatalf("content should be array of blocks: %v\nraw: %s", err, msg.Message.Content)
		}
		if len(blocks) != 2 {
			t.Fatalf("expected 2 blocks, got %d", len(blocks))
		}
		if blocks[0].Type != "image" || blocks[0].Source == nil {
			t.Errorf("blocks[0] = %+v, want image block", blocks[0])
		}
		if blocks[0].Source.MediaType != "image/png" {
			t.Errorf("media_type = %q, want %q", blocks[0].Source.MediaType, "image/png")
		}
		if blocks[1].Type != "text" || blocks[1].Text != "describe this" {
			t.Errorf("blocks[1] = %+v, want text block", blocks[1])
		}
	})

	t.Run("ImagesOnly", func(t *testing.T) {
		var buf bytes.Buffer
		var b Backend
		images := []agent.ImageData{
			{MediaType: "image/jpeg", Data: "/9j/..."},
		}
		if err := b.WritePrompt(&buf, agent.Prompt{Images: images}, nil); err != nil {
			t.Fatal(err)
		}
		var msg struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &msg); err != nil {
			t.Fatal(err)
		}
		var blocks []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg.Message.Content, &blocks); err != nil {
			t.Fatalf("content should be array: %v", err)
		}
		// Only image block, no text block (prompt is empty).
		if len(blocks) != 1 {
			t.Fatalf("expected 1 block, got %d", len(blocks))
		}
		if blocks[0].Type != "image" {
			t.Errorf("block type = %q, want %q", blocks[0].Type, "image")
		}
	})
}
