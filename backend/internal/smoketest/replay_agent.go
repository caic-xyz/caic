// Replays retained native-subagent recordings inside a real md container for the smoke test.

package smoketest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// replayAgentScript writes one retained native harness recording to stdout, one
// recorded payload per line, after waiting for the caic session's first prompt.
// It waits for the test to release the remaining payloads after caic restores
// the running card.
// Synthetic markers supply the turn boundaries missing from the minimized
// recording; a recording that ends with its own prompt response needs no end
// marker, so end_marker is 0.
const replayAgentScript = `
import sys
import time
from pathlib import Path

payloads_path, hold_after, end_marker = sys.argv[1], int(sys.argv[2]), sys.argv[3] == "1"
release_path = Path('` + replayReleasePath + `')

with open(payloads_path, encoding="utf-8") as f:
    payloads = [line.rstrip("\n") for line in f if line.strip()]

for line in sys.stdin:
    if "\x00" in line:
        sys.exit(0)
    if line.strip():
        break

sys.stdout.write('` + replayStartMarker + `\n')
sys.stdout.flush()

for index, payload in enumerate(payloads):
    if index == hold_after:
        while not release_path.exists():
            time.sleep(0.05)
    sys.stdout.write(payload + "\n")
    sys.stdout.flush()
    time.sleep(0.05)

if end_marker:
    sys.stdout.write('` + replayEndMarker + `\n')
    sys.stdout.flush()

for line in sys.stdin:
    if "\x00" in line:
        break
`

const (
	replayAgentPath   = agent.RelayDir + "/replay_agent.py"
	replayPayloadPath = agent.RelayDir + "/replay_payloads.jsonl"
	replayReleasePath = agent.RelayDir + "/replay_release"
	replayStartMarker = `{"caic_replay_start":true}`
	replayEndMarker   = `{"caic_replay_end":true}`
)

// ReplayRecording is one retained native-subagent recording to deliver through a
// caic-managed container.
type ReplayRecording struct {
	// Prompt is the delegation prompt the recording was made with. A backend that
	// serves one harness's several recordings selects the one recorded with the
	// task's prompt.
	Prompt string
	// NativePrompt is the native prompt request the recording answers, or nil.
	// The replay wire sends it as the task's prompt, so the harness wire
	// correlates the recorded prompt response, which then ends the turn.
	NativePrompt []byte
	// Harness names the recording's harness. It selects the container's agent
	// mounts and the task-log harness field; the wire parses the payloads.
	Harness harness.Name
	// Payloads are the recording's native harness records in order.
	Payloads [][]byte
	// HoldAfter is how many payloads the agent emits before pausing.
	HoldAfter int
}

// ReleaseReplay lets the paused replay agent emit the remaining payloads.
func ReleaseReplay(ctx context.Context, runtimeName string, instanceID runtime.InstanceID) error {
	out, err := runSmokeCommand(ctx, runtimeName, "container", "exec", "--user", "user", string(instanceID), "touch", replayReleasePath)
	if err != nil {
		return fmt.Errorf("release replay in %s: %w: %s", instanceID, err, out)
	}
	return nil
}

// ReplayBackend implements agent.Backend by replaying one harness's retained
// recordings through a real md container and the production relay. It has no
// handshake: a recording starts with the native records a live session would
// have already received, so only the wire's parser is exercised.
type ReplayBackend struct {
	agent.Base

	backend    agent.Backend
	recordings []ReplayRecording
}

// NewReplayBackend creates a backend that replays one harness's recordings inside
// the container. A task's prompt selects among them. backend supplies independent full and metadata parsers for each projection.
func NewReplayBackend(backend agent.Backend, recordings ...ReplayRecording) (*ReplayBackend, error) {
	if len(recordings) == 0 {
		return nil, errors.New("replay backend needs at least one recording")
	}
	if backend == nil {
		return nil, errors.New("replay backend needs a provider backend")
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
	b := &ReplayBackend{backend: backend, recordings: recordings}
	b.Base = agent.Base{HarnessID: recordings[0].Harness}
	b.SetModelInventory(agent.ModelInventory{Models: []agent.Model{{ID: "replay-model", ContextWindow: 200_000}}})
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
	endMarker := "1"
	if rec.NativePrompt != nil {
		endMarker = "0"
	}
	args := []string{
		"python3", "-u", replayAgentPath, replayPayloadPath,
		strconv.Itoa(rec.HoldAfter), endMarker,
	}
	session, err := agent.StartRelay(ctx, opts, args)
	if err != nil {
		return nil, err
	}
	// A real harness announces its session before its first record. The replay
	// does the same because a minimized recording holds native subagent records
	// only, and codex and opencode need the persisted session ID to reconnect to
	// their live relay after a caic restart.
	if err := opts.Records.AppendMessage(ctx, &agent.MetaSessionMessage{SessionID: replaySessionID(), ReportedModel: "replay-model"}); err != nil {
		session.Cancel()
		return nil, errors.Join(err, session.Wait())
	}

	return session, nil
}

// AttachRelay implements agent.Backend.
func (b *ReplayBackend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, nil)
}

// AgentArgs implements agent.Backend.
func (*ReplayBackend) AgentArgs(agent.HarnessArgs) []string { return nil }

// NewWire implements agent.Backend. Each call returns an independent wire, so a
// replayed task log never reuses the session's adapter state. It sends a
// recording's native prompt request in place of the task's prompt.
func (b *ReplayBackend) NewWire() agent.WireFormat {
	return replayWire{inner: b.backend.NewWire(), backend: b}
}

// NewMetadataParser reconstructs metadata with the same replay turn boundaries.
func (b *ReplayBackend) NewMetadataParser() agent.MessageParser {
	return replayParser{inner: b.backend.NewMetadataParser()}
}

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

// NewReplayWire wraps a harness wire to parse the replay agent's turn boundaries.
func NewReplayWire(inner agent.WireFormat) agent.WireFormat {
	return replayWire{inner: inner}
}

var _ agent.Backend = (*ReplayBackend)(nil)

// replayWire adds the replay agent's turn boundaries to the harness's native records.
type replayWire struct {
	replayParser

	// backend holds the recordings whose native prompts EncodePrompt sends; nil
	// for a wire that only parses.
	backend *ReplayBackend
}

// EncodePrompt returns the recording's native prompt request when it has one,
// and otherwise the replay process's complete JSON prompt command.
func (w replayWire) EncodePrompt(p agent.Prompt) ([]byte, error) {
	if w.backend != nil {
		if rec, err := w.backend.selectRecording(p.Text); err == nil && rec.NativePrompt != nil {
			return slices.Concat(bytes.TrimSpace(rec.NativePrompt), []byte{'\n'}), nil
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

type replayParser struct{ inner agent.MessageParser }

// ParseOutput interprets replay markers and delegates native records to the harness parser.
func (w replayParser) ParseOutput(line []byte) ([]agent.Message, error) {
	switch string(bytes.TrimSpace(line)) {
	case replayStartMarker:
		return []agent.Message{&agent.TurnStartedMessage{}}, nil
	case replayEndMarker:
		return []agent.Message{&agent.ResultMessage{
			MessageType: "result",
			Subtype:     "success",
			NumTurns:    1,
			Result:      "replayed native subagent recording",
		}}, nil
	}
	return w.inner.ParseOutput(line)
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
	if err := agent.WriteFile(ctx, target.SSHHost, replayAgentPath, []byte(replayAgentScript), 0o644); err != nil {
		return err
	}
	return agent.WriteFile(ctx, target.SSHHost, replayPayloadPath, recording.Bytes(), 0o644)
}

// replaySessionID returns a unique session identity for one replay task.
func replaySessionID() string {
	return "replay-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// ParseInput delegates client records to the same harness wire.
func (w replayParser) ParseInput(line []byte) ([]agent.Message, error) {
	return w.inner.ParseInput(line)
}
