// Tests for fake agent, runtime, and repository fixtures used by smoke and e2e tests.

package smoketest

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestFakeAgentNaturalPromptMatching(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{
			name:   "explicit lifecycle scenario",
			prompt: "FAKE_LIFECYCLE e2e lifecycle 6487f1ff-2b95-435e-b2c0-27cafedadd0d",
			want:   "Lifecycle streaming marker",
		},
		{
			name:   "keyword substring in identifier",
			prompt: "e2e lifecycle 6487f1ff-2b95-435e-b2c0-27cafedadd0d",
			want:   "Why do programmers prefer dark mode?",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "python3", "-u", "-c", string(fakeScript)) //nolint:gosec // fakeScript is an embedded constant
			cmd.Stdin = strings.NewReader(tc.prompt + "\n")
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("fake scenario output does not contain %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestFakeAgentDemoIncludesCredibleTiming(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "python3", "-u", "-c", string(fakeScript)) //nolint:gosec // fakeScript is an embedded constant
	cmd.Stdin = strings.NewReader("FAKE_DEMO timing\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		`"type":"thinking"`,
		`"type":"tool_result","tool_use_id":"toolu_read_1","duration_ms":180`,
		`"type":"tool_result","tool_use_id":"toolu_bash_1","duration_ms":620`,
		`"type":"result","subtype":"success"`,
		`"duration_ms":2200`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("fake demo output does not contain %q:\n%s", want, text)
		}
	}
}

func TestFakeAgentTimingMessagesParse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		input string
		check func(*testing.T, agent.Message)
	}{
		{
			name:  "thinking",
			input: `{"type":"thinking","text":"checking timing"}`,
			check: func(t *testing.T, msg agent.Message) {
				t.Helper()
				thinking, ok := msg.(*agent.ThinkingMessage)
				if !ok || thinking.Text != "checking timing" {
					t.Fatalf("message = %#v, want thinking message", msg)
				}
			},
		},
		{
			name:  "tool result duration",
			input: `{"type":"tool_result","tool_use_id":"tool-1","duration_ms":620}`,
			check: func(t *testing.T, msg agent.Message) {
				t.Helper()
				result, ok := msg.(*agent.ToolResultMessage)
				if !ok || result.DurationMs != 620 {
					t.Fatalf("message = %#v, want 620ms tool result", msg)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msgs, err := parseMessage([]byte(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("message count = %d, want 1", len(msgs))
			}
			tc.check(t, msgs[0])
		})
	}
}

func TestCleanupSmokeRunContainers(t *testing.T) {
	t.Parallel()

	t.Run("removes_only_containers_returned_by_run_label_filter", func(t *testing.T) {
		t.Parallel()
		var calls [][]string
		run := func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			if len(calls) == 1 {
				return []byte("owned-1\nowned-2\n"), nil
			}
			return nil, nil
		}
		if err := cleanupSmokeRunContainers(t.Context(), "podman", "unique-run", run); err != nil {
			t.Fatal(err)
		}
		want := [][]string{
			{"podman", "container", "ls", "-aq", "--filter", "label=caic.smoke_run=unique-run"},
			{"podman", "container", "rm", "-f", "owned-1", "owned-2"},
		}
		if len(calls) != len(want) {
			t.Fatalf("command count = %d, want %d: %q", len(calls), len(want), calls)
		}
		for i := range calls {
			if !slices.Equal(calls[i], want[i]) {
				t.Fatalf("command %d = %q, want %q", i, calls[i], want[i])
			}
		}
	})
	t.Run("empty_result_does_not_remove", func(t *testing.T) {
		t.Parallel()
		calls := 0
		run := func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return nil, nil
		}
		if err := cleanupSmokeRunContainers(t.Context(), "podman", "unique-run", run); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("commands = %d, want only list", calls)
		}
	})
	t.Run("rejects_missing_ownership_boundary", func(t *testing.T) {
		t.Parallel()
		err := cleanupSmokeRunContainers(t.Context(), "", "", func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("must not run")
		})
		if err == nil {
			t.Fatal("missing runtime and run token were accepted")
		}
	})
}

func TestInitHarnessCache(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()
	if err := InitHarnessCache(cacheDir); err != nil {
		t.Fatalf("InitHarnessCache: %v", err)
	}

	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
	for _, h := range []harness.Name{harness.Codex, harness.Pi, harness.OpenCode} {
		inventory, fresh := cache.ModelInventory(h, "")
		if !fresh {
			t.Errorf("ModelInventory(%q) = %#v fresh=%t, want fresh inventory", h, inventory, fresh)
		}
		if !slices.Equal(inventory.IDs(), []string{"fake-model"}) {
			t.Errorf("ModelInventory(%q).IDs() = %v, want [fake-model]", h, inventory.IDs())
		}
	}
}
