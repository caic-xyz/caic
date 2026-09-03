// Golden and invariant tests for quota handoff prompt generation.

package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func TestBuildHandoffPrompt(t *testing.T) {
	t.Parallel()

	t.Run("NormalGolden", func(t *testing.T) {
		t.Parallel()

		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "Add quota-aware task recovery."}, harness.Codex, "gpt-5.2-codex", "high")
		tk.SetTitle("Add quota-aware recovery")
		tk.Repos = []taskslog.RepoMount{{Name: "caic", BaseBranch: "main", Branch: "caic-42"}}
		tk.SeedTimeline([]agent.Message{
			&agent.UserInputMessage{Text: "Add quota-aware task recovery."},
			&agent.TextMessage{Text: "I added normalized quota state and started the recovery flow."},
			&agent.UserInputMessage{Text: "Please preserve the existing fork behavior."},
			&agent.ThinkingMessage{Text: "Private reasoning must not be transferred."},
			&agent.ToolUseMessage{Name: "Shell", Input: json.RawMessage(`{"cmd":"git status"}`)},
			&agent.ToolResultMessage{Error: "private tool error"},
			&agent.TextMessage{Text: "The backend state is complete; the prompt builder remains."},
			&agent.ResultMessage{IsError: true, Result: "The request stopped when the provider rejected the next turn."},
			&agent.DiffStatMessage{DiffStat: agent.DiffStat{
				{Path: "backend/internal/task/task.go", Added: 34, Deleted: 2},
				{Path: "frontend/public/logo.png", Binary: true},
			}},
			&agent.RateLimitMessage{
				Status:        agent.RateLimitStatusRejected,
				ResetsAt:      time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC),
				QuotaProvider: agent.QuotaProviderCodex,
				QuotaLabel:    "Codex",
				QuotaWindow:   "5h",
			},
		})

		assertHandoffGolden(t, "normal.md", BuildHandoffPrompt(tk, 0))
	})

	t.Run("FilteringAndTruncationGolden", func(t *testing.T) {
		t.Parallel()

		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "Diagnose OPENAI_API_KEY=sk-original-secret without exposing it."}, harness.Claude, "claude-sonnet", "")
		tk.SetTitle("Token ghp_title_secret investigation")
		tk.SeedTimeline([]agent.Message{
			&agent.UserInputMessage{Text: tk.InitialPrompt.Text},
			&agent.ThinkingMessage{Text: "hidden-thought ghp_thinking_secret"},
			&agent.ToolUseMessage{Name: "Shell", Input: json.RawMessage(`{"cmd":"echo ghp_tool_secret"}`)},
			&agent.ToolOutputDeltaMessage{Delta: "oversized-tool-output " + strings.Repeat("private-output", 1000)},
			&agent.ToolResultMessage{Error: "Authorization: Bearer tool-secret"},
			&agent.UserInputMessage{Text: `Credentials: {"access_token":"oauthvalue123"}; AWS_SECRET_ACCESS_KEY: AbCd1234; Authorization: Basic dXNlcjpwYXNz`},
			&agent.TextMessage{Text: "I used sk-assistant-secret while investigating. " + strings.Repeat("\u754c", 1200)},
			&agent.ResultMessage{Result: "Stopped safely before printing github_pat_result_secret."},
		})

		const maxBytes = 900
		got := BuildHandoffPrompt(tk, maxBytes)
		assertHandoffGolden(t, "filtered-truncated.md", got)
		if len(got) > maxBytes {
			t.Fatalf("prompt length = %d, want at most %d", len(got), maxBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatal("truncated prompt is not valid UTF-8")
		}
		if !strings.Contains(got, handoffTruncationNotice) {
			t.Error("truncated prompt does not contain truncation notice")
		}
		for _, forbidden := range []string{
			"hidden-thought",
			"ghp_thinking_secret",
			"ghp_tool_secret",
			"oversized-tool-output",
			"private-output",
			"private tool error",
			"tool-secret",
			"git status",
		} {
			if strings.Contains(got, forbidden) {
				t.Errorf("prompt contains forbidden content %q", forbidden)
			}
		}
		for _, preserved := range []string{
			"sk-original-secret",
			"ghp_title_secret",
			"oauthvalue123",
			"AbCd1234",
			"dXNlcjpwYXNz",
			"sk-assistant-secret",
			"github_pat_result_secret",
		} {
			if !strings.Contains(got, preserved) {
				t.Errorf("prompt does not preserve user text %q", preserved)
			}
		}
	})

	t.Run("LatestEmptyResultUsesCurrentTurnText", func(t *testing.T) {
		t.Parallel()

		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "Continue the task."}, harness.Codex, "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.ResultMessage{IsError: true, Result: "stale failure"},
			&agent.TextMessage{Text: "The newer turn completed successfully."},
			&agent.ResultMessage{},
		})
		got := BuildHandoffPrompt(tk, 0)
		if !strings.Contains(got, "## Latest assistant result") || !strings.Contains(got, "The newer turn completed successfully.") {
			t.Errorf("prompt does not use the current turn result:\n%s", got)
		}
		if strings.Contains(got, "Latest harness error") || strings.Contains(got, "stale failure") {
			t.Errorf("prompt contains stale result metadata:\n%s", got)
		}
	})

	t.Run("RejectedQuotaWithoutReset", func(t *testing.T) {
		t.Parallel()

		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "Continue the task."}, harness.Codex, "", "")
		tk.SeedTimeline([]agent.Message{&agent.RateLimitMessage{
			Status:        agent.RateLimitStatusRejected,
			RateLimitType: "thread_goal",
			QuotaProvider: agent.QuotaProviderCodex,
			QuotaLabel:    "Codex",
			QuotaWindow:   "thread_goal",
		}})
		got := BuildHandoffPrompt(tk, 0)
		if !strings.Contains(got, "Quota block: Codex thread_goal window rejected the request") {
			t.Errorf("prompt does not contain no-reset quota reason:\n%s", got)
		}
	})

	t.Run("OversizedOriginalPreservesCurrentContext", func(t *testing.T) {
		t.Parallel()

		prompt := "Start of request. " + strings.Repeat("\u754c", defaultHandoffPromptMaxBytes)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: prompt}, harness.Codex, "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.UserInputMessage{Text: prompt},
			&agent.TextMessage{Text: "most recent assistant context"},
			&agent.ResultMessage{Result: "latest result marker"},
			&agent.DiffStatMessage{DiffStat: agent.DiffStat{{Path: "current-change.go", Added: 2}}},
		})
		got := BuildHandoffPrompt(tk, 0)
		if len(got) > defaultHandoffPromptMaxBytes {
			t.Fatalf("prompt length = %d, want at most %d", len(got), defaultHandoffPromptMaxBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatal("prompt with oversized original request is not valid UTF-8")
		}
		for _, required := range []string{"Start of request.", "latest result marker", "current-change.go", "most recent assistant context"} {
			if !strings.Contains(got, required) {
				t.Errorf("prompt does not contain required current context %q", required)
			}
		}
	})

	t.Run("PreservesSelectedTextWhitespace", func(t *testing.T) {
		t.Parallel()

		prompt := "  indented request\n\tcode line\n"
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: prompt}, harness.Codex, "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.UserInputMessage{Text: prompt},
			&agent.UserInputMessage{Text: "  follow-up  \n"},
			&agent.TextMessage{Text: "  assistant context  \n"},
			&agent.ResultMessage{Result: "  exact result  \n"},
		})
		got := BuildHandoffPrompt(tk, 0)
		for _, preserved := range []string{
			">   indented request\n> \tcode line\n> \n",
			">   exact result  \n> \n",
			">   follow-up  \n> \n",
			">   assistant context  \n> \n",
		} {
			if !strings.Contains(got, preserved) {
				t.Errorf("prompt does not preserve selected whitespace %q:\n%s", preserved, got)
			}
		}
	})

	t.Run("NoLoadedMessages", func(t *testing.T) {
		t.Parallel()

		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "Inspect the failing build."}, harness.OpenCode, "", "")
		got := BuildHandoffPrompt(tk, 0)
		if !strings.Contains(got, "Inspect the failing build.") {
			t.Errorf("prompt does not contain original request:\n%s", got)
		}
		if strings.Contains(got, "## Recent conversation") {
			t.Errorf("prompt contains an empty conversation section:\n%s", got)
		}
	})
}

func assertHandoffGolden(t *testing.T, name, got string) {
	want, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "handoff", name)))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("handoff golden mismatch for %s\n--- got ---\n%s", name, got)
	}
}
