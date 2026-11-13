// Tests and benchmarks Codex handshakes, recoverable compaction rejection, subprocess failures, and task MCP launch arguments.

package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/maruel/genai/providers/codex"
	"github.com/maruel/gomode/mcp/mcptest"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

func TestBackend(t *testing.T) {
	t.Run("RejectedCompactionKeepsConnectionUsable", func(t *testing.T) { //nolint:paralleltest // Other Backend cases change the process environment.
		sink := &agenttest.LogSink{}
		wire, records := protocolRecords(t, t.Context(), sink, &agenttest.RecordHandler{})
		wire.threadID = "thread-1"
		stdin := agenttest.NewStdinPipe()
		conn := agent.NewConn(t.Context(), slog.New(slog.DiscardHandler), stdin, records, nil)
		lines := make(chan string, 2)
		var workers sync.WaitGroup
		workers.Go(func() {
			reader := bufio.NewReader(stdin.Reader)
			for range 2 {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				lines <- line
			}
		})
		t.Cleanup(func() {
			if err := conn.ReadMessages(io.NopCloser(strings.NewReader("")), nil); err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
			if err := stdin.Reader.Close(); err != nil {
				t.Error(err)
			}
			workers.Wait()
		})
		if err := conn.SendCompact("preserve the failing test"); !errors.Is(err, agent.ErrCompactInstructionsUnsupported) {
			t.Fatalf("custom instruction error = %v", err)
		}
		if sink.Len() != 0 {
			t.Fatalf("rejected input was recorded: %s", sink.String())
		}
		select {
		case <-stdin.Started:
			t.Fatal("rejected input wrote native bytes")
		default:
		}
		if err := conn.SendCompact(""); err != nil {
			t.Fatalf("ordinary compact after rejection: %v", err)
		}
		if err := conn.SendPrompt(agent.Prompt{Text: "continue after rejection"}, nil); err != nil {
			t.Fatalf("prompt after rejection: %v", err)
		}
		for i, method := range []string{"thread/compact/start", "turn/start"} {
			var request codex.JSONRPCRequest
			if err := json.Unmarshal([]byte(<-lines), &request); err != nil {
				t.Fatal(err)
			}
			if request.Method != method || request.ID != int64(i+1) {
				t.Fatalf("request = %+v, want method %q, id %d", request, method, i+1)
			}
			if i == 0 {
				var params codex.ThreadCompactStartParams
				if err := json.Unmarshal(request.Params, &params); err != nil {
					t.Fatal(err)
				}
				if params.ThreadID != "thread-1" {
					t.Fatalf("compact params = %+v", params)
				}
			} else {
				var params codex.TurnStartParams
				if err := json.Unmarshal(request.Params, &params); err != nil {
					t.Fatal(err)
				}
				if params.ThreadID != "thread-1" || len(params.Input) != 1 || params.Input[0].Type != codex.TurnInputTypeText || params.Input[0].Text != "continue after rejection" {
					t.Fatalf("prompt params = %+v", params)
				}
			}
		}
	})

	t.Run("RecordHandshake", func(t *testing.T) { //nolint:paralleltest // Other Backend cases change the process environment.
		t.Run("Continuation", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
			const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"gpt-5.4"}],"nextCursor":null}}
{"id":3,"result":{"thread":{"id":"thread_1","cliVersion":"0.133.0"}}}
{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"next","cliVersion":"1.0"}}}
`
			_, records := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
			continuation, err := New("", nil).RecordHandshake(t.Context(), io.Discard, io.NopCloser(strings.NewReader(v3Records(responses))), &agent.Options{Dir: "/workspace", Records: records})
			if err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(continuation).ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(line), `"thread/started"`) {
				t.Fatalf("continuation = %q", line)
			}
		})
		t.Run("Cancellation", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
			r, w := io.Pipe()
			t.Cleanup(func() { _ = w.Close() })
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, records := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
			_, err := New("", nil).RecordHandshake(ctx, io.Discard, r, &agent.Options{Dir: "/workspace", Records: records})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("handshake error = %v, want cancellation", err)
			}
		})
	})
	t.Run("Start", func(t *testing.T) {
		dir := t.TempDir()
		for name, script := range map[string]string{
			"ssh": "#!/bin/sh\nset -eu\ncase \"$*\" in\n*serve-attach*) shift; exec /bin/sh -c \"$*\" ;;\n*) /bin/cat >/dev/null ;;\nesac\n",
			"python3": `#!/bin/sh
set -eu
printf '%s\0' "$@" > "$CAIC_CODEX_ARGV"
# Capture launch arguments only; no app-server handshake.
exit 0
`,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { //nolint:gosec // Private SSH fixtures must be executable.
				t.Fatal(err)
			}
		}
		path := filepath.Join(dir, "argv")
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("CAIC_CODEX_ARGV", path)
		_, records := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
		_, err := New("", nil).Start(t.Context(), &agent.Options{
			Logger:  slog.New(slog.DiscardHandler),
			Target:  runtime.ConnectionTarget{SSHHost: "task"},
			Dir:     "/workspace",
			Records: records,
			MCP:     mcptest.FakeRegistry{},
		})
		if err == nil || !strings.Contains(err.Error(), "codex handshake:") {
			t.Fatalf("Start error = %v, want handshake failure from capture-only fixture", err)
		}
		data, err := os.ReadFile(path) //nolint:gosec // Private test fixture path.
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
		for _, want := range []string{
			`mcp_servers.caic.command="python3"`,
			`mcp_servers.caic.args=["/tmp/caic-relay/relay.py","caic-mcp"]`,
		} {
			i := slices.Index(args, want)
			if i < 1 || args[i-1] != "-c" {
				t.Fatalf("remote argv = %q, want -c %s", args, want)
			}
		}
	})
	t.Run("FetchModelInventory", func(t *testing.T) {
		t.Run("stderr_failure", func(t *testing.T) {
			dir := t.TempDir()
			// A failed SSH discovery must report an error even when stderr arrives.
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf 'discovery failed\\n' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // Fake SSH executable in a private test directory.
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			backend := New("", nil)
			if _, err := backend.FetchModelInventory(t.Context(), runtime.ConnectionTarget{SSHHost: "fake-container"}, nil); err == nil {
				t.Fatal("failed discovery returned no error")
			}
		})
	})
	t.Run("AgentArgs", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
		t.Run("AppServer", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
			args := New("", nil).AgentArgs(agent.HarnessArgs{Model: "o4-mini"})
			want := []string{
				"codex", "--no-daemon", "app-server",
				"-c", `approval_policy="never"`,
				"-c", `approvals_reviewer="user"`,
				"-c", "features.guardian_approval=false",
				"-c", `model_auto_compact_token_limit=350000`,
				"-c", `model_auto_compact_token_limit_scope="total"`,
				"-c", `sandbox_mode="danger-full-access"`,
			}
			if !slices.Equal(args, want) {
				t.Errorf("args = %v, want %v", args, want)
			}
		})
		t.Run("WidgetMCPConfig", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
			// Widget MCP is disabled for codex; AgentArgs should not include
			// mcp_servers config.
			args := New("", nil).AgentArgs(agent.HarnessArgs{})
			for _, arg := range args {
				if strings.Contains(arg, "mcp_servers") {
					t.Errorf("unexpected widget MCP config in args %v", args)
				}
			}
		})
	})
	t.Run("NewWire", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
		wire := New("", nil).NewWire()
		if wire == nil {
			t.Fatal("NewWire returned nil")
		}
	})
}

// protocolRecords keeps the actual startup decoder and its ordered observer in
// the same fixture; the caller joins every transport before cleanup closes it.
func protocolRecords(t testing.TB, ctx context.Context, sink agent.LogSink, handler *agenttest.RecordHandler) (*wireFormat, *agent.Records) {
	wire, ok := New("", nil).NewWire().(*wireFormat)
	if !ok {
		t.Fatal("unexpected protocol wire")
	}
	records, err := agent.NewRecords(ctx, wire, sink, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := records.Close(); err != nil {
			t.Error(err)
		}
	})
	return wire, records
}

func BenchmarkWireFormatTokenUsageUpdated(b *testing.B) {
	w := &wireFormat{}
	line := []byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"turn_1","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"cachedInputTokens":500,"cacheWriteInputTokens":100,"outputTokens":200,"reasoningOutputTokens":50},"last":{"totalTokens":100,"inputTokens":80,"cachedInputTokens":50,"cacheWriteInputTokens":12,"outputTokens":20,"reasoningOutputTokens":5},"modelContextWindow":258400}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := w.ParseOutput(line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseAccountRateLimitsUpdated(b *testing.B) {
	line := []byte(`{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1735689720.5},"secondary":{"usedPercent":85,"windowDurationMins":10080,"resetsAt":1736294520},"rateLimitReachedType":"rate_limit_reached"}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := parseTestMessage(line); err != nil {
			b.Fatal(err)
		}
	}
}
