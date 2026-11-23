// Tests agy ACP launch, protocol routing, compaction encoding, model discovery, and recorded ACP evidence.

package antigravity

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/gomodedev/gomode/mcp/mcptest"
)

//nolint:paralleltest // FetchModelInventory replaces ssh through PATH.
func TestBackend(t *testing.T) {
	b := New("", nil)
	t.Run("AgentArgs", func(t *testing.T) {
		want := []string{"agy", "acp", "--dangerously-skip-permissions"}
		got := b.AgentArgs(agent.HarnessArgs{Model: "gemini-3.8-flash", Effort: "high", ResumeSessionID: "session"})
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("args = %q, want %q", got, want)
		}
	})
	t.Run("capabilities", func(t *testing.T) {
		if b.Harness() != harness.Antigravity || !b.SupportsImages() || !b.SupportsCompact() || b.QuotaProvider() != agent.QuotaProviderAntigravity {
			t.Fatal("unexpected backend capabilities")
		}
	})
	t.Run("Start", func(t *testing.T) {
		if _, err := b.Start(t.Context(), &agent.Options{MCP: mcptest.FakeRegistry{}, Logger: slog.Default(), Dir: "/tmp"}); err == nil || !strings.Contains(err.Error(), "missing SSH host") {
			t.Fatalf("task MCP start target validation = %v", err)
		}
		if _, err := b.Start(t.Context(), &agent.Options{InitialPrompt: agent.Prompt{Images: []v3.ImageData{{Data: "image"}}}, Logger: slog.Default(), Dir: "/tmp"}); err == nil || !strings.Contains(err.Error(), "missing SSH host") {
			t.Fatalf("start with images missing SSH host = %v", err)
		}
	})
	t.Run("AttachRelay", func(t *testing.T) {
		if _, err := b.AttachRelay(t.Context(), &agent.Options{MCP: mcptest.FakeRegistry{}}); err == nil || !strings.Contains(err.Error(), "missing SSH host") {
			t.Fatalf("task MCP attach target validation = %v", err)
		}
	})
	t.Run("SetModelInventory", func(t *testing.T) {
		// sortModels prefers Gemini; alphabetical order would put Claude first.
		models := []agent.Model{{ID: "gemini-3.8-flash"}, {ID: "claude-opus-5-5"}}
		b := New("", nil)
		b.SetModelInventory(agent.ModelInventory{Models: models})
		if got := b.ModelInventory().Models; !reflect.DeepEqual(got, models) {
			t.Fatalf("models = %+v, want %+v", got, models)
		}
	})
	t.Run("NewWire", func(t *testing.T) {
		const (
			streamInit = `{"event":"init","conversation_id":"conversation","init":{"model":"gemini-3.8-flash-low"}}`
			resume     = `{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"conversation","cwd":"/w","mcpServers":[]}}`
			resumed    = `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"conversation"}}`
		)
		t.Run("new task", func(t *testing.T) {
			if _, err := New("", nil).NewWire().EncodePrompt(agent.Prompt{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "ACP") {
				t.Fatalf("EncodePrompt before handshake = %v, want ACP error", err)
			}
		})
		t.Run("stream-json conversation resumed over ACP", func(t *testing.T) {
			w := New("", nil).NewWire()
			msgs, err := w.ParseOutput([]byte(streamInit))
			if err != nil || len(msgs) != 1 {
				t.Fatalf("stream-json init = %v, %v", msgs, err)
			}
			if m, ok := msgs[0].(*agent.InitMessage); !ok || m.SessionID != "conversation" {
				t.Fatalf("stream-json init = %+v", msgs[0])
			}
			if _, err := w.ParseInput([]byte(resume)); err != nil {
				t.Fatal(err)
			}
			if _, err := w.ParseOutput([]byte(resumed)); err != nil {
				t.Fatal(err)
			}
			data, err := w.EncodePrompt(agent.Prompt{Text: "hi"})
			if err != nil {
				t.Fatal(err)
			}
			if want := `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"conversation","prompt":[{"type":"text","text":"hi"}]}}` + "\n"; string(data) != want {
				t.Fatalf("prompt = %q, want %q", data, want)
			}
		})
		t.Run("EncodeCompact", func(t *testing.T) {
			w := New("", nil).NewWire()
			if _, err := w.ParseInput([]byte(resume)); err != nil {
				t.Fatal(err)
			}
			if _, err := w.ParseOutput([]byte(resumed)); err != nil {
				t.Fatal(err)
			}
			c, ok := w.(agent.CompactCommand)
			if !ok {
				t.Fatal("agy wire does not encode compaction")
			}
			// "/compact <message>" compacts, then answers the message.
			if data, err := c.EncodeCompact("keep the failing test"); !errors.Is(err, agent.ErrCompactInstructionsUnsupported) || data != nil {
				t.Fatalf("custom instructions = %q, %v", data, err)
			}
			data, err := c.EncodeCompact("")
			if err != nil {
				t.Fatal(err)
			}
			if want := `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"conversation","prompt":[{"type":"text","text":"/compact"}]}}` + "\n"; string(data) != want {
				t.Fatalf("compact = %q, want %q", data, want)
			}
		})
	})
	t.Run("FetchModelInventory", func(t *testing.T) {
		t.Run("empty target", func(t *testing.T) {
			if _, err := b.FetchModelInventory(t.Context(), agent.Options{}.Target, nil); err == nil {
				t.Fatal("empty SSH target must fail")
			}
		})
		t.Run("missing sign-in", func(t *testing.T) {
			// agy 1.5.14 in an md container without a sign-in writes only this
			// stderr and exits 1.
			dir := t.TempDir()
			script := "#!/bin/sh\nprintf '\\r\\033[KError: Please sign in to view available models. Launch the CLI without arguments to sign in.\\n' >&2\nexit 1\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil { //nolint:gosec // Fake SSH executable in a private test directory.
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			_, err := b.FetchModelInventory(t.Context(), runtime.ConnectionTarget{SSHHost: "agy"}, nil)
			want := "agy models: exit status 1: Error: Please sign in to view available models. Launch the CLI without arguments to sign in."
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	})
}

func TestParseModels(t *testing.T) {
	t.Parallel()
	result := "\n" + `{"event":"result","result":{"status":"SUCCESS"}}`
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		out := `{"event":"command_result","command":{"name":"models","data":{"models":[{"id":"gemini-3.8-flash-low","label":"Flash"}]}}}` + result
		inv, err := parseModels([]byte(out))
		want := []agent.Model{{ID: "gemini-3.8-flash", EffortOptions: []string{"low"}}}
		if err != nil || !reflect.DeepEqual(inv.Models, want) {
			t.Fatalf("models = %+v, %v; want %+v", inv, err, want)
		}
	})
	t.Run("order", func(t *testing.T) {
		t.Parallel()
		ids := []string{
			"gemini-3.7-flash-high", "gemini-3.8-flash-low", "gemini-3.8-flash-high", "gemini-3.1-pro-low",
			"gemini-3.1-pro-high", "claude-sonnet-5-5-high", "claude-opus-5-5-low", "claude-opus-5-5-high",
			"gpt-oss-120b-medium", "gemini-3.7-flash-medium", "gemini-4-hydrogen-xhigh",
			"gemini-4-hydrogen-medium", "gemini-pro-orchestrator-g1-pro",
		}
		models := make([]string, 0, len(ids))
		for _, id := range ids {
			models = append(models, `{"id":"`+id+`"}`)
		}
		out := `{"event":"command_result","command":{"name":"models","data":{"models":[` + strings.Join(models, ",") + `]}}}` + result
		inv, err := parseModels([]byte(out))
		if err != nil {
			t.Fatal(err)
		}
		want := []agent.Model{
			{ID: "gemini-3.8-flash", EffortOptions: []string{"low", "high"}},
			{ID: "gemini-3.1-pro", EffortOptions: []string{"low", "high"}},
			{ID: "gemini-4-hydrogen", EffortOptions: []string{"medium", "xhigh"}},
			{ID: "gemini-pro-orchestrator-g1-pro"},
			{ID: "claude-opus-5-5", EffortOptions: []string{"low", "high"}},
			{ID: "claude-sonnet-5-5", EffortOptions: []string{"high"}},
		}
		if !reflect.DeepEqual(inv.Models, want) {
			t.Fatalf("models = %+v, want %+v", inv.Models, want)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, out := range []string{
			`{`,
			`{"event":"result","result":{"status":"ERROR","error":"login required"}}`,
			`{"event":"result","result":{"status":"SUCCESS"}}`,
			`{"event":"command_result","command":{"name":"models","data":{"models":[]}}}`,
			`{"event":"command_result","command":{"name":"other"}}`,
			`{"event":"command_result","command":{"name":"models","data":{"models":[{}]}}}` + result,
			`{"event":"command_result","command":{"name":"models","data":{"models":"invalid"}}}`,
		} {
			if _, err := parseModels([]byte(out)); err == nil {
				t.Fatalf("accepted %s", out)
			}
		}
	})
}

func TestRecordings(t *testing.T) {
	t.Parallel()
	replay := func(t *testing.T, name string, p agent.MessageParser) []agent.Message {
		// #nosec G304 -- name is one of this test's testdata fixtures.
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		parser, err := agent.NewLogRecordParser(p)
		if err != nil {
			t.Fatal(err)
		}
		var msgs []agent.Message
		for i, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			if i == 0 {
				continue // The caic_meta header.
			}
			record, err := parser.ParseRecord(line)
			if err != nil {
				t.Fatalf("record %d: %v", i+1, err)
			}
			for _, m := range record.Messages {
				msgs = append(msgs, m.Message)
			}
		}
		return msgs
	}
	t.Run("read-edit-bash", func(t *testing.T) {
		t.Parallel()
		var inputs, edits, bashOutputs int
		var result *agent.ResultMessage
		bash := map[string]struct{}{}
		for _, m := range replay(t, "read-edit-bash.jsonl", New("", nil).NewWire()) {
			switch m := m.(type) {
			case *agent.UserInputMessage:
				inputs++
			case *agent.ToolUseMessage:
				if m.Name == "Edit" && m.Detail == "main.go" && len(m.InputView.Files) == 1 {
					edits++
				}
				if m.Name == "Bash" {
					if cmd, err := agent.JSONString(m.Input, "command"); err != nil || cmd != "cat main.go" {
						t.Fatalf("Bash input = %s, %v", m.Input, err)
					}
					bash[m.ToolUseID] = struct{}{}
				}
			case *agent.ToolOutputDeltaMessage:
				if _, ok := bash[m.ToolUseID]; ok && strings.Contains(m.Delta, `fmt.Println("Hi, World!")`) {
					bashOutputs++
				}
			case *agent.ResultMessage:
				result = m
			}
		}
		// agy echoes the prompt as user_message_chunk; only the recorded
		// session/prompt input counts.
		if inputs != 1 || edits != 1 || bashOutputs != 1 {
			t.Fatalf("user inputs = %d, edit views = %d, cat outputs = %d", inputs, edits, bashOutputs)
		}
		if result == nil || result.IsError || result.Usage.InputTokens == 0 || result.Usage.CacheReadInputTokens == 0 || result.Usage.ReasoningOutputTokens == 0 {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("native-subagent-joke", func(t *testing.T) {
		t.Parallel()
		// invoke_subagent completes when agy has spawned the child. The prompt
		// response settles the card, here before the child reports: the
		// parent's next step streams text outside any prompt and keeps its
		// final.
		var cards []agent.NativeSubagent
		var resulted, wokeUp bool
		for _, m := range replay(t, "native-subagent-joke.jsonl", New("", nil).NewWire()) {
			switch m := m.(type) {
			case *agent.ToolUseMessage:
				if m.Name == "Run invoke_subagent" {
					t.Fatalf("delegation tool call %+v, want only its card", m)
				}
			case *agent.ResultMessage:
				resulted = true
			case *agent.TextMessage:
				wokeUp = wokeUp || (resulted && strings.Contains(m.Text, "README"))
			case *agent.NativeSubagentMessage:
				if resulted {
					t.Fatalf("card %+v after the prompt response", m.Subagent)
				}
				cards = append(cards, m.Subagent)
			}
		}
		if len(cards) != 2 || cards[0].Status != agent.NativeSubagentStatusRunning || cards[1].Status != agent.NativeSubagentStatusCompleted {
			t.Fatalf("cards = %+v, want running then completed", cards)
		}
		spawn := cards[0]
		if spawn.Background || spawn.Label == "" || spawn.Prompt == "" || spawn.ID != "antigravity:tool:"+spawn.ToolUseID || cards[1].ID != spawn.ID {
			t.Fatalf("cards = %+v", cards)
		}
		if !wokeUp {
			t.Fatal("the parent's text after the prompt response has no final message")
		}
	})
	t.Run("compact", func(t *testing.T) {
		t.Parallel()
		// The /compact prompt answers with end_turn and the usage of agy's
		// summarization call; its usage_update reports that call's request,
		// not the compacted context. Shared ACP adds the compaction boundary
		// before the result, so the context meter becomes unknown until the
		// next prompt reports the reduced occupancy. The recall answer shows
		// what the summary kept.
		var inputs []string
		var occupancy, meterAtResult, boundaries []int
		var results []*agent.ResultMessage
		var meter agent.ContextMeter
		var text string
		for _, m := range replay(t, "compact.jsonl", New("", nil).NewWire()) {
			meter.Observe(m, "")
			switch m := m.(type) {
			case *agent.UserInputMessage:
				inputs = append(inputs, m.Text)
				occupancy = append(occupancy, 0)
			case *agent.UsageMessage:
				if m.ContextUsed != nil {
					occupancy[len(occupancy)-1] = *m.ContextUsed
				}
			case *agent.SystemMessage:
				if m.Subtype == agent.SystemSubtypeCompactBoundary {
					boundaries = append(boundaries, len(inputs))
				}
			case *agent.TextMessage:
				text = m.Text
			case *agent.ResultMessage:
				if m.IsError {
					t.Fatalf("result = %+v", m)
				}
				results = append(results, m)
				meterAtResult = append(meterAtResult, meter.Tokens())
			}
		}
		if len(inputs) != 3 || inputs[1] != "/compact" || len(results) != 3 {
			t.Fatalf("inputs = %q, results = %d", inputs, len(results))
		}
		if got := results[1].Usage; got.InputTokens != 2951 || got.CacheReadInputTokens != 14879 || got.OutputTokens != 299 {
			t.Fatalf("compaction usage = %+v", got)
		}
		if want := []int{16925, 18129, 9134}; !reflect.DeepEqual(occupancy, want) {
			t.Fatalf("context occupancy = %v, want %v", occupancy, want)
		}
		if want := []int{2}; !reflect.DeepEqual(boundaries, want) {
			t.Fatalf("boundaries after inputs = %v, want %v", boundaries, want)
		}
		if want := []int{16925, 0, 9134}; !reflect.DeepEqual(meterAtResult, want) {
			t.Fatalf("context meter at results = %v, want %v", meterAtResult, want)
		}
		if text != "cobalt-otter" {
			t.Fatalf("recall after compaction = %q", text)
		}
	})
	t.Run("session-resume metadata", func(t *testing.T) {
		t.Parallel()
		// session/resume reports agy's default model; caic then selects the
		// requested one.
		var init *agent.InitMessage
		var model string
		for _, m := range replay(t, "session-resume.jsonl", New("", nil).NewMetadataParser()) {
			switch m := m.(type) {
			case *agent.InitMessage:
				init = m
			case *agent.SessionMetadataMessage:
				if m.ReportedModel != "" {
					model = m.ReportedModel
				}
			}
		}
		if init == nil || init.SessionID == "" || init.Version != "1.5.9" || model != "gemini-3.8-flash" {
			t.Fatalf("init = %+v, model = %q", init, model)
		}
	})
}
