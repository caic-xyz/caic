// Replays native-subagent evidence fixtures through the live wire parser and both task-log versions.

package agenttest

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// NativeSubagentEvidence is the canonical lifecycle observation stream for one
// evidence fixture. Live is the wire parser reading native harness records in
// order. V2 and V3 replay the identical native payloads as physical task-log
// records of each version, which is what restart and settled-history replay do.
//
// Adapters are stateful, so each field is produced by a fresh wire format.
type NativeSubagentEvidence struct {
	// Payloads are the fixture's native harness records in file order. Tests
	// split them to model a restart between two records.
	Payloads [][]byte
	Live     []agent.NativeSubagent
	V2       []agent.NativeSubagent
	V3       []agent.NativeSubagent
}

// LoadNativeSubagentEvidence reads a minimized physical fixture at fixtureVersion,
// extracts its native harness payloads, and parses them live and replayed.
//
// The fixture's control records (the caic_meta header and caic_result footer)
// never reach the native parser, so one fixture serves live and replay.
func LoadNativeSubagentEvidence(t testing.TB, path string, fixtureVersion agent.LogVersion, newWire func() agent.WireFormat) NativeSubagentEvidence {
	t.Helper()
	native := nativeSubagentPayloads(t, path, fixtureVersion)
	evidence := NativeSubagentEvidence{Payloads: native, Live: liveSubagents(t, native, newWire())}
	evidence.V2 = replaySubagents(t, native, newWire(), agent.LogVersionV2)
	evidence.V3 = replaySubagents(t, native, newWire(), agent.LogVersionV3)
	return evidence
}

// nativeSubagentPayloads returns the native harness payloads of a fixture in
// file order.
func nativeSubagentPayloads(t testing.TB, path string, version agent.LogVersion) [][]byte {
	t.Helper()
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	var payloads [][]byte
	parser, err := agent.NewLogRecordParser(version, func(line []byte) ([]agent.Message, error) {
		payloads = append(payloads, append([]byte(nil), line...))
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		record, err := parser.ParseRecord(scanner.Bytes())
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if !record.Control && !record.RelayRecord {
			// Only relay-owned agent records carry native harness output. A v3
			// input record is a client command and is not fixture evidence.
			t.Fatalf("%s contains a non-relay record; native-subagent fixtures must hold only agent records", path)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(payloads) == 0 {
		t.Fatalf("%s has no native harness records", path)
	}
	return payloads
}

// liveSubagents parses raw harness payloads exactly as a live session does.
func liveSubagents(t testing.TB, payloads [][]byte, wire agent.WireFormat) []agent.NativeSubagent {
	t.Helper()
	out := make([]agent.NativeSubagent, 0, len(payloads))
	for index, payload := range payloads {
		messages, err := wire.ParseMessage(payload)
		if err != nil {
			t.Fatalf("live parse of native record %d: %v", index, err)
		}
		out = append(out, nativeSubagents(messages)...)
	}
	return out
}

// replaySubagents replays the same native payloads as physical task-log records
// of version, which is the restart and settled-history path.
func replaySubagents(t testing.TB, payloads [][]byte, wire agent.WireFormat, version agent.LogVersion) []agent.NativeSubagent {
	t.Helper()
	parser, err := agent.NewLogRecordParser(version, wire.ParseMessage)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]agent.NativeSubagent, 0, len(payloads))
	for index, payload := range payloads {
		record, err := parser.ParseRecord(v2AgentRecord(payload))
		if err != nil {
			t.Fatalf("v%d replay of native record %d: %v", version, index, err)
		}
		for _, message := range record.Messages {
			if subagent, ok := message.Message.(*agent.NativeSubagentMessage); ok {
				out = append(out, subagent.Subagent)
			}
		}
	}
	return out
}

// RestartNativeSubagents models a restart or relay adoption: history is
// replayed through the wire with its messages discarded, exactly like
// agent.WarmRelayHistory, and the live tail then continues on that same wire.
// The returned observations are the tail's, and they must settle the same
// canonical identities the history produced.
func RestartNativeSubagents(t testing.TB, version agent.LogVersion, history, tail [][]byte, wire agent.WireFormat) []agent.NativeSubagent {
	t.Helper()
	parser, err := agent.NewLogRecordParser(version, wire.ParseMessage)
	if err != nil {
		t.Fatal(err)
	}
	for index, payload := range history {
		if _, err := parser.ParseRecord(v2AgentRecord(payload)); err != nil {
			t.Fatalf("warm history record %d: %v", index, err)
		}
	}
	out := make([]agent.NativeSubagent, 0, len(tail))
	for index, payload := range tail {
		record, err := parser.ParseRecord(v2AgentRecord(payload))
		if err != nil {
			t.Fatalf("live tail record %d: %v", index, err)
		}
		for _, message := range record.Messages {
			if subagent, ok := message.Message.(*agent.NativeSubagentMessage); ok {
				out = append(out, subagent.Subagent)
			}
		}
	}
	return out
}

// v2AgentRecord wraps one native payload in a canonical agent record. Agent
// record framing is identical in v2 and v3.
func v2AgentRecord(payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"t":"agent","ts":1.000,"msg":`)
	buf.Write(payload)
	buf.WriteByte('}')
	return buf.Bytes()
}

func nativeSubagents(messages []agent.Message) []agent.NativeSubagent {
	var out []agent.NativeSubagent
	for _, message := range messages {
		if subagent, ok := message.(*agent.NativeSubagentMessage); ok {
			out = append(out, subagent.Subagent)
		}
	}
	return out
}

// FoldNativeSubagents folds observations into the canonical card set, exactly
// like the task-wide native-activity view does.
func FoldNativeSubagents(observations []agent.NativeSubagent) (cards []agent.NativeSubagent, active int) {
	var timeline agent.NativeSubagentTimeline
	for i := range observations {
		timeline.Apply(&observations[i])
	}
	return timeline.Subagents(), timeline.ActiveCount()
}
