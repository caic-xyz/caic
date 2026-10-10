// Tests Codex subprocess failures and shell-safe task MCP launch arguments.

package codex

import (
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
}
