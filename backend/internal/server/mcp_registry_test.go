// Tests task-scoped MCP capabilities and accepted work across stream shutdown.

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"

	"github.com/invopop/jsonschema"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/metrics"
	"github.com/maruel/gomode/mcp"
)

func TestMCPRegistry(t *testing.T) {
	t.Parallel()
	t.Run("ForTask/denies_input_tools", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		registry := s.TaskMCPScoper.ForTask(ksid.NewID())
		for _, name := range []string{"task_send_message", "task_answer_question"} {
			result, err := registry.CallTool(t.Context(), &mcp.ToolsCallParams{Name: name, Arguments: json.RawMessage(`{"task_number":1,"message":"input","answer":"input"}`)})
			if err != nil || !result.IsError || result.Meta[mcp.ToolErrorCodeMetaKey] != string(api.CodeUnauthorized) {
				t.Fatalf("task capability permitted %s: result=%+v, err=%v", name, result, err)
			}
		}
	})
	for _, name := range []string{"task_send_message", "task_answer_question"} {
		t.Run("CallTool/source_cancellation_preserves_accepted_"+name, func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			target := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "target"}, harness.Claude)
			target.SetState(taskslog.StateAsking)
			insertTestTask(s, target.ID, target)
			session, gate := newGatedInputSession(t, target)
			executor := agent.NewMCPExecutor(t.Context())
			ctx, cancel := context.WithCancel(t.Context())
			stdin, input := io.Pipe()
			stdout, output := io.Pipe()
			// These local tools deliberately outlive request cancellation. The
			// task-scoped capability denies them, but executor ownership must
			// also preserve accepted work for registries that expose them.
			conn := agent.NewMCPConn(ctx, testLogger(), input, agent.DiscardLogSink{Version: agent.LogVersionV3}, claudecode.New().NewWire(), executor.Wrap(s.mcpHandlers.protocol.Registry))
			read := make(chan error, 1)
			response := make(chan string, 1)
			var workers sync.WaitGroup
			workers.Go(func() { read <- conn.ReadMessages(stdout, make(chan agent.TimedMessage, 4), nil) })
			workers.Go(func() { line, _ := bufio.NewReader(stdin).ReadString('\n'); response <- line })
			t.Cleanup(func() {
				cancel()
				conn.Cancel()
				_ = output.Close()
				_ = stdin.Close()
				close(gate.release)
				executor.Close()
				workers.Wait()
			})
			field := "message"
			if name == "task_answer_question" {
				field = "answer"
			}
			request := fmt.Sprintf(`{"t":"mcp_request","id":"accepted","method":"tools/call","name":%q,"arguments":{"task_number":1,%q:"accepted"}}`+"\n", name, field)
			if _, err := io.WriteString(output, request); err != nil {
				t.Fatal(err)
			}
			select {
			case <-gate.started:
			case err := <-read:
				t.Fatalf("source ended before accepted input: %v", err)
			case line := <-response:
				t.Fatalf("input rejected before delivery: %s", line)
			case <-time.After(time.Second):
				t.Fatal("actual task input was not accepted")
			}
			cancel()
			select {
			case err := <-read:
				if !errors.Is(err, context.Canceled) || !errors.Is(err, agent.ErrResponsesAborted) {
					t.Fatalf("source cancellation=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("source shutdown joined accepted input in independently live target")
			}
			select {
			case <-session.Done():
				t.Fatal("source cancellation terminated target")
			default:
			}
			// Cleanup releases the real target send and joins its owned call.
		})
	}
	t.Run("CallTool/local_self_stop_joins_source", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "source"}, harness.Claude)
		tk.SetState(taskslog.StateRunning)
		log, path, err := s.logStore.Open(tk.LogFilename(), tk.LogHeader())
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
		entry := s.taskMgr.NewEntry(tk, nil)
		entry.LogPath.Set(path)
		s.taskMgr.Insert(tk.ID, entry)
		ctx, cancel := context.WithCancel(t.Context())
		executor := agent.NewMCPExecutor(t.Context())
		cmd := exec.CommandContext(ctx, "cat")
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		stdout, output := io.Pipe()
		conn := agent.NewMCPConn(ctx, testLogger(), input, agent.DiscardLogSink{Version: agent.LogVersionV3}, claudecode.New().NewWire(), executor.Wrap(s.mcpHandlers.protocol.Registry))
		messages := make(chan agent.TimedMessage, 4)
		session := agent.NewSession(ctx, cmd, conn, stdout, messages, testLogger())
		dispatchDone := make(chan struct{})
		close(dispatchDone)
		h := &task.SessionHandle{Session: session, MsgCh: messages, DispatchDone: dispatchDone, Log: agent.DiscardLogSink{Version: agent.LogVersionV3}}
		tk.AttachSession(h)
		t.Cleanup(func() {
			cancel()
			_ = output.Close()
			conn.Cancel()
			_ = h.Drain()
			executor.Close()
		})
		request := fmt.Sprintf(`{"t":"mcp_request","id":"self-stop","method":"tools/call","name":"task_stop","arguments":{"task_number":%q}}`+"\n", tk.ID.String())
		if _, err := io.WriteString(output, request); err != nil {
			t.Fatal(err)
		}
		select {
		case <-entry.Done():
			if tk.GetState() != taskslog.StateStopped {
				t.Fatalf("self stop=%s", tk.GetState())
			}
		case <-time.After(time.Second):
			t.Fatalf("self-stop did not finish: state=%s", tk.GetState())
		}
		select {
		case <-session.Done():
		default:
			t.Fatal("self stop left source session running")
		}
	})
	t.Run("ForTask/catalog", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_schema", func(t *testing.T) {
			t.Parallel()
			registry := &mcpTools{}
			scopedRegistry := (&mcpRegistry{mcpTools: registry}).ForTask(ksid.NewID())
			tools, err := scopedRegistry.Tools(t.Context())
			if err != nil {
				t.Fatalf("Tools() error: %v", err)
			}
			if len(tools) != 6 {
				t.Fatalf("Tools() = %#v, want six task-scoped tools", tools)
			}
			assertMCPToolVisibility(t, tools, "task_create", true)
			assertMCPToolVisibility(t, tools, "tasks_list", true)
			assertMCPToolVisibility(t, tools, "task_fork", true)
			assertMCPToolVisibility(t, tools, "task_stop", true)
			assertMCPToolVisibility(t, tools, "task_purge", true)
			assertMCPToolVisibility(t, tools, "get_system_stats", false)
			assertMCPToolVisibility(t, tools, "task_get_detail", true)
			var create, detail mcp.ToolDescriptor
			for _, tool := range tools {
				if tool.Name == "task_create" {
					create = tool
				}
				if tool.Name == "task_get_detail" {
					detail = tool
				}
			}
			schema, err := json.Marshal(create.InputSchema)
			if err != nil {
				t.Fatalf("marshal delegated schema: %v", err)
			}
			var decoded struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(schema, &decoded); err != nil {
				t.Fatalf("decode delegated schema: %v", err)
			}
			if len(decoded.Properties) != 1 || decoded.Properties["prompt"] == nil {
				t.Fatalf("delegated input schema = %s, want prompt only", schema)
			}
			detailSchema, err := json.Marshal(detail.InputSchema)
			if err != nil {
				t.Fatalf("marshal detail schema: %v", err)
			}
			var detailDecoded mcpToolInputSchema
			if err := json.Unmarshal(detailSchema, &detailDecoded); err != nil {
				t.Fatalf("decode detail schema: %v", err)
			}
			var taskReference jsonschema.Schema
			if err := json.Unmarshal(detailDecoded.Properties["task"], &taskReference); err != nil {
				t.Fatalf("decode task reference schema: %v", err)
			}
			if len(taskReference.OneOf) != 2 || taskReference.OneOf[0].Type != "integer" || taskReference.OneOf[1].Type != "string" {
				t.Fatalf("task detail schema = %s, want a task integer-or-string reference", detailSchema)
			}
			if len(detailDecoded.Properties) != 2 || detailDecoded.Properties["include_diffstat"] == nil {
				t.Fatalf("task detail schema = %s, want task and include_diffstat", detailSchema)
			}

			if !slices.Equal(detailDecoded.Required, []string{"task"}) {
				t.Fatalf("detail required arguments=%v, want only task", detailDecoded.Required)
			}
		})
		t.Run("error_extra_task_properties", func(t *testing.T) {
			t.Parallel()
			registry := &mcpTools{metrics: metrics.NewStore(metrics.Resource{ServiceName: "caic"})}
			scopedRegistry := (&mcpRegistry{mcpTools: registry}).ForTask(ksid.NewID())
			result, err := scopedRegistry.CallTool(t.Context(), &mcp.ToolsCallParams{Name: "task_create", Arguments: json.RawMessage(`{"prompt":"child","repos":["repo"]}`)})
			if err != nil {
				t.Fatalf("CallTool() error: %v", err)
			}
			if !result.IsError {
				t.Fatal("task-scoped create with repos succeeded, want rejection")
			}
		})
	})
}
