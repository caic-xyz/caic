// Records ordered single-turn, follow-up, and resumed agent journals through relay.py and validates golden evidence.
//
// Usage:
//
//	record-trace --harness pi --scenario read-edit-bash
//	record-trace --harness claude --scenario native-subagent-joke --local
//
// Requires: podman, unless --local runs the harness on this machine. It mounts
// logged-in harness credentials when available; --api-key-env is optional for
// API-key based recording. --local is for hosts where nested rootless podman is
// unavailable: it still drives the harness through the production relay with
// the harness's real wire format, but it cannot prove md container lifecycle.
// Run from the owning harness package so its testdata remains independent.
// For resume evidence, record session-turns with --local --work-dir pointing at
// a retained checkout, then record session-resume with the same directory and
// --resume-session taken from its session/new response. --effort selects the
// requested native setting independently for each launch.
package main

import (
	"bufio"
	"bytes"
	"context"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/taskslog"

	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"

	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	claudedto "github.com/maruel/genai/providers/claudecode"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/dsh"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
	"github.com/caic-xyz/caic/backend/internal/agent/relay"
)

const image = "ghcr.io/caic-xyz/md-user:latest"

// backends is the registry of supported harnesses for record-trace.
var backends = map[string]agent.Backend{
	string(harness.Pi):       pi.New("", nil),
	string(harness.Claude):   claudecode.New(),
	string(harness.Codex):    codex.New("", nil),
	string(harness.Dsh):      dsh.New("", nil),
	string(harness.OpenCode): opencode.New("", nil),
}

type scenario struct {
	prompt    string
	followup  string
	askAnswer string
	// wantNative marks a scenario that must record native subagent activity, so a
	// regeneration that lost the delegation fails instead of publishing evidence
	// that proves nothing.
	wantNative bool
}

var scenarios = map[string]scenario{
	"native-permission": {
		prompt: `Run exactly printf 'permission-ok\n' with the bash tool, setting sandbox_permissions to danger-full-access and justification to "Exercise the native ACP permission exchange with a harmless command". Return its output.`,
	},
	"session-turns": {
		prompt:   `Remember the marker cobalt-otter. Reply with exactly FIRST-TURN.`,
		followup: `What marker did I ask you to remember? Reply with only that marker.`,
	},
	"session-resume": {
		prompt: `What marker did I ask you to remember? Reply with only that marker.`,
	},
	"native-subagent-joke": {
		prompt:     `Use a native subagent to tell a joke about README.md. Do not perform the task yourself; delegate it and return the subagent's result.`,
		wantNative: true,
	},
	"native-subagent-workflow": {
		prompt:     `Call the subagent tool exactly once with a workflowScript that runs one 'delegate' agent to tell a short original joke about README.md, then return the agent result. Do not call the tool more than once and do not tell the joke yourself.`,
		wantNative: true,
	},
	"ask-user-question": {
		prompt:    `Before editing any files, call AskUserQuestion with one question. The header must be "Greeting", the question must be "Which greeting should main.go print?", and the options must be "Hello" and "Hi". After I answer, update main.go to print that greeting, then run cat main.go.`,
		askAnswer: "Hi",
	},
	"read-edit-bash": {
		prompt: `Read main.go, edit the greeting on line 3 from "Hello" to "Hi", then run cat main.go.`,
	},
	"tool-error": {
		prompt: "Read the file nonexistent_file.txt",
	},
}

var credentialMounts = map[harness.Name][]credentialMount{
	harness.Claude: {
		{homeRelPath: ".claude", containerPath: "/home/user/.claude"},
	},
	harness.Codex: {
		{homeRelPath: ".codex", containerPath: "/home/user/.codex"},
	},
	harness.Dsh: {
		{homeRelPath: ".dsh", containerPath: "/home/user/.dsh"},
	},
	harness.OpenCode: {
		{homeRelPath: ".opencode", containerPath: "/home/user/.opencode"},
		{homeRelPath: ".local/share/opencode", containerPath: "/home/user/.local/share/opencode"},
		{homeRelPath: ".local/state/opencode", containerPath: "/home/user/.local/state/opencode"},
	},
	harness.Pi: {
		{homeRelPath: ".pi", containerPath: "/home/user/.pi"},
	},
}

type credentialMount struct {
	homeRelPath   string
	containerPath string
}

func mainImpl() error {
	harnessFlag := flag.String("harness", "", "harness to record (pi, claude, codex, dsh, opencode)")
	scenarioFlag := flag.String("scenario", "", "predefined scenario name")
	modelFlag := flag.String("model", "", "model to use (e.g. xiaomi/mimo-v2.5)")
	apiKeyEnv := flag.String("api-key-env", "", "env var name for optional API key")
	workDirFlag := flag.String("work-dir", "", "existing checkout to retain across recordings (local mode only)")
	resumeFlag := flag.String("resume-session", "", "native session ID to restore in the retained checkout")
	effortFlag := flag.String("effort", "", "native reasoning effort to select during handshake")
	localFlag := flag.Bool("local", false, "run the harness on this machine instead of a container; requires the harness CLIs on PATH")
	flag.Parse()

	if *harnessFlag == "" {
		return errors.New("--harness is required")
	}
	b, ok := backends[*harnessFlag]
	if !ok {
		return fmt.Errorf("unknown harness: %s", *harnessFlag)
	}
	if *scenarioFlag == "" {
		return errors.New("--scenario is required")
	}
	sc, ok := scenarios[*scenarioFlag]
	if !ok {
		return fmt.Errorf("unknown scenario: %s", *scenarioFlag)
	}
	if *workDirFlag != "" && !*localFlag {
		return errors.New("--work-dir requires --local")
	}
	if *resumeFlag != "" && *workDirFlag == "" {
		return errors.New("--resume-session requires --work-dir to preserve workspace identity")
	}
	outputPath := filepath.Clean(filepath.Join("testdata", *scenarioFlag+".jsonl"))
	if _, err := os.Stat(outputPath); err == nil {
		// Skip if the golden file already exists (idempotent for go generate).
		return nil
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return recordTrace(ctx, b, *apiKeyEnv, sc, outputPath, *localFlag, &agent.Options{Dir: *workDirFlag, Model: *modelFlag, Effort: *effortFlag, ResumeSessionID: *resumeFlag})
}

func recordTrace(ctx context.Context, b agent.Backend, apiKeyEnv string, sc scenario, outputPath string, local bool, opts *agent.Options) (retErr error) {
	workDir := opts.Dir
	var err error
	if workDir == "" {
		workDir, err = setupCheckout()
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(workDir) }()
	} else {
		workDir, err = filepath.Abs(workDir)
		if err != nil {
			return err
		}
	}

	// relayDir owns this recording's relay socket and output.jsonl. Keeping it
	// distinct avoids disturbing a relay that is serving a live caic task.
	relayDir, err := os.MkdirTemp(workDir, ".caic-relay-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(relayDir) }()
	ctr := ""
	if local {
		if err := deployLocalRelay(relayDir); err != nil {
			return err
		}
	} else {
		ctr, err = startContainer(ctx, workDir, b.Harness(), apiKeyEnv)
		if err != nil {
			return err
		}
		defer func() {
			_ = exec.CommandContext(context.WithoutCancel(ctx), "podman", "stop", ctr).Run()
		}()
		if err := deployRelay(ctx, ctr); err != nil {
			return err
		}
		// Create empty widget plugin dir so harnesses that reference
		// --plugin-dir (claude) don't crash on startup.
		if err := runPodman(ctx, "exec", ctr, "mkdir", "-p", agent.WidgetPluginDir); err != nil {
			return fmt.Errorf("create widget plugin dir: %w", err)
		}
		// Codex needs stored credentials for WebSocket auth. Use mounted
		// ~/.codex credentials, or create them from the optional API key.
		if b.Harness() == harness.Codex {
			if err := setupCodexAuth(ctx, ctr, apiKeyEnv); err != nil {
				return fmt.Errorf("codex auth setup: %w", err)
			}
		}
	}
	// Create an empty widget plugin dir so harnesses that reference
	// --plugin-dir (claude) don't crash on startup. The container path is
	// created inside the container by the podman branch above.
	if local {
		if err := os.MkdirAll(agent.WidgetPluginDir, 0o750); err != nil {
			return fmt.Errorf("create widget plugin dir: %w", err)
		}
	}

	binDirs := ""
	if !local {
		binDirs, err = detectBinDirs(ctx, ctr)
		if err != nil {
			return fmt.Errorf("detect tool paths: %w", err)
		}
	}
	// Keep the actual committed journal, including the outbound protocol records
	// needed to reconstruct request ownership and session metadata.
	journalDir, err := os.MkdirTemp("", "caic-record-journal-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(journalDir) }()
	writer, journalPath, err := taskslog.NewStore(journalDir).Open("recording.jsonl", &v3.MetaMessage{
		MessageType: "caic_meta", Version: agent.LogVersionV3,
		Harness: string(b.Harness()), Prompt: sc.prompt, Repos: []v3.MetaRepo{}, StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	protocol, _ := b.(agent.MessageInterceptor)
	observer := &recordObserver{done: make(chan struct{}, 1), askAnswer: sc.askAnswer, protocol: protocol}
	records, err := agent.NewRecords(ctx, b.NewWire(), agent.NewRecordedLog(writer), observer)
	if err != nil {
		return errors.Join(err, writer.Close())
	}
	ownerOpen := true
	defer func() {
		if ownerOpen {
			retErr = errors.Join(retErr, records.Close())
		}
	}()
	cmd, stdin, stdoutPipe, err := startRelayAgent(ctx, ctr, workDir, relayDir, b, opts.Model, binDirs, local)
	if err != nil {
		return err
	}
	bootstrap := true
	defer func() {
		if bootstrap {
			closeErr := errors.Join(stdin.Close(), stdoutPipe.Close())
			_ = cmd.Process.Kill()
			retErr = errors.Join(retErr, closeErr, cmd.Wait())
		}
	}()
	var stdout io.Reader = stdoutPipe
	if hs, ok := b.(agent.RecordHandshaker); ok {
		opts.Dir = harnessWorkDir(local, workDir)
		opts.Records = records
		stdout, err = hs.RecordHandshake(ctx, stdin, stdoutPipe, opts)
		if err != nil {
			return err
		}
	}
	bootstrap = false
	conn := agent.NewConn(ctx, slog.Default(), stdin, records, nil)
	if err := waitAndShutdown(ctx, cmd, conn, agent.ContinuedReader{Reader: stdout, Closer: stdoutPipe}, observer, ctr, localOutputPath(relayDir, local), sc.prompt, sc.followup); err != nil {
		return err
	}
	ownerOpen = false
	if err := records.Close(); err != nil {
		return err
	}
	return writeGoldenFile(ctx, journalPath, b, sc, outputPath)
}

// localOutputPath returns the host path of the recording's output.jsonl.
func localOutputPath(relayDir string, local bool) string {
	if local {
		return filepath.Join(relayDir, "output.jsonl")
	}
	return ""
}

// deployLocalRelay writes the maintained relay script into a dedicated
// relay directory for a local recording.
func deployLocalRelay(relayDir string) error {
	script := relay.ScriptV2
	if err := os.MkdirAll(relayDir, 0o750); err != nil {
		return fmt.Errorf("create local relay dir: %w", err)
	}
	path := filepath.Join(relayDir, "relay.py")
	if err := os.WriteFile(path, script, 0o600); err != nil {
		return fmt.Errorf("write local relay script: %w", err)
	}
	return nil
}

// setupCheckout creates a temp directory with a sample main.go for the agent.
func setupCheckout() (string, error) {
	workDir, err := os.MkdirTemp("", "caic-record-")
	if err != nil {
		return "", err
	}
	sampleMain := []byte("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello, World!\")\n}\n")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), sampleMain, 0o600); err != nil {
		_ = os.RemoveAll(workDir)
		return "", err
	}
	// The native-subagent scenarios delegate a joke about README.md, so keep a
	// small README.md in the recorded checkout.
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Sample project\n\nA tiny checkout used for recording.\n"), 0o600); err != nil {
		_ = os.RemoveAll(workDir)
		return "", err
	}
	return workDir, nil
}

// startContainer pulls the image and starts a container.
func startContainer(ctx context.Context, workDir string, h harness.Name, apiKeyEnv string) (string, error) {
	slog.InfoContext(ctx, "Pulling image", "image", image)
	if pullErr := runPodman(ctx, "pull", image); pullErr != nil {
		return "", fmt.Errorf("pull image: %w", pullErr)
	}

	slog.InfoContext(ctx, "Starting container")
	args, err := buildPodmanRunArgs(workDir, h, apiKeyEnv)
	if err != nil {
		return "", err
	}
	ctrBytes, runErr := exec.CommandContext(ctx, args[0], args[1:]...).Output() //nolint:gosec // podman args are safe
	if runErr != nil {
		return "", fmt.Errorf("start container: %w", runErr)
	}
	return strings.TrimSpace(string(ctrBytes)), nil
}

func buildPodmanRunArgs(workDir string, h harness.Name, apiKeyEnv string) ([]string, error) {
	args := []string{"podman", "run", "-d", "--rm", "--userns", "keep-id", "--user", "user", "-v", workDir + ":/workspace:rw"}
	mounts, err := credentialMountArgs(h)
	if err != nil {
		return nil, err
	}
	args = append(args, mounts...)
	if v := os.Getenv(apiKeyEnv); apiKeyEnv != "" && v != "" {
		args = append(args, "-e", apiKeyEnv+"="+v)
	}
	args = append(args, image, "sleep", "300")
	return args, nil
}

func credentialMountArgs(h harness.Name) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}
	mounts := credentialMounts[h]
	args := make([]string, 0, len(mounts)*2)
	for _, mount := range mounts {
		hostPath := filepath.Join(home, mount.homeRelPath)
		st, err := os.Stat(hostPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat credential path %s: %w", hostPath, err)
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("credential path %s is not a directory", hostPath)
		}
		args = append(args, "--mount", "type=bind,source="+hostPath+",target="+mount.containerPath)
	}
	return args, nil
}

// deployRelay pipes the embedded relay script into the container.
// deployRelay uploads the maintained relay script used by live task sessions.
func deployRelay(ctx context.Context, ctr string) error {
	script := relay.ScriptV2
	cmd := exec.CommandContext(ctx, "podman", "exec", "-i", ctr, //nolint:gosec // args from trusted source
		"sh", "-c", "mkdir -p "+agent.RelayDir+" && cat > "+agent.RelayScriptPath)
	cmd.Stdin = bytes.NewReader(script)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// detectBinDirs finds agent binary directories inside the container.
func detectBinDirs(ctx context.Context, ctr string) (string, error) {
	cmd := exec.CommandContext(ctx, "podman", "exec", ctr, //nolint:gosec // podman args are safe
		"bash", "-c",
		`test -d /home/user/.local/bin && printf ':%s' /home/user/.local/bin
test -x /home/user/.local/share/pnpm/bin/pi && printf ':%s' /home/user/.local/share/pnpm/bin
test -x /home/user/.opencode/bin/opencode && printf ':%s' /home/user/.opencode/bin
set -- /home/user/.nvm/versions/node/v*/bin; test -x "$1/node" && printf ':%s' "$1"`,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("detect paths: %w", err)
	}
	paths := strings.TrimSpace(string(out))
	if paths != "" {
		slog.InfoContext(ctx, "Detected tool paths in container", "paths", paths)
	}
	return paths, nil
}

// containerWorkDir is where container mode mounts the recorded checkout, so a container
// recording must hand the harness and the relay the container path. Only --local runs on
// this machine and therefore uses the host temp directory.
const containerWorkDir = "/workspace"

// harnessWorkDir is the working directory a recording mode tells the harness to use.
func harnessWorkDir(local bool, hostWorkDir string) string {
	if local {
		return hostWorkDir
	}
	return containerWorkDir
}

// relayAttachArgs is the relay command line both recording modes share, so a local
// recording and a container recording invoke the relay exactly like production:
// production logs the prompt it writes to the agent's stdin, so --no-log-stdin keeps
// the relay from logging those same bytes a second time.
func relayAttachArgs(dir string, agentArgs []string) []string {
	return append([]string{"serve-attach", "--dir", dir, "--no-log-stdin", "--"}, agentArgs...)
}

// startRelayAgent launches the relay with the agent process inside the container,
// or on this machine when local is set.
func startRelayAgent(ctx context.Context, ctr, workDir, relayDir string, b agent.Backend, model, binDirs string, local bool) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	slog.InfoContext(ctx, "Starting agent via relay", "harness", b.Harness(), "local", local)
	agentArgs := b.AgentArgs(agent.HarnessArgs{Model: model})
	var cmd *exec.Cmd
	if local {
		// The relay needs no shell indirection on this machine: it spawns the
		// harness with its own PATH and writes output.jsonl under relayDir.
		relayArgs := append([]string{filepath.Join(relayDir, "relay.py")}, relayAttachArgs(harnessWorkDir(local, workDir), agentArgs)...)
		cmd = exec.CommandContext(ctx, "python3", relayArgs...) //nolint:gosec // args are not user-controlled
		cmd.Env = append(os.Environ(), "CAIC_RELAY_DIR="+relayDir, "PYTHONDONTWRITEBYTECODE=1")
		cmd.Stderr = os.Stderr
	} else {
		export := ""
		if binDirs != "" {
			export = "export PATH=" + binDirs + ":$PATH && "
		}
		bashCmd := export + "PYTHONDONTWRITEBYTECODE=1 python3 " + agent.RelayScriptPath + " " + strings.Join(relayAttachArgs(harnessWorkDir(local, workDir), agentArgs), " ")
		relayFullCmd := []string{"podman", "exec", "-i", ctr, "bash", "-c", bashCmd}
		cmd = exec.CommandContext(ctx, relayFullCmd[0], relayFullCmd[1:]...) //nolint:gosec // args are not user-controlled
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start relay: %w", err)
	}
	return cmd, stdin, stdout, nil
}

// waitAndShutdown runs the regular transport owner, delivers the prompt, and
// shuts down both transport producers before its caller closes the journal.
func waitAndShutdown(ctx context.Context, cmd *exec.Cmd, conn agent.Conn, stdout io.ReadCloser, observer *recordObserver, ctr, localOutput, prompt, followup string) error {
	watchCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	stop := context.AfterFunc(watchCtx, conn.Cancel)
	defer stop()
	readDone := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() { readDone <- conn.ReadMessages(stdout, observer) })
	var failure error
	prompts := []string{prompt}
	if followup != "" {
		prompts = append(prompts, followup)
	}
	for _, text := range prompts {
		slog.InfoContext(ctx, "Sending prompt", "text", fmt.Sprintf("%.60s...", text))
		if err := conn.SendPrompt(agent.Prompt{Text: text}, nil); err != nil {
			failure = err
			break
		}
		slog.InfoContext(ctx, "Waiting for agent to finish")
		select {
		case <-observer.done:
		case failure = <-readDone:
			if failure == nil {
				failure = errors.New("agent stream ended without a result")
			}
			conn.Cancel()
			workers.Wait()
			_ = cmd.Process.Kill()
			return errors.Join(failure, cmd.Wait())
		case <-watchCtx.Done():
			failure = watchCtx.Err()
		}
		if failure != nil {
			break
		}
	}
	if failure == nil {
		conn.SendStop(watchCtx)
	}
	conn.Cancel()
	readErr := <-readDone
	workers.Wait()
	if failure != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if failure != nil {
		return errors.Join(failure, readErr, waitErr)
	}
	if readErr != nil && !errors.Is(readErr, context.Canceled) {
		return errors.Join(readErr, waitErr)
	}
	if waitErr != nil {
		return waitErr
	}
	if localOutput != "" {
		return waitForLocalFile(ctx, localOutput, 20*time.Second)
	}
	return waitForFile(ctx, ctr, agent.RelayOutputPath, 20*time.Second)
}

// recordObserver detects committed completion and encodes recorder-specific
// Claude question answers and native protocol replies; the transport queues
// and commits the returned bytes.
type recordObserver struct {
	protocol  agent.MessageInterceptor
	done      chan struct{}
	askAnswer string
}

func (*recordObserver) RecordAppended(agent.PhysicalRecord) {}
func (*recordObserver) PrepareRecords(context.Context, []agent.TimedMessage) ([]agent.Message, error) {
	return nil, nil
}
func (o *recordObserver) ApplyRecords(_ context.Context, messages []agent.TimedMessage) error {
	for _, event := range messages {
		if _, ok := event.Message.(*agent.ResultMessage); ok {
			select {
			case o.done <- struct{}{}:
			default:
			}
		}
	}
	return nil
}
func (o *recordObserver) HandleMessage(m agent.Message) (response []byte, handled bool, err error) {
	if provenance := m.Provenance(); provenance != nil && provenance.IsChild() {
		return nil, false, nil
	}
	if o.protocol != nil {
		response, handled, err := o.protocol.HandleMessage(m)
		if response != nil || handled || err != nil {
			return response, handled, err
		}
	}
	if pending, ok := m.(*agent.PendingUserActionMessage); ok && pending.Action.Kind == agent.PendingUserActionAskUserQuestion {
		if o.askAnswer == "" {
			return nil, false, errors.New("AskUserQuestion control request needs a scenario ask answer")
		}
		input, err := json.Marshal(pending.Action.Ask)
		if err != nil {
			return nil, false, err
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(input, &raw); err != nil {
			return nil, false, err
		}
		updated, err := askUserQuestionUpdatedInput(raw, o.askAnswer)
		if err != nil {
			return nil, false, err
		}
		data, err := claudeResponseBytes(pending.Action.RequestID, updated)
		return data, false, err
	}
	raw, ok := m.(*agent.RawMessage)
	if !ok || !isClaudeControlRequest(raw.Raw) {
		return nil, false, nil
	}
	data, err := claudeControlResponse(raw.Raw, o.askAnswer)
	return data, true, err
}

// waitForLocalFile polls a local path until it appears or the timeout elapses.
func waitForLocalFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not found after %v", path, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func claudeControlResponse(line []byte, askAnswer string) ([]byte, error) {
	if !isClaudeControlRequest(line) {
		return nil, nil
	}
	var msg claudedto.OutputControlRequestMsg
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal control request: %w", err)
	}
	can, err := msg.DecodeCanUseTool()
	if err != nil {
		return nil, fmt.Errorf("decode can_use_tool request: %w", err)
	}
	if can.Subtype != claudedto.ControlCanUseTool {
		return nil, fmt.Errorf("unsupported control request subtype %q", can.Subtype)
	}

	var updatedInput json.RawMessage
	if can.ToolName == "AskUserQuestion" {
		if askAnswer == "" {
			return nil, errors.New("AskUserQuestion control request needs a scenario ask answer")
		}
		var err error
		updatedInput, err = askUserQuestionUpdatedInput(can.Input, askAnswer)
		if err != nil {
			return nil, err
		}
	}
	return claudeResponseBytes(msg.RequestID, updatedInput)
}

func claudeResponseBytes(requestID string, updatedInput json.RawMessage) ([]byte, error) {
	res := claudedto.InputControlResponseMsg{
		Type: claudedto.InputControlResponse,
		Response: claudedto.ControlResponse{
			Subtype: claudedto.ControlResponseSuccess, RequestID: requestID,
			Response: claudedto.ControlResponsePayload{Behavior: claudedto.ControlCanUseToolBehaviorAllow, UpdatedInput: updatedInput},
		},
	}
	data, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("marshal control response: %w", err)
	}
	return append(data, '\n'), nil
}

func isClaudeControlRequest(line []byte) bool {
	var probe struct {
		Type claudedto.OutputType `json:"type"`
	}
	return json.Unmarshal(line, &probe) == nil && probe.Type == claudedto.OutputControlRequest
}

func askUserQuestionUpdatedInput(raw map[string]json.RawMessage, answer string) (json.RawMessage, error) {
	inputRaw, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal AskUserQuestion input: %w", err)
	}
	var input claudedto.AskUserQuestionInput
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		return nil, fmt.Errorf("unmarshal AskUserQuestion input: %w", err)
	}
	if len(input.Questions) == 0 {
		return nil, errors.New("AskUserQuestion input has no questions")
	}
	answers := make(map[string]string, len(input.Questions))
	for _, q := range input.Questions {
		answers[q.Question] = answer
	}
	updatedInput, err := json.Marshal(claudedto.AskUserQuestionUpdatedInput{
		Questions: input.Questions,
		Answers:   answers,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal AskUserQuestion updated input: %w", err)
	}
	return updatedInput, nil
}

// writeGoldenFile publishes the complete closed recording journal as sanitized
// JSONL evidence and renders its golden discussion.
func writeGoldenFile(ctx context.Context, journalPath string, b agent.Backend, sc scenario, outputPath string) error {
	raw, err := os.ReadFile(filepath.Clean(journalPath))
	if err != nil {
		return fmt.Errorf("read recording: %w", err)
	}
	// The store initialized the header; buildGoldenContent writes the sanitized
	// fixture header and completion footer around the original ordered records.
	_, raw, ok := bytes.Cut(raw, []byte{'\n'})
	if !ok {
		return errors.New("recording has no metadata header")
	}

	sanitized, err := buildGoldenContent(raw, b.Harness(), sc.prompt)
	if err != nil {
		return err
	}

	// Validate before publishing: a scenario that must delegate may not replace
	// the retained golden files with evidence-free content.
	if err := validateGoldenRecording(outputPath, sanitized, b, sc.wantNative); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Clean(outputPath), []byte(sanitized), 0o600); err != nil { //nolint:gosec // output path is from flag
		return err
	}

	lines := len(strings.Split(strings.TrimSpace(sanitized), "\n"))
	slog.InfoContext(ctx, "Trace saved", "path", outputPath, "lines", lines)

	// Generate the golden markdown file.
	mdPath := strings.TrimSuffix(outputPath, ".jsonl") + ".md"
	md, err := taskhistory.ExportDiscussion(outputPath, agent.Backends{b.Harness(): b})
	if err != nil {
		return fmt.Errorf("export discussion: %w", err)
	}
	if err := os.WriteFile(filepath.Clean(mdPath), []byte(md), 0o600); err != nil {
		return fmt.Errorf("write golden md: %w", err)
	}
	slog.InfoContext(ctx, "Golden markdown saved", "path", mdPath)
	return nil
}

// validateGoldenRecording replays a recording through the recorder's own wire
// and reports the native-subagent lifecycle it contains. wantNative requires the
// recording to hold at least one native observation, so a scenario that asks the
// harness to delegate cannot publish evidence that proves nothing.
func validateGoldenRecording(path, sanitized string, b agent.Backend, wantNative bool) error {
	parser, err := agent.NewLogRecordParser(b.NewWire())
	if err != nil {
		return err
	}
	var statuses []string
	scanner := bufio.NewScanner(strings.NewReader(sanitized))
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		record, err := parser.ParseRecord(scanner.Bytes())
		if err != nil {
			return fmt.Errorf("parse recording %s: %w", path, err)
		}
		for _, message := range record.Messages {
			if subagent, ok := message.Message.(*agent.NativeSubagentMessage); ok {
				statuses = append(statuses, string(subagent.Subagent.Status))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read recording %s: %w", path, err)
	}
	slog.Info("recording native activity", "path", path, "harness", b.Harness(), "observations", len(statuses), "statuses", statuses)
	if wantNative && len(statuses) == 0 {
		return fmt.Errorf("%s recorded no native subagent activity for %s", path, b.Harness())
	}
	return nil
}

// buildGoldenContent prepends a caic_meta header and appends a caic_result footer.
func buildGoldenContent(raw []byte, harnessName harness.Name, promptText string) (string, error) {
	meta := v3.MetaMessage{
		MessageType: "caic_meta",
		Version:     agent.LogVersionV3,
		Prompt:      promptText,
		Harness:     string(harnessName),
		Repos:       []v3.MetaRepo{},
		StartedAt:   time.Now().UTC(),
	}
	metaJSON, err := taskslog.EncodeHeader(&meta)
	if err != nil {
		return "", fmt.Errorf("marshal meta: %w", err)
	}

	result := agent.MetaResultMessage{
		MessageType: "caic_result",
		State:       "completed",
	}
	resultJSON, err := agent.MarshalLogMessage(&result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}

	return sanitize(string(metaJSON) + "\n" + string(raw) + string(resultJSON) + "\n"), nil
}

var (
	reSessionID = regexp.MustCompile(`"session_id"\s*:\s*"[^"]*"`)
	reUUID      = regexp.MustCompile(`"uuid"\s*:\s*"[^"]*"`)
	reSecret    = regexp.MustCompile(`(?i)(api_key|token|secret)\s*:\s*"[^"]*"`)
	reContainer = regexp.MustCompile(`"container"\s*:\s*"[^"]*"`)
)

func sanitize(text string) string {
	text = reSessionID.ReplaceAllString(text, `"session_id":"test-session"`)
	text = reUUID.ReplaceAllString(text, `"uuid":"test-uuid"`)
	text = strings.ReplaceAll(text, "/home/user/", "/workspace/")
	text = reSecret.ReplaceAllString(text, `$1:"REDACTED"`)
	text = reContainer.ReplaceAllString(text, `"container":"test-container"`)
	return text
}

func runPodman(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "podman", args...) //nolint:gosec // args from trusted source
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// setupCodexAuth pipes the API key through codex login inside the container.
// When no API key env var is configured or populated, Codex uses the mounted
// logged-in account from ~/.codex when available.
func setupCodexAuth(ctx context.Context, ctr, apiKeyEnv string) error {
	apiKey := os.Getenv(apiKeyEnv)
	if apiKeyEnv == "" || apiKey == "" {
		slog.InfoContext(ctx, "Skipping codex API-key auth setup; using mounted login when available")
		return nil
	}
	cmd := exec.CommandContext(ctx, "podman", "exec", "-i", ctr, //nolint:gosec // podman args are safe
		"bash", "-c", "codex --no-daemon login --with-api-key")
	cmd.Stdin = strings.NewReader(apiKey)
	cmd.Stderr = os.Stderr
	slog.InfoContext(ctx, "Setting up codex auth in container")
	return cmd.Run()
}

// waitForFile polls the container for a file until it appears or timeout is reached.
func waitForFile(ctx context.Context, ctr, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		cmd := exec.CommandContext(ctx, "podman", "exec", ctr, //nolint:gosec // podman args are safe
			"test", "-f", path)
		if cmd.Run() == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not found in container after %v", path, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func main() {
	if err := mainImpl(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}
