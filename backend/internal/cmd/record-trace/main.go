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
	pi "github.com/maruel/genai/providers/pi"
)

const (
	image   = "ghcr.io/caic-xyz/md-user:latest"
	relayPy = "backend/internal/agent/relay/relay.py"
)

// terminalEvent maps each harness to the JSON "type" value that signals
// the agent has finished its work.
var terminalEvent = map[string]string{
	string(agent.Pi):     string(pi.EventAgentEnd),
	string(agent.Claude): (&agent.ResultMessage{}).Type(),
	string(agent.Codex):  (&agent.ResultMessage{}).Type(),
}

var scenarios = map[string]string{
	"read-edit-bash": `Read main.go, edit the greeting on line 3 from "Hello" to "Hi", then run cat main.go.`,
	"tool-error":     "Read the file nonexistent_file.txt",
}

var harnessCmd = map[string][]string{
	string(agent.Pi):     {"pi", "--mode", "rpc"},
	string(agent.Claude): {"claude", "--output-format", "stream-json", "--verbose"},
	string(agent.Codex):  {"codex", "--full-auto"},
}

func mainImpl() error {
	harnessFlag := flag.String("harness", "", "harness to record (pi, claudecode, codex)")
	scenarioFlag := flag.String("scenario", "", "predefined scenario name")
	modelFlag := flag.String("model", "", "model to use (e.g. xiaomi/mimo-v2.5)")
	apiKeyEnv := flag.String("api-key-env", "", "env var name for API key")
	flag.Parse()

	if *harnessFlag == "" {
		return errors.New("--harness is required")
	}
	cmdArgs, ok := harnessCmd[*harnessFlag]
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

	apiKey := os.Getenv(*apiKeyEnv)
	if apiKey == "" {
		return errors.New("--api-key-env must name an env var that is set")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return recordTrace(ctx, *harnessFlag, cmdArgs, promptText, outputPath, *modelFlag, apiKey)
}

func recordTrace(ctx context.Context, harness string, cmdArgs []string, promptText, outputPath, model, apiKey string) error {
	root, err := findRoot()
	if err != nil {
		return fmt.Errorf("find repo root: %w", err)
	}

	workDir, err := setupWorkspace()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	ctr, err := startContainer(ctx, workDir, apiKey)
	if err != nil {
		return err
	}
	defer func() {
		_ = exec.CommandContext(context.WithoutCancel(ctx), "podman", "stop", ctr).Run() //nolint:gosec // container ID from podman
	}()
	if err := deployRelay(ctx, ctr, root); err != nil {
		return err
	}
	binDirs, err := detectBinDirs(ctx, ctr)
	if err != nil {
		return fmt.Errorf("detect tool paths: %w", err)
	}
	cmd, stdin, stdout, err := startRelayAgent(ctx, ctr, harness, cmdArgs, binDirs)
	if err != nil {
		return err
	}
	if err := sendCommands(stdin, harness, model, promptText); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	if err := waitAndShutdown(ctx, cmd, stdin, stdout, harness, ctr); err != nil {
		return err
	}
	return writeGoldenFile(ctx, ctr, workDir, harness, promptText, outputPath)
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

// startContainer pulls the image, starts a container.
func startContainer(ctx context.Context, workDir, apiKey string) (ctr string, err error) {
	slog.Info("Pulling image", "image", image)
	if pullErr := runPodman(ctx, "pull", image); pullErr != nil {
		return "", fmt.Errorf("pull image: %w", pullErr)
	}

	slog.Info("Starting container")
	ctrBytes, runErr := exec.CommandContext(ctx, "podman", "run", "-d", "--rm", //nolint:gosec // podman args are safe
		"-v", workDir+":/workspace:rw",
		"-e", "XIAOMI_API_KEY="+apiKey,
		image, "sleep", "300",
	).Output()
	if runErr != nil {
		return "", fmt.Errorf("start container: %w", runErr)
	}
	return strings.TrimSpace(string(ctrBytes)), nil
}

// deployRelay copies relay.py into the container.
func deployRelay(ctx context.Context, ctr, root string) error {
	if err := runPodman(ctx, "exec", ctr, "mkdir", "-p", "/tmp/caic-relay"); err != nil {
		return fmt.Errorf("mkdir relay dir: %w", err)
	}
	relaySrc := filepath.Join(root, relayPy)
	if err := runPodman(ctx, "cp", relaySrc, ctr+":/tmp/caic-relay/relay.py"); err != nil {
		return fmt.Errorf("copy relay.py: %w", err)
	}
	return nil
}

// detectBinDirs finds pi and node bin directories inside the container by
// checking well-known install paths. Returns a colon-separated PATH fragment.
func detectBinDirs(ctx context.Context, ctr string) (string, error) {
	cmd := exec.CommandContext(ctx, "podman", "exec", ctr, //nolint:gosec // podman args are safe
		"bash", "-c",
		// Probe well-known install locations for pi and node. These
		// directories are not on the default PATH in a non-interactive
		// container, so command -v is unreliable.
		`test -x /home/user/.local/share/pnpm/bin/pi && printf ':%s' /home/user/.local/share/pnpm/bin
set -- /home/user/.nvm/versions/node/v*/bin; test -x "$1/node" && printf ':%s' "$1"`,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("detect paths: %w", err)
	}
	paths := strings.TrimSpace(string(out))
	if paths == "" {
		return "", nil
	}
	slog.Info("Detected tool paths in container", "paths", paths)
	return paths, nil
}

// startRelayAgent launches the relay with the agent process inside the container.
func startRelayAgent(ctx context.Context, ctr, harness string, cmdArgs []string, binDirs string) (*exec.Cmd, io.WriteCloser, io.Reader, error) {
	slog.Info("Starting agent via relay", "harness", harness)
	export := ""
	if binDirs != "" {
		export = "export PATH=" + binDirs + ":$PATH && "
	}
	bashCmd := export + "python3 /tmp/caic-relay/relay.py serve-attach --dir /workspace -- " + strings.Join(cmdArgs, " ")
	relayFullCmd := []string{
		"podman", "exec", "-i", ctr,
		"bash", "-c", bashCmd,
	}

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

// sendCommands optionally sends set_model (pi only) and always sends the prompt.
func sendCommands(stdin io.WriteCloser, harness, model, promptText string) error {
	if harness == string(agent.Pi) && model != "" {
		if err := sendSetModel(stdin, model); err != nil {
			return err
		}
	}
	return sendPrompt(stdin, promptText)
}

// sendSetModel sends a set_model command to the pi agent.
func sendSetModel(w io.WriteCloser, model string) error {
	slog.Info("Setting model", "model", model)
	provider, modelID := "", model
	if p, m, ok := strings.Cut(model, "/"); ok {
		provider, modelID = p, m
	}
	cmd := pi.SetModelCmd{
		Type:     pi.CmdSetModel,
		Provider: provider,
		ModelID:  modelID,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal set_model: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write set_model: %w", err)
	}
	return nil
}

// sendPrompt sends a prompt command via the relay.
func sendPrompt(w io.WriteCloser, text string) error {
	slog.Info("Sending prompt", "text", fmt.Sprintf("%.60s...", text))
	cmdJSON, err := json.Marshal(map[string]string{
		"type":    "prompt",
		"message": text,
	})
	if err != nil {
		return fmt.Errorf("marshal prompt: %w", err)
	}
	if _, err := w.Write(append(cmdJSON, '\n')); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	return nil
}

// waitAndShutdown watches for the terminal event, then sends the null-byte sentinel and waits.
func waitAndShutdown(ctx context.Context, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader, harness, ctr string) error {
	agentDone := watchTerminalEvent(stdout, terminalEvent[harness])

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

	// The relay may still be flushing output.jsonl to disk. Poll until
	// the file is visible in the container.
	return waitForFile(ctx, ctr, "/tmp/caic-relay/output.jsonl", 20*time.Second)
}

// watchTerminalEvent reads NDJSON from stdout and signals when the terminal event type is seen.
func watchTerminalEvent(stdout io.Reader, termType string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 32<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &probe) == nil && probe.Type == termType {
				_, _ = io.ReadAll(stdout)
				return
			}
		}
	}()
	return done
}

// writeGoldenFile copies output.jsonl from the container, sanitizes it, and writes the golden file.
func writeGoldenFile(ctx context.Context, ctr, workDir, harness, promptText, outputPath string) error {
	slog.Info("Copying output.jsonl")
	localOutput := filepath.Join(workDir, "output.jsonl")
	if err := runPodman(ctx, "cp", ctr+":/tmp/caic-relay/output.jsonl", localOutput); err != nil {
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

// buildGoldenContent prepends a caic_meta header and appends a caic_result footer to raw output.
func buildGoldenContent(raw []byte, harness, promptText string) (string, error) {
	meta := map[string]any{
		"type":       "caic_meta",
		"version":    1,
		"prompt":     promptText,
		"harness":    harness,
		"repos":      []any{},
		"started_at": time.Now().UTC().Format(time.RFC3339),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("marshal meta: %w", err)
	}

	result := map[string]any{
		"type":  "caic_result",
		"state": "completed",
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

func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found")
		}
		dir = parent
	}
}

func main() {
	if err := mainImpl(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}
