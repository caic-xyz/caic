// Tests for the Claude Code backend, verifying prompt writing and message parsing.

package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/maruel/genai/providers/claudecode"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestWritePrompt(t *testing.T) {
	t.Parallel()
	t.Run("TextOnly", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
	prompts  []agent.Prompt
	sent     [][]byte
}

func (f *fakeConn) SendPrompt(p agent.Prompt) error {
	f.prompts = append(f.prompts, p)
	return nil
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

// TestHasOAuth uses t.Setenv to point HOME at a temp dir and therefore cannot
// run in parallel.
func TestHasOAuth(t *testing.T) {
	writeClaudeJSON := func(t *testing.T, home, contents string) {
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".claude", "claude.json"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("present", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeClaudeJSON(t, home, `{"oauthAccount":{"emailAddress":"x@y.z"}}`)
		if !hasOAuth() {
			t.Error("hasOAuth() = false, want true when oauthAccount is present")
		}
	})
	t.Run("absent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeClaudeJSON(t, home, `{"numStartups":3}`)
		if hasOAuth() {
			t.Error("hasOAuth() = true, want false when oauthAccount is missing")
		}
	})
	t.Run("noFile", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if hasOAuth() {
			t.Error("hasOAuth() = true, want false when claude.json is absent")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeClaudeJSON(t, home, `not json`)
		if hasOAuth() {
			t.Error("hasOAuth() = true, want false when claude.json is malformed")
		}
	})
}

func TestBackend(t *testing.T) {
	t.Parallel()
	t.Run("AgentArgsUsesDeterministicPermissionMode", func(t *testing.T) {
		t.Parallel()
		got := (&Backend{}).AgentArgs(agent.HarnessArgs{})
		if !containsArgPair(got, "--permission-prompt-tool", "stdio") {
			t.Fatalf("AgentArgs = %v, want --permission-prompt-tool stdio", got)
		}
		if !containsArgPair(got, "--permission-mode", "acceptEdits") {
			t.Fatalf("AgentArgs = %v, want --permission-mode acceptEdits", got)
		}
		if slices.Contains(got, "--dangerously-skip-permissions") {
			t.Fatalf("AgentArgs = %v, want no --dangerously-skip-permissions", got)
		}
		if containsArgPair(got, "--permission-mode", "auto") {
			t.Fatalf("AgentArgs = %v, want explicit non-auto permission mode", got)
		}
		if containsArgPair(got, "--permission-mode", "default") {
			t.Fatalf("AgentArgs = %v, want deterministic permission mode", got)
		}
	})

	t.Run("NewExposesClaudeModelAliases", func(t *testing.T) {
		t.Parallel()
		got := New().Models()
		want := []string{"opus", "sonnet", "haiku", "fable"}
		if !slices.Equal(got, want) {
			t.Fatalf("Models = %v, want %v", got, want)
		}
	})
}

func containsArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestControlConn(t *testing.T) {
	t.Parallel()
	t.Run("AnswersPendingAskUserQuestion", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{
			messages: []agent.Message{
				&agent.TextMessage{Text: "I need one choice."},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: agent.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: agent.PendingAskAction{
							Questions: []agent.AskQuestion{{
								Question: "Which login boundary should Google use in caic?",
								Header:   "Login",
								Options: []agent.AskOption{
									{Label: "Identity only"},
									{Label: "Forge-coupled"},
								},
							}},
						},
					},
				},
			},
		}
		c := &controlConn{Conn: inner}

		msgCh := make(chan agent.Message, 10)
		if err := c.ReadMessages(nil, msgCh); err != nil {
			t.Fatal(err)
		}
		close(msgCh)
		var forwarded []agent.Message
		for m := range msgCh {
			forwarded = append(forwarded, m)
		}
		if len(forwarded) != 2 {
			t.Fatalf("forwarded len = %d, want 2", len(forwarded))
		}
		if _, ok := forwarded[0].(*agent.TextMessage); !ok {
			t.Fatalf("forwarded[0] = %T, want *agent.TextMessage", forwarded[0])
		}
		if _, ok := forwarded[1].(*agent.PendingUserActionMessage); !ok {
			t.Fatalf("forwarded[1] = %T, want *agent.PendingUserActionMessage", forwarded[1])
		}

		if err := c.SendPrompt(agent.Prompt{Text: "Identity only"}); err != nil {
			t.Fatal(err)
		}
		if len(inner.prompts) != 0 {
			t.Fatalf("delegated prompts = %d, want 0", len(inner.prompts))
		}
		if len(inner.sent) != 1 {
			t.Fatalf("SendRaw calls = %d, want 1", len(inner.sent))
		}
		got := decodeControlResponse(t, inner.sent[0])
		if got.Type != claudecode.InputControlResponse {
			t.Errorf("Type = %q, want %q", got.Type, claudecode.InputControlResponse)
		}
		if got.Response.Subtype != claudecode.ControlResponseSuccess {
			t.Errorf("Subtype = %q, want %q", got.Response.Subtype, claudecode.ControlResponseSuccess)
		}
		if got.Response.RequestID != "req-1" {
			t.Errorf("RequestID = %q, want req-1", got.Response.RequestID)
		}
		if got.Response.Response.Behavior != claudecode.ControlCanUseToolBehaviorAllow {
			t.Errorf("behavior = %q, want %q", got.Response.Response.Behavior, claudecode.ControlCanUseToolBehaviorAllow)
		}
		var updated claudecode.AskUserQuestionUpdatedInput
		if err := json.Unmarshal(got.Response.Response.UpdatedInput, &updated); err != nil {
			t.Fatal(err)
		}
		if len(updated.Questions) != 1 {
			t.Fatalf("updated questions len = %d, want 1", len(updated.Questions))
		}
		const question = "Which login boundary should Google use in caic?"
		if updated.Questions[0].Question != question {
			t.Errorf("question = %q, want %q", updated.Questions[0].Question, question)
		}
		if updated.Answers[question] != "Identity only" {
			t.Errorf("answer = %q, want Identity only", updated.Answers[question])
		}
	})

	t.Run("AutoAllowsOtherCanUseTool", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{
			messages: []agent.Message{
				&agent.RawMessage{
					MessageType: "control_request",
					Raw:         []byte(`{"type":"control_request","request_id":"req-2","request":{"subtype":"can_use_tool","tool_name":"Read","input":{"file_path":"README.md"},"tool_use_id":"toolu-2"}}`),
				},
			},
		}
		c := &controlConn{Conn: inner}

		msgCh := make(chan agent.Message, 10)
		if err := c.ReadMessages(nil, msgCh); err != nil {
			t.Fatal(err)
		}
		close(msgCh)
		for m := range msgCh {
			t.Fatalf("forwarded %T, want no forwarded messages", m)
		}
		if len(inner.sent) != 1 {
			t.Fatalf("SendRaw calls = %d, want 1", len(inner.sent))
		}
		got := decodeControlResponse(t, inner.sent[0])
		if got.Response.RequestID != "req-2" {
			t.Errorf("RequestID = %q, want req-2", got.Response.RequestID)
		}
		if got.Response.Response.Behavior != claudecode.ControlCanUseToolBehaviorAllow {
			t.Errorf("behavior = %q, want %q", got.Response.Response.Behavior, claudecode.ControlCanUseToolBehaviorAllow)
		}
		var updated claudecode.ReadInput
		if err := json.Unmarshal(got.Response.Response.UpdatedInput, &updated); err != nil {
			t.Fatal(err)
		}
		if updated.FilePath != "README.md" {
			t.Errorf("updatedInput file_path = %q, want README.md", updated.FilePath)
		}
	})

	t.Run("AnswersRestoredAskUserQuestion", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{}
		c := &controlConn{Conn: inner}
		pending := agent.PendingUserAction{
			Kind:      agent.PendingUserActionAskUserQuestion,
			RequestID: "req-restored",
			ToolUseID: "toolu-restored",
			Ask: agent.PendingAskAction{
				Questions: []agent.AskQuestion{{
					Question: "Which login boundary should Google use in caic?",
					Header:   "Login",
					Options: []agent.AskOption{
						{Label: "Identity only"},
						{Label: "Forge-coupled"},
					},
				}},
			},
		}
		if err := c.restorePendingActions([]agent.PendingUserAction{pending}); err != nil {
			t.Fatal(err)
		}

		if err := c.SendPrompt(agent.Prompt{Text: "Forge-coupled"}); err != nil {
			t.Fatal(err)
		}
		if len(inner.prompts) != 0 {
			t.Fatalf("delegated prompts = %d, want 0", len(inner.prompts))
		}
		if len(inner.sent) != 1 {
			t.Fatalf("SendRaw calls = %d, want 1", len(inner.sent))
		}
		got := decodeControlResponse(t, inner.sent[0])
		if got.Response.RequestID != "req-restored" {
			t.Errorf("RequestID = %q, want req-restored", got.Response.RequestID)
		}
		var updated claudecode.AskUserQuestionUpdatedInput
		if err := json.Unmarshal(got.Response.Response.UpdatedInput, &updated); err != nil {
			t.Fatal(err)
		}
		const question = "Which login boundary should Google use in caic?"
		if updated.Answers[question] != "Forge-coupled" {
			t.Errorf("answer = %q, want Forge-coupled", updated.Answers[question])
		}
	})

	t.Run("DuplicateRestoredAskIsIdempotent", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{}
		c := &controlConn{Conn: inner}
		pending := &agent.PendingUserActionMessage{
			MessageType: agent.PendingUserActionMessageType,
			Action: agent.PendingUserAction{
				Kind:      agent.PendingUserActionAskUserQuestion,
				RequestID: "req-restored",
				ToolUseID: "toolu-restored",
				Ask: agent.PendingAskAction{
					Questions: []agent.AskQuestion{{Question: "Which?"}},
				},
			},
		}
		if err := c.restorePendingActions([]agent.PendingUserAction{pending.Action}); err != nil {
			t.Fatal(err)
		}
		handled, err := c.handleControlMessage(pending)
		if err != nil {
			t.Fatal(err)
		}
		if handled {
			t.Fatal("duplicate pending ask was handled, want forwarded")
		}

		if err := c.SendPrompt(agent.Prompt{Text: "answer"}); err != nil {
			t.Fatal(err)
		}
		if len(inner.sent) != 1 {
			t.Fatalf("SendRaw calls = %d, want 1", len(inner.sent))
		}
	})

	t.Run("DuplicateRestoredAskUserQuestionIsIgnored", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{}
		c := &controlConn{Conn: inner}
		pending := agent.PendingUserAction{
			Kind:      agent.PendingUserActionAskUserQuestion,
			RequestID: "req-restored",
			ToolUseID: "toolu-restored",
			Ask: agent.PendingAskAction{
				Questions: []agent.AskQuestion{{Question: "Which?"}},
			},
		}
		if err := c.restorePendingActions([]agent.PendingUserAction{pending, pending}); err != nil {
			t.Fatal(err)
		}

		if err := c.SendPrompt(agent.Prompt{Text: "answer"}); err != nil {
			t.Fatal(err)
		}
		if len(inner.prompts) != 0 {
			t.Fatalf("delegated prompts = %d, want 0", len(inner.prompts))
		}
		if len(inner.sent) != 1 {
			t.Fatalf("SendRaw calls = %d, want 1", len(inner.sent))
		}
	})

	t.Run("RejectsMultipleRestoredAskUserQuestions", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{}
		c := &controlConn{Conn: inner}
		actions := []agent.PendingUserAction{
			{
				Kind:      agent.PendingUserActionAskUserQuestion,
				RequestID: "req-1",
				ToolUseID: "toolu-1",
				Ask: agent.PendingAskAction{
					Questions: []agent.AskQuestion{{Question: "First?"}},
				},
			},
			{
				Kind:      agent.PendingUserActionAskUserQuestion,
				RequestID: "req-2",
				ToolUseID: "toolu-2",
				Ask: agent.PendingAskAction{
					Questions: []agent.AskQuestion{{Question: "Second?"}},
				},
			},
		}
		err := c.restorePendingActions(actions)
		if err == nil {
			t.Fatal("restorePendingActions returned nil error")
		}
		if !strings.Contains(err.Error(), "multiple pending AskUserQuestion") {
			t.Fatalf("err = %v, want multiple pending AskUserQuestion", err)
		}
	})

	t.Run("RejectsUnknownPendingAction", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{}
		c := &controlConn{Conn: inner}
		action := agent.PendingUserAction{
			Kind:      agent.PendingUserActionKind("future_action"),
			RequestID: "req-future",
			ToolUseID: "toolu-future",
		}
		if err := c.restorePendingActions([]agent.PendingUserAction{action}); err == nil {
			t.Fatal("restorePendingActions returned nil error")
		} else if !strings.Contains(err.Error(), "unsupported pending user action kind") {
			t.Fatalf("err = %v, want unsupported pending user action kind", err)
		}

		handled, err := c.handleControlMessage(&agent.PendingUserActionMessage{
			MessageType: agent.PendingUserActionMessageType,
			Action:      action,
		})
		if err == nil {
			t.Fatal("handleControlMessage returned nil error")
		}
		if !strings.Contains(err.Error(), "unsupported pending user action kind") {
			t.Fatalf("err = %v, want unsupported pending user action kind", err)
		}
		if handled {
			t.Fatal("handled = true, want false")
		}
	})

	t.Run("RestoreWithoutPendingAskDelegates", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{}
		c := &controlConn{Conn: inner}
		if err := c.restorePendingActions(nil); err != nil {
			t.Fatal(err)
		}

		if err := c.SendPrompt(agent.Prompt{Text: "next turn"}); err != nil {
			t.Fatal(err)
		}
		if len(inner.prompts) != 1 {
			t.Fatalf("delegated prompts = %d, want 1", len(inner.prompts))
		}
		if len(inner.sent) != 0 {
			t.Fatalf("SendRaw calls = %d, want 0", len(inner.sent))
		}
	})

	t.Run("DelegatesWithoutPendingAsk", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{}
		c := &controlConn{Conn: inner}
		if err := c.SendPrompt(agent.Prompt{Text: "hello"}); err != nil {
			t.Fatal(err)
		}
		if len(inner.prompts) != 1 {
			t.Fatalf("delegated prompts = %d, want 1", len(inner.prompts))
		}
		if inner.prompts[0].Text != "hello" {
			t.Errorf("prompt = %q, want hello", inner.prompts[0].Text)
		}
		if len(inner.sent) != 0 {
			t.Fatalf("SendRaw calls = %d, want 0", len(inner.sent))
		}
	})
}

func decodeControlResponse(t *testing.T, data []byte) claudecode.InputControlResponseMsg {
	var got claudecode.InputControlResponseMsg
	if err := json.Unmarshal(bytes.TrimSpace(data), &got); err != nil {
		t.Fatal(err)
	}
	return got
}
