// Tests Codex backend handshakes, subprocess failures, and shell-safe task MCP launch arguments.

package codex

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/maruel/gomode/mcp/mcptest"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

func TestBackend(t *testing.T) {
	t.Run("RecordHandshake", func(t *testing.T) { //nolint:paralleltest // Other Backend cases change the process environment.
		t.Run("Continuation", func(t *testing.T) { //nolint:paralleltest // Backend cases share the process environment.
			const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"gpt-5.4"}],"nextCursor":null}}
{"id":3,"result":{"thread":{"id":"thread_1","cliVersion":"0.133.0"}}}
{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"next","cliVersion":"1.0"}}}
`
			_, continuation, err := New("", nil).RecordHandshake(t.Context(), io.Discard, io.NopCloser(strings.NewReader(responses)), "/workspace", "", agent.LogVersionV1)
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
			_, _, err := New("", nil).RecordHandshake(ctx, io.Discard, r, "/workspace", "", agent.LogVersionV1)
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
		_, err := New("", nil).Start(t.Context(), &agent.Options{
			Logger: slog.New(slog.DiscardHandler),
			Target: runtime.ConnectionTarget{SSHHost: "task"},
			Dir:    "/workspace",
			Log:    &agenttest.LogSink{Version: agent.LogVersionV2},
			MsgCh:  make(chan agent.TimedMessage, 1),
			MCP:    mcptest.FakeRegistry{},
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
				"-c", `model_auto_compact_token_limit=244800`,
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
