// Records agent session traces through relay.py for golden-file tests.
//
// Usage:
//
//	record-trace --harness pi --scenario read-edit-bash
//
// Requires: podman. It mounts logged-in harness credentials when available;
// --api-key-env is optional for API-key based recording.
package main

import (
	"bufio"
	"bytes"
	"context"
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

	claudedto "github.com/maruel/genai/providers/claudecode"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
	"github.com/caic-xyz/caic/backend/internal/agent/relay"
	"github.com/caic-xyz/caic/backend/internal/task"
)

const image = "ghcr.io/caic-xyz/md-user:latest"

// backends is the registry of supported harnesses for record-trace.
var backends = map[string]agent.Backend{
	string(harness.Pi):       pi.New("", nil),
	string(harness.Claude):   claudecode.New(),
	string(harness.Codex):    codex.New("", nil),
	string(harness.OpenCode): opencode.New("", nil),
}

type scenario struct {
	prompt    string
	askAnswer string
}

var scenarios = map[string]scenario{
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
	harnessFlag := flag.String("harness", "", "harness to record (pi, claude, codex, opencode)")
	scenarioFlag := flag.String("scenario", "", "predefined scenario name")
	modelFlag := flag.String("model", "", "model to use (e.g. xiaomi/mimo-v2.5)")
	apiKeyEnv := flag.String("api-key-env", "", "env var name for optional API key")
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
	outputPath := filepath.Clean(filepath.Join("testdata", *scenarioFlag+".jsonl"))
	if _, err := os.Stat(outputPath); err == nil {
		// Skip if the golden file already exists (idempotent for go generate).
		return nil
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return recordTrace(ctx, b, *apiKeyEnv, sc, outputPath, *modelFlag)
}

func recordTrace(ctx context.Context, b agent.Backend, apiKeyEnv string, sc scenario, outputPath, model string) error {
	workDir, err := setupWorkspace()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	ctr, err := startContainer(ctx, workDir, b.Harness(), apiKeyEnv)
	if err != nil {
		return err
	}
	defer func() {
		_ = exec.CommandContext(context.WithoutCancel(ctx), "podman", "stop", ctr).Run() //nolint:gosec // container ID from podman
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
	binDirs, err := detectBinDirs(ctx, ctr)
	if err != nil {
		return fmt.Errorf("detect tool paths: %w", err)
	}
	cmd, stdin, stdout, err := startRelayAgent(ctx, ctr, b, model, binDirs)
	if err != nil {
		return err
	}
	var wire agent.WireFormat
	if hs, ok := b.(agent.RecordHandshaker); ok {
		wire, stdout, err = hs.RecordHandshake(ctx, stdin, stdout, model)
		if err != nil {
			_ = cmd.Process.Kill()
			return err
		}
	}
	if err := sendCommands(stdin, b, model, sc.prompt, wire); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	if err := waitAndShutdown(ctx, cmd, stdin, stdout, b, ctr, wire, sc.askAnswer); err != nil {
		return err
	}
	return writeGoldenFile(ctx, ctr, workDir, b, sc.prompt, outputPath)
}

// setupWorkspace creates a temp directory with a sample main.go for the agent.
func setupWorkspace() (string, error) {
	workDir, err := os.MkdirTemp("", "caic-record-")
	if err != nil {
		return "", err
	}
	sampleMain := []byte("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello, World!\")\n}\n")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), sampleMain, 0o600); err != nil {
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
func deployRelay(ctx context.Context, ctr string) error {
	cmd := exec.CommandContext(ctx, "podman", "exec", "-i", ctr, //nolint:gosec // args from trusted source
		"sh", "-c", "mkdir -p "+agent.RelayDir+" && cat > "+agent.RelayScriptPath)
	cmd.Stdin = bytes.NewReader(relay.Script)
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

// startRelayAgent launches the relay with the agent process inside the container.
func startRelayAgent(ctx context.Context, ctr string, b agent.Backend, model, binDirs string) (*exec.Cmd, io.WriteCloser, io.Reader, error) {
	slog.InfoContext(ctx, "Starting agent via relay", "harness", b.Harness())
	export := ""
	if binDirs != "" {
		export = "export PATH=" + binDirs + ":$PATH && "
	}
	agentArgs := b.AgentArgs(agent.HarnessArgs{Model: model})
	bashCmd := export + "python3 " + agent.RelayScriptPath + " serve-attach --dir /workspace -- " + strings.Join(agentArgs, " ")
	relayFullCmd := []string{"podman", "exec", "-i", ctr, "bash", "-c", bashCmd}

	cmd := exec.CommandContext(ctx, relayFullCmd[0], relayFullCmd[1:]...) //nolint:gosec // args are not user-controlled
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

// sendCommands sends any pre-prompt harness setup and then the prompt.
// When wire is non-nil (from a RecordHandshake), it is used instead of
// NewWire() and PrePromptWriter is skipped (handshake already set up).
func sendCommands(stdin io.WriteCloser, b agent.Backend, model, promptText string, wire agent.WireFormat) error {
	if wire == nil {
		if pp, ok := b.(agent.PrePromptWriter); ok {
			if err := pp.WritePrePrompt(stdin, model, agent.DiscardLogSink); err != nil {
				return err
			}
		}
		wire = b.NewWire()
	}
	slog.Info("Sending prompt", "text", fmt.Sprintf("%.60s...", promptText))
	return wire.WritePrompt(stdin, agent.Prompt{Text: promptText}, agent.DiscardLogSink)
}

// waitAndShutdown watches for a ResultMessage, then sends the null-byte sentinel and waits.
func waitAndShutdown(ctx context.Context, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader, b agent.Backend, ctr string, wire agent.WireFormat, askAnswer string) error {
	if wire == nil {
		wire = b.NewWire()
	}
	agentDone := watchAgent(stdout, stdin, wire.ParseMessage, askAnswer)

	slog.InfoContext(ctx, "Waiting for agent to finish")
	select {
	case err := <-agentDone:
		if err != nil {
			_ = cmd.Process.Kill()
			return err
		}
		slog.InfoContext(ctx, "Agent completed, stopping relay")
	case <-time.After(120 * time.Second):
		slog.WarnContext(ctx, "Timeout waiting for agent, terminating")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return ctx.Err()
	}

	_, _ = stdin.Write([]byte("\x00\n"))
	_ = stdin.Close()
	_ = cmd.Wait()

	return waitForFile(ctx, ctr, agent.RelayOutputPath, 20*time.Second)
}

// watchAgent reads NDJSON from stdout, answers Claude control requests, and
// signals when a ResultMessage is parsed.
func watchAgent(stdout io.Reader, stdin io.Writer, parse func([]byte) ([]agent.Message, error), askAnswer string) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 32<<20)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			if err := answerClaudeControlRequest(stdin, line, askAnswer); err != nil {
				done <- err
				return
			}
			msgs, err := parse(line)
			if err != nil {
				continue
			}
			for _, m := range msgs {
				if _, ok := m.(*agent.ResultMessage); ok {
					done <- nil
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			done <- err
		}
	}()
	return done
}

func answerClaudeControlRequest(w io.Writer, line []byte, askAnswer string) error {
	if !isClaudeControlRequest(line) {
		return nil
	}
	var msg claudedto.OutputControlRequestMsg
	if err := json.Unmarshal(line, &msg); err != nil {
		return fmt.Errorf("unmarshal control request: %w", err)
	}
	can, err := msg.DecodeCanUseTool()
	if err != nil {
		return fmt.Errorf("decode can_use_tool request: %w", err)
	}
	if can.Subtype != claudedto.ControlCanUseTool {
		return fmt.Errorf("unsupported control request subtype %q", can.Subtype)
	}

	res := claudedto.InputControlResponseMsg{
		Type: claudedto.InputControlResponse,
		Response: claudedto.ControlResponse{
			Subtype:   claudedto.ControlResponseSuccess,
			RequestID: msg.RequestID,
			Response: claudedto.ControlResponsePayload{
				Behavior: claudedto.ControlCanUseToolBehaviorAllow,
			},
		},
	}
	if can.ToolName == "AskUserQuestion" {
		if askAnswer == "" {
			return errors.New("AskUserQuestion control request needs a scenario ask answer")
		}
		updatedInput, err := askUserQuestionUpdatedInput(can.Input, askAnswer)
		if err != nil {
			return err
		}
		res.Response.Response.UpdatedInput = updatedInput
	}
	data, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal control response: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
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

// writeGoldenFile copies output.jsonl from the container, sanitizes it,
// writes the JSONL trace, and generates the .golden.md markdown file.
func writeGoldenFile(ctx context.Context, ctr, workDir string, b agent.Backend, promptText, outputPath string) error {
	slog.InfoContext(ctx, "Copying output.jsonl")
	localOutput := filepath.Join(workDir, "output.jsonl")
	if err := runPodman(ctx, "cp", ctr+":"+agent.RelayOutputPath, localOutput); err != nil {
		return fmt.Errorf("copy output: %w", err)
	}

	raw, err := os.ReadFile(localOutput) //nolint:gosec // temp dir is safe
	if err != nil {
		return fmt.Errorf("read output: %w", err)
	}

	sanitized, err := buildGoldenContent(raw, b.Harness(), promptText)
	if err != nil {
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
	md, err := task.ExportDiscussion(outputPath, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
		if h != b.Harness() {
			return nil, fmt.Errorf("golden log harness %q does not match recorder harness %q", h, b.Harness())
		}
		return b.NewWire().ParseMessage, nil
	})
	if err != nil {
		return fmt.Errorf("export discussion: %w", err)
	}
	if err := os.WriteFile(filepath.Clean(mdPath), []byte(md), 0o600); err != nil {
		return fmt.Errorf("write golden md: %w", err)
	}
	slog.InfoContext(ctx, "Golden markdown saved", "path", mdPath)
	return nil
}

// buildGoldenContent prepends a caic_meta header and appends a caic_result footer.
func buildGoldenContent(raw []byte, harnessName harness.Name, promptText string) (string, error) {
	meta := agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     1,
		Prompt:      promptText,
		Harness:     harnessName,
		Repos:       []agent.MetaRepo{},
		StartedAt:   time.Now().UTC(),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("marshal meta: %w", err)
	}

	result := agent.MetaResultMessage{
		MessageType: "caic_result",
		State:       "completed",
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}

	return sanitize(string(metaJSON) + "\n" + string(raw) + "\n" + string(resultJSON) + "\n"), nil
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
		"bash", "-c", "codex login --with-api-key")
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
