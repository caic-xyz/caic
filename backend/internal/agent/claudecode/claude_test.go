// Tests Claude prompts, parsing, bounded automatic replies, and question recovery.

package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/task"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/genai/providers/claudecode"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestWritePrompt(t *testing.T) {
	t.Parallel()
	t.Run("TextOnly", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logBuf := &agenttest.LogSink{Version: agent.LogVersionV2}
		var b wireFormat
		if err := b.WritePrompt(&buf, agent.Prompt{Text: "hello"}, logBuf); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logBuf.String(), `"t":"agent"`) || !strings.Contains(logBuf.String(), `"msg":{"type":"user"`) {
			t.Errorf("log = %q, want canonical v2 agent record", logBuf.String())
		}
		if !strings.Contains(buf.String(), `"content":[{"type":"text","text":"hello"}]`) {
			t.Errorf("unexpected output: %s", buf.String())
		}
	})

	t.Run("WithImages", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		var b wireFormat
		images := []v3.ImageData{
			{MediaType: "image/png", Data: "iVBOR..."},
		}
		if err := b.WritePrompt(&buf, agent.Prompt{Text: "describe this", Images: images}, agent.DiscardLogSink{Version: agent.LogVersionV1}); err != nil {
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
		var b wireFormat
		images := []v3.ImageData{
			{MediaType: "image/jpeg", Data: "/9j/..."},
		}
		if err := b.WritePrompt(&buf, agent.Prompt{Images: images}, agent.DiscardLogSink{Version: agent.LogVersionV1}); err != nil {
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

func (f *fakeConn) SendPrompt(p agent.Prompt, data []byte) error {
	if data == nil {
		f.prompts = append(f.prompts, p)
		return nil
	}
	return f.SendRaw(data)
}

func (f *fakeConn) SendRaw(data []byte) error {
	f.sent = append(f.sent, append([]byte(nil), data...))
	return nil
}

func (f *fakeConn) ReadMessages(_ io.ReadCloser, msgCh chan<- agent.TimedMessage, intercept agent.MessageInterceptor) error {
	for _, m := range f.messages {
		if intercept != nil {
			data, handled, err := intercept.HandleMessage(m)
			if err != nil {
				return err
			}
			if len(data) > 0 {
				if err := f.SendRaw(data); err != nil {
					return err
				}
			}
			if handled {
				continue
			}
		}
		msgCh <- agent.TimedMessage{Message: m}
	}
	return nil
}

// TestWireFormatSkillAndModel tests the stateful ParseMessage behavior:
// Skill tool results stay suppressed, analytics reads stay at the invocation,
// only a successful main-agent Skill call loads a skill, and model-less usage
// records are attributed to the session's active model.
func TestWireFormatSkillAndModel(t *testing.T) {
	t.Parallel()
	const (
		mainUse      = `{"type":"assistant","parent_tool_use_id":null,"message":{"model":"m","content":[{"type":"tool_use","id":"sk_1","name":"Skill","input":{"skill":"code-quality"}}],"usage":{}}}`
		mainResult   = `{"type":"user","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"sk_1","content":"Launching skill: code-quality"}]}}`
		injection    = `{"type":"user","isSynthetic":true,"parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /home/user/.claude/skills/code-quality\n\n# Code Quality\n"}]}}`
		failedUse    = `{"type":"assistant","parent_tool_use_id":null,"message":{"model":"m","content":[{"type":"tool_use","id":"sk_2","name":"Skill","input":{"skill":"missing"}}],"usage":{}}}`
		failedResult = `{"type":"user","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"sk_2","content":"Unknown skill: missing","is_error":true}]}}`
		// A subagent's records carry delegation metadata; its tool result
		// still pairs with the inner Skill invocation.
		childUse       = `{"type":"assistant","parent_tool_use_id":"agent_1","message":{"model":"m","content":[{"type":"tool_use","id":"sk_3","name":"Skill","input":{"skill":"review"}}],"usage":{}}}`
		childResult    = `{"type":"user","parent_tool_use_id":"agent_1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"sk_3","content":"Launching skill: review"}]}}`
		childInjection = `{"type":"user","isSynthetic":true,"parent_tool_use_id":"agent_1","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /home/user/.claude/skills/review\n\n# Review\n"}]}}`
		otherResult    = `{"type":"user","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`
		turnResult     = `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"duration_api_ms":1,"num_turns":1,"result":"","session_id":"s","usage":{}}`
	)
	// parse feeds lines through one session and returns each line's messages.
	parse := func(t *testing.T, lines ...string) [][]agent.Message {
		b := newWireFormat()
		out := make([][]agent.Message, len(lines))
		for i, line := range lines {
			msgs, err := b.ParseMessage([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			out[i] = msgs
		}
		return out
	}
	// loads returns the skill loads of every line, in order.
	loads := func(perLine [][]agent.Message) []agent.SkillLoadMessage {
		var out []agent.SkillLoadMessage
		for _, msgs := range perLine {
			for _, m := range msgs {
				if l, ok := m.(*agent.SkillLoadMessage); ok {
					out = append(out, *l)
				}
			}
		}
		return out
	}

	t.Run("ReadsStayAtInvocation", func(t *testing.T) {
		t.Parallel()
		// The messages other than loads match the wire output at c8791f8d,
		// line by line, so usage rollups keep their counts and timestamps.
		perLine := parse(t, mainUse, mainResult, injection, failedUse, failedResult, childUse, childResult, childInjection, turnResult)
		want := [][]agent.Message{
			{&agent.SkillReadMessage{ToolUseID: "sk_1", Skill: "code-quality"}},
			nil,
			nil,
			{&agent.SkillReadMessage{ToolUseID: "sk_2", Skill: "missing"}},
			nil,
			{&agent.SkillReadMessage{Source: agent.MessageSource{DelegationToolUseID: "agent_1"}, ToolUseID: "sk_3", Skill: "review"}},
			nil,
			nil,
			nil, // The ResultMessage is compared by type below.
		}
		for i, msgs := range perLine {
			var got []agent.Message
			for _, m := range msgs {
				switch m.(type) {
				case *agent.SkillLoadMessage:
				case *agent.ResultMessage:
					if i != len(perLine)-1 {
						t.Errorf("line %d: unexpected result", i)
					}
				default:
					got = append(got, m)
				}
			}
			if !reflect.DeepEqual(got, want[i]) {
				t.Errorf("line %d = %#v, want %#v", i, got, want[i])
			}
		}
	})

	t.Run("SuccessfulCallLoadsInjectedPath", func(t *testing.T) {
		t.Parallel()
		perLine := parse(t, mainUse, mainResult, injection, otherResult)
		want := []agent.SkillLoadMessage{{Skill: "code-quality", Path: "/home/user/.claude/skills/code-quality/SKILL.md"}}
		if got := loads(perLine); !reflect.DeepEqual(got, want) {
			t.Fatalf("loads = %+v, want %+v", got, want)
		}
		if len(perLine[2]) != 1 {
			t.Errorf("injection msgs = %#v, want only the load", perLine[2])
		}
		if len(perLine[3]) != 1 {
			t.Fatalf("msgs = %d, want 1", len(perLine[3]))
		}
		if _, ok := perLine[3][0].(*agent.ToolResultMessage); !ok {
			t.Errorf("type = %T, want *agent.ToolResultMessage", perLine[3][0])
		}
	})

	t.Run("LoadWithoutInjectionHasNoPath", func(t *testing.T) {
		t.Parallel()
		perLine := parse(t, mainUse, mainResult, otherResult)
		want := []agent.SkillLoadMessage{{Skill: "code-quality"}}
		if got := loads(perLine); !reflect.DeepEqual(got, want) {
			t.Fatalf("loads = %+v, want %+v", got, want)
		}
		if _, ok := perLine[2][0].(*agent.SkillLoadMessage); !ok || len(perLine[2]) != 2 {
			t.Errorf("msgs = %#v, want the load before the tool result", perLine[2])
		}
	})

	t.Run("FailedCallLoadsNothing", func(t *testing.T) {
		t.Parallel()
		if got := loads(parse(t, failedUse, failedResult, otherResult)); len(got) != 0 {
			t.Errorf("loads = %+v, want none", got)
		}
	})

	t.Run("SubagentCallLoadsNothing", func(t *testing.T) {
		t.Parallel()
		perLine := parse(t, childUse, childResult, childInjection, otherResult)
		if got := loads(perLine); len(got) != 0 {
			t.Errorf("loads = %+v, want none", got)
		}
		if len(perLine[0]) != 1 {
			t.Fatalf("msgs = %#v, want the analytics read", perLine[0])
		}
		if read, ok := perLine[0][0].(*agent.SkillReadMessage); !ok || read.Skill != "review" {
			t.Errorf("msgs[0] = %#v, want the review read", perLine[0][0])
		}
	})

	t.Run("SubagentFileReadLoadsNothing", func(t *testing.T) {
		t.Parallel()
		read := func(parent string) string {
			return `{"type":"assistant","parent_tool_use_id":` + parent + `,"message":{"model":"m","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/home/user/.agents/skills/review/SKILL.md"}}],"usage":{}}}`
		}
		for _, line := range []struct {
			parent string
			loads  int
		}{{"null", 1}, {`"agent_1"`, 0}} {
			msgs := parse(t, read(line.parent))[0]
			var reads, got int
			for _, m := range msgs {
				switch m.(type) {
				case *agent.SkillReadMessage:
					reads++
				case *agent.SkillLoadMessage:
					got++
				}
			}
			if reads != 1 || got != line.loads {
				t.Errorf("parent %s: reads = %d, loads = %d, want 1 and %d", line.parent, reads, got, line.loads)
			}
		}
	})

	t.Run("SkillPendingUsesClearedAtResult", func(t *testing.T) {
		t.Parallel()
		b := newWireFormat()
		if _, err := b.ParseMessage([]byte(mainUse)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.ParseMessage([]byte(turnResult)); err != nil {
			t.Fatal(err)
		}
		if len(b.pendingSkillUses) != 0 {
			t.Errorf("pendingSkillUses = %v, want empty after turn result", b.pendingSkillUses)
		}
		// The result no longer suppresses an unrelated result with the same ID.
		msgs, err := b.ParseMessage([]byte(mainResult))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
	})

	t.Run("ModellessUsageAttributedToSessionModel", func(t *testing.T) {
		t.Parallel()
		var b wireFormat
		b.widgetTracker = newWidgetTracker()
		b.pendingSkillUses = make(map[agent.ToolKey]string)
		assistant := []byte(`{"type":"assistant","message":{"model":"claude-opus","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":5}}}`)
		msgs, err := b.ParseMessage(assistant)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if um, ok := m.(*agent.UsageMessage); ok {
				if um.ReportedModel != "claude-opus" {
					t.Errorf("assistant usage model = %q, want claude-opus", um.ReportedModel)
				}
				if um.ModelDerived {
					t.Errorf("assistant usage ModelDerived = true, want false (harness reported the model)")
				}
			}
		}
		delta := []byte(`{"type":"stream_event","event":{"type":"message_delta","delta":{},"usage":{"input_tokens":10,"output_tokens":5}}}`)
		msgs, err = b.ParseMessage(delta)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		um, ok := msgs[0].(*agent.UsageMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UsageMessage", msgs[0])
		}
		if um.ReportedModel != "claude-opus" {
			t.Errorf("delta usage model = %q, want claude-opus", um.ReportedModel)
		}
		if !um.ModelDerived {
			t.Errorf("delta usage ModelDerived = false, want true (model derived from session state)")
		}
	})

	t.Run("SkilllessSkillCallSuppressedWithResult", func(t *testing.T) {
		t.Parallel()
		// A Skill tool_use without a skill name is dropped by the wire layer,
		// but its ID is still tracked so the paired tool result stays
		// suppressed too.
		var b wireFormat
		b.widgetTracker = newWidgetTracker()
		b.pendingSkillUses = make(map[agent.ToolKey]string)
		toolUse := []byte(`{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"sk_9","name":"Skill","input":{}}],"usage":{}}}`)
		msgs, err := b.ParseMessage(toolUse)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if _, ok := m.(*agent.SkillReadMessage); ok {
				t.Errorf("skill-less Skill call should be dropped, got %T", m)
			}
		}
		result := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"sk_9"}]}}`)
		msgs, err = b.ParseMessage(result)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if _, ok := m.(*agent.ToolResultMessage); ok {
				t.Errorf("paired Skill tool_result should be suppressed, got %T", m)
			}
		}
	})
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
		got := New().ModelInventory().IDs()
		want := []string{"fable", "opus", "sonnet", "haiku"}
		if !slices.Equal(got, want) {
			t.Fatalf("ModelInventory().IDs() = %v, want %v", got, want)
		}
	})

	// Backend is registered once per process (backends.Default) and shared by
	// every concurrent Claude Code task. It must not double as a WireFormat:
	// Start/AttachRelay have to build a fresh per-session wireFormat instead of
	// reusing the shared Backend, or reasoning-token accounting and widget
	// tracking would race and leak between unrelated concurrent tasks.
	t.Run("BackendIsNotAWireFormat", func(t *testing.T) {
		t.Parallel()
		if _, ok := agent.Backend(New()).(agent.WireFormat); ok {
			t.Fatal("Backend must not implement WireFormat")
		}
	})

	t.Run("SessionsFromSameBackendDoNotShareState", func(t *testing.T) {
		t.Parallel()
		b := New()
		w1 := b.NewWire()
		w2 := b.NewWire()
		// Feed a thinking-tokens estimate into w1's session only.
		if _, err := w1.ParseMessage([]byte(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":50,"estimated_tokens_delta":50,"uuid":"u1","session_id":"s1"}`)); err != nil {
			t.Fatal(err)
		}
		resultLine := []byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"num_turns":1,"result":"done","total_cost_usd":0,"usage":{"input_tokens":1,"output_tokens":1}}`)
		msgs, err := w2.ParseMessage(resultLine)
		if err != nil {
			t.Fatal(err)
		}
		m, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ResultMessage", msgs[0])
		}
		if m.Usage.ReasoningOutputTokens != 0 {
			t.Errorf("w2 leaked w1's pending reasoning estimate: ReasoningOutputTokens = %d, want 0", m.Usage.ReasoningOutputTokens)
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
	t.Run("ReadMessages/AutomaticResponseWhileInputBlocked", func(t *testing.T) {
		t.Parallel()
		stdin := agenttest.NewStdinPipe()
		stdout, output := io.Pipe()
		c := &controlConn{Conn: agent.NewConn(t.Context(), slog.New(slog.DiscardHandler), stdin, agent.DiscardLogSink{Version: agent.LogVersionV3}, newWireFormat())}
		var workers sync.WaitGroup
		t.Cleanup(func() { c.Cancel(); _ = output.Close(); _ = stdin.Reader.Close(); workers.Wait() })
		sent := make(chan error, 1)
		workers.Go(func() { sent <- c.SendPrompt(agent.Prompt{Text: "prompt"}, nil) })
		<-stdin.Started
		messages := make(chan agent.TimedMessage, 8)
		read := make(chan error, 1)
		workers.Go(func() { read <- c.ReadMessages(stdout, messages, c) })
		workers.Go(func() {
			request := `{"t":"agent","ts":1.000,"msg":{"type":"control_request","request_id":"allow","request":{"subtype":"can_use_tool","tool_name":"Read","input":{}}}}` + "\n"
			var requestSb563 strings.Builder
			for _, text := range []string{"first", "second", "third"} {
				requestSb563.WriteString(fmt.Sprintf(`{"t":"agent","ts":2.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}}`, text) + "\n")
			}
			request += requestSb563.String()
			if _, err := io.WriteString(output, request); err != nil {
				t.Errorf("feed output: %v", err)
			}
		})
		for range 3 {
			select {
			case <-messages:
			case err := <-read:
				t.Fatalf("reader ended before output: %v", err)
			case <-time.After(time.Second):
				t.Fatal("stdout stalled behind automatic permission response")
			}
		}
		input := bufio.NewReader(stdin.Reader)
		if _, err := input.ReadBytes('\n'); err != nil {
			t.Fatal(err)
		}
		response, err := input.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeControlResponse(t, response); got.Response.RequestID != "allow" {
			t.Fatalf("response = %#v", got)
		}
		if err := <-sent; err != nil {
			t.Fatal(err)
		}
		if err := output.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-read; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ReadMessages/PermissionReplyWhileMCPAcceptedElsewhere", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		release := make(chan struct{})
		registry := &agenttest.BlockingRegistry{Started: make(chan struct{}, 1), Release: release}
		executor := agent.NewMCPExecutor(t.Context())
		stdin := agenttest.NewStdinPipe()
		stdout, output := io.Pipe()
		c := &controlConn{Conn: agent.NewMCPConn(ctx, slog.New(slog.DiscardHandler), stdin, agent.DiscardLogSink{Version: agent.LogVersionV3}, newWireFormat(), executor.Wrap(registry))}
		var workers sync.WaitGroup
		t.Cleanup(func() {
			cancel()
			c.Cancel()
			_ = output.Close()
			_ = stdin.Reader.Close()
			close(release)
			executor.Close()
			workers.Wait()
		})
		read := make(chan error, 1)
		workers.Go(func() { read <- c.ReadMessages(stdout, make(chan agent.TimedMessage, 1), c) })
		if _, err := io.WriteString(output, `{"t":"mcp_request","id":"slow","method":"tools/call","name":"echo","arguments":{}}`+"\n"); err != nil {
			t.Fatal(err)
		}
		<-registry.Started
		if _, err := io.WriteString(output, `{"t":"agent","ts":1.000,"msg":{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"Read","input":{}}}}`+"\n"); err != nil {
			t.Fatal(err)
		}
		reply := make(chan []byte, 1)
		workers.Go(func() {
			line, err := bufio.NewReader(stdin.Reader).ReadBytes('\n')
			if err != nil {
				t.Errorf("read permission reply: %v", err)
			}
			reply <- line
		})
		select {
		case data := <-reply:
			if got := decodeControlResponse(t, data); got.Response.RequestID != "permission" {
				t.Fatalf("reply=%#v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("MCP work blocked automatic permission reply")
		}
		cancel()
		select {
		case err := <-read:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("stream cancellation=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("source shutdown waited for accepted work elsewhere")
		}
	})

	t.Run("ReadMessages/error_response_lifecycle", func(t *testing.T) {
		t.Parallel()
		for _, end := range []string{"cancel", "interrupt", "eof", "overload_count", "overload_bytes", "write_failure", "blocked_dispatch"} {
			t.Run(end, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithCancel(t.Context())
				stdin := agenttest.NewStdinPipe()
				stdout, output := io.Pipe()
				c := &controlConn{Conn: agent.NewConn(ctx, slog.New(slog.DiscardHandler), stdin, agent.DiscardLogSink{Version: agent.LogVersionV3}, newWireFormat())}
				var workers sync.WaitGroup
				t.Cleanup(func() { cancel(); c.Cancel(); _ = output.Close(); _ = stdin.Reader.Close(); workers.Wait() })
				sent := make(chan error, 1)
				blockedPrompt := end != "write_failure" && end != "blocked_dispatch"
				if blockedPrompt {
					workers.Go(func() { sent <- c.SendPrompt(agent.Prompt{Text: "prompt"}, nil) })
					<-stdin.Started
				}
				messages := make(chan agent.TimedMessage, 1)
				if end == "blocked_dispatch" {
					messages = make(chan agent.TimedMessage)
				}
				read := make(chan error, 1)
				workers.Go(func() { read <- c.ReadMessages(stdout, messages, c) })
				input := `{"t":"agent","ts":1.000,"msg":{"type":"control_request","request_id":"allow","request":{"subtype":"can_use_tool","tool_name":"Read","input":{}}}}` + "\n"
				if end == "overload_count" {
					input = strings.Repeat(input, 100)
				}
				if end == "overload_bytes" {
					input = `{"t":"agent","ts":1.000,"msg":{"type":"control_request","request_id":"allow","request":{"subtype":"can_use_tool","tool_name":"Read","input":{"large":"` + strings.Repeat("x", 5<<20) + `"}}}}` + "\n"
				}
				input += `{"t":"agent","ts":2.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"queued"}]}}}` + "\n"
				workers.Go(func() {
					if _, err := io.WriteString(output, input); err != nil && !errors.Is(err, io.ErrClosedPipe) {
						t.Errorf("feed output: %v", err)
					}
				})
				switch end {
				case "write_failure", "blocked_dispatch":
					<-stdin.Started
					if end == "write_failure" {
						_ = stdin.Reader.CloseWithError(io.ErrUnexpectedEOF)
					} else {
						cancel()
					}
				case "cancel", "interrupt", "eof":
					select {
					case <-messages:
					case err := <-read:
						t.Fatalf("reader ended before queued reply: %v", err)
					case <-time.After(time.Second):
						t.Fatal("reader blocked behind pending reply")
					}
					switch end {
					case "cancel":
						cancel()
					case "interrupt":
						c.Cancel()
					case "eof":
						if err := output.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
				err := <-read
				if err == nil {
					t.Fatal("response failure was not surfaced")
				}
				if strings.HasPrefix(end, "overload") && !errors.Is(err, agent.ErrResponseQueueFull) {
					t.Fatalf("queue error = %v", err)
				}
				if end == "write_failure" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("write error = %v", err)
				}
				if (end == "cancel" || end == "blocked_dispatch") && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error = %v", err)
				}
				if !strings.HasPrefix(end, "overload") && !errors.Is(err, agent.ErrResponsesAborted) {
					t.Fatalf("aborted reply error = %v", err)
				}
				if blockedPrompt {
					select {
					case err := <-sent:
						if err == nil {
							t.Fatal("blocked prompt succeeded after transport closed")
						}
					case <-time.After(time.Second):
						t.Fatal("stdin was not interrupted before joining")
					}
				}
			})
		}
	})
	t.Run("ReconnectAfterAcceptedAnswer", func(t *testing.T) {
		t.Parallel()
		tk, err := task.NewTask(ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		tk.SeedTimeline([]agent.Message{
			&agent.AskMessage{ToolUseID: "ask", Questions: []v3.AskQuestion{{Question: "Which?"}}},
			&agent.PendingUserActionMessage{Action: v3.PendingUserAction{Kind: agent.PendingUserActionAskUserQuestion, RequestID: "request", ToolUseID: "ask", Ask: v3.PendingAskAction{Questions: []v3.AskQuestion{{Question: "Which?"}}}}},
			agent.InputDeliveryStarted(), &agent.UserInputMessage{Text: "answer"}, agent.InputDeliveryCompleted(),
		})
		inner := &fakeConn{}
		c := &controlConn{Conn: inner}
		if err := c.restorePendingActions(tk.PendingUserActions()); err != nil {
			t.Fatal(err)
		}
		if err := c.SendPrompt(agent.Prompt{Text: "next turn"}, nil); err != nil {
			t.Fatal(err)
		}
		if len(inner.sent) != 0 || len(inner.prompts) != 1 || inner.prompts[0].Text != "next turn" {
			t.Fatalf("reconnected routing: controls=%q prompts=%#v", inner.sent, inner.prompts)
		}
	})

	t.Run("SendPrompt/CancellationWhileInputBlocked", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		writer := &agenttest.GatedWriter{Started: make(chan struct{}), Release: release}
		log := slog.New(slog.DiscardHandler)
		c := &controlConn{Conn: agent.NewConn(t.Context(), log, writer, agent.DiscardLogSink{Version: agent.LogVersionV3}, newWireFormat())}
		if err := c.setPendingAsk(pendingControlAsk{requestID: "first", questions: []v3.AskQuestion{{Question: "Choice?"}}}); err != nil {
			t.Fatal(err)
		}
		var workers sync.WaitGroup
		var writeErr error
		t.Cleanup(func() {
			close(release)
			workers.Wait()
			if writeErr != nil {
				t.Errorf("input failed after release: %v", writeErr)
			}
			if c.pendingAsk == nil || c.pendingAsk.requestID != "replacement" {
				t.Errorf("completed answer discarded the replacement Ask: %#v", c.pendingAsk)
			}
		})
		written := make(chan error, 1)
		workers.Go(func() {
			writeErr = c.SendPrompt(agent.Prompt{Text: "answer"}, nil)
			written <- writeErr
		})
		<-writer.Started
		parsed := make(chan error, 1)
		workers.Go(func() {
			_, _, err := c.HandleMessage(&agent.RawMessage{MessageType: "control_cancel_request", Raw: []byte(`{"type":"control_cancel_request","request_id":"first"}`)})
			if err == nil {
				err = c.setPendingAsk(pendingControlAsk{requestID: "replacement", questions: []v3.AskQuestion{{Question: "Next?"}}})
			}
			parsed <- err
		})
		select {
		case err := <-parsed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("stdout cancellation blocked behind answer delivery")
		}
		select {
		case err := <-written:
			t.Fatalf("input unexpectedly finished: %v", err)
		default:
		}
	})
	t.Run("AnswersPendingAskUserQuestion", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{
			messages: []agent.Message{
				&agent.TextMessage{Text: "I need one choice."},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: v3.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: v3.PendingAskAction{
							Questions: []v3.AskQuestion{{
								Question: "Which login boundary should Google use in caic?",
								Header:   "Login",
								Options: []v3.AskOption{
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

		msgCh := make(chan agent.TimedMessage, 10)
		if err := c.ReadMessages(io.NopCloser(strings.NewReader("")), msgCh, c); err != nil {
			t.Fatal(err)
		}
		close(msgCh)
		var forwarded []agent.Message
		for m := range msgCh {
			forwarded = append(forwarded, m.Message)
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

		if err := c.SendPrompt(agent.Prompt{Text: "Identity only"}, nil); err != nil {
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

		log := &agenttest.LogSink{Version: agent.LogVersionV3}
		if err := agent.AppendInputNativeRecord(log, log.LogVersion(), inner.sent[0]); err != nil {
			t.Fatal(err)
		}
		var payload []byte
		parser, err := agent.NewLogRecordParser(agent.LogVersionV3, func(data []byte) ([]agent.Message, error) {
			payload = bytes.Clone(data)
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		record, err := parser.ParseRecord(bytes.TrimSpace(log.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if record.RelayRecord || !bytes.Equal(payload, bytes.TrimSpace(inner.sent[0])) {
			t.Fatalf("AskUserQuestion input provenance = %#v, payload = %s", record, payload)
		}
		replayed, err := newWireFormat().ParseMessage(payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(replayed) != 1 {
			t.Fatalf("replayed messages = %d, want one user input", len(replayed))
		}
		input, ok := replayed[0].(*agent.UserInputMessage)
		if !ok || input.Text != "Identity only" {
			t.Fatalf("replayed answer = %#v", replayed[0])
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

		msgCh := make(chan agent.TimedMessage, 10)
		if err := c.ReadMessages(io.NopCloser(strings.NewReader("")), msgCh, c); err != nil {
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
		replayed, err := newWireFormat().ParseMessage(inner.sent[0])
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range replayed {
			if _, ok := message.(*agent.UserInputMessage); ok {
				t.Fatal("automatic permission response became a user prompt")
			}
		}
	})

	t.Run("AnswersRestoredAskUserQuestion", func(t *testing.T) {
		t.Parallel()
		inner := &fakeConn{}
		c := &controlConn{Conn: inner}
		pending := v3.PendingUserAction{
			Kind:      agent.PendingUserActionAskUserQuestion,
			RequestID: "req-restored",
			ToolUseID: "toolu-restored",
			Ask: v3.PendingAskAction{
				Questions: []v3.AskQuestion{{
					Question: "Which login boundary should Google use in caic?",
					Header:   "Login",
					Options: []v3.AskOption{
						{Label: "Identity only"},
						{Label: "Forge-coupled"},
					},
				}},
			},
		}
		if err := c.restorePendingActions([]v3.PendingUserAction{pending}); err != nil {
			t.Fatal(err)
		}

		if err := c.SendPrompt(agent.Prompt{Text: "Forge-coupled"}, nil); err != nil {
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
			Action: v3.PendingUserAction{
				Kind:      agent.PendingUserActionAskUserQuestion,
				RequestID: "req-restored",
				ToolUseID: "toolu-restored",
				Ask: v3.PendingAskAction{
					Questions: []v3.AskQuestion{{Question: "Which?"}},
				},
			},
		}
		if err := c.restorePendingActions([]v3.PendingUserAction{pending.Action}); err != nil {
			t.Fatal(err)
		}
		_, handled, err := c.HandleMessage(pending)
		if err != nil {
			t.Fatal(err)
		}
		if handled {
			t.Fatal("duplicate pending ask was handled, want forwarded")
		}

		if err := c.SendPrompt(agent.Prompt{Text: "answer"}, nil); err != nil {
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
		pending := v3.PendingUserAction{
			Kind:      agent.PendingUserActionAskUserQuestion,
			RequestID: "req-restored",
			ToolUseID: "toolu-restored",
			Ask: v3.PendingAskAction{
				Questions: []v3.AskQuestion{{Question: "Which?"}},
			},
		}
		if err := c.restorePendingActions([]v3.PendingUserAction{pending, pending}); err != nil {
			t.Fatal(err)
		}

		if err := c.SendPrompt(agent.Prompt{Text: "answer"}, nil); err != nil {
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
		actions := []v3.PendingUserAction{
			{
				Kind:      agent.PendingUserActionAskUserQuestion,
				RequestID: "req-1",
				ToolUseID: "toolu-1",
				Ask: v3.PendingAskAction{
					Questions: []v3.AskQuestion{{Question: "First?"}},
				},
			},
			{
				Kind:      agent.PendingUserActionAskUserQuestion,
				RequestID: "req-2",
				ToolUseID: "toolu-2",
				Ask: v3.PendingAskAction{
					Questions: []v3.AskQuestion{{Question: "Second?"}},
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
		action := v3.PendingUserAction{
			Kind:      v3.PendingUserActionKind("future_action"),
			RequestID: "req-future",
			ToolUseID: "toolu-future",
		}
		if err := c.restorePendingActions([]v3.PendingUserAction{action}); err == nil {
			t.Fatal("restorePendingActions returned nil error")
		} else if !strings.Contains(err.Error(), "unsupported pending user action kind") {
			t.Fatalf("err = %v, want unsupported pending user action kind", err)
		}

		_, handled, err := c.HandleMessage(&agent.PendingUserActionMessage{
			MessageType: agent.PendingUserActionMessageType,
			Action:      action,
		})
		if err == nil {
			t.Fatal("HandleMessage returned nil error")
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

		if err := c.SendPrompt(agent.Prompt{Text: "next turn"}, nil); err != nil {
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
		if err := c.SendPrompt(agent.Prompt{Text: "hello"}, nil); err != nil {
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

func TestReplayBufferOwnership(t *testing.T) {
	t.Parallel()
	wire := New().NewWire()
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}}`)
	msgs, err := wire.ParseMessage(line)
	if err != nil {
		t.Fatal(err)
	}
	for i := range line {
		line[i] = 'x'
	}
	msg, ok := msgs[0].(*agent.TextDeltaMessage)
	if !ok || msg.Text != "hello" {
		t.Fatalf("message = %#v", msgs)
	}
}
