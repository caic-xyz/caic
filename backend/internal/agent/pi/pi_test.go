// Tests Pi discovery, startup recovery, wire configuration, and automatic reply delivery.

package pi

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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	genaipi "github.com/maruel/genai/providers/pi"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/maruel/gomode/mcp/mcptest"
)

const piSSHHelperEnv = "GO_WANT_PI_SSH_HELPER"

func init() {
	if os.Getenv(piSSHHelperEnv) != "1" {
		return
	}
	runPiSSHHelper()
	os.Exit(0)
}

func runPiSSHHelper() {
	args := os.Args
	for i, arg := range args {
		if arg == "task" {
			args = args[i:]
			break
		}
	}

	switch {
	case slices.Equal(args, []string{"task", "pi", "--version"}):
		//nolint:gosec // The test-only helper receives its state directory through the environment.
		if err := os.WriteFile(filepath.Join(os.Getenv("PI_SSH_HELPER_DIR"), "version-called"), nil, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case slices.Equal(args, []string{"task", "pi", "update", "--all"}):
		//nolint:gosec // The test-only helper receives its state directory through the environment.
		if err := os.WriteFile(filepath.Join(os.Getenv("PI_SSH_HELPER_DIR"), "updated"), nil, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Pi and extensions updated")
	case slices.Contains(args, "'serve-attach'"):
		runPiRelayHelper()
	}
}

func runPiRelayHelper() {
	dir := os.Getenv("PI_SSH_HELPER_DIR")
	path := filepath.Join(dir, "relay-count")
	count, err := os.ReadFile(path) //nolint:gosec // The path is constructed by the test-only helper.
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(path, append(count, '1'), 0o600); err != nil { //nolint:gosec // The path is constructed by the test-only helper.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(dir, "relay-args"), []byte(strings.Join(os.Args, " ")), 0o600); err != nil { //nolint:gosec // test-controlled helper directory
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	failedStartup := len(count) == 0
	fmt.Printf(`{"t":"relay_generation","generation":"generation-%d"}`+"\n", len(count)+1)
	if failedStartup {
		fmt.Println(`{"t":"exit","exit_code":1,"error":"extension load failure"}`)
	}

	s := bufio.NewScanner(os.Stdin)
	for s.Scan() {
		if bytes.Equal(s.Bytes(), []byte{0}) {
			return
		}
		if failedStartup {
			continue
		}
		var cmd struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(s.Bytes(), &cmd); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		switch cmd.Type {
		case "set_model":
			fmt.Println(`{"t":"agent","ts":1.000,"msg":{"type":"response","command":"set_model","success":true,"data":{}}}`)
		case "get_state":
			fmt.Println(`{"t":"agent","ts":2.000,"msg":{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1","model":{"provider":"openai-codex","id":"gpt-5.6-terra"},"thinkingLevel":"high"}}}`)
		}
	}
	if err := s.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func TestModelsForPiModels(t *testing.T) {
	t.Parallel()

	models := modelsForPiModels([]genaipi.Model{
		{ID: "non-reasoning", Provider: "test"},
		{ID: "defaults", Provider: "test", Reasoning: true},
		{
			ID:            "mapped",
			Provider:      "test",
			Reasoning:     true,
			ContextWindow: 1_048_576,
			ThinkingLevelMap: map[genaipi.ThinkingLevel]string{
				genaipi.ThinkingOff:     "",
				genaipi.ThinkingMinimal: "minimal",
				genaipi.ThinkingLow:     "",
				genaipi.ThinkingHigh:    "high",
				genaipi.ThinkingXHigh:   "xhigh",
				genaipi.ThinkingMax:     "",
			},
		},
	})
	if got, want := (agent.ModelInventory{Models: models}).IDs(), []string{"test/defaults", "test/mapped", "test/non-reasoning"}; !slices.Equal(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	if got, want := models[0].EffortOptions, []string{"off", "minimal", "low", "medium", "high"}; !slices.Equal(got, want) {
		t.Fatalf("default effort options = %v, want %v", got, want)
	}
	if got, want := models[1].EffortOptions, []string{"minimal", "medium", "high", "xhigh"}; !slices.Equal(got, want) {
		t.Fatalf("mapped effort options = %v, want %v", got, want)
	}
	if got, want := models[2].EffortOptions, []string{"off"}; !slices.Equal(got, want) {
		t.Fatalf("non-reasoning effort options = %v, want %v", got, want)
	}
	if got, want := models[1].ContextWindow, 1_048_576; got != want {
		t.Fatalf("mapped context window = %d, want %d", got, want)
	}
	if got := models[0].ContextWindow; got != 0 {
		t.Fatalf("unpublished context window = %d, want 0", got)
	}
}

func TestCachedModels(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	envVars := []string{"PI_API_KEY=secret"}
	cache := agent.OpenHarnessCache(filepath.Join(dir, "harnesses.json"))
	cache.SetModelInventory(harness.Pi, agent.ModelInventory{Models: []agent.Model{{ID: "openai/gpt-5", EffortOptions: []string{"low", "high"}}}}, agent.APIKeyHash(envVars))

	b := New(dir, envVars)
	inventory := b.ModelInventory()
	if len(inventory.Models) != 1 || !slices.Equal(inventory.Models[0].EffortOptions, []string{"low", "high"}) {
		t.Fatalf("ModelInventory() = %#v, want cached effort options", inventory)
	}
}

func TestWaitForResponse(t *testing.T) {
	t.Parallel()

	t.Run("caic_exit surfaces relay stderr", func(t *testing.T) {
		t.Parallel()
		r, err := agent.NewRelayRecordReader(bytes.NewBufferString(`{"type":"caic_exit","exit_code":2,"error":"Unknown option: --approve"}`+"\n"), agent.LogVersionV1, agent.DiscardLogSink{Version: agent.LogVersionV1})
		if err != nil {
			t.Fatal(err)
		}
		_, err = waitForResponse(r, "set_model")
		if err == nil {
			t.Fatal("waitForResponse returned nil error")
		}
		if !strings.Contains(err.Error(), "Unknown option: --approve") {
			t.Fatalf("err = %v, want relay stderr", err)
		}
		if _, ok := errors.AsType[*piProcessExitError](err); !ok {
			t.Fatalf("errors.AsType[*piProcessExitError](%v) = (_, false), want (_, true)", err)
		}
	})
	t.Run("v2 response is persisted once", func(t *testing.T) {
		t.Parallel()
		line := `{"t":"agent","ts":1.000,"msg":{"type":"response","command":"set_model","success":true,"data":{}}}`
		log := agenttest.LogSink{Version: agent.LogVersionV2}
		records, err := agent.NewRelayRecordReader(strings.NewReader(line+"\n"), agent.LogVersionV2, &log)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := waitForResponse(records, "set_model")
		if err != nil {
			t.Fatal(err)
		}
		if resp.Command != "set_model" || !resp.Success {
			t.Fatalf("response = %#v", resp)
		}
		if log.String() != line+"\n" {
			t.Fatalf("persisted record = %q", log.String())
		}
	})
}

func TestPiConn(t *testing.T) {
	t.Parallel()
	t.Run("ReadMessages", func(t *testing.T) {
		t.Parallel()
		for _, kind := range []string{"ExtensionUI", "TaskMCP"} {
			t.Run(kind, func(t *testing.T) {
				t.Parallel()
				stdin := agenttest.NewStdinPipe()
				stdout, output := io.Pipe()
				wire, ok := New("", nil).NewWire().(*piWireFormat)
				if !ok {
					t.Fatal("unexpected Pi wire")
				}
				conn := newPiConn(t.Context(), slog.New(slog.DiscardHandler), stdin, agent.DiscardLogSink{Version: agent.LogVersionV3}, wire, &agent.Options{MCP: mcptest.FakeRegistry{}})
				var workers sync.WaitGroup
				t.Cleanup(func() { conn.Cancel(); _ = output.Close(); _ = stdin.Reader.Close(); workers.Wait() })
				sent := make(chan error, 1)
				workers.Go(func() { sent <- conn.SendPrompt(agent.Prompt{Text: "prompt"}, nil) })
				<-stdin.Started
				messages := make(chan agent.TimedMessage, 8)
				read := make(chan error, 1)
				workers.Go(func() { read <- conn.ReadMessages(stdout, messages, conn) })
				request := `{"t":"agent","ts":1.000,"msg":{"type":"extension_ui_request","id":"confirm","method":"confirm","title":"Allow?"}}`
				if kind == "TaskMCP" {
					request = `{"t":"mcp_request","id":"tools","method":"tools/list"}`
				}
				workers.Go(func() {
					if _, err := io.WriteString(output, request+"\n"+`{"t":"agent","ts":2.000,"msg":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"working"}}}`+"\n"); err != nil {
						t.Errorf("feed output: %v", err)
					}
				})
				select {
				case parsed := <-messages:
					if m, ok := parsed.Message.(*agent.TextDeltaMessage); !ok || m.Text != "working" {
						t.Fatalf("output = %#v", parsed.Message)
					}
				case err := <-read:
					t.Fatalf("reader ended: %v", err)
				case <-time.After(time.Second):
					t.Fatal("stdout blocked behind automatic reply")
				}
				input := bufio.NewReader(stdin.Reader)
				if _, err := input.ReadBytes('\n'); err != nil {
					t.Fatal(err)
				}
				data, err := input.ReadBytes('\n')
				if err != nil {
					t.Fatal(err)
				}
				if kind == "TaskMCP" {
					var response agent.MCPResponseEnvelope
					if err := json.Unmarshal(data, &response); err != nil {
						t.Fatal(err)
					}
					if response.ID != "tools" || response.Error != "" {
						t.Fatalf("MCP response = %#v", response)
					}
					var list agent.MCPToolsListResponse
					if err := json.Unmarshal(response.Result, &list); err != nil {
						t.Fatal(err)
					}
					if len(list.Tools) != 1 || list.Tools[0].Name != "echo" {
						t.Fatalf("tools = %#v", list.Tools)
					}
				} else {
					var response genaipi.ExtensionUIResponseConfirm
					if err := json.Unmarshal(data, &response); err != nil {
						t.Fatal(err)
					}
					if response.ID != "confirm" || !response.Confirmed {
						t.Fatalf("extension response = %#v", response)
					}
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
		}
	})
}

func TestReapPiProcessExit(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "bash", "-c", "printf '%s\\n' '{\"type\":\"caic_exit\",\"exit_code\":1,\"error\":\"extension load failure\"}'; cat >/dev/null")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
	})

	records, recordsErr := agent.NewRelayRecordReader(stdout, agent.LogVersionV1, agent.DiscardLogSink{Version: agent.LogVersionV1})
	if recordsErr != nil {
		t.Fatal(recordsErr)
	}
	_, err = waitForResponse(records, "set_model")
	if err == nil {
		t.Fatal("waitForResponse returned nil error")
	}
	rp := &agent.RelayProcess{Cmd: cmd, Stdin: stdin, Stdout: stdout}
	done := make(chan error, 1)
	var workers sync.WaitGroup
	t.Cleanup(func() { _ = stdin.Close(); workers.Wait() })
	workers.Go(func() { done <- reapPiProcessExit(rp, err) })
	select {
	case got := <-done:
		if !errors.Is(got, err) {
			t.Errorf("reapPiProcessExit error = %v, want wrapped %v", got, err)
		}
	case <-time.After(time.Second):
		t.Fatal("reapPiProcessExit did not close the relay stdin")
	}
}

func TestBackend(t *testing.T) {
	t.Run("Start", func(t *testing.T) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.Symlink(exe, filepath.Join(dir, "ssh")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(piSSHHelperEnv, "1")
		t.Setenv("PATH", dir)
		t.Setenv("PI_SSH_HELPER_DIR", dir)

		msgs := make(chan agent.TimedMessage, 1)
		log := &agenttest.LogSink{Version: agent.LogVersionV2}
		sess, err := New("", nil).Start(t.Context(), &agent.Options{
			ResumeSessionID: "ses-1",
			Logger:          slog.New(slog.DiscardHandler),
			Target:          runtime.ConnectionTarget{SSHHost: "task"},
			Dir:             "/workspace",
			Model:           "openai-codex/gpt-5.6-terra",
			MsgCh:           msgs,
			Log:             log,
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		args, err := os.ReadFile(filepath.Join(dir, "relay-args")) //nolint:gosec // test-owned temporary directory
		if err != nil || !strings.Contains(string(args), "'--session' 'ses-1'") {
			t.Fatalf("Pi resume command=%s err=%v", args, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "updated")); err != nil {
			t.Fatalf("pi update was not run: %v", err)
		}
		if !strings.Contains(log.String(), `"t":"exit"`) {
			t.Fatalf("startup log = %q, want failed launch diagnostics", log.String())
		}
		if strings.Count(log.String(), `"t":"relay_generation"`) != 2 ||
			!strings.Contains(log.String(), `"generation":"generation-1"`) ||
			!strings.Contains(log.String(), `"generation":"generation-2"`) {
			t.Fatalf("startup log = %q, want one distinct generation per Pi relay launch", log.String())
		}
		for _, want := range []string{
			"Pi startup failed; running pi update --all ...",
			"Pi and extensions updated",
			"Pi update completed; retrying startup ...",
		} {
			if !strings.Contains(log.String(), want) {
				t.Fatalf("startup log = %q, want %q", log.String(), want)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "version-called")); err == nil {
			t.Fatal("pi --version was called")
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		count, err := os.ReadFile(filepath.Join(dir, "relay-count")) //nolint:gosec // dir is a test-owned temporary directory.
		if err != nil {
			t.Fatal(err)
		}
		if string(count) != "11" {
			t.Errorf("Pi relay launches = %d, want 2", len(count))
		}
		msg := <-msgs
		meta, ok := msg.Message.(*agent.MetaSessionMessage)
		if !ok {
			t.Fatalf("startup message = %T, want *agent.MetaSessionMessage", msg.Message)
		}
		if meta.ReportedModel != "openai-codex/gpt-5.6-terra" || meta.ReportedEffort != "high" {
			t.Fatalf("reported settings = %q/%q, want openai-codex/gpt-5.6-terra/high", meta.ReportedModel, meta.ReportedEffort)
		}

		// Race-instrumented SSH helpers also wait for the race runtime at exit.
		stopCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := sess.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	t.Run("ResumeMismatch", func(t *testing.T) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.Symlink(exe, filepath.Join(dir, "ssh")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(piSSHHelperEnv, "1")
		t.Setenv("PATH", dir)
		t.Setenv("PI_SSH_HELPER_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "relay-count"), []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = New("", nil).Start(t.Context(), &agent.Options{Logger: slog.New(slog.DiscardHandler), Target: runtime.ConnectionTarget{SSHHost: "task"}, Dir: "/workspace", ResumeSessionID: "original-session", MsgCh: make(chan agent.TimedMessage, 1), Log: &agenttest.LogSink{Version: agent.LogVersionV2}})
		if err == nil || !strings.Contains(err.Error(), "does not match requested session") {
			t.Fatalf("resume mismatch error=%v", err)
		}
	})

	t.Run("AgentArgs", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
		t.Run("resume_exact_session", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
			args := New("", nil).AgentArgs(agent.HarnessArgs{ResumeSessionID: "saved-session"})
			if !slices.Equal(args, []string{"pi", "--mode", "rpc", "--approve", "--session", "saved-session"}) {
				t.Fatalf("resume args = %v", args)
			}
		})
		t.Run("approves project-local inputs in rpc mode", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
			args := New("", nil).AgentArgs(agent.HarnessArgs{})
			want := []string{"pi", "--mode", "rpc", "--approve"}
			if !slices.Equal(args, want) {
				t.Errorf("args = %v, want %v", args, want)
			}
		})
	})

	t.Run("NewWire", func(t *testing.T) {
		msgUpdateWithAccumulatedMessage := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hello"},"message":{"role":"assistant","content":[{"type":"text","text":"ignored accumulated text"}]}}`
		// Regression: replay (relay output during adoption, and on-disk logs) uses
		// NewWire().ParseMessage. It must use the stateful wire format so agent_end
		// produces a terminal ResultMessage; without it SeedTimeline cannot infer
		// the waiting state and adopted pi tasks stay stuck as "running".
		t.Run("synthesizes ResultMessage on agent_end", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
			agentEnd := []byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input":10,"output":5}}]}`)
			msgs, err := New("", nil).NewWire().ParseMessage(agentEnd)
			if err != nil {
				t.Fatal(err)
			}
			var hasResult bool
			for _, m := range msgs {
				if _, ok := m.(*agent.ResultMessage); ok {
					hasResult = true
				}
			}
			if !hasResult {
				t.Fatalf("NewWire().ParseMessage produced no ResultMessage for agent_end: %#v", msgs)
			}
		})

		t.Run("message_update ignores accumulated message payload", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
			msgs, err := New("", nil).NewWire().ParseMessage([]byte(msgUpdateWithAccumulatedMessage))
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
			delta, ok := msgs[0].(*agent.TextDeltaMessage)
			if !ok {
				t.Fatalf("got %T, want TextDeltaMessage", msgs[0])
			}
			if delta.Text != "hello" {
				t.Errorf("delta = %q, want hello", delta.Text)
			}
		})

		t.Run("message_end emits consolidated text", func(t *testing.T) {
			t.Parallel()
			msgs, err := New("", nil).NewWire().ParseMessage([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"final text"}],"stopReason":"stop"}}`))
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
			text, ok := msgs[0].(*agent.TextMessage)
			if !ok {
				t.Fatalf("got %T, want TextMessage", msgs[0])
			}
			if text.Text != "final text" {
				t.Errorf("text = %q, want final text", text.Text)
			}
		})

		t.Run("tool_execution_update emits incremental deltas", func(t *testing.T) {
			t.Parallel()
			parser := New("", nil).NewWire().ParseMessage

			// Simulate two tool_execution_update events with accumulated text.
			// The second event contains the full accumulated text (first + new).
			update1 := []byte(`{"type":"tool_execution_update","toolCallId":"t1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"hello"}]}}`)
			update2 := []byte(`{"type":"tool_execution_update","toolCallId":"t1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"hello world"}]}}`)

			msgs1, err := parser(update1)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs1) != 1 {
				t.Fatalf("update1: got %d messages, want 1", len(msgs1))
			}
			tod1, ok := msgs1[0].(*agent.ToolOutputDeltaMessage)
			if !ok {
				t.Fatalf("update1: got %T, want ToolOutputDeltaMessage", msgs1[0])
			}
			if tod1.Delta != "hello" {
				t.Errorf("update1 delta: got %q, want %q", tod1.Delta, "hello")
			}

			msgs2, err := parser(update2)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs2) != 1 {
				t.Fatalf("update2: got %d messages, want 1", len(msgs2))
			}
			tod2, ok := msgs2[0].(*agent.ToolOutputDeltaMessage)
			if !ok {
				t.Fatalf("update2: got %T, want ToolOutputDeltaMessage", msgs2[0])
			}
			// Should be only the incremental part, not the full accumulated text.
			if tod2.Delta != " world" {
				t.Errorf("update2 delta: got %q, want %q", tod2.Delta, " world")
			}

			// Third update with same text should produce empty delta (filtered out).
			update3 := []byte(`{"type":"tool_execution_update","toolCallId":"t1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"hello world"}]}}`)
			msgs3, err := parser(update3)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs3) != 0 {
				t.Fatalf("update3: got %d messages, want 0 (empty delta filtered)", len(msgs3))
			}
		})

		t.Run("tool output tracking is bounded and evicted on completion", func(t *testing.T) {
			t.Parallel()
			wire := &piWireFormat{}
			update := func(id string) []byte {
				return []byte(`{"type":"tool_execution_update","toolCallId":"` + id + `","partialResult":{"content":[{"type":"text","text":"x"}]}}`)
			}
			for i := range maxTrackedToolOutputs {
				if _, err := wire.ParseMessage(update("tool-" + strconv.Itoa(i))); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(wire.toolOutputLen); got != maxTrackedToolOutputs {
				t.Fatalf("tracked tool outputs = %d, want %d", got, maxTrackedToolOutputs)
			}
			if _, err := wire.ParseMessage(update("overflow")); err != nil {
				t.Fatal(err)
			}
			if got := len(wire.toolOutputLen); got != 1 {
				t.Fatalf("tracked tool outputs after reset = %d, want 1", got)
			}
			if _, err := wire.ParseMessage([]byte(`{"type":"tool_execution_end","toolCallId":"overflow"}`)); err != nil {
				t.Fatal(err)
			}
			if got := len(wire.toolOutputLen); got != 0 {
				t.Fatalf("tracked tool outputs after result = %d, want 0", got)
			}
		})

		t.Run("caic_exit carries relay stderr", func(t *testing.T) {
			t.Parallel()
			parser := New("", nil).NewWire().ParseMessage
			line := []byte(`{"type":"caic_exit","exit_code":2,"error":"Unknown option: --approve"}`)
			msgs, err := parser(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
			exit, ok := msgs[0].(*agent.ExitMessage)
			if !ok {
				t.Fatalf("got %T, want ExitMessage", msgs[0])
			}
			if exit.ExitCode != 2 || exit.Error != "Unknown option: --approve" {
				t.Errorf("exit = %+v, want code 2 with stderr", exit)
			}
		})

		t.Run("prompt command parses to UserInputMessage", func(t *testing.T) {
			t.Parallel()
			parser := New("", nil).NewWire().ParseMessage
			line := []byte(`{"type":"prompt","message":"run rpi/446b-camera/build-annotate-cv.sh without docker","streamingBehavior":"steer"}`)
			msgs, err := parser(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
			ui, ok := msgs[0].(*agent.UserInputMessage)
			if !ok {
				t.Fatalf("got %T, want UserInputMessage", msgs[0])
			}
			want := "run rpi/446b-camera/build-annotate-cv.sh without docker"
			if ui.Text != want {
				t.Errorf("text: got %q, want %q", ui.Text, want)
			}
		})
	})
}

func TestPiWireFormat(t *testing.T) {
	t.Parallel()
	t.Run("MessageStartIncludesSessionMetadata", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{sessionID: "ses-1"}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_start","message":{"role":"assistant","provider":"openrouter","model":"auto","responseModel":"anthropic/claude-opus-4-6"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len(msgs) = %d, want 1", len(msgs))
		}
		init, ok := msgs[0].(*agent.InitMessage)
		if !ok {
			t.Fatalf("message type = %T, want *agent.InitMessage", msgs[0])
		}
		if init.SessionID != "ses-1" || init.ReportedModel != "openrouter/anthropic/claude-opus-4-6" || init.Version != "" {
			t.Fatalf("InitMessage = %+v", init)
		}
	})

	t.Run("MessageEndQuotaErrorEmitsRateLimit", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Codex error: The usage limit has been reached"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("len(msgs) = %d, want 2", len(msgs))
		}
		rl, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("message[0] type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if rl.Status != "rejected" {
			t.Fatalf("RateLimitMessage.Status = %q, want rejected", rl.Status)
		}
		// Without a seen model provider the rejection stays unattributed.
		if rl.QuotaProvider != "" || rl.QuotaWindow != "" {
			t.Fatalf("RateLimitMessage attribution = %q/%q, want empty", rl.QuotaProvider, rl.QuotaWindow)
		}
		res, ok := msgs[1].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("message[1] type = %T, want *agent.ResultMessage", msgs[1])
		}
		if !res.IsError || res.Subtype != "error" || !strings.Contains(res.Result, "usage limit") {
			t.Fatalf("ResultMessage = %+v", res)
		}
	})

	t.Run("QuotaErrorAttributedToActiveModelProvider", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		// A message_start from the codex provider records the active provider.
		if _, err := w.ParseMessage([]byte(`{"type":"message_start","message":{"role":"assistant","content":[],"model":"gpt-5.6-terra","provider":"openai-codex"}}`)); err != nil {
			t.Fatal(err)
		}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"The usage limit has been reached"}}`))
		if err != nil {
			t.Fatal(err)
		}
		rl, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("message[0] type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if rl.QuotaProvider != agent.QuotaProviderCodex || rl.QuotaLabel != "Codex" || rl.QuotaWindow != agent.QuotaWindowRequest {
			t.Fatalf("RateLimitMessage attribution = %q/%q/%q, want codex/Codex/request", rl.QuotaProvider, rl.QuotaLabel, rl.QuotaWindow)
		}
	})

	t.Run("MessageEndGenericErrorEmitsNoRateLimit", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"connection refused"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len(msgs) = %d, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*agent.ResultMessage); !ok {
			t.Fatalf("message[0] type = %T, want *agent.ResultMessage", msgs[0])
		}
	})
}

func TestToAgentUsage(t *testing.T) {
	t.Parallel()
	for _, hour := range []int64{20, 40} {
		u := toAgentUsage(&genaipi.MessageUsage{CacheWrite: 30, CacheWrite1h: hour})
		if u.CacheWriteBuckets().Total() != 30 {
			t.Fatalf("total = %+v", u)
		}
		for _, b := range u.CacheWriteBuckets() {
			if b.Tokens < 0 {
				t.Fatalf("negative write bucket: %+v", u)
			}
		}
	}
}

func TestWireUsageAccounting(t *testing.T) {
	t.Parallel()
	t.Run("native run boundaries", func(t *testing.T) {
		t.Parallel()
		const start = `{"type":"agent_start"}`
		const turn = `{"type":"turn_end","message":{"role":"assistant","usage":{"input":10,"totalTokens":10}}}`
		const end = `{"type":"agent_end","messages":[{"role":"assistant","usage":{"input":10}}]}`
		const summary = `{"type":"compaction_end","reason":"manual","result":{"usage":{"input":3,"totalTokens":3}}}`
		const retry = `{"type":"agent_end","willRetry":true,"messages":[{"role":"assistant","usage":{"input":10}}]}`
		for _, tc := range []struct {
			name      string
			events    []string
			want      int
			snapshots []bool
		}{
			{"request result is a snapshot", []string{start, turn, end}, 10, []bool{true}},
			{"result-only invocation", []string{start, end}, 10, []bool{false}},
			{"compaction within a run", []string{start, summary, turn, end}, 13, []bool{true}},
			{"standalone compaction precedes a new run", []string{summary, start, end}, 13, []bool{false}},
			{"retry run has independent result coverage", []string{start, turn, retry, start, end}, 20, []bool{false}},
			{"result coverage resets", []string{start, turn, end, start, end}, 20, []bool{true, false}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				w := &piWireFormat{}
				var total int
				var snapshots []bool
				for _, event := range tc.events {
					messages, err := w.ParseMessage([]byte(event))
					if err != nil {
						t.Fatal(err)
					}
					for _, m := range messages {
						switch m := m.(type) {
						case *agent.UsageMessage:
							if !m.Additive {
								t.Fatalf("native usage is not additive: %+v", m)
							}
							total += m.Usage.InputTokens
						case *agent.ResultMessage:
							snapshots = append(snapshots, m.UsageIsSnapshot)
							if !m.UsageIsSnapshot {
								total += m.Usage.InputTokens
							}
						}
					}
				}
				if total != tc.want || !slices.Equal(snapshots, tc.snapshots) {
					t.Fatalf("total=%d snapshots=%v, want %d/%v", total, snapshots, tc.want, tc.snapshots)
				}
			})
		}
	})

	t.Run("error result preserves coverage until agent_end", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		events := []string{
			`{"type":"turn_end","message":{"role":"assistant","usage":{"input":10,"totalTokens":10}}}`,
			`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"failed"}}`,
			`{"type":"agent_end","messages":[{"role":"assistant","usage":{"input":10}}]}`,
		}
		var snapshots []bool
		for _, event := range events {
			messages, err := w.ParseMessage([]byte(event))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range messages {
				if result, ok := m.(*agent.ResultMessage); ok {
					snapshots = append(snapshots, result.UsageIsSnapshot)
				}
			}
		}
		if !slices.Equal(snapshots, []bool{true, true}) {
			t.Fatalf("snapshot markers = %v, want both results covered", snapshots)
		}
	})
}
