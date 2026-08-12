// Real runtime fixtures for the caic smoke test.

package smoketest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// SmokeRuntime returns the container runtime selected for smoke tests.
func SmokeRuntime() string {
	if rt := os.Getenv("CAIC_SMOKE_RUNTIME"); rt != "" {
		return rt
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return "docker"
	}
	if _, err := exec.LookPath("podman"); err == nil {
		return "podman"
	}
	return "docker"
}

// SmokeBackend is a deterministic no-LLM agent backend that runs inside the
// real md container through the normal relay over SSH.
type SmokeBackend struct {
	agent.Base
}

var _ agent.Backend = (*SmokeBackend)(nil)

// NewSmokeBackend creates the deterministic relay-backed smoke agent.
func NewSmokeBackend() *SmokeBackend {
	b := &SmokeBackend{}
	b.Base = agent.Base{
		HarnessID:     harness.Codex,
		Inventory:     agent.ModelInventory{Models: []agent.Model{{ID: "smoke-model"}}},
		ContextWindow: 200_000,
	}
	return b
}

// Start deploys the smoke agent into the container and starts it via a relay
// that relies on WritePrompt to record its plain-text input.
func (b *SmokeBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	if err := deploySmokeAgent(ctx, opts.Target); err != nil {
		return nil, err
	}
	return agent.StartRelay(ctx, opts, b.AgentArgs(agent.HarnessArgs{}), b)
}

// AttachRelay implements agent.Backend.
func (b *SmokeBackend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, b, nil)
}

// AgentArgs implements agent.Backend.
func (*SmokeBackend) AgentArgs(agent.HarnessArgs) []string {
	return []string{"python3", "-u", smokeAgentPath}
}

// NewWire implements agent.Backend.
func (*SmokeBackend) NewWire() agent.WireFormat {
	return NewSmokeBackend()
}

// WritePrompt writes the prompt as plain text.
func (*SmokeBackend) WritePrompt(w io.Writer, p agent.Prompt, log agent.LogSink) error {
	return agent.PlainTextWritePrompt(w, p, log)
}

// ParseMessage decodes one smoke-agent NDJSON line.
func (*SmokeBackend) ParseMessage(line []byte) ([]agent.Message, error) {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	switch env.Type {
	case "init":
		var m agent.InitMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "text":
		var m agent.TextMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "result":
		var m agent.ResultMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	default:
		return []agent.Message{&agent.RawMessage{
			MessageType: env.Type,
			Raw:         append([]byte(nil), line...),
		}}, nil
	}
}

const smokeAgentScript = `
import json
import sys

print(json.dumps({
    "type": "init",
    "session_id": "smoke-session",
    "model": "smoke-model",
}), flush=True)

for line in sys.stdin:
    if "\x00" in line:
        break
    text = line.strip()
    if not text:
        continue
    result = "smoke agent received: " + text
    print(json.dumps({"type": "text", "text": result}), flush=True)
    print(json.dumps({
        "type": "result",
        "subtype": "success",
        "duration_ms": 1,
        "num_turns": 1,
        "result": result,
        "session_id": "smoke-session",
        "total_cost_usd": 0,
    }), flush=True)
`

const smokeAgentPath = agent.RelayDir + "/smoke_agent.py"

func deploySmokeAgent(ctx context.Context, target runtime.ConnectionTarget) error {
	if target.SSHHost == "" {
		return errors.New("agent connection target missing SSH host")
	}
	cmd := exec.CommandContext(ctx, "ssh", target.SSHHost, "mkdir -p "+agent.RelayDir+" && cat > "+smokeAgentPath) //nolint:gosec // target and path are internally controlled.
	cmd.Stdin = bytes.NewReader([]byte(smokeAgentScript))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy smoke agent: %w: %s", err, out)
	}
	return nil
}

// InitSmokeRepos creates two local repositories for the runtime smoke test.
func InitSmokeRepos(ctx context.Context, tmpDir string) (string, error) {
	if err := initOneRepo(ctx, tmpDir, filepath.Join("remotes", "remote.git"), filepath.Join("repos", "clone")); err != nil {
		return "", err
	}
	if err := initOneRepo(ctx, tmpDir, filepath.Join("remotes", "remote2.git"), filepath.Join("repos", "clone2")); err != nil {
		return "", err
	}
	return filepath.Join(tmpDir, "repos", "clone"), nil
}

// InitSmokeHarnessCache pre-populates model cache entries used by smoke tests.
func InitSmokeHarnessCache(cacheDir string) error {
	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
	for _, h := range []harness.Name{harness.Codex, harness.Pi, harness.OpenCode} {
		cache.SetModelInventory(h, agent.ModelInventory{Models: []agent.Model{{ID: "smoke-model"}}}, "")
	}
	return nil
}

// WaitForRuntimeGone waits until the runtime no longer knows an instance.
func WaitForRuntimeGone(ctx context.Context, runtimeName string, id runtime.ID) error {
	containerName := string(id.InstanceID())
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("container %s still exists after purge: %w", containerName, ctx.Err())
		default:
		}
		cmd := exec.CommandContext(ctx, runtimeName, "container", "inspect", containerName) //nolint:gosec // runtime and container name are controlled by the smoke test.
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("container %s still exists after purge: %w", containerName, ctx.Err())
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("container %s still exists after purge: %w", containerName, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}
