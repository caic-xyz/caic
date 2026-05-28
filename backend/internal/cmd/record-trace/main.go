// Records agent session traces through relay.py for golden-file tests.
//
// Usage:
//
//	record-trace --harness pi --scenario read-edit-bash
//
// Requires: podman, env var (override with --api-key-env).
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

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/gemini"
	"github.com/caic-xyz/caic/backend/internal/agent/kilo"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
	"github.com/caic-xyz/caic/backend/internal/agent/relay"
)

const image = "ghcr.io/caic-xyz/md-user:latest"

// backends is the registry of supported harnesses for record-trace.
var backends = map[string]agent.Backend{
	string(agent.Pi):       pi.New("", nil),
	string(agent.Claude):   claudecode.New(),
	string(agent.Codex):    codex.New(),
	string(agent.Gemini):   gemini.New(),
	string(agent.Kilo):     kilo.New(),
	string(agent.OpenCode): opencode.New("", nil),
}

var scenarios = map[string]string{
	"read-edit-bash": `Read main.go, edit the greeting on line 3 from "Hello" to "Hi", then run cat main.go.`,
	"tool-error":     "Read the file nonexistent_file.txt",
}

func mainImpl() error {
	harnessFlag := flag.String("harness", "", "harness to record (pi, claude, codex, gemini, kilo, opencode)")
	scenarioFlag := flag.String("scenario", "", "predefined scenario name")
	modelFlag := flag.String("model", "", "model to use (e.g. xiaomi/mimo-v2.5)")
	apiKeyEnv := flag.String("api-key-env", "", "env var name for API key")
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
	promptText, ok := scenarios[*scenarioFlag]
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
	return recordTrace(ctx, b, *apiKeyEnv, promptText, outputPath, *modelFlag)
}

func recordTrace(ctx context.Context, b agent.Backend, apiKeyEnv, promptText, outputPath, model string) error {
	workDir, err := setupWorkspace()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	ctr, err := startContainer(ctx, workDir, apiKeyEnv)
	if err != nil {
		return err
	}
	defer func() {
		_ = exec.CommandContext(context.WithoutCancel(ctx), "podman", "stop", ctr).Run() //nolint:gosec // container ID from podman
	}()
	if err := deployRelay(ctx, ctr); err != nil {
		return err
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
	if err := sendCommands(stdin, b, model, promptText, wire); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	if err := waitAndShutdown(ctx, cmd, stdin, stdout, b, ctr, wire); err != nil {
		return err
	}
	return writeGoldenFile(ctx, ctr, workDir, b.Harness(), promptText, outputPath)
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
func startContainer(ctx context.Context, workDir, apiKeyEnv string) (string, error) {
	slog.Info("Pulling image", "image", image)
	if pullErr := runPodman(ctx, "pull", image); pullErr != nil {
		return "", fmt.Errorf("pull image: %w", pullErr)
	}

	slog.Info("Starting container")
	args := []string{"podman", "run", "-d", "--rm", "-v", workDir + ":/workspace:rw"}
	if v := os.Getenv(apiKeyEnv); apiKeyEnv != "" && v != "" {
		args = append(args, "-e", apiKeyEnv+"="+v)
	}
	args = append(args, image, "sleep", "300")
	ctrBytes, runErr := exec.CommandContext(ctx, args[0], args[1:]...).Output() //nolint:gosec // podman args are safe
	if runErr != nil {
		return "", fmt.Errorf("start container: %w", runErr)
	}
	return strings.TrimSpace(string(ctrBytes)), nil
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
		`test -x /home/user/.local/share/pnpm/bin/pi && printf ':%s' /home/user/.local/share/pnpm/bin
test -x /home/user/.opencode/bin/opencode && printf ':%s' /home/user/.opencode/bin
set -- /home/user/.nvm/versions/node/v*/bin; test -x "$1/node" && printf ':%s' "$1"`,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("detect paths: %w", err)
	}
	paths := strings.TrimSpace(string(out))
	if paths != "" {
		slog.Info("Detected tool paths in container", "paths", paths)
	}
	return paths, nil
}

// startRelayAgent launches the relay with the agent process inside the container.
func startRelayAgent(ctx context.Context, ctr string, b agent.Backend, model, binDirs string) (*exec.Cmd, io.WriteCloser, io.Reader, error) {
	slog.Info("Starting agent via relay", "harness", b.Harness())
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
			if err := pp.WritePrePrompt(stdin, model, io.Discard); err != nil {
				return err
			}
		}
		wire = b.NewWire()
	}
	slog.Info("Sending prompt", "text", fmt.Sprintf("%.60s...", promptText))
	return wire.WritePrompt(stdin, agent.Prompt{Text: promptText}, io.Discard)
}

// waitAndShutdown watches for a ResultMessage, then sends the null-byte sentinel and waits.
func waitAndShutdown(ctx context.Context, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader, b agent.Backend, ctr string, wire agent.WireFormat) error {
	if wire == nil {
		wire = b.NewWire()
	}
	agentDone := watchResult(stdout, wire.ParseMessage)

	slog.Info("Waiting for agent to finish")
	select {
	case <-agentDone:
		slog.Info("Agent completed, stopping relay")
	case <-time.After(120 * time.Second):
		slog.Warn("Timeout waiting for agent, terminating")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return ctx.Err()
	}

	_, _ = stdin.Write([]byte("\x00\n"))
	_ = stdin.Close()
	_ = cmd.Wait()

	return waitForFile(ctx, ctr, agent.RelayOutputPath, 20*time.Second)
}

// watchResult reads NDJSON from stdout and signals when a ResultMessage is parsed.
func watchResult(stdout io.Reader, parse func([]byte) ([]agent.Message, error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 32<<20)
		for scanner.Scan() {
			msgs, err := parse(scanner.Bytes())
			if err != nil {
				continue
			}
			for _, m := range msgs {
				if _, ok := m.(*agent.ResultMessage); ok {
					return
				}
			}
		}
	}()
	return done
}

// writeGoldenFile copies output.jsonl from the container, sanitizes it, and writes the golden file.
func writeGoldenFile(ctx context.Context, ctr, workDir string, harness agent.Harness, promptText, outputPath string) error {
	slog.Info("Copying output.jsonl")
	localOutput := filepath.Join(workDir, "output.jsonl")
	if err := runPodman(ctx, "cp", ctr+":"+agent.RelayOutputPath, localOutput); err != nil {
		return fmt.Errorf("copy output: %w", err)
	}

	raw, err := os.ReadFile(localOutput) //nolint:gosec // temp dir is safe
	if err != nil {
		return fmt.Errorf("read output: %w", err)
	}

	sanitized, err := buildGoldenContent(raw, harness, promptText)
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
	slog.Info("Trace saved", "path", outputPath, "lines", lines)
	return nil
}

// buildGoldenContent prepends a caic_meta header and appends a caic_result footer.
func buildGoldenContent(raw []byte, harness agent.Harness, promptText string) (string, error) {
	meta := agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     1,
		Prompt:      promptText,
		Harness:     harness,
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
