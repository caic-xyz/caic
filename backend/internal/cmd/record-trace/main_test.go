// Tests for the record-trace command helpers.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	claudedto "github.com/maruel/genai/providers/claudecode"
)

// TestRelayAttachArgsMatchProduction pins the flag set every recording mode passes to
// the relay. The client logs the prompt it writes to the agent's stdin, so a recording
// without --no-log-stdin would classify those bytes as agent output instead.
func TestRelayAttachArgsMatchProduction(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"/workspace", t.TempDir()} {
		args := relayAttachArgs(dir, []string{"--model", "test-model"})
		want := []string{"serve-attach", "--dir", dir, "--no-log-stdin", "--", "--model", "test-model"}
		if !slices.Equal(args, want) {
			t.Fatalf("relayAttachArgs(%q) = %v, want %v", dir, args, want)
		}
	}
}

// TestHarnessWorkDir pins the directory each recording mode hands the harness: a
// containerized harness must be told the container path, while --local runs on this
// machine and keeps the host checkout.
func TestHarnessWorkDir(t *testing.T) {
	t.Parallel()
	host := t.TempDir()
	if got := harnessWorkDir(true, host); got != host {
		t.Fatalf("harnessWorkDir(local) = %q, want %q", got, host)
	}
	if got := harnessWorkDir(false, host); got != "/workspace" {
		t.Fatalf("harnessWorkDir(container) = %q, want the container work dir", got)
	}
}

func TestBuildPodmanRunArgs(t *testing.T) {
	t.Run("valid_mounts_logged_in_account", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		claudeDir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(claudeDir, 0o700); err != nil {
			t.Fatalf("mkdir .claude: %v", err)
		}
		workDir := filepath.Join(t.TempDir(), "work")

		args, err := buildPodmanRunArgs(workDir, harness.Claude, "")
		if err != nil {
			t.Fatalf("buildPodmanRunArgs: %v", err)
		}
		want := fmt.Sprintf("type=bind,source=%s,target=/home/user/.claude", claudeDir)
		if !slices.Contains(args, "--mount") || !slices.Contains(args, want) {
			t.Fatalf("args = %v, want credential mount %q", args, want)
		}
	})

	t.Run("valid_mounts_opencode_login_paths", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		paths := []string{".opencode", ".local/share/opencode", ".local/state/opencode"}
		for _, path := range paths {
			if err := os.MkdirAll(filepath.Join(home, path), 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", path, err)
			}
		}
		workDir := filepath.Join(t.TempDir(), "work")

		args, err := buildPodmanRunArgs(workDir, harness.OpenCode, "")
		if err != nil {
			t.Fatalf("buildPodmanRunArgs: %v", err)
		}
		wants := []string{
			fmt.Sprintf("type=bind,source=%s,target=/home/user/.opencode", filepath.Join(home, ".opencode")),
			fmt.Sprintf("type=bind,source=%s,target=/home/user/.local/share/opencode", filepath.Join(home, ".local", "share", "opencode")),
			fmt.Sprintf("type=bind,source=%s,target=/home/user/.local/state/opencode", filepath.Join(home, ".local", "state", "opencode")),
		}
		for _, want := range wants {
			if !slices.Contains(args, want) {
				t.Fatalf("args = %v, want credential mount %q", args, want)
			}
		}
	})

	t.Run("valid_api_key_env_is_optional_but_supported", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("OPENAI_API_KEY", "test-key")
		workDir := filepath.Join(t.TempDir(), "work")

		args, err := buildPodmanRunArgs(workDir, harness.Codex, "OPENAI_API_KEY")
		if err != nil {
			t.Fatalf("buildPodmanRunArgs: %v", err)
		}
		if !slices.Contains(args, "-e") || !slices.Contains(args, "OPENAI_API_KEY=test-key") {
			t.Fatalf("args = %v, want API-key env injection", args)
		}
	})
}

func TestSetupCodexAuth(t *testing.T) {
	t.Parallel()

	t.Run("valid_skips_when_api_key_env_is_unset", func(t *testing.T) {
		t.Parallel()
		if err := setupCodexAuth(t.Context(), "unused", ""); err != nil {
			t.Fatalf("setupCodexAuth: %v", err)
		}
	})
}

func TestAnswerClaudeControlRequest(t *testing.T) {
	t.Parallel()
	t.Run("AskUserQuestion", func(t *testing.T) {
		t.Parallel()
		line := []byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which greeting should main.go print?","header":"Greeting","options":[{"label":"Hello"},{"label":"Hi"}],"multiSelect":false}]},"tool_use_id":"toolu-1"}}`)
		out, err := claudeControlResponse(line, "Hi")
		if err != nil {
			t.Fatal(err)
		}

		var got claudedto.InputControlResponseMsg
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got.Type != claudedto.InputControlResponse {
			t.Fatalf("Type = %q, want %q", got.Type, claudedto.InputControlResponse)
		}
		if got.Response.RequestID != "req-1" {
			t.Fatalf("RequestID = %q, want req-1", got.Response.RequestID)
		}
		if got.Response.Response.Behavior != claudedto.ControlCanUseToolBehaviorAllow {
			t.Fatalf("Behavior = %q, want %q", got.Response.Response.Behavior, claudedto.ControlCanUseToolBehaviorAllow)
		}

		var updated claudedto.AskUserQuestionUpdatedInput
		if err := json.Unmarshal(got.Response.Response.UpdatedInput, &updated); err != nil {
			t.Fatal(err)
		}
		if len(updated.Questions) != 1 {
			t.Fatalf("Questions len = %d, want 1", len(updated.Questions))
		}
		const question = "Which greeting should main.go print?"
		if updated.Answers[question] != "Hi" {
			t.Fatalf("answer = %q, want Hi", updated.Answers[question])
		}
	})
	t.Run("OtherTool", func(t *testing.T) {
		t.Parallel()
		line := []byte(`{"type":"control_request","request_id":"req-2","request":{"subtype":"can_use_tool","tool_name":"Read","input":{"file_path":"main.go"},"tool_use_id":"toolu-2"}}`)
		out, err := claudeControlResponse(line, "")
		if err != nil {
			t.Fatal(err)
		}

		var got claudedto.InputControlResponseMsg
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got.Response.RequestID != "req-2" {
			t.Fatalf("RequestID = %q, want req-2", got.Response.RequestID)
		}
		if len(got.Response.Response.UpdatedInput) != 0 {
			t.Fatalf("UpdatedInput = %s, want empty", got.Response.Response.UpdatedInput)
		}
	})
}

// TestValidateGoldenRecording pins the recorder's evidence check: a scenario that
// must delegate is rejected when its recording carries no native subagent
// observation, while a scenario that does not delegate accepts one.
func TestValidateGoldenRecording(t *testing.T) {
	t.Parallel()
	claude, ok := backends[string(harness.Claude)]
	if !ok {
		t.Fatalf("no %s backend registered", harness.Claude)
	}
	const (
		header = `{"t":"caic_meta","version":3,"prompt":"delegate a joke","repos":[],"harness":"claude"}`
		// The Claude task_started record is the native delegation evidence.
		delegation = `{"t":"agent","ts":1.000,"msg":{"type":"system","subtype":"task_started","task_id":"task-1","tool_use_id":"spawn-1","description":"Tell a joke","subagent_type":"general-purpose","task_type":"local_agent","prompt":"Tell a joke about README.md","uuid":"u","session_id":"s"}}`
		unrelated  = `{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"no delegation here"}]}}}`
		footer     = `{"t":"result","state":"completed"}`
	)
	record := func(lines ...string) string {
		return strings.Join(lines, "\n") + "\n"
	}

	t.Run("DelegatingScenario", func(t *testing.T) {
		t.Parallel()
		if err := validateGoldenRecording("recording.jsonl", record(header, delegation, footer), claude, true); err != nil {
			t.Fatalf("validateGoldenRecording = %v, want the delegation accepted", err)
		}
	})
	t.Run("MissingDelegation", func(t *testing.T) {
		t.Parallel()
		err := validateGoldenRecording("recording.jsonl", record(header, unrelated, footer), claude, true)
		if err == nil || !strings.Contains(err.Error(), "no native subagent activity") {
			t.Fatalf("validateGoldenRecording = %v, want a missing-delegation failure", err)
		}
	})
	t.Run("ScenarioWithoutNativeEvidence", func(t *testing.T) {
		t.Parallel()
		if err := validateGoldenRecording("recording.jsonl", record(header, unrelated, footer), claude, false); err != nil {
			t.Fatalf("validateGoldenRecording = %v, want a non-delegating scenario accepted", err)
		}
	})
	t.Run("UnparseableRecording", func(t *testing.T) {
		t.Parallel()
		if err := validateGoldenRecording("recording.jsonl", record(header, `{"t":"agent","ts":1.000,"msg":{"broken"}}`), claude, false); err == nil {
			t.Fatal("validateGoldenRecording accepted an unparseable recording")
		}
	})
}

func TestRecordingRetainsNativeCorrelation(t *testing.T) {
	t.Parallel()
	backend := codex.New("", nil)
	writer, journalPath, err := taskslog.NewStore(t.TempDir()).Open("recording.jsonl", &v3.MetaMessage{
		MessageType: "caic_meta", Version: agent.LogVersionV3, Harness: string(harness.Codex), Prompt: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	observer := &recordObserver{done: make(chan struct{}, 1)}
	records, err := agent.NewRecords(t.Context(), agent.ApplyRequestedEffort(backend.NewWire(), "high"), agent.NewRecordedLog(writer), observer)
	if err != nil {
		t.Fatal(errors.Join(err, writer.Close()))
	}
	ownerOpen := true
	t.Cleanup(func() {
		if ownerOpen {
			_ = records.Close()
		}
	})
	var encoded strings.Builder
	for _, line := range []string{
		`{"id":1,"result":{"userAgent":"caic/0.1"}}`,
		`{"id":2,"result":{"data":[]}}`,
		`{"id":3,"result":{"thread":{"id":"recorded-thread","cliVersion":"0.133.0"},"model":"gpt-5.6","reasoningEffort":"medium"}}`,
		`{"id":4,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/started","params":{"threadId":"recorded-thread","turnId":"turn-1","item":{"id":"echo-1","type":"userMessage","content":[{"type":"text","text":"hello"}]}}}`,
		`{"method":"turn/completed","params":{"threadId":"recorded-thread","turn":{"id":"turn-1","status":"completed"}}}`,
	} {
		fmt.Fprintf(&encoded, "{\"t\":\"agent\",\"ts\":1.000,\"msg\":%s}\n", line)
	}
	stdout := io.NopCloser(strings.NewReader(encoded.String()))
	stdin, err := os.CreateTemp(t.TempDir(), "stdin-")
	if err != nil {
		t.Fatal(err)
	}
	continuation, err := backend.RecordHandshake(t.Context(), stdin, stdout, &agent.Options{Dir: "/workspace", Records: records})
	if err != nil {
		t.Fatal(err)
	}
	conn := agent.NewConn(t.Context(), slog.New(slog.DiscardHandler), stdin, records, nil)
	if err := conn.SendPrompt(agent.Prompt{Text: "hello"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.ReadMessages(agent.ContinuedReader{Reader: continuation, Closer: stdout}, observer); err != nil {
		t.Fatal(err)
	}
	ownerOpen = false
	if err := records.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observer.done:
	default:
		t.Fatal("the committed result did not finish the recorder")
	}
	raw, err := os.ReadFile(filepath.Clean(journalPath))
	if err != nil {
		t.Fatal(err)
	}
	parser, err := agent.NewLogRecordParser(backend.NewWire())
	if err != nil {
		t.Fatal(err)
	}
	var metadata agent.SessionMetadata
	inputs := 0
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line == "" {
			continue
		}
		record, err := parser.ParseRecord([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range record.Messages {
			metadata.Apply(event.Message)
			if input, ok := event.Message.(*agent.UserInputMessage); ok {
				inputs++
				if input.Text != "hello" {
					t.Fatalf("recorded input = %q", input.Text)
				}
			}
		}
	}
	want := agent.SessionMetadata{SessionID: "recorded-thread", ReportedModel: "gpt-5.6", ReportedEffort: "medium", AgentVersion: "0.133.0"}
	if metadata != want || inputs != 1 || strings.Count(string(raw), `"t":"input"`) != 5 {
		t.Fatalf("recording metadata = %+v, canonical inputs = %d; want %+v, 1 and 5 native inputs\n%s", metadata, inputs, want, raw)
	}
}

func TestRecorderAnswersNormalizedQuestion(t *testing.T) {
	t.Parallel()
	wire := claudecode.New().NewWire()
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"session-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"ask-1","name":"AskUserQuestion","input":{"questions":[{"question":"Which greeting?","options":[{"label":"Hello"},{"label":"Hi"}]}]}}]}}`,
	} {
		if _, err := wire.ParseOutput([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := wire.ParseOutput([]byte(`{"type":"control_request","request_id":"question-1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask-1","input":{"questions":[{"question":"Which greeting?","options":[{"label":"Hello"},{"label":"Hi"}]}]}}}`))
	if err != nil || len(messages) != 1 {
		t.Fatalf("question = %#v, %v", messages, err)
	}
	observer := &recordObserver{askAnswer: "Hi"}
	data, hidden, err := observer.HandleMessage(messages[0])
	if err != nil || hidden {
		t.Fatalf("answer hidden = %t, error = %v", hidden, err)
	}
	inputs, err := wire.ParseInput(data)
	if err != nil || len(inputs) != 1 {
		t.Fatalf("recorded answer = %#v, %v", inputs, err)
	}
	if input, ok := inputs[0].(*agent.UserInputMessage); !ok || input.Text != "Hi" {
		t.Fatalf("recorded answer = %#v", inputs[0])
	}
}
