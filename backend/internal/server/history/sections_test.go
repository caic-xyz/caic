// Tests context manifests, payload-free summaries, exact slices, finite activity disclosures, and turn context sizes that match the task meter.

package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	taskslogv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
	"github.com/maruel/ksid"
)

func TestSourceSections(t *testing.T) {
	t.Parallel()
	t.Run("turns record the context size the task meter shows", func(t *testing.T) {
		t.Parallel()
		child := taskslogv3.MessageSource{AgentID: "child"}
		compaction := func(after int64) agent.Message {
			return &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary, ContextTokensAfter: after}
		}
		usage := func(u agent.Usage) agent.Message { return &agent.UsageMessage{Usage: u} }
		occupancy := func(used int, perCall bool) agent.Message {
			return &agent.UsageMessage{ContextUsed: new(used), PerCall: perCall}
		}
		groups := [][]agent.Message{
			{&agent.InitMessage{SessionID: "one"}},
			// The last root call's input context, not a child's.
			{usage(agent.Usage{InputTokens: 10, CacheCreationInputTokens: 100, CacheReadInputTokens: 1000, OutputTokens: 5})},
			{&agent.UsageMessage{Source: child, Usage: agent.Usage{InputTokens: 99_999}}},
			{&agent.ResultMessage{}},
			// Native occupancy takes precedence over billed calls.
			{occupancy(5000, false)},
			{usage(agent.Usage{InputTokens: 7, CacheReadInputTokens: 2000})},
			{&agent.ResultMessage{}},
			// Compaction discards the occupancy and reports the context after it.
			{compaction(300)},
			{&agent.ResultMessage{}},
			// One API call that reports only occupancy replaces the call
			// snapshot, so a later compaction without a size leaves none.
			{occupancy(800, true)},
			{&agent.ResultMessage{}},
			{compaction(0)},
			{&agent.ResultMessage{}},
			// Only a new session discards the occupancy.
			{usage(agent.Usage{InputTokens: 50})},
			{&agent.InitMessage{SessionID: "one"}, occupancy(900, false)},
			{&agent.ResultMessage{}},
			{&agent.InitMessage{SessionID: "two"}},
			{&agent.ResultMessage{}},
		}
		source, _ := costFixture(t, harness.Codex, groups...)
		if _, err := source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, _, metadata, err := source.Sections(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}
		got := make([]int, 0, len(metadata.Turns))
		for _, turn := range metadata.Turns {
			got = append(got, turn.ContextTokens)
		}
		// The live task folds the same records into the same sizes.
		var meter []int
		var messages []agent.Message
		for _, group := range groups {
			messages = append(messages, group...)
			if _, ok := group[0].(*agent.ResultMessage); !ok {
				continue
			}
			tk, err := taskrun.NewTask(ksid.NewID(), agent.Prompt{Text: "context"}, harness.Codex, "", "", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(messages), nil)
			snap := tk.Snapshot()
			meter = append(meter, (&agent.ContextMeter{Used: snap.ContextUsed, API: snap.LastAPIUsage}).Tokens())
		}
		if want := []int{1110, 5000, 300, 800, 0, 900, 50}; !slices.Equal(got, want) || !slices.Equal(meter, want) {
			t.Fatalf("turn context tokens = %v, task meter = %v, want %v", got, meter, want)
		}
	})
	t.Run("resumed transcript anchor survives routing changes and rebuilds", func(t *testing.T) {
		t.Parallel()
		for _, incremental := range []bool{true, false} {
			s := newSectionIndex()
			for i, m := range []v1.EventMessage{
				{Kind: v1.EventKindNativeSubagent, Ts: 1000, NativeSubagent: &v1.EventNativeSubagent{ID: "child", RunID: "001", Status: v1.EventNativeSubagentStatusRunning}},
				{Kind: v1.EventKindText, Ts: 2000, Source: v1.EventMessageSource{AgentID: "child"}, Text: &v1.EventText{Text: "checking"}},
				{Kind: v1.EventKindNativeSubagent, Ts: 3000, NativeSubagent: &v1.EventNativeSubagent{ID: "child", RunID: "001", Status: v1.EventNativeSubagentStatusCompleted}},
				{Kind: v1.EventKindNativeSubagent, Ts: 4000, NativeSubagent: &v1.EventNativeSubagent{ID: "child", RunID: "002", Status: v1.EventNativeSubagentStatusRunning}},
				{Kind: v1.EventKindNativeSubagent, Ts: 5000, NativeSubagent: &v1.EventNativeSubagent{ID: "other", ToolUseID: "new-route", Status: v1.EventNativeSubagentStatusRunning}},
			} {
				s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: m})
				if incremental {
					s.snapshot()
				}
			}
			_, meta := s.snapshot()
			for _, a := range meta.Activities {
				if a.ID == "child" && !a.AnchorAt.Equal(time.UnixMilli(1000)) {
					t.Fatalf("incremental=%v: anchor = %v, want original spawn", incremental, a.AnchorAt)
				}
			}
		}
	})
	t.Run("resumed native runs reopen manifest activity", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		for i, m := range []v1.EventMessage{
			{Kind: v1.EventKindNativeSubagent, Ts: 1000, NativeSubagent: &v1.EventNativeSubagent{ID: "child", RunID: "001", Status: v1.EventNativeSubagentStatusRunning}},
			{Kind: v1.EventKindNativeSubagent, Ts: 2000, NativeSubagent: &v1.EventNativeSubagent{ID: "child", RunID: "001", Status: v1.EventNativeSubagentStatusCompleted}},
			{Kind: v1.EventKindNativeSubagent, Ts: 3000, NativeSubagent: &v1.EventNativeSubagent{ID: "child", RunID: "002", Status: v1.EventNativeSubagentStatusRunning}},
			{Kind: v1.EventKindNativeSubagent, Ts: 4000, NativeSubagent: &v1.EventNativeSubagent{ID: "child", RunID: "001", Status: v1.EventNativeSubagentStatusCompleted}},
		} {
			s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: m})
		}
		_, meta := s.snapshot()
		if len(meta.Activities) != 1 {
			t.Fatalf("activities = %#v", meta.Activities)
		}
		a := meta.Activities[0]
		if a.RunID != "002" || a.Status != "running" || a.EndedAt != nil || a.StartedAt == nil || !a.StartedAt.Equal(time.UnixMilli(3000)) {
			t.Fatalf("activity = %#v, want the reopened run", a)
		}
	})
	t.Run("clean root exit ends a command that does not wake the agent", func(t *testing.T) {
		t.Parallel()
		// Minimized from a task log: the Codex process outlived its turn,
		// and the harness exited cleanly before reporting its end.
		path := writeFixture(t,
			`{"t":"caic_meta","version":3,"harness":"codex","prompt":"hi"}`,
			`{"t":"agent","ts":1.000,"msg":{"method":"thread/started","params":{"thread":{"id":"thread-root"}}}}`,
			`{"t":"agent","ts":1.001,"msg":{"method":"item/started","params":{"item":{"type":"commandExecution","id":"exec-smoke","command":"go test ./...","source":"unifiedExecStartup","status":"inProgress"},"threadId":"thread-root","turnId":"turn-1"}}}`,
			`{"t":"agent","ts":1.002,"msg":{"method":"turn/completed","params":{"threadId":"thread-root","turn":{"id":"turn-1","status":"completed"}}}}`,
		)
		source, _ := newFixtureSource(t, path)
		command := func(t *testing.T) v1.HistoryBackgroundCommand {
			if _, err := source.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, _, metadata, err := source.Sections(t.Context(), "hi")
			if err != nil {
				t.Fatal(err)
			}
			if len(metadata.BackgroundCommands) != 1 {
				t.Fatalf("background commands = %#v, want one", metadata.BackgroundCommands)
			}
			return metadata.BackgroundCommands[0]
		}
		if got := command(t); got.Status != "running" || got.WakesAgent {
			t.Fatalf("command = %#v, want running without waking the agent", got)
		}
		appendFixture(t, path, `{"t":"exit","ts":2.000,"exit_code":0}`+"\n")
		if got := command(t); got.Status != "unknown" {
			t.Fatalf("status after a clean exit = %q, want unknown", got.Status)
		}
	})
	t.Run("settlement trailer ends a command without an exit record", func(t *testing.T) {
		t.Parallel()
		// Minimized from a task log: the log has no exit record and ends
		// with the purge trailer while a Codex process was last seen running.
		path := writeFixture(t,
			`{"t":"caic_meta","version":3,"harness":"codex","prompt":"hi"}`,
			`{"t":"agent","ts":1.000,"msg":{"method":"thread/started","params":{"thread":{"id":"thread-root"}}}}`,
			`{"t":"agent","ts":1.001,"msg":{"method":"item/started","params":{"item":{"type":"commandExecution","id":"exec-serve","command":"npm run dev","source":"unifiedExecStartup","status":"inProgress"},"threadId":"thread-root","turnId":"turn-1"}}}`,
			`{"t":"agent","ts":1.002,"msg":{"method":"turn/completed","params":{"threadId":"thread-root","turn":{"id":"turn-1","status":"completed"}}}}`,
			`{"t":"result","state":"purged","title":"t"}`,
		)
		source, _ := newFixtureSource(t, path)
		if _, err := source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, _, metadata, err := source.Sections(t.Context(), "hi")
		if err != nil {
			t.Fatal(err)
		}
		if len(metadata.BackgroundCommands) != 1 || metadata.BackgroundCommands[0].Status != "unknown" {
			t.Fatalf("background commands = %#v, want one unknown", metadata.BackgroundCommands)
		}
	})
	t.Run("native settings preserve session sections across restart", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t,
			`{"t":"caic_meta","version":3,"harness":"opencode","prompt":"hi"}`,
			`{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}}`,
			`{"t":"agent","ts":1.001,"msg":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"version":"1.15.10"}}}}`,
			`{"t":"input","ts":1.002,"msg":{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}}`,
			`{"t":"agent","ts":1.003,"msg":{"jsonrpc":"2.0","id":2,"result":{"sessionId":"ses_1","models":{"currentModelId":"a/b"}}}}`,
			`{"t":"session","session_id":"ses_1","model":"a/b","agent_version":"1.15.10"}`,
			`{"t":"input","ts":1.004,"msg":{"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"sessionId":"ses_1","configId":"model","value":"c/d"}}}`,
			`{"t":"agent","ts":1.005,"msg":{"jsonrpc":"2.0","id":3,"result":{"configOptions":[{"id":"model","currentValue":"c/d"}]}}}`,
			`{"t":"agent","ts":1.006,"msg":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_1","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"effort","currentValue":"high"}]}}}}`,
			`{"t":"input","ts":1.007,"msg":{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"ses_1","prompt":[{"type":"text","text":"hi"}]}}}`,
			`{"t":"agent","ts":1.008,"msg":{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn","usage":{"inputTokens":100000}}}}`,
			`{"t":"exit","exit_code":0}`,
			`{"t":"input","ts":2.000,"msg":{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}}`,
			`{"t":"agent","ts":2.001,"msg":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"version":"1.15.10"}}}}`,
			`{"t":"input","ts":2.002,"msg":{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"ses_1"}}}`,
			`{"t":"agent","ts":2.003,"msg":{"jsonrpc":"2.0","id":2,"result":{"models":{"currentModelId":"c/d"}}}}`,
			`{"t":"session","session_id":"ses_1","model":"c/d","agent_version":"1.15.10"}`,
			`{"t":"input","ts":2.004,"msg":{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_1","prompt":[{"type":"text","text":"again"}]}}}`,
			`{"t":"agent","ts":2.005,"msg":{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn","usage":{"inputTokens":100000}}}}`,
		)
		// Each subtest releases its decoded records and anonymous projection
		// spools before constructing a fresh source from the same durable bytes.
		for attempt := range 2 {
			t.Run(fmt.Sprintf("projection=%d", attempt), func(t *testing.T) { //nolint:paralleltest // The next rebuild starts only after this source and its spools close.
				var suffix []byte
				if attempt == 0 {
					complete, err := os.ReadFile(filepath.Clean(path))
					if err != nil {
						t.Fatal(err)
					}
					end := bytes.IndexByte(complete, '\n') + 1
					suffix = complete[end:]
					if err := os.WriteFile(path, complete[:end], 0o600); err != nil { //nolint:gosec // writeFixture returns a path inside this test's private temporary directory.
						t.Fatal(err)
					}
				}
				source, _ := newFixtureSource(t, path)
				for line := range bytes.SplitSeq(bytes.TrimSuffix(suffix, []byte("\n")), []byte("\n")) {
					if len(line) == 0 {
						continue
					}
					appendFixture(t, path, string(line)+"\n")
					if _, err := source.Refresh(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				snapshot, err := source.Refresh(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				_, sections, metadata, err := source.Sections(t.Context(), "hi")
				if err != nil {
					t.Fatal(err)
				}
				if len(sections) != 1 || sections[0].TurnCount != 2 {
					t.Fatalf("sections = %+v, want one session containing two turns", sections)
				}
				if metadata.InitialInit == nil || metadata.InitialInit.RecordID != 5 || metadata.InitialInit.Message.Init.SessionID != "ses_1" || metadata.InitialInit.Message.Init.AgentVersion != "1.15.10" {
					t.Fatalf("session card = %+v, want proven session at record 5", metadata.InitialInit)
				}
				var initIDs []uint64
				var inputTokens, results int
				for event, err := range source.Events(t.Context(), snapshot, 1, snapshot.Through) {
					if err != nil {
						t.Fatal(err)
					}
					if event.Message.Init != nil {
						initIDs = append(initIDs, event.RecordID)
					}
					if event.Message.Result != nil {
						inputTokens += event.Message.Result.Usage.InputTokens
						results++
					}
				}
				if !slices.Equal(initIDs, []uint64{5, 16}) || inputTokens != 200000 || results != 2 {
					t.Fatalf("presentation init IDs=%v input tokens=%d results=%d", initIDs, inputTokens, results)
				}
				var total float64
				var turns int
				for cost, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, journalPricer{}) {
					if err != nil {
						t.Fatal(err)
					}
					if cost.Model != "c/d" {
						t.Fatalf("partial model missing from rebuilt pricing journal: %+v", cost)
					}
					total += cost.Cost.USD
					turns++
				}
				if turns != 2 || math.Abs(total-0.4) > 1e-9 {
					t.Fatalf("repriced session turns=%d total=%v", turns, total)
				}
			})
		}
	})
	t.Run("Pi model reports preserve session sections", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t,
			`{"t":"caic_meta","version":3,"harness":"pi","prompt":"hi"}`,
			`{"t":"input","ts":1.000,"msg":{"type":"set_model","provider":"openrouter","modelId":"auto"}}`,
			`{"t":"agent","ts":1.001,"msg":{"type":"response","command":"set_model","success":true,"data":{"provider":"openrouter","id":"auto","contextWindow":262144}}}`,
			`{"t":"input","ts":1.002,"msg":{"type":"get_state"}}`,
			`{"t":"agent","ts":1.003,"msg":{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1","model":{"provider":"openrouter","id":"auto"},"thinkingLevel":"high"}}}`,
			`{"t":"agent","ts":1.004,"msg":{"type":"agent_start"}}`,
			`{"t":"agent","ts":1.005,"msg":{"type":"message_start","message":{"role":"assistant","provider":"openrouter","model":"auto","responseModel":"anthropic/claude-opus-4.7","content":[]}}}`,
			`{"t":"agent","ts":1.006,"msg":{"type":"turn_end","message":{"role":"assistant","provider":"openrouter","model":"auto","responseModel":"anthropic/claude-opus-4.7","usage":{"input":10,"output":4,"totalTokens":14,"cost":{"total":0.25}}}}}`,
			`{"t":"agent","ts":1.007,"msg":{"type":"agent_end","messages":[{"role":"assistant","usage":{"input":10,"output":4,"totalTokens":14,"cost":{"total":0.25}}}]}}`,
			`{"t":"exit","exit_code":0}`,
			`{"t":"input","ts":2.000,"msg":{"type":"set_model","provider":"openrouter","modelId":"selected"}}`,
			`{"t":"agent","ts":2.001,"msg":{"type":"response","command":"set_model","success":true,"data":{"provider":"openrouter","id":"selected"}}}`,
			`{"t":"input","ts":2.002,"msg":{"type":"get_state"}}`,
			`{"t":"agent","ts":2.003,"msg":{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1"}}}`,
			`{"t":"agent","ts":2.004,"msg":{"type":"agent_start"}}`,
			`{"t":"agent","ts":2.005,"msg":{"type":"turn_end","message":{"role":"assistant","provider":"openrouter","model":"selected","usage":{"input":10,"output":4,"totalTokens":14,"cost":{"total":0.25}}}}}`,
			`{"t":"agent","ts":2.006,"msg":{"type":"agent_end","messages":[{"role":"assistant","usage":{"input":10,"output":4,"totalTokens":14,"cost":{"total":0.25}}}]}}`,
		)
		// Each subtest releases its decoded records and anonymous projection
		// spools before constructing a fresh source from the same durable bytes.
		for attempt := range 2 {
			t.Run(fmt.Sprintf("projection=%d", attempt), func(t *testing.T) { //nolint:paralleltest // The next rebuild starts only after this source and its spools close.
				var suffix []byte
				if attempt == 0 {
					complete, err := os.ReadFile(filepath.Clean(path))
					if err != nil {
						t.Fatal(err)
					}
					end := bytes.IndexByte(complete, '\n') + 1
					suffix = complete[end:]
					if err := os.WriteFile(path, complete[:end], 0o600); err != nil { //nolint:gosec // writeFixture returns a path inside this test's private temporary directory.
						t.Fatal(err)
					}
				}
				source, _ := newFixtureSource(t, path)
				for line := range bytes.SplitSeq(bytes.TrimSuffix(suffix, []byte("\n")), []byte("\n")) {
					if len(line) == 0 {
						continue
					}
					appendFixture(t, path, string(line)+"\n")
					if _, err := source.Refresh(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				snapshot, err := source.Refresh(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				_, sections, metadata, err := source.Sections(t.Context(), "hi")
				if err != nil {
					t.Fatal(err)
				}
				if len(sections) != 1 || sections[0].TurnCount != 2 {
					t.Fatalf("sections = %+v, want one Pi session containing two turns", sections)
				}
				if metadata.InitialInit == nil || metadata.InitialInit.RecordID != 5 || metadata.InitialInit.Message.Init.SessionID != "ses-1" || metadata.InitialInit.Message.Init.ReportedModel != "openrouter/auto" {
					t.Fatalf("session card = %+v, want proven Pi session at record 5", metadata.InitialInit)
				}
				var initIDs []uint64
				var inputTokens, results int
				for event, err := range source.Events(t.Context(), snapshot, 1, snapshot.Through) {
					if err != nil {
						t.Fatal(err)
					}
					if event.Message.Init != nil {
						initIDs = append(initIDs, event.RecordID)
					}
					if event.Message.Result != nil {
						inputTokens += event.Message.Result.Usage.InputTokens
						results++
					}
				}
				if !slices.Equal(initIDs, []uint64{5}) || inputTokens != 20 || results != 2 {
					t.Fatalf("presentation init IDs=%v input tokens=%d results=%d", initIDs, inputTokens, results)
				}
				var total float64
				var turns int
				for cost, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, journalPricer{}) {
					if err != nil {
						t.Fatal(err)
					}
					total += cost.Cost.USD
					turns++
				}
				if turns != 2 || total != 0.5 {
					t.Fatalf("repriced session turns=%d total=%v", turns, total)
				}
			})
		}
	})
	t.Run("actor body bounds ignore unrelated records and include ownership revisions", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		s.append(&v1.HistoryEvent{RecordID: 1, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, NativeSubagent: &v1.EventNativeSubagent{ID: "child", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		s.append(&v1.HistoryEvent{RecordID: 2, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{AgentID: "child"}, Text: &v1.EventText{Text: "direct"}}})
		assertBounds := func(body, ownership uint64) {
			_, metadata := s.snapshot()
			for _, a := range metadata.Activities {
				if a.ID == "child" {
					if a.BodyThrough != body || a.OwnershipThrough != ownership {
						t.Fatalf("actor bounds = (%d, %d), want body %d, ownership %d", a.BodyThrough, a.OwnershipThrough, body, ownership)
					}
					return
				}
			}
			t.Fatal("child missing")
		}
		assertBounds(2, 1)
		s.append(&v1.HistoryEvent{RecordID: 3, Message: v1.EventMessage{Kind: v1.EventKindText, Text: &v1.EventText{Text: "root"}}})
		s.append(&v1.HistoryEvent{RecordID: 4, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, NativeSubagent: &v1.EventNativeSubagent{ID: "other", ToolUseID: "unrelated", Status: v1.EventNativeSubagentStatusRunning}}})
		assertBounds(2, 1)
		s.append(&v1.HistoryEvent{RecordID: 5, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{DelegationToolUseID: "spawn", ThreadID: "root"}, Text: &v1.EventText{Text: "delegated"}}})
		assertBounds(5, 5)
		s.append(&v1.HistoryEvent{RecordID: 6, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{DelegationToolUseID: "spawn", ThreadID: "root"}, Text: &v1.EventText{Text: "more delegated output"}}})
		assertBounds(6, 5)
		s.append(&v1.HistoryEvent{RecordID: 7, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, NativeSubagent: &v1.EventNativeSubagent{ID: "competing", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		assertBounds(7, 7)
		s.append(&v1.HistoryEvent{RecordID: 8, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{DelegationToolUseID: "spawn", ThreadID: "another-root"}, Text: &v1.EventText{Text: "namespace"}}})
		assertBounds(8, 8)
		// Multiple outputs in one complete raw record expose one coherent pair
		// of bounds, whether its routing observation is related or unrelated.
		s.append(&v1.HistoryEvent{RecordID: 9, Index: 0, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{AgentID: "child"}, Text: &v1.EventText{Text: "same-record output"}}})
		s.append(&v1.HistoryEvent{RecordID: 9, Index: 1, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, NativeSubagent: &v1.EventNativeSubagent{ID: "sibling", ToolUseID: "another-tool", Status: v1.EventNativeSubagentStatusRunning}}})
		assertBounds(9, 8)
		s.append(&v1.HistoryEvent{RecordID: 10, Index: 0, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{AgentID: "child"}, Text: &v1.EventText{Text: "same-record related output"}}})
		s.append(&v1.HistoryEvent{RecordID: 10, Index: 1, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "scoped-competitor", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		assertBounds(10, 10)
	})
	t.Run("captured setup survives concurrent refresh", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader, `{"t":"log","line":"before"}`)
		source, _ := newFixtureSource(t, path)
		if _, err := source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		snap, _, metadata, err := source.Sections(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}
		if metadata.Setup == nil {
			t.Fatal("missing setup")
		}
		expected := *metadata.Setup
		f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
		start := make(chan struct{})
		failures := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			for range 100 {
				if _, err := f.WriteString(`{"t":"log","line":"after"}` + "\n"); err != nil {
					failures <- err
					return
				}
				if _, err := source.Refresh(t.Context()); err != nil {
					failures <- err
					return
				}
			}
		})
		wg.Go(func() {
			<-start
			for range 2000 {
				data, err := json.Marshal(metadata)
				if err != nil {
					failures <- err
					return
				}
				var decoded v1.HistoryMetadata
				if err = json.Unmarshal(data, &decoded); err != nil {
					failures <- err
					return
				}
				if decoded.Setup == nil || *decoded.Setup != expected || decoded.Setup.Last.RecordID > snap.Through {
					failures <- errors.New("captured setup changed beyond snapshot")
					return
				}
			}
		})
		close(start)
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		if *metadata.Setup != expected {
			t.Fatalf("captured setup = %#v, expected %#v", metadata.Setup, expected)
		}
	})
	t.Run("manifest partitions normalized outputs", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			batches [][]agent.Message
			want    [][]v1.HistoryPosition
			turns   []int
		}{
			{name: "clear and empty context", batches: [][]agent.Message{{&agent.InitMessage{SessionID: "one"}, &agent.TextMessage{Text: "before"}, &agent.ResultMessage{DurationMs: 1000}, agent.ContextCleared(), &agent.InitMessage{SessionID: "two"}, &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}}}, want: [][]v1.HistoryPosition{{{RecordID: 2, Index: 0}, {RecordID: 2, Index: 2}}, {{RecordID: 2, Index: 3}, {RecordID: 2, Index: 4}}, {{RecordID: 2, Index: 5}}}, turns: []int{1, 0, 0}},
			{name: "changed session restart", batches: [][]agent.Message{{&agent.InitMessage{SessionID: "one"}, &agent.TextMessage{Text: "before"}, &agent.ResultMessage{DurationMs: 1000}, &agent.UserInputMessage{Text: "carry"}, &agent.InitMessage{SessionID: "two"}, &agent.TextMessage{Text: "after"}, &agent.ResultMessage{DurationMs: 2000}}}, want: [][]v1.HistoryPosition{{{RecordID: 2, Index: 0}, {RecordID: 2, Index: 2}}, {{RecordID: 2, Index: 3}, {RecordID: 2, Index: 6}}}, turns: []int{1, 1}},
			{name: "unidentified init keeps the session", batches: [][]agent.Message{{&agent.InitMessage{SessionID: "one"}, &agent.UserInputMessage{Text: "prompt"}, &agent.InitMessage{}, &agent.TextMessage{Text: "answer"}, &agent.ResultMessage{DurationMs: 1000}}}, want: [][]v1.HistoryPosition{{{RecordID: 2, Index: 0}, {RecordID: 2, Index: 4}}}, turns: []int{1}},
			{name: "compaction restart and repeated init", batches: [][]agent.Message{{&agent.InitMessage{SessionID: "one"}, &agent.ResultMessage{DurationMs: 1000}}, {&agent.UserInputMessage{Text: "carry"}, &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, &agent.InitMessage{SessionID: "two"}, &agent.InitMessage{SessionID: "two"}, &agent.TextMessage{Text: "after"}, &agent.ResultMessage{DurationMs: 2000}}}, want: [][]v1.HistoryPosition{{{RecordID: 2, Index: 0}, {RecordID: 2, Index: 1}}, {{RecordID: 3, Index: 0}, {RecordID: 3, Index: 5}}}, turns: []int{1, 1}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				lines := []string{claudeHeader}
				for i := range c.batches {
					lines = append(lines, fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":{"batch":%d}}`, i))
				}
				path := writeFixture(t, lines...)
				resolver := agent.Backends{harness.Claude: &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
					return &agenttest.Wire{Output: func(data []byte) ([]agent.Message, error) {
						var input map[string]int
						if err := json.Unmarshal(data, &input); err != nil {
							return nil, err
						}
						return c.batches[input["batch"]], nil
					}}
				}}}
				source, err := NewSource(t.Context(), path, "", resolver)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := source.Close(); err != nil {
						t.Error(err)
					}
				})
				if _, err = source.Refresh(t.Context()); err != nil {
					t.Fatal(err)
				}
				snap, sections, metadata, err := source.Sections(t.Context(), "")
				if err != nil {
					t.Fatal(err)
				}
				if len(sections) != len(c.want) {
					t.Fatalf("sections = %#v", sections)
				}
				var joined []v1.HistoryPosition
				for i, section := range sections {
					if section.Body == nil || section.TurnCount != c.turns[i] {
						t.Fatalf("section %d = %#v", i, section)
					}
					var body bytes.Buffer
					if err = source.CheckSlice(t.Context(), snap, *section.Body); err != nil {
						t.Fatal(err)
					}
					if err = source.WriteSlice(t.Context(), snap, *section.Body, &body); err != nil {
						t.Fatal(err)
					}
					var events []v1.HistoryEvent
					if err = json.Unmarshal(body.Bytes(), &events); err != nil {
						t.Fatal(err)
					}
					if len(events) == 0 {
						t.Fatal("empty slice despite explicit manifest bounds")
					}
					bounds := []v1.HistoryPosition{eventPosition(&events[0])}
					if len(events) > 1 {
						bounds = append(bounds, eventPosition(&events[len(events)-1]))
					}
					if !reflect.DeepEqual(bounds, c.want[i]) {
						t.Fatalf("section %d bounds = %#v, expected %#v", i, bounds, c.want[i])
					}
					for j := range events {
						joined = append(joined, eventPosition(&events[j]))
					}
				}
				var canonical []v1.HistoryPosition
				for e, err := range source.Events(t.Context(), snap, 1, snap.Through) {
					if err != nil {
						t.Fatal(err)
					}
					canonical = append(canonical, eventPosition(&e))
				}
				if !reflect.DeepEqual(joined, canonical) {
					t.Fatalf("partition lost/duplicated outputs: %#v vs %#v", joined, canonical)
				}
				wantFacts := 0
				for _, batch := range c.batches {
					for _, message := range batch {
						if _, ok := message.(*agent.ResultMessage); ok {
							wantFacts++
						}
					}
				}
				if len(metadata.Turns) != wantFacts {
					t.Fatalf("turn facts = %#v, want %d", metadata.Turns, wantFacts)
				}
			})
		}
	})
	t.Run("setup excludes interleaved transcript", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader, `{"t":"log","line":"first setup line"}`, twoTexts, `{"t":"log","line":"later setup line"}`)
		source, err := NewSource(t.Context(), path, "", backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
		snap, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		if err = source.WriteSetup(t.Context(), snap, snap.Through, &body); err != nil {
			t.Fatal(err)
		}
		var events []v1.HistoryEvent
		if err = json.Unmarshal(body.Bytes(), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[0].Message.Log == nil || events[1].Message.Log == nil || events[0].Message.Log.Line != "first setup line" || events[1].Message.Log.Line != "later setup line" {
			t.Fatalf("setup events = %#v", events)
		}
	})
	t.Run("sync activity streams in order outside setup", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader,
			`{"t":"log","line":"setup line"}`,
			twoTexts,
			`{"t":"sync_activity","sync_id":"s1","phase":"start","ts":5.000,"target":"origin","branch":"caic-1"}`,
			`{"t":"sync_activity","sync_id":"s1","phase":"output","ts":6.000,"output":"checking\n"}`,
			`{"t":"sync_activity","sync_id":"s1","phase":"end","ts":7.000,"status":"failed","error":"rejected","duration_ms":2000}`,
		)
		source, _ := newFixtureSource(t, path)
		snap, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var phases []v1.EventSyncActivityPhase
		var logs int
		for event, err := range source.Events(t.Context(), snap, 1, snap.Through) {
			if err != nil {
				t.Fatal(err)
			}
			if event.Message.Log != nil {
				logs++
			}
			if sa := event.Message.SyncActivity; sa != nil {
				phases = append(phases, sa.Phase)
				if event.Message.Kind != v1.EventKindSyncActivity || sa.SyncID != "s1" {
					t.Errorf("event = %+v", event.Message)
				}
				if want := int64(5000 + 1000*len(phases) - 1000); event.Message.Ts != want {
					t.Errorf("phase %s ts = %d, want %d", sa.Phase, event.Message.Ts, want)
				}
			}
		}
		if want := []v1.EventSyncActivityPhase{v1.EventSyncActivityPhaseStart, v1.EventSyncActivityPhaseOutput, v1.EventSyncActivityPhaseEnd}; !slices.Equal(phases, want) || logs != 1 {
			t.Fatalf("phases = %v, logs = %d, want %v and 1", phases, logs, want)
		}
		_, _, metadata, err := source.Sections(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}
		if metadata.Setup == nil || metadata.Setup.First != metadata.Setup.Last {
			t.Fatalf("setup = %+v, want only the setup line", metadata.Setup)
		}
	})
	t.Run("activity homes follow written carried bodies", func(t *testing.T) {
		t.Parallel()
		for _, boundary := range []string{"compaction", "changed init"} {
			for _, layout := range []string{"same record", "distinct records"} {
				t.Run(boundary+"/"+layout, func(t *testing.T) {
					t.Parallel()
					prefix := []agent.Message{
						&agent.InitMessage{SessionID: "one"},
						&agent.TextMessage{Text: "first turn"},
						&agent.ResultMessage{DurationMs: 1000},
						&agent.UserInputMessage{Text: "unfinished turn"},
						&agent.ToolUseMessage{ToolUseID: "spawn", Name: "Agent"},
						&agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: "child", ToolUseID: "spawn", Status: agent.NativeSubagentStatusRunning}},
						&agent.TextMessage{Source: taskslogv3.MessageSource{AgentID: "child"}, Text: "child transcript"},
					}
					var opening agent.Message = &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}
					if boundary == "changed init" {
						opening = &agent.InitMessage{SessionID: "two"}
					}
					suffix := []agent.Message{opening,
						&agent.TextMessage{Text: "second turn"},
						&agent.ResultMessage{DurationMs: 1000},
						&agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: "child", Status: agent.NativeSubagentStatusCompleted}},
					}
					batches := [][]agent.Message{prefix, suffix, append(slices.Clone(prefix), suffix...)}
					resolver := agent.Backends{harness.Claude: &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
						return &agenttest.Wire{Output: func(data []byte) ([]agent.Message, error) {
							var input struct {
								Batch int `json:"batch"`
							}
							if err := json.Unmarshal(data, &input); err != nil {
								return nil, err
							}
							return batches[input.Batch], nil
						}}
					}}}
					open := func(path string) *Source {
						source, err := NewSource(t.Context(), path, "", resolver)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := source.Close(); err != nil {
								t.Error(err)
							}
						})
						return source
					}
					check := func(source *Source, home string) {
						if _, err := source.Refresh(t.Context()); err != nil {
							t.Fatal(err)
						}
						snap, sections, metadata, err := source.Sections(t.Context(), "")
						if err != nil {
							t.Fatal(err)
						}
						// The oracle is actual WriteSlice membership, independent of
						// opening boundaries or metadata's conservative read bounds.
						owners := map[v1.HistoryPosition]string{}
						origins := map[string]v1.HistoryPosition{}
						for _, section := range sections {
							if section.Body == nil {
								continue
							}
							var body bytes.Buffer
							if err := source.WriteSlice(t.Context(), snap, *section.Body, &body); err != nil {
								t.Fatal(err)
							}
							var events []v1.HistoryEvent
							if err := json.Unmarshal(body.Bytes(), &events); err != nil {
								t.Fatal(err)
							}
							for _, event := range events {
								position := eventPosition(&event)
								if previous := owners[position]; previous != "" {
									t.Fatalf("position %#v duplicated in %s and %s", position, previous, section.ID)
								}
								owners[position] = section.ID
								id := ""
								switch {
								case event.Message.NativeSubagent != nil:
									id = event.Message.NativeSubagent.ID
								case event.Message.Source.AgentID != "":
									id = event.Message.Source.AgentID
								}
								if id != "" {
									if _, seen := origins[id]; !seen {
										origins[id] = position
									}
								}
							}
						}
						if len(metadata.Activities) != 1 {
							t.Fatalf("activities = %#v", metadata.Activities)
						}
						for _, activity := range metadata.Activities {
							origin, found := origins[activity.ID]
							if !found || owners[origin] != home || activity.SectionID != owners[origin] {
								t.Errorf("%s home = %s, written origin %#v belongs to %s; want %s", activity.ID, activity.SectionID, origin, owners[origin], home)
							}
						}
					}
					path := writeFixture(t, claudeHeader, `{"t":"agent","ts":1.000,"msg":{"batch":0}}`)
					source := open(path)
					check(source, "1")
					if layout == "same record" {
						// Immutable raw records cannot grow. Replay the complete batch
						// to exercise both sides of the boundary within record 2.
						source = open(writeFixture(t, claudeHeader, `{"t":"agent","ts":1.000,"msg":{"batch":2}}`))
					} else {
						f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0)
						if err != nil {
							t.Fatal(err)
						}
						_, writeErr := f.WriteString(`{"t":"agent","ts":1.000,"msg":{"batch":1}}` + "\n")
						if err := errors.Join(writeErr, f.Close()); err != nil {
							t.Fatal(err)
						}
					}
					check(source, "2")
				})
			}
		}
	})
	t.Run("carry and summaries", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader,
			`{"t":"user_input","ts":1.000,"text":"history"}`,
			`{"t":"agent","ts":2.000,"msg":{"type":"system","subtype":"init","session_id":"first","model":"claude-sonnet-4-6"}}`,
			twoTexts,
			`{"t":"agent","ts":3.000,"msg":{"type":"result","subtype":"success","duration_ms":1000,"duration_api_ms":700,"usage":{"input_tokens":10,"output_tokens":20}}}`,
			`{"t":"user_input","ts":4.000,"text":"follow-up"}`,
			`{"t":"agent","ts":5.000,"msg":{"type":"system","subtype":"compact_boundary","compact_metadata":{"pre_tokens":100}}}`,
			`{"t":"agent","ts":6.000,"msg":{"type":"system","subtype":"init","session_id":"second","model":"claude-sonnet-4-6"}}`,
			delta)
		source, err := NewSource(t.Context(), path, "", backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
		if _, err = source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		snap, sections, metadata, err := source.Sections(t.Context(), "history")
		if err != nil {
			t.Fatal(err)
		}
		if len(sections) != 2 {
			t.Fatalf("sections = %#v", sections)
		}
		first, current := sections[0], sections[1]
		if first.ID != "1" || first.TurnCount != 1 || first.TextCount != 2 || first.ToolCount != 0 || first.DurationMS != 1000 {
			t.Fatalf("first summary = %#v", first)
		}
		if first.Body == nil || first.Body.First.RecordID != 2 || first.Body.Last.RecordID != 5 || current.Body == nil || current.Body.First.RecordID != 6 || current.Body.Last.RecordID != 9 {
			t.Fatalf("bodies = %#v, %#v", first.Body, current.Body)
		}
		if first.EndBoundary == nil || first.EndBoundary.Message.System.Subtype != "compact_boundary" || current.Boundary.RecordID != 7 {
			t.Fatalf("boundaries = %#v, %#v", first.EndBoundary, current.Boundary)
		}
		if !metadata.HasInitialPrompt || len(metadata.Turns) != 1 || metadata.Turns[0].SectionID != "1" || metadata.Turns[0].WaitMS == nil || *metadata.Turns[0].WaitMS != 1000 {
			t.Fatalf("metadata = %#v", metadata)
		}
		var body bytes.Buffer
		if err = source.CheckSlice(t.Context(), snap, *current.Body); err != nil {
			t.Fatal(err)
		}
		if err = source.WriteSlice(t.Context(), snap, *current.Body, &body); err != nil {
			t.Fatal(err)
		}
		var events []v1.HistoryEvent
		if err = json.Unmarshal(body.Bytes(), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 4 || events[0].Message.UserInput == nil || events[0].Message.UserInput.Text != "follow-up" {
			t.Fatalf("current slice = %#v", events)
		}
		if metadata.InitialInit == nil || metadata.InitialInit.RecordID != 3 {
			t.Fatalf("setup init = %#v", metadata.InitialInit)
		}
	})
	t.Run("within one raw record", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader, twoTexts, noOutput)
		source, err := NewSource(t.Context(), path, "", backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
		snap, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		selected := v1.HistorySlice{First: v1.HistoryPosition{RecordID: 2, Index: 1}, Last: v1.HistoryPosition{RecordID: 2, Index: 1}}
		if err = source.CheckSlice(t.Context(), snap, selected); err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		if err = source.WriteSlice(t.Context(), snap, selected, &body); err != nil {
			t.Fatal(err)
		}
		var events []v1.HistoryEvent
		if err = json.Unmarshal(body.Bytes(), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Index != 1 || events[0].Message.Text.Text != "second" {
			t.Fatalf("selected = %#v", events)
		}
		selected.Last = v1.HistoryPosition{RecordID: 3, Index: RecordEndIndex}
		if err = source.CheckSlice(t.Context(), snap, selected); err != nil {
			t.Fatal(err)
		}
		body.Reset()
		if err = source.WriteSlice(t.Context(), snap, selected, &body); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(body.Bytes(), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Index != 1 || events[0].Message.Text.Text != "second" {
			t.Fatalf("record-end slice = %#v", events)
		}
		empty := v1.HistorySlice{First: v1.HistoryPosition{RecordID: 3}, Last: v1.HistoryPosition{RecordID: 3, Index: RecordEndIndex}}
		if err = source.CheckSlice(t.Context(), snap, empty); err != nil {
			t.Fatal(err)
		}
		var emptyBody bytes.Buffer
		if err = source.WriteSlice(t.Context(), snap, empty, &emptyBody); err != nil {
			t.Fatal(err)
		}
		var emptyEvents []v1.HistoryEvent
		if err = json.Unmarshal(emptyBody.Bytes(), &emptyEvents); err != nil {
			t.Fatal(err)
		}
		if len(emptyEvents) != 0 {
			t.Fatalf("outputless record page = %#v", emptyEvents)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err = source.WriteSlice(ctx, snap, selected, io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled slice = %v", err)
		}
		if err = source.WriteSlice(t.Context(), snap, selected, rejectedSliceWriter{}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("failed slice output = %v", err)
		}
		selected.Last = v1.HistoryPosition{RecordID: 2, Index: 2}
		if err = source.CheckSlice(t.Context(), snap, selected); err == nil {
			t.Fatal("accepted nonexistent output index")
		}
	})
}

type rejectedSliceWriter struct{}

func (rejectedSliceWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestSectionIndex(t *testing.T) {
	t.Parallel()
	t.Run("legacy tools preserve model message boundaries", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			name  string
			usage bool
			texts int
		}{{name: "interleaved commentary within one message", texts: 1}, {name: "usage starts another message", usage: true, texts: 2}} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				s := newSectionIndex()
				messages := []v1.EventMessage{{Kind: v1.EventKindInit, Init: &v1.EventInit{SessionID: "root"}}, {Kind: v1.EventKindToolUse, ToolUse: &v1.EventToolUse{ToolUseID: "a", Name: "Bash"}}, {Kind: v1.EventKindTextDelta, TextDelta: &v1.EventTextDelta{Text: "first "}}}
				if c.usage {
					messages = append(messages, v1.EventMessage{Kind: v1.EventKindUsage, Usage: &v1.EventUsage{}})
				}
				messages = append(messages, v1.EventMessage{Kind: v1.EventKindToolUse, ToolUse: &v1.EventToolUse{ToolUseID: "b", Name: "Bash"}}, v1.EventMessage{Kind: v1.EventKindTextDelta, TextDelta: &v1.EventTextDelta{Text: "second"}}, v1.EventMessage{Kind: v1.EventKindText, Text: &v1.EventText{Text: "first second"}}, v1.EventMessage{Kind: v1.EventKindResult, Result: &v1.EventResult{Duration: 1}}, v1.EventMessage{Kind: v1.EventKindSystem, System: &v1.EventSystem{Subtype: "compact_boundary"}})
				for i, m := range messages {
					m.Ts = int64(i + 1)
					s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: m})
				}
				sections, _ := s.snapshot()
				if sections[0].TextCount != c.texts || sections[0].ToolCount != 2 || sections[0].TurnCount != 1 {
					t.Fatalf("summary = %#v", sections[0])
				}
			})
		}
	})
	t.Run("turn duration falls back to elapsed time", func(t *testing.T) {
		t.Parallel()
		// An ACP prompt response reports no duration; a native one wins.
		s := newSectionIndex()
		for i, m := range []v1.EventMessage{
			{Kind: v1.EventKindUserInput, Ts: 1000, UserInput: &v1.Prompt{Text: "prompt"}},
			{Kind: v1.EventKindResult, Ts: 3500, Result: &v1.EventResult{}},
			{Kind: v1.EventKindUserInput, Ts: 4000, UserInput: &v1.Prompt{Text: "prompt"}},
			{Kind: v1.EventKindResult, Ts: 9000, Result: &v1.EventResult{Duration: 0.5}},
			{Kind: v1.EventKindResult, Ts: 9500, Result: &v1.EventResult{}},
		} {
			s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: m})
		}
		_, metadata := s.snapshot()
		got := make([]int64, 0, len(metadata.Turns))
		for _, tr := range metadata.Turns {
			got = append(got, tr.DurationMS)
		}
		if want := []int64{2500, 500, 0}; !slices.Equal(got, want) {
			t.Fatalf("durations = %v, want %v", got, want)
		}
	})
	t.Run("turns are numbered across contexts with explicit starts", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		child := v1.EventMessageSource{AgentID: "child"}
		input := func(ts int64) v1.EventMessage {
			return v1.EventMessage{Kind: v1.EventKindUserInput, Ts: ts, UserInput: &v1.Prompt{Text: "prompt"}}
		}
		failed := func(ts int64) v1.EventMessage {
			return v1.EventMessage{Kind: v1.EventKindSystem, Ts: ts, System: &v1.EventSystem{Subtype: v1.EventSystemSubtypeInputDeliveryFailed}}
		}
		result := func(ts int64, src v1.EventMessageSource) v1.EventMessage {
			return v1.EventMessage{Kind: v1.EventKindResult, Ts: ts, Source: src, Result: &v1.EventResult{Duration: 0.5}}
		}
		for i, m := range []v1.EventMessage{
			input(1000),
			{Kind: v1.EventKindInit, Ts: 1500, Init: &v1.EventInit{SessionID: "first", ReportedModel: "model", ReportedEffort: "high"}},
			result(1800, child),
			result(2000, v1.EventMessageSource{}),
			input(3000),
			{Kind: v1.EventKindSystem, Ts: 3500, System: &v1.EventSystem{Subtype: "compact_boundary"}},
			result(4000, v1.EventMessageSource{}),
			// A turn the harness queues after background work ends starts at its
			// first root activity, not at the lifecycle update or child output.
			{Kind: v1.EventKindBackgroundCommand, Ts: 4200, BackgroundCommand: &v1.EventBackgroundCommand{ID: "cmd", Status: "completed"}},
			{Kind: v1.EventKindText, Ts: 4300, Source: child, Text: &v1.EventText{Text: "child"}},
			{Kind: v1.EventKindThinking, Ts: 4600, Thinking: &v1.EventThinking{}},
			result(5000, v1.EventMessageSource{}),
			// The cleared context abandons this prompt's unfinished turn; a turn
			// without root prompt or activity starts at its result.
			input(6000),
			{Kind: v1.EventKindSystem, Ts: 6500, System: &v1.EventSystem{Subtype: "context_cleared"}},
			{Kind: v1.EventKindInit, Ts: 6600, Init: &v1.EventInit{SessionID: "second", ReportedModel: "model"}},
			result(7000, v1.EventMessageSource{}),
			// An undelivered prompt neither starts the turn nor ends the wait.
			input(8000),
			failed(8100),
			input(9000),
			result(10000, v1.EventMessageSource{}),
			// Root output accepted the prompt; a later failed steer keeps it.
			input(11000),
			{Kind: v1.EventKindText, Ts: 11500, Text: &v1.EventText{Text: "working"}},
			input(11800),
			failed(11900),
			result(12000, v1.EventMessageSource{}),
			// Activity before a failed prompt keeps the turn's start.
			{Kind: v1.EventKindToolUse, Ts: 13000, ToolUse: &v1.EventToolUse{ToolUseID: "tool", Name: "Bash"}},
			input(13500),
			failed(13600),
			result(14000, v1.EventMessageSource{}),
		} {
			s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: m})
		}
		sections, metadata := s.snapshot()
		if len(sections) != 3 {
			t.Fatalf("sections = %#v", sections)
		}
		type turn struct {
			number         int
			section        string
			started, ended int64
			effort         string
		}
		got := make([]turn, 0, len(metadata.Turns))
		for _, tr := range metadata.Turns {
			got = append(got, turn{tr.Number, tr.SectionID, tr.StartedAt.UnixMilli(), tr.At.UnixMilli(), tr.Effort})
		}
		want := []turn{
			{1, "1", 1000, 2000, "high"},
			{2, "2", 3000, 4000, "high"},
			{3, "2", 4600, 5000, "high"},
			{4, "3", 7000, 7000, ""},
			{5, "3", 9000, 10000, ""},
			{6, "3", 11000, 12000, ""},
			{7, "3", 13000, 14000, ""},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("turns = %+v, want %+v", got, want)
		}
		// Each wait ends at the delivered prompt that starts the next turn.
		for i, w := range []struct {
			ms     int64
			record uint64
		}{{1000, 5}, {}, {1000, 12}, {2000, 18}, {1000, 20}, {}} {
			tr := metadata.Turns[i]
			if w.ms == 0 {
				if tr.WaitMS != nil || tr.WaitInput != nil {
					t.Errorf("turn %d wait = %v at %v, want none", tr.Number, *tr.WaitMS, tr.WaitInput)
				}
				continue
			}
			if tr.WaitMS == nil || *tr.WaitMS != w.ms || tr.WaitInput == nil || tr.WaitInput.RecordID != w.record {
				t.Errorf("turn %d wait = %v at %v, want %dms at record %d", tr.Number, tr.WaitMS, tr.WaitInput, w.ms, w.record)
			}
		}
	})
	t.Run("consolidated and interrupted text counts", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name     string
			messages []v1.EventMessage
			tools    int
		}{
			{name: "Pi consolidated thinking and text", messages: []v1.EventMessage{{Kind: v1.EventKindThinkingDelta, ThinkingDelta: &v1.EventThinkingDelta{Text: "thought"}}, {Kind: v1.EventKindTextDelta, TextDelta: &v1.EventTextDelta{Text: "answer"}}, {Kind: v1.EventKindThinking, Thinking: &v1.EventThinking{Text: "thought"}}, {Kind: v1.EventKindText, Text: &v1.EventText{Text: "answer"}}}},
			{name: "matched legacy tool result", tools: 1, messages: []v1.EventMessage{{Kind: v1.EventKindToolUse, ToolUse: &v1.EventToolUse{ToolUseID: "tool", Name: "Bash"}}, {Kind: v1.EventKindTextDelta, TextDelta: &v1.EventTextDelta{Text: "answer"}}, {Kind: v1.EventKindToolResult, ToolResult: &v1.EventToolResult{ToolUseID: "tool"}}, {Kind: v1.EventKindText, Text: &v1.EventText{Text: "answer"}}}},
			{name: "hidden allowed quota", messages: []v1.EventMessage{{Kind: v1.EventKindTextDelta, TextDelta: &v1.EventTextDelta{Text: "first"}}, {Kind: v1.EventKindRateLimit, RateLimit: &v1.EventRateLimit{Status: v1.EventRateLimitStatusAllowed}}, {Kind: v1.EventKindTextDelta, TextDelta: &v1.EventTextDelta{Text: "last"}}, {Kind: v1.EventKindText, Text: &v1.EventText{Text: "first last"}}}},
			{name: "single thinking with blank text", messages: []v1.EventMessage{{Kind: v1.EventKindThinking, Thinking: &v1.EventThinking{Text: "thought"}}, {Kind: v1.EventKindText, Text: &v1.EventText{Text: "\n"}}}},
			{name: "blank final snapshot preserves nonblank group", messages: []v1.EventMessage{{Kind: v1.EventKindTextDelta, TextDelta: &v1.EventTextDelta{Text: "answer"}}, {Kind: v1.EventKindText, Text: &v1.EventText{Text: "answer"}}, {Kind: v1.EventKindUsage, Usage: &v1.EventUsage{}}, {Kind: v1.EventKindText, Text: &v1.EventText{Text: "\n"}}}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				s := newSectionIndex()
				s.append(&v1.HistoryEvent{RecordID: 1, Message: v1.EventMessage{Kind: v1.EventKindInit, Init: &v1.EventInit{SessionID: "root"}}})
				for i, m := range c.messages {
					m.Ts = int64(i + 1)
					s.append(&v1.HistoryEvent{RecordID: uint64(i + 2), Message: m})
				}
				s.append(&v1.HistoryEvent{RecordID: uint64(len(c.messages) + 2), Message: v1.EventMessage{Kind: v1.EventKindResult, Result: &v1.EventResult{Duration: 1}}})
				s.append(&v1.HistoryEvent{RecordID: uint64(len(c.messages) + 3), Message: v1.EventMessage{Kind: v1.EventKindSystem, System: &v1.EventSystem{Subtype: "compact_boundary"}}})
				sections, _ := s.snapshot()
				if sections[0].TextCount != 1 || sections[0].ToolCount != c.tools || sections[0].TurnCount != 1 {
					t.Fatalf("summary = %#v", sections[0])
				}
			})
		}
	})
	t.Run("first canonical child publishes once", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		before := s.revision
		message := v1.EventMessage{Kind: v1.EventKindTextDelta, Ts: 1000, Source: v1.EventMessageSource{AgentID: "child"}, TextDelta: &v1.EventTextDelta{Text: "first"}}
		s.append(&v1.HistoryEvent{RecordID: 1, Message: message})
		if s.revision <= before {
			t.Fatal("new child did not revise manifest")
		}
		published := s.revision
		message.Ts = 2000
		message.Source.ThreadID = "another-observation-thread"
		message.Source.DelegationToolUseID = "reported-later"
		s.append(&v1.HistoryEvent{RecordID: 2, Message: message})
		if s.revision != published {
			t.Fatal("ordinary child delta revised manifest")
		}
		_, meta := s.snapshot()
		if len(meta.Activities) != 1 || meta.Activities[0].ID != "child" {
			t.Fatalf("activities = %#v", meta.Activities)
		}
	})
	t.Run("activity origin sections use exact positions and resolved ownership", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		s.append(&v1.HistoryEvent{RecordID: 1, Message: v1.EventMessage{Kind: v1.EventKindInit, Ts: 1000, Init: &v1.EventInit{SessionID: "a"}}})
		s.append(&v1.HistoryEvent{RecordID: 2, Index: 0, Message: v1.EventMessage{Kind: v1.EventKindText, Ts: 1000, Source: v1.EventMessageSource{DelegationToolUseID: "spawn", ThreadID: "root"}, Text: &v1.EventText{Text: "delegated origin"}}})
		s.append(&v1.HistoryEvent{RecordID: 2, Index: 2, Message: v1.EventMessage{Kind: v1.EventKindSystem, Ts: 1000, System: &v1.EventSystem{Subtype: "compact_boundary"}}})
		s.append(&v1.HistoryEvent{RecordID: 2, Index: 3, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 1000, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "child", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		s.append(&v1.HistoryEvent{RecordID: 4, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 2000, NativeSubagent: &v1.EventNativeSubagent{ID: "status-only", Status: v1.EventNativeSubagentStatusRunning}}})
		s.append(&v1.HistoryEvent{RecordID: 5, Message: v1.EventMessage{Kind: v1.EventKindSystem, Ts: 2000, System: &v1.EventSystem{Subtype: "compact_boundary"}}})
		s.append(&v1.HistoryEvent{RecordID: 6, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 3000, NativeSubagent: &v1.EventNativeSubagent{ID: "status-only", Status: v1.EventNativeSubagentStatusCompleted}}})
		_, meta := s.snapshot()
		for _, activity := range meta.Activities {
			want := "1"
			if activity.ID == "status-only" {
				want = "2"
			}
			if activity.SectionID != want {
				t.Fatalf("%s section = %q, want %q", activity.ID, activity.SectionID, want)
			}
		}
		// A competing correlation removes the old delegated origin. The child's
		// conservative resource First still includes it, but its home must move.
		s.append(&v1.HistoryEvent{RecordID: 7, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 4000, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "other", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		_, ambiguous := s.snapshot()
		for _, activity := range ambiguous.Activities {
			if activity.ID == "child" && (activity.SectionID != "2" || activity.First.RecordID != 2 || activity.First.Index != 0) {
				t.Fatalf("ambiguous origin = %#v", activity)
			}
		}
		if meta.Activities[0].SectionID != "1" {
			t.Fatal("captured section mutated")
		}
	})
	t.Run("late correlation pins transcript with scoped ambiguity", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		s.append(&v1.HistoryEvent{RecordID: 1, Message: v1.EventMessage{Kind: v1.EventKindTextDelta, Ts: 1000, Source: v1.EventMessageSource{ThreadID: "root", DelegationToolUseID: "spawn"}, TextDelta: &v1.EventTextDelta{Text: "early"}}})
		for i, status := range []v1.EventNativeSubagentStatus{v1.EventNativeSubagentStatusRunning, v1.EventNativeSubagentStatusCompleted} {
			s.append(&v1.HistoryEvent{RecordID: uint64(i + 2), Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: int64(i+2) * 1000, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "child", ToolUseID: "spawn", Status: status}}})
		}
		_, meta := s.snapshot()
		if len(meta.Activities) != 1 || meta.Activities[0].AnchorAt.UnixMilli() != 1000 {
			t.Fatalf("late anchor = %#v", meta.Activities)
		}
		s.append(&v1.HistoryEvent{RecordID: 4, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 4000, Source: v1.EventMessageSource{ThreadID: "other-root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "other", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		_, scoped := s.snapshot()
		for _, a := range scoped.Activities {
			if a.ID == "child" && a.AnchorAt.UnixMilli() != 1000 {
				t.Fatalf("foreign namespace changed anchor = %#v", a)
			}
		}
		s.append(&v1.HistoryEvent{RecordID: 5, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 5000, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "ambiguous", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		_, ambiguous := s.snapshot()
		for _, a := range ambiguous.Activities {
			if a.ID == "child" && a.AnchorAt.UnixMilli() != 3000 {
				t.Fatalf("ambiguous transcript still owns anchor = %#v", a)
			}
		}
		if meta.Activities[0].AnchorAt.UnixMilli() != 1000 {
			t.Fatal("captured anchor mutated")
		}
	})
	t.Run("late and ambiguous ownership revises closed headers", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		messages := []v1.EventMessage{
			{Kind: v1.EventKindInit, Init: &v1.EventInit{SessionID: "root"}},
			{Kind: v1.EventKindText, Text: &v1.EventText{Text: "root"}},
			{Kind: v1.EventKindText, Text: &v1.EventText{Text: "pending child"}, Source: v1.EventMessageSource{ThreadID: "root", DelegationToolUseID: "spawn"}},
			{Kind: v1.EventKindResult, Result: &v1.EventResult{Duration: 1}},
			{Kind: v1.EventKindSystem, System: &v1.EventSystem{Subtype: "compact_boundary"}},
		}
		for i, m := range messages {
			s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: m})
		}
		before, _ := s.snapshot()
		if before[0].TextCount != 2 {
			t.Fatalf("uncorrelated header = %#v", before[0])
		}
		s.append(&v1.HistoryEvent{RecordID: 6, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "first", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		resolved, _ := s.snapshot()
		if resolved[0].TextCount != 1 {
			t.Fatalf("resolved header = %#v", resolved[0])
		}
		s.append(&v1.HistoryEvent{RecordID: 7, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "second", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		ambiguous, _ := s.snapshot()
		if ambiguous[0].TextCount != 2 {
			t.Fatalf("ambiguous header = %#v", ambiguous[0])
		}
		if before[0].TextCount != 2 || resolved[0].TextCount != 1 {
			t.Fatal("published snapshots were mutated")
		}
	})
	t.Run("compact numeric and activity metadata", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		events := []v1.EventMessage{
			{Kind: v1.EventKindInit, Init: &v1.EventInit{SessionID: "first", ReportedModel: "model"}},
			{Kind: v1.EventKindToolUse, Ts: 100, ToolUse: &v1.EventToolUse{ToolUseID: "child-tool", Name: "Bash", Input: json.RawMessage(`{"command":"large"}`)}, Source: v1.EventMessageSource{AgentID: "child"}},
			{Kind: v1.EventKindToolResult, Ts: 200, ToolResult: &v1.EventToolResult{ToolUseID: "child-tool", Error: strings.Repeat("payload", 100)}, Source: v1.EventMessageSource{AgentID: "child"}},
			{Kind: v1.EventKindNativeSubagent, Ts: 100, NativeSubagent: &v1.EventNativeSubagent{ID: "child", ToolUseID: "spawn", Kind: "general-purpose", Prompt: strings.Repeat("prompt", 100), Status: v1.EventNativeSubagentStatusRunning}},
			{Kind: v1.EventKindSkillLoad, SkillLoad: &v1.EventSkillLoad{Skill: "quality"}},
			{Kind: v1.EventKindSkillLoad, SkillLoad: &v1.EventSkillLoad{Skill: "quality", Path: "/quality/SKILL.md"}},
			{Kind: v1.EventKindResult, Ts: 300, Result: &v1.EventResult{Result: strings.Repeat("result", 100), Duration: 0.1, DurationAPI: 0.05, Usage: v1.EventUsage{InputTokens: 10}}},
			{Kind: v1.EventKindSystem, System: &v1.EventSystem{Subtype: "compact_boundary"}},
			{Kind: v1.EventKindNativeSubagent, Ts: 400, NativeSubagent: &v1.EventNativeSubagent{ID: "child", Status: v1.EventNativeSubagentStatusCompleted, Result: strings.Repeat("child result", 100)}},
		}
		for i, m := range events {
			s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: m})
		}
		sections, metadata := s.snapshot()
		if len(sections) != 2 || !reflect.DeepEqual(sections[0].Skills, []v1.EventSkillLoad{{Skill: "quality", Path: "/quality/SKILL.md"}}) {
			t.Fatalf("sections = %#v", sections)
		}
		if len(metadata.Tools) != 1 || metadata.Tools[0].Name != "Bash" || metadata.Tools[0].Calls != 1 || metadata.Tools[0].DurationMS != 100 {
			t.Fatalf("tool totals = %#v", metadata.Tools)
		}
		// The kind survives the terminal update that does not repeat it.
		if len(metadata.Activities) != 1 || metadata.Activities[0].Status != "completed" || metadata.Activities[0].Kind != "general-purpose" || metadata.Activities[0].StartedAt == nil || metadata.Activities[0].EndedAt == nil {
			t.Fatalf("activities = %#v", metadata.Activities)
		}
		encoded, err := json.Marshal(v1.HistoryManifest{Sections: sections, Metadata: metadata})
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]json.RawMessage
		if err = json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		// The repeated strings are actual transcript payloads, not metadata.
		if bytes.Contains(encoded, []byte(strings.Repeat("prompt", 100))) || bytes.Contains(encoded, []byte(strings.Repeat("result", 100))) || bytes.Contains(encoded, []byte(strings.Repeat("payload", 100))) {
			t.Fatal("manifest retained transcript payload")
		}
	})
	t.Run("root exit ends running lifecycles with unknown outcome", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		child := v1.EventMessageSource{AgentID: "done"}
		for i, m := range []v1.EventMessage{
			{Kind: v1.EventKindNativeSubagent, Ts: 1000, NativeSubagent: &v1.EventNativeSubagent{ID: "running", Status: v1.EventNativeSubagentStatusRunning, Background: true}},
			{Kind: v1.EventKindNativeSubagent, Ts: 1000, NativeSubagent: &v1.EventNativeSubagent{ID: "done", Status: v1.EventNativeSubagentStatusRunning}},
			{Kind: v1.EventKindNativeSubagent, Ts: 2000, NativeSubagent: &v1.EventNativeSubagent{ID: "done", Status: v1.EventNativeSubagentStatusCompleted}},
			{Kind: v1.EventKindBackgroundCommand, Ts: 2000, BackgroundCommand: &v1.EventBackgroundCommand{ID: "watch", Status: "running"}},
			// An exit error event alone ends nothing: Source folds the exit record.
			{Kind: v1.EventKindError, Ts: 2500, Source: child, Error: &v1.EventError{Err: "child exited"}},
		} {
			s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: m})
		}
		s.hostExited(time.UnixMilli(3000).UTC())
		_, meta := s.snapshot()
		statuses := map[string]string{}
		for _, a := range meta.Activities {
			statuses[a.ID] = a.Status
			if a.ID == "running" && !a.AnchorAt.Equal(time.UnixMilli(3000).UTC()) {
				t.Errorf("running anchor = %v, want the exit time", a.AnchorAt)
			}
		}
		if want := map[string]string{"running": "unknown", "done": "completed"}; !reflect.DeepEqual(statuses, want) {
			t.Fatalf("activity statuses = %v, want %v", statuses, want)
		}
		if len(meta.BackgroundCommands) != 1 || meta.BackgroundCommands[0].Status != "unknown" {
			t.Fatalf("background commands = %#v", meta.BackgroundCommands)
		}
		// A resumed host reporting the card again reopens it.
		s.append(&v1.HistoryEvent{RecordID: 7, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 4000, NativeSubagent: &v1.EventNativeSubagent{ID: "running", Status: v1.EventNativeSubagentStatusRunning}}})
		_, meta = s.snapshot()
		for _, a := range meta.Activities {
			if a.ID == "running" && a.Status != "running" {
				t.Fatalf("reopened status = %q, want running", a.Status)
			}
		}
	})
	t.Run("retirement ends only the named running card", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		for i, m := range []v1.EventMessage{
			{Kind: v1.EventKindNativeSubagent, Ts: 1000, NativeSubagent: &v1.EventNativeSubagent{ID: "agent", Status: v1.EventNativeSubagentStatusRunning, Background: true}},
			{Kind: v1.EventKindNativeSubagent, Ts: 1000, NativeSubagent: &v1.EventNativeSubagent{ID: "kept", Status: v1.EventNativeSubagentStatusRunning, Background: true}},
			// A lifecycle update without a status, as Codex re-sends, keeps running.
			{Kind: v1.EventKindNativeSubagent, Ts: 1500, NativeSubagent: &v1.EventNativeSubagent{ID: "kept", Status: v1.EventNativeSubagentStatusUnknown}},
			{Kind: v1.EventKindBackgroundCommand, Ts: 1000, BackgroundCommand: &v1.EventBackgroundCommand{ID: "cmd", Status: "running"}},
			{Kind: v1.EventKindBackgroundCommand, Ts: 1000, BackgroundCommand: &v1.EventBackgroundCommand{ID: "done", Status: "completed"}},
			{Kind: v1.EventKindBackgroundRetired, Ts: 2000, BackgroundRetired: &v1.EventBackgroundRetired{ID: "agent"}},
			{Kind: v1.EventKindBackgroundRetired, Ts: 2000, BackgroundRetired: &v1.EventBackgroundRetired{ID: "cmd"}},
			{Kind: v1.EventKindBackgroundRetired, Ts: 2000, BackgroundRetired: &v1.EventBackgroundRetired{ID: "done"}},
		} {
			s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: m})
		}
		_, meta := s.snapshot()
		statuses := map[string]string{}
		for _, a := range meta.Activities {
			statuses[a.ID] = a.Status
			// The disclosure body includes the retirement.
			if a.ID == "agent" && a.BodyThrough != 6 {
				t.Errorf("bodyThrough = %d, want the retirement record", a.BodyThrough)
			}
		}
		for _, c := range meta.BackgroundCommands {
			statuses[c.ID] = c.Status
		}
		if want := map[string]string{"agent": "unknown", "kept": "running", "cmd": "unknown", "done": "completed"}; !reflect.DeepEqual(statuses, want) {
			t.Fatalf("statuses = %v, want %v", statuses, want)
		}
	})
	t.Run("background commands fold outside native activities", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		source := v1.EventMessageSource{ThreadID: "root"}
		exit := 2
		for i, b := range []v1.EventBackgroundCommand{
			{ID: "watch", ToolUseID: "watch-tool", Status: "running", Watch: true, WakesAgent: true},
			{ID: "watch", Status: "running"},
			{ID: "lint", ToolUseID: "lint-tool", Label: "make lint", Status: "running"},
			{ID: "lint", Status: "failed", ExitCode: &exit, OutputRef: "/tmp/lint.output"},
		} {
			s.append(&v1.HistoryEvent{RecordID: uint64(i + 1), Message: v1.EventMessage{Kind: v1.EventKindBackgroundCommand, Ts: int64(i+1) * 1000, Source: source, BackgroundCommand: &b}})
		}
		_, meta := s.snapshot()
		if len(meta.Activities) != 0 {
			t.Fatalf("activities = %#v", meta.Activities)
		}
		started, watchStarted, ended := time.UnixMilli(3000).UTC(), time.UnixMilli(1000).UTC(), time.UnixMilli(4000).UTC()
		want := []v1.HistoryBackgroundCommand{
			{ID: "watch", Source: source, ToolUseID: "watch-tool", Status: "running", StartedAt: &watchStarted, Watch: true, WakesAgent: true},
			{ID: "lint", Source: source, ToolUseID: "lint-tool", Label: "make lint", Status: "failed", StartedAt: &started, EndedAt: &ended, ExitCode: &exit, OutputRef: "/tmp/lint.output"},
		}
		if !reflect.DeepEqual(meta.BackgroundCommands, want) {
			t.Fatalf("background commands = %#v, want %#v", meta.BackgroundCommands, want)
		}
	})
}
