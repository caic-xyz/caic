// Records agent session traces through relay.py for golden-file tests.
//
// Usage:
//
//	record-trace --harness pi --scenario read-edit-bash
//
// Requires: podman, XIAOMI_API_KEY env var.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	image   = "ghcr.io/caic-xyz/md-user:latest"
	relayPy = "backend/internal/agent/relay/relay.py"
)

// terminalEvent maps each harness to the JSON "type" value that signals
// the agent has finished its work.
// TODO: Use agent parser.
var terminalEvent = map[string]string{
	"pi":         "agent_end",
	"claudecode": "result",
	"codex":      "result",
}

var scenarios = map[string]string{
	"read-edit-bash": `Read main.go, edit the greeting on line 3 from "Hello" to "Hi", then run cat main.go.`,
	"tool-error":     "Read the file nonexistent_file.txt",
}

var harnessCmd = map[string][]string{
	"pi":         {"pi", "--mode", "rpc"},
	"claudecode": {"claude", "--output-format", "stream-json", "--verbose"},
	"codex":      {"codex", "--full-auto"},
}

func mainImpl() error {
	harnessFlag := flag.String("harness", "", "harness to record (pi, claudecode, codex)")
	scenarioFlag := flag.String("scenario", "", "predefined scenario name")
	modelFlag := flag.String("model", "", "model to use (e.g. xiaomi/mimo-v2.5)")
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

	// TODO: Make this configurable.
	apiKey := os.Getenv("XIAOMI_API_KEY")
	if apiKey == "" {
		return errors.New("XIAOMI_API_KEY must be set")
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

	// Create temp workspace with a sample file.
	workDir, err := os.MkdirTemp("", "caic-record-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	sampleMain := []byte("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello, World!\")\n}\n")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), sampleMain, 0o600); err != nil {
		return err
	}

	// Pull image.
	fmt.Fprintf(os.Stderr, "Pulling %s...\n", image)
	if err := runPodman(ctx, "pull", image); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	// Start container.
	fmt.Fprintln(os.Stderr, "Starting container...")
	ctrBytes, err := exec.CommandContext(ctx, "podman", "run", "-d", "--rm", //nolint:gosec // podman args are safe
		"-v", workDir+":/workspace:rw",
		"-e", "XIAOMI_API_KEY="+apiKey,
		image, "sleep", "300",
	).Output()
	if err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	ctr := strings.TrimSpace(string(ctrBytes))

	defer func() { _ = exec.CommandContext(ctx, "podman", "stop", ctr).Run() }() //nolint:gosec // container ID from podman

	// Create relay dir and copy relay.py into container.
	if err := runPodman(ctx, "exec", ctr, "mkdir", "-p", "/tmp/caic-relay"); err != nil {
		return fmt.Errorf("mkdir relay dir: %w", err)
	}
	relaySrc := filepath.Join(root, relayPy)
	if err := runPodman(ctx, "cp", relaySrc, ctr+":/tmp/caic-relay/relay.py"); err != nil {
		return fmt.Errorf("copy relay.py: %w", err)
	}

	// Start relay with agent. PATH must include nvm node and pnpm bin.
	fmt.Fprintf(os.Stderr, "Starting %s agent via relay...\n", harness)
	// TODO: Remove hardcoding.
	nodePath := "/home/user/.nvm/versions/node/v24.16.0/bin"
	pnpmPath := "/home/user/.local/share/pnpm/bin"
	bashCmd := "export PATH=" + nodePath + ":" + pnpmPath + ":$PATH && python3 /tmp/caic-relay/relay.py serve-attach --dir /workspace -- " + strings.Join(cmdArgs, " ")
	relayFullCmd := []string{
		"podman", "exec", "-i", ctr,
		"bash", "-c", bashCmd,
	}

	cmd := exec.CommandContext(ctx, relayFullCmd[0], relayFullCmd[1:]...) //nolint:gosec // args are not user-controlled
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start relay: %w", err)
	}

	// Watch stdout for terminal event to know when the agent is done.
	termType := terminalEvent[harness]
	agentDone := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 32<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &probe) == nil && probe.Type == termType {
				close(agentDone)
				// Drain remaining output.
				_, _ = io.ReadAll(stdout)
				return
			}
		}
		// Agent exited without terminal event; still signal done.
		close(agentDone)
	}()

	// Optionally send set_model before prompt (pi harness only).
	if harness == "pi" && model != "" {
		fmt.Fprintf(os.Stderr, "Setting model: %s\n", model)
		provider, modelID := "", model
		if p, m, ok := strings.Cut(model, "/"); ok {
			provider, modelID = p, m
		}
		// TODO: Use pi agent object.
		setModelJSON, err := json.Marshal(map[string]any{
			"type":     "set_model",
			"provider": provider,
			"model_id": modelID,
		})
		if err != nil {
			return fmt.Errorf("marshal set_model: %w", err)
		}
		if _, err := stdin.Write(append(setModelJSON, '\n')); err != nil {
			return fmt.Errorf("write set_model: %w", err)
		}
	}

	// Send prompt.
	fmt.Fprintf(os.Stderr, "Sending prompt: %.60s...\n", promptText)
	cmdJSON, err := json.Marshal(map[string]string{
		"type":    "prompt",
		"message": promptText,
	})
	if err != nil {
		return fmt.Errorf("marshal prompt: %w", err)
	}
	if _, err := stdin.Write(append(cmdJSON, '\n')); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}

	// Wait for agent to complete its work, then stop the relay.
	fmt.Fprintln(os.Stderr, "Waiting for agent to finish...")
	select {
	case <-agentDone:
		fmt.Fprintln(os.Stderr, "Agent completed, stopping relay...")
	case <-time.After(120 * time.Second):
		fmt.Fprintln(os.Stderr, "Timeout waiting for agent, terminating...")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return ctx.Err()
	}

	// Send shutdown sentinel and wait for clean relay exit.
	_, _ = stdin.Write([]byte("\x00\n"))
	_ = stdin.Close()
	_ = cmd.Wait()

	// Copy output.jsonl from container.
	fmt.Fprintln(os.Stderr, "Copying output.jsonl...")
	localOutput := filepath.Join(workDir, "output.jsonl")
	if err := runPodman(ctx, "cp", ctr+":/tmp/caic-relay/output.jsonl", localOutput); err != nil {
		return fmt.Errorf("copy output: %w", err)
	}

	// Sanitize and write.
	raw, err := os.ReadFile(localOutput) //nolint:gosec // temp dir is safe
	if err != nil {
		return fmt.Errorf("read output: %w", err)
	}

	// Prepend caic_meta header.
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
		return fmt.Errorf("marshal meta: %w", err)
	}

	// Append caic_result footer.
	result := map[string]any{
		"type":  "caic_result",
		"state": "completed",
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}

	sanitized := sanitize(string(metaJSON) + "\n" + string(raw) + "\n" + string(resultJSON) + "\n")
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Clean(outputPath), []byte(sanitized), 0o600); err != nil { //nolint:gosec // output path is from flag
		return err
	}

	lines := len(strings.Split(strings.TrimSpace(sanitized), "\n"))
	fmt.Fprintf(os.Stderr, "Trace saved to %s (%d lines)\n", outputPath, lines)
	return nil
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
		log.Fatal(err)
	}
}
