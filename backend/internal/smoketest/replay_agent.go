// Replays retained native-subagent recordings inside a real md container for the smoke test.

package smoketest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// replayAgentScript writes one retained native harness recording to stdout, one
// recorded payload per line, after waiting for the caic session's first prompt.
// It pauses before the payload at hold_after for hold_ms so a smoke test can
// restart caic between the recorded running card and the records that settle it.
// The synthetic end marker lets the replay wire end the turn: a minimized
// recording holds native harness records, not a harness turn result.
const replayAgentScript = `
import sys
import time

payloads_path, hold_after, hold_ms = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])

with open(payloads_path, encoding="utf-8") as f:
    payloads = [line.rstrip("\n") for line in f if line.strip()]

for line in sys.stdin:
    if "\x00" in line:
        sys.exit(0)
    if line.strip():
        break

for index, payload in enumerate(payloads):
    if index == hold_after:
        time.sleep(hold_ms / 1000.0)
    sys.stdout.write(payload + "\n")
    sys.stdout.flush()
    time.sleep(0.05)

sys.stdout.write('{"caic_replay_end":true}\n')
sys.stdout.flush()

for line in sys.stdin:
    if "\x00" in line:
        break
`

const (
	replayAgentPath   = agent.RelayDir + "/replay_agent.py"
	replayPayloadPath = agent.RelayDir + "/replay_payloads.jsonl"
)

// replayEndMarker is the synthetic line the replay agent writes after the last
// recorded payload. It never appears in a recording.
var replayEndMarker = []byte(`{"caic_replay_end":true}`)

// ReplayRecording is one retained native-subagent recording to deliver through a
// caic-managed container.
type ReplayRecording struct {
	// Prompt is the delegation prompt the recording was made with. A backend that
	// serves one harness's several recordings selects the one recorded with the
	// task's prompt.
	Prompt string
	// Harness names the recording's harness. It selects the container's agent
	// mounts and the task-log harness field; the wire parses the payloads.
	Harness harness.Name
	// Payloads are the recording's native harness records in order.
	Payloads [][]byte
	// HoldAfter is how many payloads the agent emits before pausing.
	HoldAfter int
	// Hold is how long the agent pauses before the remaining payloads.
	Hold time.Duration
}

// ReplayBackend implements agent.Backend by replaying one harness's retained
// recordings through a real md container and the production relay. It has no
// handshake: a recording starts with the native records a live session would
// have already received, so only the wire's parser is exercised.
type ReplayBackend struct {
	agent.Base

	newWire    func() agent.WireFormat
	recordings []ReplayRecording
}

// NewReplayBackend creates a backend that replays one harness's recordings inside
// the container. A task's prompt selects among them. newWire creates the harness
// wire format; adapters are stateful, so each caller needs its own factory.
func NewReplayBackend(newWire func() agent.WireFormat, recordings ...ReplayRecording) (*ReplayBackend, error) {
	if len(recordings) == 0 {
		return nil, errors.New("replay backend needs at least one recording")
	}
	if newWire == nil {
		return nil, errors.New("replay backend needs a wire factory")
	}
	seen := make(map[string]struct{}, len(recordings))
	for _, rec := range recordings {
		if rec.Harness != recordings[0].Harness {
			return nil, fmt.Errorf("replay recordings mix harnesses %q and %q", recordings[0].Harness, rec.Harness)
		}
		if len(recordings) == 1 {
			continue
		}
		// A task's prompt selects its recording, so the key must identify one.
		if rec.Prompt == "" {
			return nil, fmt.Errorf("replay recording for %s has no prompt to select it by", rec.Harness)
		}
		if _, dup := seen[rec.Prompt]; dup {
			return nil, fmt.Errorf("replay recordings share the prompt %q", rec.Prompt)
		}
		seen[rec.Prompt] = struct{}{}
	}
	b := &ReplayBackend{newWire: newWire, recordings: recordings}
	b.Base = agent.Base{HarnessID: recordings[0].Harness, ContextWindow: 200_000}
	b.SetModelInventory(agent.ModelInventory{Models: []agent.Model{{ID: "replay-model"}}})
	return b, nil
}

// Start deploys the selected recording and the replay agent, then starts the relay.
func (b *ReplayBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	rec, err := b.selectRecording(opts.InitialPrompt.Text)
	if err != nil {
		return nil, err
	}
	if err := deployReplayRecording(ctx, opts.Target, rec.Payloads); err != nil {
		return nil, err
	}
	args := []string{
		"python3", "-u", replayAgentPath, replayPayloadPath,
		strconv.Itoa(rec.HoldAfter), strconv.FormatInt(rec.Hold.Milliseconds(), 10),
	}
	session, err := agent.StartRelay(ctx, opts, args, b.NewWire())
	if err != nil {
		return nil, err
	}
	// A real harness announces its session before its first record. The replay
	// does the same because a minimized recording holds native subagent records
	// only, and codex and opencode need the persisted session ID to reconnect to
	// their live relay after a caic restart.
	init := &agent.InitMessage{SessionID: replaySessionID(), ReportedModel: "replay-model"}
	opts.MsgCh <- agent.TimedMessage{Message: init}
	if err := agent.WriteMetaSession(opts.Log, init); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("write replay session metadata: %w", err)
	}
	return session, nil
}

// AttachRelay implements agent.Backend.
func (b *ReplayBackend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, b.NewWire(), nil)
}

// AgentArgs implements agent.Backend.
func (*ReplayBackend) AgentArgs(agent.HarnessArgs) []string { return nil }

// NewWire implements agent.Backend. Each call returns an independent wire, so a
// replayed task log never reuses the session's adapter state.
func (b *ReplayBackend) NewWire() agent.WireFormat { return NewReplayWire(b.newWire()) }

// selectRecording returns the recording a task's prompt identifies. A backend
// that holds one recording accepts any prompt; a harness that retains several
// recordings selects the one recorded with that exact prompt.
func (b *ReplayBackend) selectRecording(prompt string) (ReplayRecording, error) {
	if len(b.recordings) == 1 {
		return b.recordings[0], nil
	}
	for _, rec := range b.recordings {
		if rec.Prompt == prompt {
			return rec, nil
		}
	}
	held := make([]string, 0, len(b.recordings))
	for _, rec := range b.recordings {
		held = append(held, rec.Prompt)
	}
	return ReplayRecording{}, fmt.Errorf("no replay recording for prompt %q; held %q", prompt, held)
}

// NewReplayWire wraps a harness wire so a replay agent's end marker turns into a
// completed turn, which a minimized recording does not carry.
func NewReplayWire(inner agent.WireFormat) agent.WireFormat { return replayWire{inner: inner} }

var _ agent.Backend = (*ReplayBackend)(nil)

// replayWire parses one recording's native records with the harness's own wire
// and turns the replay agent's end marker into a completed turn.
type replayWire struct {
	inner agent.WireFormat
}

// WritePrompt writes the prompt as plain text: the replay agent only needs to
// know that a turn started.
func (replayWire) WritePrompt(w io.Writer, p agent.Prompt, log agent.LogSink) error {
	return agent.PlainTextWritePrompt(w, p, log)
}

// ParseMessage delegates to the harness wire and ends the turn on the marker.
func (w replayWire) ParseMessage(line []byte) ([]agent.Message, error) {
	if bytes.Equal(bytes.TrimSpace(line), replayEndMarker) {
		return []agent.Message{&agent.ResultMessage{
			MessageType: "result",
			Subtype:     "success",
			NumTurns:    1,
			Result:      "replayed native subagent recording",
		}}, nil
	}
	return w.inner.ParseMessage(line)
}

// deployReplayRecording writes the replay agent and one recording into the
// container, one native payload per line.
func deployReplayRecording(ctx context.Context, target runtime.ConnectionTarget, payloads [][]byte) error {
	if target.SSHHost == "" {
		return errors.New("agent connection target missing SSH host")
	}
	var recording bytes.Buffer
	for _, payload := range payloads {
		recording.Write(bytes.TrimSpace(payload))
		recording.WriteByte('\n')
	}
	if err := writeContainerFile(ctx, target.SSHHost, replayAgentPath, []byte(replayAgentScript)); err != nil {
		return err
	}
	return writeContainerFile(ctx, target.SSHHost, replayPayloadPath, recording.Bytes())
}

// writeContainerFile writes content to path inside the container over SSH.
func writeContainerFile(ctx context.Context, host, path string, content []byte) error {
	cmd := exec.CommandContext(ctx, "ssh", host, "mkdir -p "+agent.RelayDir+" && cat > "+path) //nolint:gosec // host and path are internally controlled.
	cmd.Stdin = bytes.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write %s: %w: %s", path, err, out)
	}
	return nil
}

// replaySessionID returns a unique session identity for one replay task.
func replaySessionID() string {
	return "replay-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}
