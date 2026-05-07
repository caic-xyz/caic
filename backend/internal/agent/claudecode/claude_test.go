// Tests for the Claude Code backend, verifying prompt writing and message parsing.

package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	cc "github.com/maruel/genai/providers/claudecode"
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
		if !strings.Contains(buf.String(), `"content":[{"type":"text","text":"hello"}]`) {
			t.Errorf("unexpected output: %s", buf.String())
		}
	})

	t.Run("WithImages", func(t *testing.T) {
		var buf bytes.Buffer
		var b Backend
		images := []agent.ImageData{
			{MediaType: "image/png", Data: "iVBOR..."},
		}
		if err := b.WritePrompt(&buf, agent.Prompt{Text: "describe this", Images: images}, io.Discard); err != nil {
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
		if err := b.WritePrompt(&buf, agent.Prompt{Images: images}, io.Discard); err != nil {
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

// fakeConn is a test double for agent.Conn that records SendRaw calls and
// feeds pre-canned messages through ReadMessages.
type fakeConn struct {
	agent.Conn // embed to satisfy interface; unused methods will panic

	messages []agent.Message
	sent     [][]byte
}

func (f *fakeConn) SendRaw(data []byte) error {
	f.sent = append(f.sent, append([]byte(nil), data...))
	return nil
}

func (f *fakeConn) ReadMessages(_ io.Reader, msgCh chan<- agent.Message) error {
	for _, m := range f.messages {
		msgCh <- m
	}
	return nil
}

func TestEnvInjectorConn(t *testing.T) {
	t.Run("InjectsOnStrippedEnv", func(t *testing.T) {
		key := "sk-ant-container-key"
		inner := &fakeConn{
			// The relay emits caic_stripped_env after system/init.
			messages: []agent.Message{
				&agent.InitMessage{SessionID: "sess-1", Model: "opus"},
				&agent.StrippedEnvMessage{
					MessageType: "caic_stripped_env",
					Variables:   map[string]string{"ANTHROPIC_API_KEY": key},
				},
				&agent.TextMessage{Text: "hello"},
			},
		}
		c := &envInjectorConn{Conn: inner}

		msgCh := make(chan agent.Message, 10)
		_ = c.ReadMessages(nil, msgCh)
		close(msgCh)
		var forwarded []agent.Message
		for m := range msgCh {
			forwarded = append(forwarded, m)
		}

		// StrippedEnvMessage must not be forwarded.
		for _, m := range forwarded {
			if _, ok := m.(*agent.StrippedEnvMessage); ok {
				t.Error("StrippedEnvMessage was forwarded to consumer")
			}
		}

		// SendRaw must have been called once with the correct key.
		if len(inner.sent) != 1 {
			t.Fatalf("SendRaw called %d times, want 1", len(inner.sent))
		}
		var got cc.InputUpdateEnvVarsMsg
		if err := json.Unmarshal(bytes.TrimSpace(inner.sent[0]), &got); err != nil {
			t.Fatal(err)
		}
		if got.Variables["ANTHROPIC_API_KEY"] != key {
			t.Errorf("injected key = %q, want %q", got.Variables["ANTHROPIC_API_KEY"], key)
		}
	})

	t.Run("NoStrippedEnv", func(t *testing.T) {
		// When no StrippedEnvMessage arrives, no injection occurs.
		inner := &fakeConn{
			messages: []agent.Message{
				&agent.InitMessage{SessionID: "sess-2", Model: "sonnet"},
			},
		}
		c := &envInjectorConn{Conn: inner}
		msgCh := make(chan agent.Message, 10)
		_ = c.ReadMessages(nil, msgCh)
		close(msgCh)
		for range msgCh {
		}
		if len(inner.sent) != 0 {
			t.Errorf("SendRaw called %d times, want 0", len(inner.sent))
		}
	})
}
