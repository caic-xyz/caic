// Opt-in live harness smoke check runs one real harness through the server: model discovery, task history and MCP, relay adoption, imported recovery, revival, compaction, fork identity, and purge.

//go:build smoke

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/smoketest"
)

// TestHarnessLifecycle runs CAIC_SMOKE_HARNESS through the real server and md
// with the standard backend set. It spends the harness's quota or API credit,
// so it runs only when that variable names a harness; use it to diagnose a
// harness, not as a regular check. The harness signs in the way md provides:
// mounted host credentials, or CAIC_SMOKE_HARNESS_ENV.
//
//   - CAIC_SMOKE_MODEL and CAIC_SMOKE_EFFORT select the model and effort; empty
//     uses the harness defaults.
//   - CAIC_SMOKE_HARNESS_ENV lists space-separated container environment
//     entries: NAME forwards the host value, NAME=value sets it.
//   - CAIC_SMOKE_MD_CONFIG_HOME reuses a disposable md configuration and its
//     image; do not run other tests using it concurrently.
//
// Run: CAIC_SMOKE_HARNESS=antigravity go test -tags=smoke -run TestHarnessLifecycle -v -timeout=60m ./backend/cmd/caic/
//
//nolint:paralleltest // The smoke fixture sets process-wide XDG_CONFIG_HOME.
func TestHarnessLifecycle(t *testing.T) {
	requested := os.Getenv("CAIC_SMOKE_HARNESS")
	if requested == "" {
		t.Skip("set CAIC_SMOKE_HARNESS to a harness name for a paid live lifecycle check")
	}
	var name harness.Name
	for h := range backends.Default("", nil) {
		if string(h) == requested {
			name = h
		}
	}
	if name == "" {
		t.Fatalf("CAIC_SMOKE_HARNESS=%q names no standard harness", requested)
	}
	entries := strings.Fields(os.Getenv("CAIC_SMOKE_HARNESS_ENV"))
	env := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !strings.Contains(entry, "=") {
			v := os.Getenv(entry)
			if v == "" {
				t.Fatalf("CAIC_SMOKE_HARNESS_ENV forwards %s, which is empty", entry)
			}
			entry += "=" + v
		}
		env = append(env, entry)
	}
	model, effort := os.Getenv("CAIC_SMOKE_MODEL"), os.Getenv("CAIC_SMOKE_EFFORT")
	smoke := startServerFixture(t, serverFixture{
		mdConfigDir: os.Getenv("CAIC_SMOKE_MD_CONFIG_HOME"),
		harnessEnv: func(string) map[string][]string {
			return map[string][]string{string(name): env}
		},
	})
	var inventory v1.HarnessInfo
	postJSON(t, smoke.baseURL, "/api/caic/v1/server/harnesses/"+string(name)+"/refresh", nil, &inventory)
	if len(inventory.Models) == 0 {
		t.Fatal("model discovery returned no models")
	}
	if model != "" && !slices.ContainsFunc(inventory.Models, func(m v1.Model) bool { return m.ID == model }) {
		t.Fatalf("model %q is unavailable", model)
	}
	t.Logf("model discovery returned %d models", len(inventory.Models))

	var prefs v1.PreferencesResp
	getJSON(t, smoke.baseURL, "/api/caic/v1/server/preferences", &prefs)
	prefs.Settings.WellKnownCaches, prefs.Settings.CacheMappings = nil, nil
	postJSON(t, smoke.baseURL, "/api/caic/v1/server/preferences", v1.UpdatePreferencesReq{Settings: prefs.Settings}, &prefs)
	var repos []v1.Repo
	getJSON(t, smoke.baseURL, "/api/caic/v1/server/repos", &repos)
	if len(repos) == 0 {
		t.Fatal("smoke fixture has no repository")
	}
	workspace := "/home/user/src/" + filepath.Base(repos[0].Path)
	nonce := rand.Text()
	marker := "workspace-" + rand.Text()
	const recallPrompt = "What conversation-only token did I give you? Reply only with that token. Do not use tools."

	var created v1.Task
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks", v1.CreateTaskReq{
		InitialPrompt: v1.Prompt{Text: fmt.Sprintf("Remember the conversation-only token %s. Do not save it to a file. Run one shell command that writes exactly %s into fork-marker.txt in the current directory. Then reply FIRST_OK.", nonce, marker)},
		Repos:         []v1.RepoSpec{{Name: repos[0].Path}}, Harness: v1.Harness(name),
		Model: model, Effort: effort, RuntimeName: smoke.runtimeName, CaicMCP: true,
	}, &created)
	id := created.ID.String()
	t.Logf("task url=%s/task/%s", smoke.baseURL, id)
	first := waitForTaskState(t, smoke, id, "waiting")
	firstText := smokeHistory(t, smoke, id)
	if first.SessionID == "" || (model != "" && first.ReportedModel != model) || (effort != "" && first.ReportedEffort != effort) || firstText.Results != 1 || len(firstText.Tools) == 0 || !strings.Contains(firstText.Text, "FIRST_OK") {
		t.Fatalf("first turn: session=%s model=%s effort=%s history=%+v error=%q", first.SessionID, first.ReportedModel, first.ReportedEffort, firstText, first.Error)
	}
	instance := string(runtime.ID(first.Runtime.ID).InstanceID())
	if got, err := smoketest.ContainerFile(t.Context(), smoke.runtimeName, instance, workspace+"/fork-marker.txt"); err != nil || strings.TrimSpace(got) != marker {
		t.Fatalf("first turn tool effect: %q, %v", got, err)
	}
	t.Logf("first turn ran a tool with model %s", first.ReportedModel)

	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/input", v1.InputReq{Prompt: v1.Prompt{Text: "Run exactly this shell command: sleep 5; printf 'adopted-tool-ok\\n'. Then reply ADOPTED_OK."}}, nil)
	waitForTaskState(t, smoke, id, "running")
	smoke.restart()
	adopted := waitForTaskState(t, smoke, id, "waiting")
	adoptedText := smokeHistory(t, smoke, id)
	if adopted.SessionID != first.SessionID || adopted.Runtime.ID != first.Runtime.ID || adoptedText.Results != 2 || !strings.Contains(adoptedText.Text, "ADOPTED_OK") {
		t.Fatalf("relay adoption: session=%s runtime=%s history=%+v error=%q", adopted.SessionID, adopted.Runtime.ID, adoptedText, adopted.Error)
	}
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/input", v1.InputReq{Prompt: v1.Prompt{Text: "Call the caic MCP tasks_list tool exactly once. Then reply with the conversation-only token I gave you. Do not use shell or files."}}, nil)
	mcp := waitForTaskState(t, smoke, id, "waiting")
	mcpText := smokeHistory(t, smoke, id)
	if mcpText.Results != 3 || !strings.Contains(mcpText.Text, nonce) || !slices.ContainsFunc(mcpText.Tools, func(name string) bool { return strings.Contains(name, "tasks_list") }) {
		t.Fatalf("task MCP after adoption: history=%+v error=%q", mcpText, mcp.Error)
	}
	t.Log("server restart adopted the live relay; task MCP and the conversation survived")

	smoke.stop()
	if err := exec.CommandContext(t.Context(), smoke.runtimeName, "stop", "--time", "0", instance).Run(); err != nil { //nolint:gosec // Runtime and instance are owned by this smoke fixture.
		t.Fatalf("simulate container crash while server is offline: %v", err)
	}
	smoke.start()
	waitForTaskState(t, smoke, id, "waiting")
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/input", v1.InputReq{Prompt: v1.Prompt{Text: recallPrompt}}, nil)
	recovered := waitForTaskState(t, smoke, id, "waiting")
	recoveredText := smokeHistory(t, smoke, id)
	if recovered.SessionID != first.SessionID || recoveredText.Results != 4 || !strings.Contains(recoveredText.Text, nonce) {
		t.Fatalf("imported recovery: session=%s history=%+v error=%q", recovered.SessionID, recoveredText, recovered.Error)
	}
	t.Log("imported recovery resumed the conversation after an unexpected container exit")

	smokeStop(t, smoke, id)
	waitForTaskState(t, smoke, id, "stopped")
	smoke.restart()
	if stopped := waitForTaskState(t, smoke, id, "stopped"); stopped.State != v1.TaskStateStopped {
		t.Fatalf("stopped task revived during server restart: %s", stopped.State)
	}
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/revive", nil, nil)
	waitForTaskState(t, smoke, id, "waiting")
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/input", v1.InputReq{Prompt: v1.Prompt{Text: recallPrompt}}, nil)
	resumed := waitForTaskState(t, smoke, id, "waiting")
	resumedText := smokeHistory(t, smoke, id)
	if resumed.SessionID != first.SessionID || resumedText.Results != 5 || !strings.Contains(resumedText.Text, nonce) {
		t.Fatalf("explicit revival: session=%s history=%+v error=%q", resumed.SessionID, resumedText, resumed.Error)
	}
	t.Log("explicit revival resumed the conversation")

	if inventory.SupportsCompact {
		// The task UI's compact action sends no instructions. Every harness
		// reports a finished compaction as a compact_boundary event; Pi's runs
		// without starting a turn, so the task state alone cannot show it.
		postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/compact", v1.CompactReq{}, nil)
		deadline := time.Now().Add(5 * time.Minute)
		for smokeHistory(t, smoke, id).Compactions == resumedText.Compactions {
			if time.Now().After(deadline) {
				t.Fatalf("compaction reported no compact_boundary within 5m; task state %s", findTask(t, smoke, id).State)
			}
			time.Sleep(time.Second)
		}
		waitForTaskState(t, smoke, id, "waiting")
		compactedResults := smokeHistory(t, smoke, id).Results
		postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/input", v1.InputReq{Prompt: v1.Prompt{Text: recallPrompt}}, nil)
		recalled := waitForTaskState(t, smoke, id, "waiting")
		recalledText := smokeHistory(t, smoke, id)
		if recalled.SessionID != first.SessionID || recalledText.Results != compactedResults+1 || !strings.Contains(recalledText.Text, nonce) {
			t.Fatalf("compaction: session=%s results before recall=%d history=%+v error=%q", recalled.SessionID, compactedResults, recalledText, recalled.Error)
		}
		t.Logf("compaction kept the conversation; context before %s, after recall %s", smokeTokens(resumed.ContextUsed), smokeTokens(recalled.ContextUsed))
	}

	var fork v1.Task
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/fork", v1.ForkTaskReq{Prompt: v1.Prompt{Text: "Read fork-marker.txt. Reply FORK_OK followed by its exact contents."}}, &fork)
	forkID := fork.ID.String()
	forked := waitForTaskState(t, smoke, forkID, "waiting")
	forkText := smokeHistory(t, smoke, forkID)
	if forked.SessionID == "" || forked.SessionID == first.SessionID || forked.ForkedFromTaskID != first.ID || forkText.Results != 1 || !strings.Contains(forkText.Text, "FORK_OK") || !strings.Contains(forkText.Text, marker) {
		t.Fatalf("fork identity: source=%s fork=%s parent=%s history=%+v error=%q", first.SessionID, forked.SessionID, forked.ForkedFromTaskID, forkText, forked.Error)
	}
	t.Log("fork read the source workspace in its own conversation")

	for _, taskID := range []string{forkID, id} {
		smokeStop(t, smoke, taskID)
		task := waitForTaskState(t, smoke, taskID, "stopped")
		postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+taskID+"/purge", nil, nil)
		waitForTaskState(t, smoke, taskID, "purged")
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		err := smoketest.WaitForRuntimeGone(ctx, smoke.runtimeName, runtime.ID(task.Runtime.ID))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	smoke.restart()
	for _, taskID := range []string{forkID, id} {
		if task := waitForTaskState(t, smoke, taskID, "purged"); task.State != v1.TaskStatePurged {
			t.Fatalf("purged task revived after restart: state=%s", task.State)
		}
	}
	t.Log("purge removed both containers and survived a server restart")
}

// smokeStop stops a task. It waits through the lifecycle reservation held
// while imported recovery verifies its reattached process is alive: the
// visible turn can finish before that independent startup check releases its
// operation guard.
func smokeStop(t *testing.T, smoke *smokeServer, taskID string) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	ticker := time.NewTicker(100 * time.Millisecond)
	t.Cleanup(ticker.Stop)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, smoke.baseURL+"/api/caic/v1/tasks/"+taskID+"/stop", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		err = json.NewDecoder(resp.Body).Decode(&failure)
		if err := errors.Join(err, resp.Body.Close()); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusConflict || failure.Error.Message != "task lifecycle operation is in progress" {
			t.Fatalf("stop native task: status=%d message=%s", resp.StatusCode, failure.Error.Message)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// smokeTokens formats a reported context occupancy.
func smokeTokens(used *int) string {
	if used == nil {
		return "unknown"
	}
	return strconv.Itoa(*used)
}

// smokeHistorySummary is what a smoke check reads from a task's history.
type smokeHistorySummary struct {
	// Text is the last text message.
	Text string
	// Results counts turn results.
	Results int
	// Tools lists tool-use names in order.
	Tools []string
	// Compactions counts compact_boundary system events.
	Compactions int
}

// smokeHistory summarizes the task's complete projected history.
func smokeHistory(t *testing.T, smoke *smokeServer, taskID string) smokeHistorySummary {
	var manifest v1.HistoryManifest
	getJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+taskID+"/history/manifest", &manifest)
	bootstrap := manifest.Bootstrap
	var summary smokeHistorySummary
	for first := uint64(1); first <= bootstrap.Through; first += bootstrap.RecordsPerChunk {
		end := min(first+bootstrap.RecordsPerChunk-1, bootstrap.Through)
		var events []v1.HistoryEvent
		getJSON(t, smoke.baseURL, fmt.Sprintf("%s/slice/%d/0/%d/end", bootstrap.ResourceBase, first, end), &events)
		for i := range events {
			message := &events[i].Message
			if text := message.Text; text != nil {
				summary.Text = text.Text
			}
			if message.Result != nil {
				summary.Results++
			}
			if message.ToolUse != nil {
				summary.Tools = append(summary.Tools, message.ToolUse.Name)
			}
			if message.System != nil && message.System.Subtype == v1.EventSystemSubtypeCompactBoundary {
				summary.Compactions++
			}
		}
	}
	return summary
}
