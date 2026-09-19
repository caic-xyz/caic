// Replays native-subagent evidence fixtures through the live wire parser and both task-log versions, and projects the harness-neutral contract each fixture pins.

package agenttest

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// NativeSubagentEvidence is the canonical lifecycle observation stream for one
// evidence fixture. Live is the wire parser reading native harness records in
// order. PerPayload groups the same observations by the payload that produced
// them, so a test can model a restart after an exact record. V2 and V3 replay the
// identical native payloads as physical task-log records of each version, which
// is what restart and settled-history replay do.
//
// Adapters are stateful, so each field is produced by a fresh wire format.
type NativeSubagentEvidence struct {
	// Payloads are the fixture's native harness records in file order. Tests
	// split them to model a restart between two records.
	Payloads   [][]byte
	PerPayload [][]agent.NativeSubagent
	Live       []agent.NativeSubagent
	V2         []agent.NativeSubagent
	V3         []agent.NativeSubagent
}

// LoadNativeSubagentEvidence reads a minimized physical fixture at fixtureVersion,
// extracts its native harness payloads, and parses them live and replayed.
//
// The fixture's control records (the caic_meta header and caic_result footer)
// never reach the native parser, so one fixture serves live and replay.
func LoadNativeSubagentEvidence(t testing.TB, path string, fixtureVersion agent.LogVersion, newWire func() agent.WireFormat) NativeSubagentEvidence {
	t.Helper()
	native := nativeSubagentPayloads(t, path, fixtureVersion)
	evidence := NativeSubagentEvidence{Payloads: native}
	evidence.PerPayload, evidence.Live = liveSubagents(t, native, newWire())
	evidence.V2 = replaySubagents(t, native, newWire(), agent.LogVersionV2)
	evidence.V3 = replaySubagents(t, native, newWire(), agent.LogVersionV3)
	return evidence
}

// RunningSplit returns the number of leading payloads whose observations leave
// at least one card running. A test that restarts caic mid-turn replays that
// prefix inside the container, holds, and lets the remaining recorded payloads
// settle the restored card from live records.
func (e *NativeSubagentEvidence) RunningSplit(t testing.TB) int {
	t.Helper()
	var observations []agent.NativeSubagent
	for index, group := range e.PerPayload {
		observations = append(observations, group...)
		if _, active := FoldNativeSubagents(observations); active > 0 {
			return index + 1
		}
	}
	t.Fatalf("native-subagent recording never observes a running card: %#v", e.Live)
	return 0
}

// Contract returns the contract this evidence fixture pins. Live parsing and
// both task-log replays must agree before the contract is trusted.
func (e *NativeSubagentEvidence) Contract(t testing.TB) NativeSubagentContract {
	t.Helper()
	live := Contract(e.Live)
	for name, observations := range map[string][]agent.NativeSubagent{"v2 replay": e.V2, "v3 replay": e.V3} {
		if got := Contract(observations); !got.Equal(live) {
			t.Fatalf("%s contract = %s, want the live contract %s", name, got, live)
		}
	}
	return live
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

// liveSubagents parses raw harness payloads exactly as a live session does and
// groups the observations by the payload that produced them.
func liveSubagents(t testing.TB, payloads [][]byte, wire agent.WireFormat) (perPayload [][]agent.NativeSubagent, all []agent.NativeSubagent) {
	t.Helper()
	perPayload = make([][]agent.NativeSubagent, 0, len(payloads))
	for index, payload := range payloads {
		messages, err := wire.ParseMessage(payload)
		if err != nil {
			t.Fatalf("live parse of native record %d: %v", index, err)
		}
		group := nativeSubagents(messages)
		perPayload = append(perPayload, group)
		all = append(all, group...)
	}
	return perPayload, all
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
// like the task-wide native-activity view does, and counts the cards still
// running. Unknown, paused, and terminal cards are not active.
func FoldNativeSubagents(observations []agent.NativeSubagent) (cards []agent.NativeSubagent, active int) {
	var timeline agent.NativeSubagentTimeline
	for i := range observations {
		timeline.Apply(&observations[i])
	}
	cards = timeline.Subagents()
	for i := range cards {
		if cards[i].Status == agent.NativeSubagentStatusRunning {
			active++
		}
	}
	return cards, active
}

// NativeSubagentContractCard is one card's harness-neutral shape. A boolean
// fact is true only when the harness reported it for that card.
type NativeSubagentContractCard struct {
	// Identity is the harness-native identity scheme, for example "claude:task".
	Identity  string
	Scope     agent.NativeSubagentScope
	Status    agent.NativeSubagentStatus
	GroupID   bool
	ToolUseID bool
	Label     bool
	Prompt    bool
	Result    bool
}

// NativeSubagentContract is the harness-neutral shape one evidence fixture pins
// for a native-subagent scenario. It is what a caic-managed recording must
// reproduce: how many cards the harness reports, which harness-native identity
// scheme names each one, whether it is a single agent or an aggregate batch, how
// it settles, which optional facts the harness exposed, and the lifecycle
// vocabulary the harness actually reported.
//
// The contract deliberately excludes harness-owned values (native IDs, labels,
// prompts, results, timestamps) so a live recording can be compared with a
// minimized fixture without pretending the harness must say the same words.
type NativeSubagentContract struct {
	// Cards is the folded card set in first-observed order.
	Cards []NativeSubagentContractCard
	// Statuses lists the distinct observed lifecycle statuses in first-observed
	// order, ignoring observations the fold drops.
	Statuses []agent.NativeSubagentStatus
	// Active counts cards still running after folding.
	Active int
}

// Contract folds observations exactly like the task-wide native-activity view
// and projects them into the shape a fixture pins.
func Contract(observations []agent.NativeSubagent) NativeSubagentContract {
	cards, active := FoldNativeSubagents(observations)
	out := NativeSubagentContract{Active: active}
	for i := range cards {
		out.Cards = append(out.Cards, NativeSubagentContractCard{
			Identity:  nativeIdentityScheme(cards[i].ID),
			Scope:     cards[i].Scope,
			Status:    cards[i].Status,
			GroupID:   cards[i].GroupID != "",
			ToolUseID: cards[i].ToolUseID != "",
			Label:     cards[i].Label != "",
			Prompt:    cards[i].Prompt != "",
			Result:    cards[i].Result != "",
		})
	}
	for i := range observations {
		if observations[i].ID == "" {
			// The fold cannot correlate an observation without an identity.
			continue
		}
		if !slices.Contains(out.Statuses, observations[i].Status) {
			out.Statuses = append(out.Statuses, observations[i].Status)
		}
	}
	return out
}

// Equal reports whether two contracts pin the same observable facts.
func (c NativeSubagentContract) Equal(other NativeSubagentContract) bool {
	return c.Active == other.Active && slices.Equal(c.Cards, other.Cards) && slices.Equal(c.Statuses, other.Statuses)
}

// String renders the contract for failure messages.
func (c NativeSubagentContract) String() string {
	statuses := make([]string, 0, len(c.Statuses))
	for _, status := range c.Statuses {
		statuses = append(statuses, string(status))
	}
	parts := make([]string, 0, len(c.Cards))
	for _, card := range c.Cards {
		parts = append(parts, fmt.Sprintf("{identity:%s scope:%q status:%q group:%t tool:%t label:%t prompt:%t result:%t}",
			card.Identity, card.Scope, card.Status, card.GroupID, card.ToolUseID, card.Label, card.Prompt, card.Result))
	}
	return fmt.Sprintf("cards:[%s] statuses:[%s] active:%d", strings.Join(parts, " "), strings.Join(statuses, " "), c.Active)
}

// NativeSubagentEvidencePrompt returns the delegation prompt one evidence
// fixture was recorded with, so a live acceptance drives the identical request.
func NativeSubagentEvidencePrompt(t testing.TB, path string, version agent.LogVersion) string {
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
	parser, err := agent.NewLogRecordParser(version, func([]byte) ([]agent.Message, error) { return nil, nil })
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
		for _, message := range record.Messages {
			if meta, ok := message.Message.(*agent.MetaMessage); ok && meta.Prompt != "" {
				return meta.Prompt
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("%s has no caic_meta prompt", path)
	return ""
}

// nativeIdentityScheme returns the harness-native identity scheme of an ID, for
// example "claude:task" for "claude:task:agent-1".
func nativeIdentityScheme(id string) string {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) < 2 {
		return id
	}
	return parts[0] + ":" + parts[1]
}
