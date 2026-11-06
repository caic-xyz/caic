// Tests OpenCode model discovery and initial inventory.

package opencode

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/maruel/genai/acp"
)

func TestParseModels(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		models, err := parseModels([]byte("\x1b[92mModels cache refreshed\x1b[0m\nopencode/alpha\n{\n  \"variants\": {\n    \"high\": {},\n    \"low\": {}\n  },\n  \"limit\": {\n    \"context\": 200000,\n    \"output\": 32000\n  }\n}\nanthropic/bravo\n{\"variants\": {}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := (agent.ModelInventory{Models: models}).IDs(), []string{"anthropic/bravo", "opencode/alpha"}; !slices.Equal(got, want) {
			t.Fatalf("models = %v, want %v", got, want)
		}
		if got, want := models[1].EffortOptions, []string{"low", "high"}; !slices.Equal(got, want) {
			t.Fatalf("effort options = %v, want %v", got, want)
		}
		if got, want := models[1].ContextWindow, 200_000; got != want {
			t.Fatalf("context window = %d, want %d", got, want)
		}
		if got := models[0].ContextWindow; got != 0 {
			t.Fatalf("unpublished context window = %d, want 0", got)
		}
	})

	t.Run("missing_metadata", func(t *testing.T) {
		t.Parallel()

		if _, err := parseModels([]byte("opencode/alpha\n")); err == nil {
			t.Fatal("parseModels() succeeded, want error")
		}
	})
}
func TestNew(t *testing.T) {
	t.Parallel()

	if got := New("", nil).ModelInventory(); len(got.Models) != 0 {
		t.Fatalf("ModelInventory() = %#v, want an empty inventory before discovery", got)
	}
}

func TestBackend(t *testing.T) {
	t.Parallel()
	t.Run("recorded native handshake", func(t *testing.T) {
		t.Parallel()
		backend := New("", nil)
		for name, parser := range map[string]agent.MessageParser{"full": backend.NewWire(), "metadata": backend.NewMetadataParser()} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var got agent.SessionMetadata
				for _, msg := range agenttest.ParseJSONL(t, "testdata/read-edit-bash.jsonl", parser) {
					got.Apply(msg)
				}
				want := agent.SessionMetadata{SessionID: "ses_18effbc8effePX3MNj9OckdALX", ReportedModel: "opencode/big-pickle", AgentVersion: "1.15.10"}
				if got != want {
					t.Fatalf("session metadata = %+v, want %+v", got, want)
				}
			})
		}
	})
	t.Run("RecordHandshake", func(t *testing.T) {
		t.Parallel()
		b := New("", nil)
		records, err := agent.NewRecords(t.Context(), b.NewWire(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := records.Close(); err != nil {
				t.Error(err)
			}
		})
		native := agenttest.ACPRecords(`{"id":1,"result":{}}` + "\n" +
			`{"id":2,"result":{"sessionId":"original","configOptions":[{"id":"effort","type":"select","currentValue":"low","options":[{"value":"low"},{"value":"high"}]}]}}` + "\n" +
			`{"id":3,"result":{"configOptions":[{"id":"effort","type":"select","currentValue":"high","options":[{"value":"high"}]}]}}`)
		var stdin bytes.Buffer
		if _, err := b.RecordHandshake(t.Context(), &stdin, io.NopCloser(strings.NewReader(native)), &agent.Options{Dir: "/workspace", ResumeSessionID: "original", Effort: "high", Records: records}); err != nil {
			t.Fatal(err)
		}
		var requests []acp.JSONRPCRequest
		for line := range strings.SplitSeq(strings.TrimSpace(stdin.String()), "\n") {
			var request acp.JSONRPCRequest
			if err := json.Unmarshal([]byte(line), &request); err != nil {
				t.Fatal(err)
			}
			requests = append(requests, request)
		}
		if len(requests) != 3 || requests[1].Method != acp.MethodSessionLoad || requests[2].Method != acp.MethodSessionSetConfigOption {
			t.Fatalf("OpenCode handshake=%+v", requests)
		}
		var selected acp.SetSessionConfigOptionParams
		if err := json.Unmarshal(requests[2].Params, &selected); err != nil {
			t.Fatal(err)
		}
		if selected.ConfigID != "effort" || selected.SessionID != "original" || selected.Value != "high" {
			t.Fatalf("OpenCode settings=%+v", selected)
		}
		if args := b.AgentArgs(agent.HarnessArgs{}); !slices.Equal(args, []string{"opencode", "acp"}) {
			t.Fatalf("OpenCode launch=%v", args)
		}
		if !b.SupportsImages() || !b.SupportsCompact() {
			t.Fatal("OpenCode lost native image or compaction capability")
		}
	})
}
