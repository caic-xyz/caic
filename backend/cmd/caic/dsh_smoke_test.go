// Native DeepSeek server smoke checks cover md task adoption, persisted restart, fork identity, and purge.

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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/dsh"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// TestDSHNativeDiscovery exercises the actual server's temporary md discovery
// containers without inference prompts. It uses the same opt-in credentials
// and disposable md configuration as TestDSHNativeLifecycle.
//
//nolint:paralleltest // The smoke fixture sets process-wide XDG_CONFIG_HOME.
func TestDSHNativeDiscovery(t *testing.T) {
	if os.Getenv("CAIC_DSH_SMOKE") != "1" {
		t.Skip("set CAIC_DSH_SMOKE=1 and DEEPSEEK_API_KEY for native discovery acceptance")
	}
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Fatal("native discovery requires DEEPSEEK_API_KEY")
	}
	b := dsh.New("", nil)
	smoke := startServerFixture(t, serverFixture{
		mdConfigDir: os.Getenv("CAIC_DSH_MD_CONFIG_HOME"),
		backends:    map[harness.Name]agent.Backend{harness.Dsh: b},
		harnessEnv: func(runToken string) map[string][]string {
			return map[string][]string{"dsh": {"DEEPSEEK_API_KEY=" + key, "DSH_HOME=/home/user/.dsh/caic-smoke-" + runToken}}
		},
	})
	var inventory v1.HarnessInfo
	postJSON(t, smoke.baseURL, "/api/caic/v1/server/harnesses/dsh/refresh", nil, &inventory)
	if len(inventory.Models) == 0 {
		t.Fatal("native server refresh returned no models")
	}
	t.Log("actual temporary md discovery returned native routes")
}

// TestDSHNativeLifecycle spends API credit and provisions real md containers.
// CAIC_DSH_SMOKE=1 explicitly enables the test; CAIC_DSH_SSH names an existing
// authenticated disposable dsh container used only for initial model discovery.
// DEEPSEEK_API_KEY is injected through the production harness environment.
// An isolated DSH_HOME under md's persistent .dsh mount proves configured
// storage remains available after the original task process is gone.
// CAIC_DSH_MD_CONFIG_HOME optionally reuses an operator-owned disposable md
// configuration and image; do not run other tests using that configuration
// concurrently. Repository, server configuration, and task state remain isolated.
//
//nolint:paralleltest // The shared smoke fixture sets process-wide XDG_CONFIG_HOME.
func TestDSHNativeLifecycle(t *testing.T) {
	if os.Getenv("CAIC_DSH_SMOKE") != "1" {
		t.Skip("set CAIC_DSH_SMOKE=1, CAIC_DSH_SSH, and DEEPSEEK_API_KEY for paid native lifecycle acceptance")
	}
	key, host := os.Getenv("DEEPSEEK_API_KEY"), os.Getenv("CAIC_DSH_SSH")
	if key == "" || host == "" {
		t.Fatal("native lifecycle requires DEEPSEEK_API_KEY and CAIC_DSH_SSH")
	}
	b := dsh.New("", nil)
	inventory, err := b.FetchModelInventory(t.Context(), runtime.ConnectionTarget{SSHHost: host}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.SetModelInventory(inventory)
	smoke := startServerFixture(t, serverFixture{
		mdConfigDir: os.Getenv("CAIC_DSH_MD_CONFIG_HOME"),
		backends:    map[harness.Name]agent.Backend{harness.Dsh: b},
		harnessEnv: func(runToken string) map[string][]string {
			return map[string][]string{"dsh": {"DEEPSEEK_API_KEY=" + key, "DSH_HOME=/home/user/.dsh/caic-smoke-" + runToken}}
		},
	})
	var prefs v1.PreferencesResp
	getJSON(t, smoke.baseURL, "/api/caic/v1/server/preferences", &prefs)
	prefs.Settings.WellKnownCaches, prefs.Settings.CacheMappings = nil, nil
	postJSON(t, smoke.baseURL, "/api/caic/v1/server/preferences", v1.UpdatePreferencesReq{Settings: prefs.Settings}, &prefs)
	var repos []v1.Repo
	getJSON(t, smoke.baseURL, "/api/caic/v1/server/repos", &repos)
	if len(repos) == 0 {
		t.Fatal("native smoke fixture has no repository")
	}
	nonce := rand.Text()
	workspaceMarker := "workspace-" + rand.Text()
	var created v1.Task
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks", v1.CreateTaskReq{
		InitialPrompt: v1.Prompt{Text: fmt.Sprintf("Remember the conversation-only token %s. Do not save that token to any file. Separately use the write tool to write exactly %s into fork-marker.txt. Use the read tool to read it, the edit tool to append -edited, then bash to run cat fork-marker.txt. Also read missing-file.txt to exercise a failed tool. Finish by replying FIRST_OK.", nonce, workspaceMarker)},
		Repos:         []v1.RepoSpec{{Name: repos[0].Path}}, Harness: v1.HarnessDsh,
		Model: "deepseek-official/deepseek-flash", Effort: "off", RuntimeName: smoke.runtimeName, CaicMCP: true,
	}, &created)
	id := created.ID.String()
	first := waitForTaskState(t, smoke, id, "waiting")
	firstText := dshSmokeHistory(t, smoke, id)
	if first.SessionID == "" || first.ReportedModel != "deepseek-official/deepseek-flash" || first.ReportedEffort != "off" || firstText.Results != 1 || !strings.Contains(firstText.Text, "FIRST_OK") {
		t.Fatalf("native task startup: session=%s model=%s effort=%s history=%+v error=%q", first.SessionID, first.ReportedModel, first.ReportedEffort, firstText, first.Error)
	}
	t.Log("native server task waiting with persisted custom DSH_HOME")
	t.Logf("native browser task url=%s/task/%s", smoke.baseURL, id)
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/input", v1.InputReq{Prompt: v1.Prompt{Text: "Reply ADOPTION_READY, then run exactly sleep 5; printf 'adopted-tool-ok\\n' in bash, then reply ADOPTED_OK."}}, nil)
	waitForTaskState(t, smoke, id, "running")
	smoke.restart()
	adopted := waitForTaskState(t, smoke, id, "waiting")
	adoptedText := dshSmokeHistory(t, smoke, id)
	if adopted.SessionID != first.SessionID || adopted.Runtime.ID != first.Runtime.ID || adoptedText.Results != 2 || !strings.Contains(adoptedText.Text, "ADOPTED_OK") {
		t.Fatal("server restart changed native session or runtime identity")
	}
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/input", v1.InputReq{Prompt: v1.Prompt{Text: "Call the current caic MCP tasks_list tool exactly once. Then reply with the conversation-only token I gave you. Do not use shell or files."}}, nil)
	second := waitForTaskState(t, smoke, id, "waiting")
	secondText := dshSmokeHistory(t, smoke, id)
	if secondText.Results != 3 || !strings.Contains(secondText.Text, nonce) || !slices.ContainsFunc(secondText.Tools, func(name string) bool { return strings.Contains(name, "tasks_list") }) {
		t.Fatalf("server adoption lost native conversation: history=%+v error=%q", secondText, second.Error)
	}
	t.Log("server restart adopted a live native relay and retained conversation")
	smoke.stop()
	instance := string(runtime.ID(first.Runtime.ID).InstanceID())
	if err := exec.CommandContext(t.Context(), smoke.runtimeName, "stop", "--time", "0", instance).Run(); err != nil { //nolint:gosec // Runtime and instance are owned by this smoke fixture.
		t.Fatalf("simulate container crash while server is offline: %v", err)
	}
	smoke.start()
	waitForTaskState(t, smoke, id, "waiting")
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/input", v1.InputReq{Prompt: v1.Prompt{Text: "What conversation-only token did I give you? Reply only with that token. Do not use tools."}}, nil)
	recovered := waitForTaskState(t, smoke, id, "waiting")
	recoveredText := dshSmokeHistory(t, smoke, id)
	if recovered.SessionID != first.SessionID || recoveredText.Results != 4 || !strings.Contains(recoveredText.Text, nonce) {
		t.Fatalf("imported crash recovery lost native conversation: session=%s history=%+v", recovered.SessionID, recoveredText)
	}
	t.Log("server restart recovered trusted native history after unexpected container exit")
	dshSmokeStop(t, smoke, id)
	waitForTaskState(t, smoke, id, "stopped")
	smoke.restart()
	stopped := waitForTaskState(t, smoke, id, "stopped")
	if stopped.State != v1.TaskStateStopped {
		t.Fatalf("deliberately stopped native task revived during server restart: %s", stopped.State)
	}
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/revive", nil, nil)
	waitForTaskState(t, smoke, id, "waiting")
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/input", v1.InputReq{Prompt: v1.Prompt{Text: "What conversation-only token did I give you? Reply only with that token. Do not use tools."}}, nil)
	resumed := waitForTaskState(t, smoke, id, "waiting")
	resumedText := dshSmokeHistory(t, smoke, id)
	if resumed.SessionID != first.SessionID || resumedText.Results != 5 || !strings.Contains(resumedText.Text, nonce) {
		t.Fatalf("explicit native restart lost conversation: session=%s history=%+v error=%q", resumed.SessionID, resumedText, resumed.Error)
	}
	t.Log("explicit stopped-task restart resumed persisted native conversation")
	var fork v1.Task
	postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+id+"/fork", v1.ForkTaskReq{Prompt: v1.Prompt{Text: "Read fork-marker.txt. Reply FORK_OK followed by its exact contents."}}, &fork)
	forked := waitForTaskState(t, smoke, fork.ID.String(), "waiting")
	forkText := dshSmokeHistory(t, smoke, fork.ID.String())
	if forked.SessionID == "" || forked.SessionID == first.SessionID || forked.ForkedFromTaskID != first.ID || forkText.Results != 1 || !strings.Contains(forkText.Text, "FORK_OK") || !strings.Contains(forkText.Text, workspaceMarker+"-edited") {
		t.Fatalf("fork identity: source=%s fork=%s parent=%s history=%+v", first.SessionID, forked.SessionID, forked.ForkedFromTaskID, forkText)
	}
	t.Log("container fork used an independent native session with caic lineage")
	for _, taskID := range []string{fork.ID.String(), id} {
		dshSmokeStop(t, smoke, taskID)
		waitForTaskState(t, smoke, taskID, "stopped")
		postJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+taskID+"/purge", nil, nil)
		waitForTaskState(t, smoke, taskID, "purged")
	}
	smoke.restart()
	for _, taskID := range []string{fork.ID.String(), id} {
		if task := waitForTaskState(t, smoke, taskID, "purged"); task.State != v1.TaskStatePurged {
			t.Fatalf("purged task revived after restart: state=%s", task.State)
		}
	}
	t.Log("normal stop, purge, and server restart preserved terminal task intent")
}

// dshSmokeStop waits through the lifecycle reservation held while imported
// recovery verifies its reattached process is alive. The visible turn can
// finish before that independent startup check releases its operation guard.
func dshSmokeStop(t *testing.T, smoke *smokeServer, taskID string) {
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

type dshSmokeSummary struct {
	Text    string
	Results int
	Tools   []string
}

func dshSmokeHistory(t *testing.T, smoke *smokeServer, taskID string) dshSmokeSummary {
	var manifest v1.HistoryManifest
	getJSON(t, smoke.baseURL, "/api/caic/v1/tasks/"+taskID+"/history/manifest", &manifest)
	bootstrap := manifest.Bootstrap
	var summary dshSmokeSummary
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
		}
	}
	return summary
}
