// Tests Pi CLI model discovery, startup, update recovery, and wire configuration.

package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	genaipi "github.com/maruel/genai/providers/pi"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

const piSSHHelperEnv = "GO_WANT_PI_SSH_HELPER"

func init() {
	if os.Getenv(piSSHHelperEnv) != "1" {
		return
	}
	runPiSSHHelper()
	os.Exit(0)
}

func runPiSSHHelper() {
	args := os.Args
	for i, arg := range args {
		if arg == "task" {
			args = args[i:]
			break
		}
	}

	switch {
	case slices.Equal(args, []string{"task", "pi", "--version"}):
		//nolint:gosec // The test-only helper receives its state directory through the environment.
		if err := os.WriteFile(filepath.Join(os.Getenv("PI_SSH_HELPER_DIR"), "version-called"), nil, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case slices.Equal(args, []string{"task", "pi", "update", "--all"}):
		//nolint:gosec // The test-only helper receives its state directory through the environment.
		if err := os.WriteFile(filepath.Join(os.Getenv("PI_SSH_HELPER_DIR"), "updated"), nil, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Pi and extensions updated")
	case slices.Contains(args, "serve-attach"):
		runPiRelayHelper()
	}
}

func runPiRelayHelper() {
	dir := os.Getenv("PI_SSH_HELPER_DIR")
	path := filepath.Join(dir, "relay-count")
	count, err := os.ReadFile(path) //nolint:gosec // The path is constructed by the test-only helper.
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(path, append(count, '1'), 0o600); err != nil { //nolint:gosec // The path is constructed by the test-only helper.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(count) == 0 {
		fmt.Println(`{"type":"caic_exit","exit_code":1,"error":"extension load failure"}`)
		return
	}

	s := bufio.NewScanner(os.Stdin)
	for s.Scan() {
		if bytes.Equal(s.Bytes(), []byte{0}) {
			return
		}
		var cmd struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(s.Bytes(), &cmd); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		switch cmd.Type {
		case "set_model":
			fmt.Println(`{"type":"response","command":"set_model","success":true,"data":{}}`)
		case "get_state":
			fmt.Println(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1"}}`)
		}
	}
	if err := s.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func TestModelsForPiModels(t *testing.T) {
	t.Parallel()

	models := modelsForPiModels([]genaipi.Model{
		{ID: "non-reasoning", Provider: "test"},
		{ID: "defaults", Provider: "test", Reasoning: true},
		{
			ID:        "mapped",
			Provider:  "test",
			Reasoning: true,
			ThinkingLevelMap: map[genaipi.ThinkingLevel]string{
				genaipi.ThinkingOff:     "",
				genaipi.ThinkingMinimal: "minimal",
				genaipi.ThinkingLow:     "",
				genaipi.ThinkingHigh:    "high",
				genaipi.ThinkingXHigh:   "xhigh",
				genaipi.ThinkingMax:     "",
			},
		},
	})
	if got, want := (agent.ModelInventory{Models: models}).IDs(), []string{"test/defaults", "test/mapped", "test/non-reasoning"}; !slices.Equal(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	if got, want := models[0].EffortOptions, []string{"off", "minimal", "low", "medium", "high"}; !slices.Equal(got, want) {
		t.Fatalf("default effort options = %v, want %v", got, want)
	}
	if got, want := models[1].EffortOptions, []string{"minimal", "medium", "high", "xhigh"}; !slices.Equal(got, want) {
		t.Fatalf("mapped effort options = %v, want %v", got, want)
	}
	if got, want := models[2].EffortOptions, []string{"off"}; !slices.Equal(got, want) {
		t.Fatalf("non-reasoning effort options = %v, want %v", got, want)
	}
}

func TestCachedModels(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	envVars := []string{"PI_API_KEY=secret"}
	cache := agent.OpenHarnessCache(filepath.Join(dir, "harnesses.json"))
	cache.SetModelInventory(harness.Pi, agent.ModelInventory{Models: []agent.Model{{ID: "openai/gpt-5", EffortOptions: []string{"low", "high"}}}}, agent.APIKeyHash(envVars))

	b := New(dir, envVars)
	inventory := b.ModelInventory()
	if len(inventory.Models) != 1 || !slices.Equal(inventory.Models[0].EffortOptions, []string{"low", "high"}) {
		t.Fatalf("ModelInventory() = %#v, want cached effort options", inventory)
	}
}

func TestWaitForResponse(t *testing.T) {
	t.Parallel()

	t.Run("caic_exit surfaces relay stderr", func(t *testing.T) {
		t.Parallel()
		r, err := agent.NewRelayRecordReader(bytes.NewBufferString(`{"type":"caic_exit","exit_code":2,"error":"Unknown option: --approve"}`+"\n"), agent.LogVersionV1, agent.DiscardLogSink{Version: agent.LogVersionV1})
		if err != nil {
			t.Fatal(err)
		}
		_, err = waitForResponse(r, "set_model")
		if err == nil {
			t.Fatal("waitForResponse returned nil error")
		}
		if !strings.Contains(err.Error(), "Unknown option: --approve") {
			t.Fatalf("err = %v, want relay stderr", err)
		}
		if _, ok := errors.AsType[*piProcessExitError](err); !ok {
			t.Fatalf("errors.AsType[*piProcessExitError](%v) = (_, false), want (_, true)", err)
		}
	})
	t.Run("v2 response is persisted once", func(t *testing.T) {
		t.Parallel()
		line := `{"t":"agent","ts":1.000,"msg":{"type":"response","command":"set_model","success":true,"data":{}}}`
		log := agenttest.LogSink{Version: agent.LogVersionV2}
		records, err := agent.NewRelayRecordReader(strings.NewReader(line+"\n"), agent.LogVersionV2, &log)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := waitForResponse(records, "set_model")
		if err != nil {
			t.Fatal(err)
		}
		if resp.Command != "set_model" || !resp.Success {
			t.Fatalf("response = %#v", resp)
		}
		if log.String() != line+"\n" {
			t.Fatalf("persisted record = %q", log.String())
		}
	})
}

func TestReapPiProcessExit(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "bash", "-c", "printf '%s\\n' '{\"type\":\"caic_exit\",\"exit_code\":1,\"error\":\"extension load failure\"}'; cat >/dev/null")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
	})

	records, recordsErr := agent.NewRelayRecordReader(stdout, agent.LogVersionV1, agent.DiscardLogSink{Version: agent.LogVersionV1})
	if recordsErr != nil {
		t.Fatal(recordsErr)
	}
	_, err = waitForResponse(records, "set_model")
	if err == nil {
		t.Fatal("waitForResponse returned nil error")
	}
	rp := &agent.RelayProcess{Cmd: cmd, Stdin: stdin, Stdout: stdout}
	done := make(chan error, 1)
	go func() { done <- reapPiProcessExit(rp, err) }()
	select {
	case got := <-done:
		if !errors.Is(got, err) {
			t.Errorf("reapPiProcessExit error = %v, want wrapped %v", got, err)
		}
	case <-time.After(time.Second):
		t.Fatal("reapPiProcessExit did not close the relay stdin")
	}
}

func TestBackendStart(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(dir, "ssh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(piSSHHelperEnv, "1")
	t.Setenv("PATH", dir)
	t.Setenv("PI_SSH_HELPER_DIR", dir)

	msgs := make(chan agent.ParsedMessage, 1)
	log := &agenttest.LogSink{Version: agent.LogVersionV1}
	sess, err := New("", nil).Start(t.Context(), &agent.Options{
		Target: runtime.ConnectionTarget{SSHHost: "task"},
		Dir:    "/workspace",
		Model:  "openai-codex/gpt-5.6-terra",
		MsgCh:  msgs,
		Log:    log,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "updated")); err != nil {
		t.Fatalf("pi update was not run: %v", err)
	}
	if !strings.Contains(log.String(), `"type":"caic_exit"`) {
		t.Fatalf("startup log = %q, want failed launch diagnostics", log.String())
	}
	for _, want := range []string{
		"Pi startup failed; running pi update --all ...",
		"Pi and extensions updated",
		"Pi update completed; retrying startup ...",
	} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("startup log = %q, want %q", log.String(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "version-called")); err == nil {
		t.Fatal("pi --version was called")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	count, err := os.ReadFile(filepath.Join(dir, "relay-count")) //nolint:gosec // dir is a test-owned temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	if string(count) != "11" {
		t.Errorf("Pi relay launches = %d, want 2", len(count))
	}

	stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := sess.Stop(stopCtx); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestPiWireFormat(t *testing.T) {
	t.Parallel()
	t.Run("MessageStartIncludesSessionMetadata", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{sessionID: "ses-1"}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_start","message":{"role":"assistant","provider":"openai","model":"gpt-5"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len(msgs) = %d, want 1", len(msgs))
		}
		init, ok := msgs[0].(*agent.InitMessage)
		if !ok {
			t.Fatalf("message type = %T, want *agent.InitMessage", msgs[0])
		}
		if init.SessionID != "ses-1" || init.Model != "openai/gpt-5" || init.Version != "" {
			t.Fatalf("InitMessage = %+v", init)
		}
	})

	t.Run("MessageEndQuotaErrorEmitsRateLimit", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Codex error: The usage limit has been reached"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("len(msgs) = %d, want 2", len(msgs))
		}
		rl, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("message[0] type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if rl.Status != "rejected" {
			t.Fatalf("RateLimitMessage.Status = %q, want rejected", rl.Status)
		}
		res, ok := msgs[1].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("message[1] type = %T, want *agent.ResultMessage", msgs[1])
		}
		if !res.IsError || res.Subtype != "error" || !strings.Contains(res.Result, "usage limit") {
			t.Fatalf("ResultMessage = %+v", res)
		}
	})

	t.Run("MessageEndGenericErrorEmitsNoRateLimit", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"connection refused"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len(msgs) = %d, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*agent.ResultMessage); !ok {
			t.Fatalf("message[0] type = %T, want *agent.ResultMessage", msgs[0])
		}
	})
}

func TestAgentArgs(t *testing.T) {
	t.Parallel()

	t.Run("approves project-local inputs in rpc mode", func(t *testing.T) {
		t.Parallel()
		args := New("", nil).AgentArgs(agent.HarnessArgs{})
		want := []string{"pi", "--mode", "rpc", "--approve"}
		if !slices.Equal(args, want) {
			t.Errorf("args = %v, want %v", args, want)
		}
	})
}
