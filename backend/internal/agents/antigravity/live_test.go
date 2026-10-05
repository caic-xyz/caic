// Live Antigravity relay checks for tools, task-MCP isolation, SSH reconnect, and conversation resume.

//go:build smoke

package antigravity

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/mcp/mcptest"
)

// TestLiveRelay spends subscription quota. CAIC_AGY_SSH names a disposable md
// container with agy installed and authenticated. It must have no running relay.
// The test owns /tmp/caic-relay and creates an isolated workspace. agy stores
// conversation state in the container's ~/.gemini, which md mounts from the
// host. CAIC_AGY_MODEL optionally selects a model.
//
// Run: CAIC_AGY_SSH=<host> go test -tags=smoke -run TestLiveRelay -v -timeout=10m ./backend/internal/agents/antigravity/
func TestLiveRelay(t *testing.T) {
	host := os.Getenv("CAIC_AGY_SSH")
	if host == "" {
		t.Skip("set CAIC_AGY_SSH to an authenticated disposable md container")
	}
	// Keep SSH alive until cleanup sends the relay shutdown sentinel.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 8*time.Minute)
	t.Cleanup(cancel)
	target := runtime.ConnectionTarget{SSHHost: host}
	alive, err := agent.IsRelayRunning(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("container already has a running relay")
	}
	b := New(t.TempDir(), nil)
	inv, err := b.FetchModelInventory(ctx, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	model := os.Getenv("CAIC_AGY_MODEL")
	if model == "" {
		model = "gemini-3.8-flash-low"
	}
	if !slices.ContainsFunc(inv.Models, func(m agent.Model) bool { return m.ID == model }) {
		t.Fatalf("model %q is unavailable", model)
	}
	out, err := exec.CommandContext(ctx, "ssh", host, "mktemp -d /tmp/caic-agy-live.XXXXXX").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSpace(string(out))
	if !strings.HasPrefix(dir, "/tmp/caic-agy-live.") || strings.ContainsAny(dir, "\n\r '\"") {
		t.Fatalf("unexpected workspace path %q", dir)
	}
	ch := make(chan agent.TimedMessage, 256)
	opts := agent.Options{
		Target: target, Dir: dir, Model: model, MsgCh: ch,
		Logger: slog.Default(), Log: agent.DiscardLogSink{Version: agent.LogVersionV3},
	}
	var session *agent.Session
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer done()
		if session != nil {
			if err := session.Stop(cleanup); err != nil {
				t.Errorf("stop session: %v", err)
			}
		}
		if alive, err := agent.IsRelayRunning(cleanup, host); err != nil {
			t.Errorf("check relay for cleanup: %v", err)
		} else if alive {
			if err := agent.StopRelay(cleanup, target); err != nil {
				t.Errorf("stop detached relay: %v", err)
			}
		}
		if err := agent.CleanRelayState(cleanup, host); err != nil {
			t.Errorf("remove relay state: %v", err)
		}
		if err := exec.CommandContext(cleanup, "ssh", host, "rm -rf -- "+dir).Run(); err != nil {
			t.Errorf("remove workspace: %v", err)
		}
	})
	nonce := rand.Text()
	opts.InitialPrompt.Text = fmt.Sprintf("Remember this conversation-only token: %s. Do not save it to a file. Run a shell command to write exactly relay-tool-ok into %s/marker. Then reply FIRST_OK.", nonce, dir)
	session, err = b.Start(ctx, &opts)
	if err != nil {
		t.Fatal(err)
	}
	first, tools := liveResult(t, ctx, session, ch)
	if first.SessionID == "" || tools == 0 {
		t.Fatalf("first turn: session ID=%q, completed tools=%d", first.SessionID, tools)
	}
	marker, err := exec.CommandContext(ctx, "ssh", host, "cat "+dir+"/marker").Output()
	if err != nil || strings.TrimSpace(string(marker)) != "relay-tool-ok" {
		t.Fatalf("tool marker=%q, err=%v", marker, err)
	}
	t.Log("first turn: tool execution verified")
	if err := session.SendPrompt(agent.Prompt{Text: "Reply SECOND_OK. Do not use tools."}); err != nil {
		t.Fatal(err)
	}
	second, tools := liveResult(t, ctx, session, ch)
	if tools != 0 || second.SessionID != first.SessionID || !strings.Contains(second.Result, "SECOND_OK") {
		t.Fatalf("second turn: tools=%d, result=%+v", tools, second)
	}
	t.Log("second turn: same process and conversation")
	offset, err := agent.RelayOutputSize(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	session = nil
	alive, err = agent.IsRelayRunning(ctx, host)
	if err != nil || !alive {
		t.Fatalf("relay after disconnect: alive=%v, err=%v", alive, err)
	}
	opts.InitialPrompt = agent.Prompt{}
	opts.RelayOffset = offset
	opts.WarmHistory = true
	session, err = b.AttachRelay(ctx, &opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.SendPrompt(agent.Prompt{Text: "What is the conversation-only token from the first turn? Reply with the token only. Do not use tools."}); err != nil {
		t.Fatal(err)
	}
	third, tools := liveResult(t, ctx, session, ch)
	if tools != 0 || third.SessionID != first.SessionID || !strings.Contains(third.Result, nonce) {
		t.Fatalf("reconnect lost conversation context: tools=%d, result=%+v", tools, third)
	}
	t.Log("reconnect: retained conversation-only token")
	if err := session.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	session = nil
	if alive, err := agent.IsRelayRunning(ctx, host); err != nil || alive {
		t.Fatalf("relay after stop: alive=%v, err=%v", alive, err)
	}
	if err := agent.CleanRelayState(ctx, host); err != nil {
		t.Fatal(err)
	}
	// A stopped agy process can emit a final cancellation result. Give the
	// resumed process its own channel so that result cannot satisfy this turn.
	ch = make(chan agent.TimedMessage, 256)
	opts.MsgCh = ch
	opts.ResumeSessionID = first.SessionID
	opts.InitialPrompt.Text = "What is the conversation-only token from the first turn? Reply with the token only. Do not use tools."
	session, err = b.Start(ctx, &opts)
	if err != nil {
		t.Fatal(err)
	}
	fourth, tools := liveResult(t, ctx, session, ch)
	if tools != 0 || fourth.SessionID != first.SessionID || !strings.Contains(fourth.Result, nonce) {
		t.Fatalf("resume lost conversation context: tools=%d, result=%+v", tools, fourth)
	}
	t.Log("resume: new process retained conversation-only token")
}

// TestLiveTaskMCP spends subscription quota on two distinct task registries.
// It uses the same disposable CAIC_AGY_SSH target as TestLiveRelay, never writes
// shared ~/.gemini settings, and checks fresh calls across reconnect/resume.
func TestLiveTaskMCP(t *testing.T) {
	host := os.Getenv("CAIC_AGY_SSH")
	if host == "" {
		t.Skip("set CAIC_AGY_SSH to an authenticated disposable md container")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 8*time.Minute)
	t.Cleanup(cancel)
	if alive, err := agent.IsRelayRunning(ctx, host); err != nil || alive {
		t.Fatalf("disposable target readiness: alive=%v, err=%v", alive, err)
	}
	out, err := exec.CommandContext(ctx, "ssh", host, "mktemp -d /tmp/caic-agy-mcp-live.XXXXXX").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSpace(string(out))
	if !strings.HasPrefix(dir, "/tmp/caic-agy-mcp-live.") || strings.ContainsAny(dir, "\n\r '\"") {
		t.Fatalf("unexpected workspace path %q", dir)
	}
	if err := exec.CommandContext(ctx, "ssh", host, "git init -q "+dir).Run(); err != nil {
		t.Fatal(err)
	}
	settingsHash := liveGlobalSettingsHash(t, ctx, host)
	var session *agent.Session
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer done()
		if session != nil {
			if err := session.Stop(cleanup); err != nil {
				t.Errorf("stop task MCP session: %v", err)
			}
		}
		if alive, err := agent.IsRelayRunning(cleanup, host); err != nil {
			t.Errorf("task MCP relay cleanup check: %v", err)
		} else if alive {
			if err := agent.StopRelay(cleanup, runtime.ConnectionTarget{SSHHost: host}); err != nil {
				t.Errorf("stop task MCP relay: %v", err)
			}
		}
		if err := agent.CleanRelayState(cleanup, host); err != nil {
			t.Errorf("remove task MCP relay state: %v", err)
		}
		if err := exec.CommandContext(cleanup, "ssh", host, "rm -rf -- "+dir).Run(); err != nil {
			t.Errorf("remove task MCP workspace: %v", err)
		}
	})
	b := New(t.TempDir(), nil)
	model := os.Getenv("CAIC_AGY_MODEL")
	if model == "" {
		model = "gemini-3.8-flash-low"
	}
	for _, task := range []string{"task-a", "task-b"} {
		t.Run(task, func(t *testing.T) {
			registry := &liveTaskRegistry{task: task}
			ch := make(chan agent.TimedMessage, 256)
			opts := agent.Options{
				Target: runtime.ConnectionTarget{SSHHost: host}, Dir: dir, Model: model, MsgCh: ch,
				Logger: slog.Default(), Log: agent.DiscardLogSink{Version: agent.LogVersionV3}, MCP: registry,
			}
			opts.InitialPrompt.Text = "Call the current caic task MCP echo tool exactly once with empty arguments. Reply with its returned result only. Do not use shell or write files."
			registry.setToken()
			var err error
			session, err = b.Start(ctx, &opts)
			if err != nil {
				t.Fatal(err)
			}
			first, _ := liveResult(t, ctx, session, ch)
			registry.verify(t, first, 1)
			t.Log("initial task-scoped MCP call verified")
			offset, err := agent.RelayOutputSize(ctx, host)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if err := session.Wait(); err != nil {
				t.Fatal(err)
			}
			session = nil
			opts.InitialPrompt = agent.Prompt{}
			opts.RelayOffset = offset
			opts.WarmHistory = true
			session, err = b.AttachRelay(ctx, &opts)
			if err != nil {
				t.Fatal(err)
			}
			registry.setToken()
			if err := session.SendPrompt(agent.Prompt{Text: "Call the current caic task MCP echo tool again exactly once, with empty arguments; return the fresh result only."}); err != nil {
				t.Fatal(err)
			}
			second, _ := liveResult(t, ctx, session, ch)
			registry.verify(t, second, 2)
			if second.SessionID != first.SessionID {
				t.Fatal("MCP reconnect changed conversation")
			}
			t.Log("reconnected task-scoped MCP call verified")
			if err := session.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			session = nil
			liveMCPRemoved(t, ctx, host)
			t.Log("shutdown removed MCP config and access")
			if err := agent.CleanRelayState(ctx, host); err != nil {
				t.Fatal(err)
			}
			ch = make(chan agent.TimedMessage, 256)
			opts.MsgCh = ch
			opts.RelayOffset = 0
			opts.WarmHistory = false
			opts.ResumeSessionID = first.SessionID
			opts.InitialPrompt.Text = "Call the current caic task MCP echo tool exactly once, with empty arguments. Discover the current server namespace rather than using obsolete names. Return its fresh result only; do not use shell or write files."
			registry.setToken()
			session, err = b.Start(ctx, &opts)
			if err != nil {
				t.Fatal(err)
			}
			third, _ := liveResult(t, ctx, session, ch)
			registry.verify(t, third, 3)
			if third.SessionID != first.SessionID {
				t.Fatal("MCP resume changed conversation")
			}
			t.Log("resumed process task-scoped MCP call verified")
			if err := session.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			session = nil
			liveMCPRemoved(t, ctx, host)
			if err := agent.CleanRelayState(ctx, host); err != nil {
				t.Fatal(err)
			}
		})
	}
	if got := liveGlobalSettingsHash(t, ctx, host); got != settingsHash {
		t.Fatal("task MCP modified shared global settings")
	}
	out, err = exec.CommandContext(ctx, "ssh", host, "git -C "+dir+" status --porcelain").Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("task MCP left workspace changes: %q, %v", out, err)
	}
}

type liveTaskRegistry struct {
	mcptest.FakeRegistry
	task string

	mu    sync.Mutex
	token string
	calls int
}

func (r *liveTaskRegistry) CallTool(_ context.Context, name string, args json.RawMessage) (mcp.RawToolResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name != "echo" || string(args) != "{}" {
		return mcp.RawToolResult{}, mcp.ErrInvalidParams("expected task echo with empty arguments")
	}
	r.calls++
	return mcp.RawToolResult{Structured: mcp.TextOutput{Result: r.token}}, nil
}

func (r *liveTaskRegistry) setToken() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.token = r.task + "-" + rand.Text()
}

func (r *liveTaskRegistry) verify(t *testing.T, result *agent.ResultMessage, calls int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != calls || !strings.Contains(result.Result, r.token) {
		t.Fatalf("task %s MCP routing: calls=%d want=%d, result did not match fresh task token", r.task, r.calls, calls)
	}
}

func liveGlobalSettingsHash(t *testing.T, ctx context.Context, host string) string {
	// Only compare hashes; never dump credentials, shared config or history.
	out, err := exec.CommandContext(ctx, "ssh", host, `python3 -c 'import hashlib,json,pathlib; p=pathlib.Path.home()/".gemini/config"; print(json.dumps({n:hashlib.sha256((p/n).read_bytes()).hexdigest() if (p/n).exists() else "absent" for n in ["mcp_config.json","plugins.json","config.json"]},sort_keys=True))'`).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func liveMCPRemoved(t *testing.T, ctx context.Context, host string) {
	// Client EOF can precede daemon cleanup briefly; wait for its owned files.
	cmd := `for i in $(seq 1 50); do if [ ! -e /tmp/caic-relay/antigravity-mcp ] && [ ! -e /tmp/caic-relay/caic-mcp.sock ]; then exit 0; fi; sleep 0.1; done; exit 1`
	if err := exec.CommandContext(ctx, "ssh", host, cmd).Run(); err != nil {
		t.Fatalf("task MCP config/access survived shutdown: %v", err)
	}
	out, err := exec.CommandContext(ctx, "ssh", host, `printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | python3 /tmp/caic-relay/relay.py caic-mcp`).Output()
	var response struct {
		Error json.RawMessage `json:"error"`
	}
	if err != nil || json.Unmarshal(out, &response) != nil || len(response.Error) == 0 {
		t.Fatalf("stopped task MCP was still accessible: %v", err)
	}
}

func liveResult(t *testing.T, ctx context.Context, s *agent.Session, ch <-chan agent.TimedMessage) (*agent.ResultMessage, int) {
	tools := make(map[string]string)
	started := 0
	for {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-s.Done():
			t.Fatalf("session exited before result: %v", s.Wait())
		case event := <-ch:
			switch m := event.Message.(type) {
			case *agent.ToolUseMessage:
				tools[m.ToolUseID] = m.Name
				started++
			case *agent.ToolResultMessage:
				if tools[m.ToolUseID] == "" || m.Error != "" {
					t.Fatalf("unmatched or failed tool result: %+v", m)
				}
				delete(tools, m.ToolUseID)
			case *agent.ResultMessage:
				if m.IsError {
					t.Fatalf("agent result: %s", m.Result)
				}
				if len(tools) != 0 {
					t.Fatalf("result arrived with %d unfinished tools", len(tools))
				}
				return m, started
			}
		}
	}
}
