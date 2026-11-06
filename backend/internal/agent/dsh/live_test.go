// Live DeepSeek checks cover native discovery, tools, task MCP, in-flight SSH recovery, and persisted-session resume.

//go:build smoke

package dsh

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/maruel/genai/acp"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/mcp/mcptest"
)

// TestDSHLiveConfiguredDiscovery checks explicit configured credentials and a
// writable custom DSH_HOME through the production SSH discovery command.
// It does not send prompts or spend inference credit.
//
//nolint:paralleltest // The operator owns the disposable SSH target.
func TestDSHLiveConfiguredDiscovery(t *testing.T) {
	host, key := os.Getenv("CAIC_DSH_SSH"), os.Getenv("DEEPSEEK_API_KEY")
	if host == "" || key == "" {
		t.Skip("set CAIC_DSH_SSH and DEEPSEEK_API_KEY for configured native discovery")
	}
	target := runtime.ConnectionTarget{SSHHost: host}
	for _, home := range []string{"/home/user/.dsh", "/home/user/.dsh/caic-configured-discovery"} {
		t.Run(home, func(t *testing.T) {
			inventory, err := New("", nil).FetchModelInventory(t.Context(), target, []string{"DEEPSEEK_API_KEY=" + key, "DSH_HOME=" + home})
			if err != nil {
				t.Fatal(err)
			}
			if len(inventory.Models) == 0 {
				t.Fatal("configured discovery returned no native models")
			}
		})
	}
}

// TestDSHLiveRelay spends API credit. CAIC_DSH_SSH names a disposable md
// container with dsh installed and DEEPSEEK_API_KEY configured through md's
// ~/.env contract. It must have no running relay. Dsh's writable ~/.dsh mount
// must survive process restart. The test owns /tmp/caic-relay, creates its own
// workspace, and leaves shared dsh configuration unchanged.
//
// Run:
//
//	CAIC_DSH_SSH=<host> go test -tags=smoke -run TestDSHLiveRelay -v -timeout=10m ./backend/internal/agent/dsh/
//
//nolint:paralleltest // This test owns the disposable target's single relay.
func TestDSHLiveRelay(t *testing.T) {
	host := os.Getenv("CAIC_DSH_SSH")
	if host == "" {
		t.Skip("set CAIC_DSH_SSH to an authenticated disposable md container")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 8*time.Minute)
	t.Cleanup(cancel)
	target := runtime.ConnectionTarget{SSHHost: host}
	if alive, err := agent.IsRelayRunning(ctx, host); err != nil || alive {
		t.Fatalf("disposable target readiness: alive=%v, err=%v", alive, err)
	}
	if err := exec.CommandContext(ctx, "ssh", host, "test -w /home/user/.dsh && dsh --version").Run(); err != nil { //nolint:gosec // The operator selects this disposable SSH target.
		t.Fatalf("native installation and writable state: %v", err)
	}
	cacheDir := t.TempDir()
	b := New(cacheDir, nil)
	inventory, err := b.FetchModelInventory(ctx, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	model := "deepseek-official/deepseek-flash"
	if !slices.ContainsFunc(inventory.Models, func(m agent.Model) bool { return m.ID == model && slices.Contains(m.EffortOptions, "off") }) {
		t.Fatalf("live catalog does not advertise %s with off effort", model)
	}
	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
	cache.SetModelInventory(harness.Dsh, inventory, "")
	if got := New(cacheDir, nil).ModelInventory(); !reflect.DeepEqual(got, inventory) {
		t.Fatalf("cached startup catalog differs: got=%+v want=%+v", got, inventory)
	}
	refreshed, err := b.FetchModelInventory(ctx, target, nil)
	if err != nil || !reflect.DeepEqual(refreshed, inventory) {
		t.Fatalf("explicit remote refresh differs: err=%v", err)
	}
	if _, err := b.FetchModelInventory(ctx, runtime.ConnectionTarget{}, nil); err == nil {
		t.Fatal("discovery with no SSH target succeeded")
	}
	t.Log("native SSH discovery, model efforts, cached startup, and refresh verified")
	out, err := exec.CommandContext(ctx, "ssh", host, "mktemp -d /tmp/caic-dsh-live.XXXXXX").Output() //nolint:gosec // The operator selects this disposable SSH target.
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSpace(string(out))
	if !strings.HasPrefix(dir, "/tmp/caic-dsh-live.") || strings.ContainsAny(dir, "\n\r '\"") {
		t.Fatalf("unexpected workspace %q", dir)
	}
	if err := exec.CommandContext(ctx, "ssh", host, "printf 'Hello\\n' > "+dir+"/marker.txt").Run(); err != nil { //nolint:gosec // The operator owns the target and dir is validated above.
		t.Fatal(err)
	}
	var session *agent.Session
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer done()
		if session != nil {
			if err := session.Stop(cleanup); err != nil {
				t.Errorf("stop live session: %v", err)
			}
		}
		if alive, err := agent.IsRelayRunning(cleanup, host); err != nil {
			t.Errorf("relay cleanup check: %v", err)
		} else if alive {
			if err := agent.StopRelay(cleanup, target); err != nil {
				t.Errorf("stop detached relay: %v", err)
			}
		}
		if err := agent.CleanRelayState(cleanup, host); err != nil {
			t.Errorf("remove relay state: %v", err)
		}
		if err := exec.CommandContext(cleanup, "ssh", host, "rm -rf -- "+dir).Run(); err != nil { //nolint:gosec // The operator owns the target and dir is a validated test workspace.
			t.Errorf("remove workspace: %v", err)
		}
	})
	writer, logPath, err := taskslog.NewStore(t.TempDir()).Open("live.jsonl", &v3.MetaMessage{
		MessageType: "caic_meta", Version: 3, Prompt: "native dsh acceptance", Harness: "dsh",
	})
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan agent.TimedMessage, 256)
	handler := &dshLiveHandler{}
	handler.Messages = ch
	records, err := agent.NewRecords(ctx, b.NewWire(), agent.NewRecordedLog(writer), handler)
	if err != nil {
		t.Fatal(errors.Join(err, writer.Close()))
	}
	t.Cleanup(func() {
		if session != nil {
			if err := session.Stop(ctx); err != nil {
				t.Errorf("stop recording session: %v", err)
			}
			session = nil
		}
		if err := records.Close(); err != nil {
			t.Error(err)
		}
	})
	registry := &dshLiveRegistry{token: "task-" + rand.Text()}
	nonce := rand.Text()
	opts := agent.Options{Target: target, Dir: dir, Model: model, Effort: "off", Records: records, Logger: slog.Default(), MCP: registry}
	opts.InitialPrompt.Text = fmt.Sprintf("Remember this conversation-only token: %s. Do not save it. Read marker.txt, edit Hello to Hi, then use bash to run exactly cat marker.txt with sandbox_permissions danger-full-access and justification 'Test native ACP approval with a harmless command'. Also read missing-file.txt to exercise a failed tool. Finish by replying FIRST_OK.", nonce)
	session, err = b.Start(ctx, &opts)
	if err != nil {
		t.Fatal(err)
	}
	first := dshLiveResult(t, ctx, session, ch, "")
	if first.SessionID == "" || !strings.Contains(first.Result, "FIRST_OK") {
		t.Fatalf("first result=%+v", first)
	}
	out, err = exec.CommandContext(ctx, "ssh", host, "cat "+dir+"/marker.txt").Output() //nolint:gosec // The operator owns the target and dir is a validated test workspace.
	if err != nil || string(out) != "Hi\n" {
		t.Fatalf("file edit output=%q err=%v", out, err)
	}
	var permissions, failures int
	var outputs strings.Builder
	for _, event := range handler.Events() {
		switch m := event.Message.(type) {
		case *agent.ToolResultMessage:
			if strings.Contains(m.Error, "missing-file.txt") {
				failures++
			}
		case *agent.ToolOutputDeltaMessage:
			outputs.WriteString(m.Delta)
		}
	}
	// Internal permission messages are recorded but deliberately suppressed
	// from product events by the live protocol interceptor.
	if err := taskslog.ScanFile(logPath, true, func(_ os.FileInfo, sc *taskslog.Scanner, _ v3.MetaMessage) error {
		for sc.Scan() {
			var frame struct {
				Type    string             `json:"t"`
				Message acp.JSONRPCMessage `json:"msg"`
			}
			if err := json.Unmarshal(sc.Bytes(), &frame); err != nil {
				return err
			}
			if frame.Type == "agent" && frame.Message.Method == acp.MethodSessionRequestPermission {
				permissions++
			}
		}
		return sc.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if permissions == 0 || failures == 0 || !strings.Contains(outputs.String(), "Hello") || !strings.Contains(outputs.String(), "Hi") {
		t.Fatalf("first turn evidence: permissions=%d failed reads=%d terminal outputs=%q", permissions, failures, outputs.String())
	}
	t.Log("native read/edit/command output, permission exchange, failed tool, and result verified")
	if err := session.SendPrompt(agent.Prompt{Text: "Call the current caic task MCP echo tool exactly once with empty arguments. Return its result only; do not use bash or files."}); err != nil {
		t.Fatal(err)
	}
	second := dshLiveResult(t, ctx, session, ch, first.SessionID)
	registry.mu.Lock()
	calls := registry.calls
	registry.mu.Unlock()
	if second.SessionID != first.SessionID || !strings.Contains(second.Result, registry.token) || calls != 1 {
		t.Fatalf("task MCP: session=%s calls=%d result=%q", second.SessionID, calls, second.Result)
	}
	t.Log("second prompt used task-scoped native ACP stdio MCP registration")
	if err := session.SendPrompt(agent.Prompt{Text: "Run exactly sleep 3; printf 'reconnected-ok\\n' in bash, then reply RECONNECTED_OK. Do not use other tools."}); err != nil {
		t.Fatal(err)
	}
	dshLiveWaitBash(t, ctx, session, ch)
	session.Cancel()
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	session = nil
	if alive, err := agent.IsRelayRunning(ctx, host); err != nil || !alive {
		t.Fatalf("running relay after disconnect: alive=%v err=%v", alive, err)
	}
	opts.InitialPrompt = agent.Prompt{}
	opts.RelayOffset = handler.offsetValue()
	session, err = b.AttachRelay(ctx, &opts)
	if err != nil {
		t.Fatal(err)
	}
	third := dshLiveResult(t, ctx, session, ch, first.SessionID)
	if third.SessionID != first.SessionID || !strings.Contains(third.Result, "RECONNECTED_OK") {
		t.Fatalf("in-flight reconnect result=%+v", third)
	}
	t.Log("in-flight SSH disconnect and exact retained/live handoff verified")
	if err := session.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	session = nil
	if alive, err := agent.IsRelayRunning(ctx, host); err != nil || alive {
		t.Fatalf("relay survived normal stop: alive=%v err=%v", alive, err)
	}
	if err := agent.CleanRelayState(ctx, host); err != nil {
		t.Fatal(err)
	}
	for len(ch) > 0 {
		<-ch
	}
	opts.ResumeSessionID = first.SessionID
	opts.RelayOffset = 0
	opts.Effort = "low"
	opts.InitialPrompt.Text = "What conversation-only token did I ask you to remember in the first turn? Reply with the token only. Do not use tools."
	session, err = b.Start(ctx, &opts)
	if err != nil {
		t.Fatal(err)
	}
	fourth := dshLiveResult(t, ctx, session, ch, first.SessionID)
	if fourth.SessionID != first.SessionID || !strings.Contains(fourth.Result, nonce) {
		t.Fatalf("stopped-session resume lost conversation: session=%s answer=%q", fourth.SessionID, fourth.Result)
	}
	if err := session.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	session = nil
	parser, err := agent.NewLogRecordParser(b.NewWire())
	if err != nil {
		t.Fatal(err)
	}
	var replay []agent.TimedMessage
	if err := taskslog.ScanFile(logPath, true, func(_ os.FileInfo, sc *taskslog.Scanner, _ v3.MetaMessage) error {
		if _, err := parser.ParseRecord(sc.HeaderFrame.Encoded); err != nil {
			return err
		}
		for sc.Scan() {
			record, err := parser.ParseRecord(sc.Bytes())
			if err != nil {
				return err
			}
			replay = append(replay, record.Messages...)
		}
		return sc.Err()
	}); err != nil {
		t.Fatal(err)
	}
	dshLiveCounts(t, handler.Events(), first.SessionID)
	dshLiveCounts(t, replay, first.SessionID)
	t.Log("normal stop, persisted session resume, requested settings, and live/restored event uniqueness verified")
	if err := agent.CleanRelayState(ctx, host); err != nil {
		t.Fatal(err)
	}
	opts.InitialPrompt.Text = "Run exactly sleep 30 in bash. Do not use other tools."
	session, err = b.Start(ctx, &opts)
	if err != nil {
		t.Fatal(err)
	}
	dshLiveWaitBash(t, ctx, session, ch)
	if err := session.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	session = nil
	if alive, err := agent.IsRelayRunning(ctx, host); err != nil || alive {
		t.Fatalf("running native command survived cancellation: alive=%v err=%v", alive, err)
	}
	t.Log("cancellation of an active native command terminated its relay and streams")
	if err := agent.CleanRelayState(ctx, host); err != nil {
		t.Fatal(err)
	}
	opts.ResumeSessionID = ""
	opts.InitialPrompt = agent.Prompt{}
	opts.Model = "unadvertised/not-a-model"
	if failed, err := b.Start(ctx, &opts); err == nil || failed != nil {
		t.Fatal("invalid model unexpectedly started a session")
	}
	if alive, err := agent.IsRelayRunning(ctx, host); err != nil || alive {
		t.Fatalf("failed startup leaked a native relay: alive=%v err=%v", alive, err)
	}
	t.Log("invalid native model failed closed and terminated its startup process")
}

type dshLiveHandler struct {
	agenttest.RecordHandler

	mu     sync.Mutex
	offset int64
}

func (h *dshLiveHandler) RecordAppended(record agent.PhysicalRecord) {
	if record.Provenance.Source == agent.RecordSourceRelay {
		h.mu.Lock()
		if record.Provenance.Generation != "" {
			h.offset = 0
		}
		h.offset += int64(len(record.Encoded))
		h.mu.Unlock()
	}
}

func (h *dshLiveHandler) offsetValue() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.offset
}

type dshLiveRegistry struct {
	mcptest.FakeRegistry

	token string

	mu    sync.Mutex
	calls int
}

func (r *dshLiveRegistry) CallTool(_ context.Context, params *mcp.ToolsCallParams) (mcp.RawToolResult, error) {
	if params.Name != "echo" || string(params.Arguments) != "{}" {
		return mcp.RawToolResult{}, mcp.ErrInvalidParams("expected task echo with empty arguments")
	}
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return mcp.RawToolResult{Structured: mcp.TextOutput{Result: r.token}}, nil
}

func dshLiveResult(t *testing.T, ctx context.Context, session *agent.Session, ch <-chan agent.TimedMessage, sessionID string) *agent.ResultMessage {
	var text strings.Builder
	for {
		select {
		case event := <-ch:
			switch m := event.Message.(type) {
			case *agent.InitMessage:
				sessionID = m.SessionID
			case *agent.TextDeltaMessage:
				text.WriteString(m.Text)
			}
			if m, ok := event.Message.(*agent.ResultMessage); ok {
				if m.IsError {
					t.Fatalf("native turn failed: %s", m.Result)
				}
				result := *m
				result.SessionID = sessionID
				result.Result = text.String()
				return &result
			}
		case <-session.Done():
			t.Fatalf("native session exited before result: %v", session.Wait())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func dshLiveWaitBash(t *testing.T, ctx context.Context, session *agent.Session, ch <-chan agent.TimedMessage) {
	for {
		select {
		case event := <-ch:
			if m, ok := event.Message.(*agent.ToolUseMessage); ok && m.Name == "Bash" {
				return
			}
		case <-session.Done():
			t.Fatalf("native session ended before command execution: %v", session.Wait())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func dshLiveCounts(t *testing.T, events []agent.TimedMessage, sessionID string) {
	inputs, results := 0, 0
	tools := map[string]struct{}{}
	var metadata agent.SessionMetadata
	for _, event := range events {
		metadata.Apply(event.Message)
		switch m := event.Message.(type) {
		case *agent.UserInputMessage:
			inputs++
		case *agent.ResultMessage:
			results++
		case *agent.ToolUseMessage:
			if _, ok := tools[m.ToolUseID]; ok {
				t.Fatalf("duplicated tool use %s", m.ToolUseID)
			}
			tools[m.ToolUseID] = struct{}{}
		}
	}
	if inputs != 4 || results != 4 || metadata.SessionID != sessionID || metadata.ReportedEffort != "low" {
		t.Fatalf("history: inputs=%d results=%d metadata=%+v", inputs, results, metadata)
	}
}
