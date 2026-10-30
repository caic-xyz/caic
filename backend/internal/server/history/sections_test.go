// Tests context manifests, payload-free summaries, exact slices, and finite activity disclosures.

package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestSourceSections(t *testing.T) {
	t.Parallel()
	t.Run("actor body bounds ignore unrelated records and include ownership revisions", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		s.append(&Event{RecordID: 1, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, NativeSubagent: &v1.EventNativeSubagent{ID: "child", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		s.append(&Event{RecordID: 2, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{AgentID: "child"}, Text: &v1.EventText{Text: "direct"}}})
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
		s.append(&Event{RecordID: 3, Message: v1.EventMessage{Kind: v1.EventKindText, Text: &v1.EventText{Text: "root"}}})
		s.append(&Event{RecordID: 4, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, NativeSubagent: &v1.EventNativeSubagent{ID: "other", ToolUseID: "unrelated", Status: v1.EventNativeSubagentStatusRunning}}})
		assertBounds(2, 1)
		s.append(&Event{RecordID: 5, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{DelegationToolUseID: "spawn", ThreadID: "root"}, Text: &v1.EventText{Text: "delegated"}}})
		assertBounds(5, 5)
		s.append(&Event{RecordID: 6, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{DelegationToolUseID: "spawn", ThreadID: "root"}, Text: &v1.EventText{Text: "more delegated output"}}})
		assertBounds(6, 5)
		s.append(&Event{RecordID: 7, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, NativeSubagent: &v1.EventNativeSubagent{ID: "competing", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		assertBounds(7, 7)
		s.append(&Event{RecordID: 8, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{DelegationToolUseID: "spawn", ThreadID: "another-root"}, Text: &v1.EventText{Text: "namespace"}}})
		assertBounds(8, 8)
		// Multiple outputs in one complete raw record expose one coherent pair
		// of bounds, whether its routing observation is related or unrelated.
		s.append(&Event{RecordID: 9, Index: 0, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{AgentID: "child"}, Text: &v1.EventText{Text: "same-record output"}}})
		s.append(&Event{RecordID: 9, Index: 1, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, NativeSubagent: &v1.EventNativeSubagent{ID: "sibling", ToolUseID: "another-tool", Status: v1.EventNativeSubagentStatusRunning}}})
		assertBounds(9, 8)
		s.append(&Event{RecordID: 10, Index: 0, Message: v1.EventMessage{Kind: v1.EventKindText, Source: v1.EventMessageSource{AgentID: "child"}, Text: &v1.EventText{Text: "same-record related output"}}})
		s.append(&Event{RecordID: 10, Index: 1, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "scoped-competitor", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
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
			want    [][]Position
			turns   []int
		}{
			{name: "clear and empty context", batches: [][]agent.Message{{&agent.InitMessage{SessionID: "one"}, &agent.TextMessage{Text: "before"}, &agent.ResultMessage{DurationMs: 1000}, agent.ContextCleared(), &agent.InitMessage{SessionID: "two"}, &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}}}, want: [][]Position{{{RecordID: 2, Index: 0}, {RecordID: 2, Index: 2}}, {{RecordID: 2, Index: 3}, {RecordID: 2, Index: 4}}, {{RecordID: 2, Index: 5}}}, turns: []int{1, 0, 0}},
			{name: "changed session restart", batches: [][]agent.Message{{&agent.InitMessage{SessionID: "one"}, &agent.TextMessage{Text: "before"}, &agent.ResultMessage{DurationMs: 1000}, &agent.UserInputMessage{Text: "carry"}, &agent.InitMessage{SessionID: "two"}, &agent.TextMessage{Text: "after"}, &agent.ResultMessage{DurationMs: 2000}}}, want: [][]Position{{{RecordID: 2, Index: 0}, {RecordID: 2, Index: 2}}, {{RecordID: 2, Index: 3}, {RecordID: 2, Index: 6}}}, turns: []int{1, 1}},
			{name: "compaction restart and repeated init", batches: [][]agent.Message{{&agent.InitMessage{SessionID: "one"}, &agent.ResultMessage{DurationMs: 1000}}, {&agent.UserInputMessage{Text: "carry"}, &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, &agent.InitMessage{SessionID: "two"}, &agent.InitMessage{SessionID: "two"}, &agent.TextMessage{Text: "after"}, &agent.ResultMessage{DurationMs: 2000}}}, want: [][]Position{{{RecordID: 2, Index: 0}, {RecordID: 2, Index: 1}}, {{RecordID: 3, Index: 0}, {RecordID: 3, Index: 5}}}, turns: []int{1, 1}},
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
					return &agenttest.Wire{Parse: func(data []byte) ([]agent.Message, error) {
						var input map[string]int
						if err := json.Unmarshal(data, &input); err != nil {
							return nil, err
						}
						return c.batches[input["batch"]], nil
					}}
				}}}
				source, err := NewSource(t.Context(), path, resolver)
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
				var joined []Position
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
					var events []Event
					if err = json.Unmarshal(body.Bytes(), &events); err != nil {
						t.Fatal(err)
					}
					if len(events) == 0 {
						t.Fatal("empty slice despite explicit manifest bounds")
					}
					bounds := []Position{eventPosition(&events[0])}
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
				var canonical []Position
				for e, err := range source.Events(t.Context(), snap, 1, snap.Through) {
					if err != nil {
						t.Fatal(err)
					}
					canonical = append(canonical, eventPosition(&e))
				}
				if !reflect.DeepEqual(joined, canonical) {
					t.Fatalf("partition lost/duplicated outputs: %#v vs %#v", joined, canonical)
				}
				if len(metadata.Turns) != 2 && c.name != "clear and empty context" {
					t.Fatalf("turn facts = %#v", metadata.Turns)
				}
			})
		}
	})
	t.Run("setup excludes interleaved transcript", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader, `{"t":"log","line":"first setup line"}`, twoTexts, `{"t":"log","line":"later setup line"}`)
		source, err := NewSource(t.Context(), path, backends.Default("", nil))
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
		var events []Event
		if err = json.Unmarshal(body.Bytes(), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[0].Message.Log == nil || events[1].Message.Log == nil || events[0].Message.Log.Line != "first setup line" || events[1].Message.Log.Line != "later setup line" {
			t.Fatalf("setup events = %#v", events)
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
						&agent.TextMessage{Source: agent.MessageSource{AgentID: "child"}, Text: "child transcript"},
						&agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "shell", Status: agent.BackgroundCommandStatusRunning}},
					}
					var opening agent.Message = &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}
					if boundary == "changed init" {
						opening = &agent.InitMessage{SessionID: "two"}
					}
					suffix := []agent.Message{opening,
						&agent.TextMessage{Text: "second turn"},
						&agent.ResultMessage{DurationMs: 1000},
						&agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: "child", Status: agent.NativeSubagentStatusCompleted}},
						&agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "shell", Status: agent.BackgroundCommandStatusCompleted}},
					}
					batches := [][]agent.Message{prefix, suffix, append(slices.Clone(prefix), suffix...)}
					resolver := agent.Backends{harness.Claude: &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
						return &agenttest.Wire{Parse: func(data []byte) ([]agent.Message, error) {
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
						source, err := NewSource(t.Context(), path, resolver)
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
						owners := map[Position]string{}
						origins := map[string]Position{}
						for _, section := range sections {
							if section.Body == nil {
								continue
							}
							var body bytes.Buffer
							if err := source.WriteSlice(t.Context(), snap, *section.Body, &body); err != nil {
								t.Fatal(err)
							}
							var events []Event
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
								case event.Message.BackgroundCommand != nil:
									id = event.Message.BackgroundCommand.ID
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
						if len(metadata.Activities) != 2 {
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
		source, err := NewSource(t.Context(), path, backends.Default("", nil))
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
		var events []Event
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
		source, err := NewSource(t.Context(), path, backends.Default("", nil))
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
		selected := v1.HistorySlice{First: Position{RecordID: 2, Index: 1}, Last: Position{RecordID: 2, Index: 1}}
		if err = source.CheckSlice(t.Context(), snap, selected); err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		if err = source.WriteSlice(t.Context(), snap, selected, &body); err != nil {
			t.Fatal(err)
		}
		var events []Event
		if err = json.Unmarshal(body.Bytes(), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Index != 1 || events[0].Message.Text.Text != "second" {
			t.Fatalf("selected = %#v", events)
		}
		selected.Last = Position{RecordID: 3, Index: RecordEndIndex}
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
		empty := v1.HistorySlice{First: Position{RecordID: 3}, Last: Position{RecordID: 3, Index: RecordEndIndex}}
		if err = source.CheckSlice(t.Context(), snap, empty); err != nil {
			t.Fatal(err)
		}
		var emptyBody bytes.Buffer
		if err = source.WriteSlice(t.Context(), snap, empty, &emptyBody); err != nil {
			t.Fatal(err)
		}
		var emptyEvents []Event
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
		selected.Last = Position{RecordID: 2, Index: 2}
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
					s.append(&Event{RecordID: uint64(i + 1), Message: m})
				}
				sections, _ := s.snapshot()
				if sections[0].TextCount != c.texts || sections[0].ToolCount != 2 || sections[0].TurnCount != 1 {
					t.Fatalf("summary = %#v", sections[0])
				}
			})
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
				s.append(&Event{RecordID: 1, Message: v1.EventMessage{Kind: v1.EventKindInit, Init: &v1.EventInit{SessionID: "root"}}})
				for i, m := range c.messages {
					m.Ts = int64(i + 1)
					s.append(&Event{RecordID: uint64(i + 2), Message: m})
				}
				s.append(&Event{RecordID: uint64(len(c.messages) + 2), Message: v1.EventMessage{Kind: v1.EventKindResult, Result: &v1.EventResult{Duration: 1}}})
				s.append(&Event{RecordID: uint64(len(c.messages) + 3), Message: v1.EventMessage{Kind: v1.EventKindSystem, System: &v1.EventSystem{Subtype: "compact_boundary"}}})
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
		s.append(&Event{RecordID: 1, Message: message})
		if s.revision <= before {
			t.Fatal("new child did not revise manifest")
		}
		published := s.revision
		message.Ts = 2000
		message.Source.ThreadID = "another-observation-thread"
		message.Source.DelegationToolUseID = "reported-later"
		s.append(&Event{RecordID: 2, Message: message})
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
		s.append(&Event{RecordID: 1, Message: v1.EventMessage{Kind: v1.EventKindInit, Ts: 1000, Init: &v1.EventInit{SessionID: "a"}}})
		s.append(&Event{RecordID: 2, Index: 0, Message: v1.EventMessage{Kind: v1.EventKindText, Ts: 1000, Source: v1.EventMessageSource{DelegationToolUseID: "spawn", ThreadID: "root"}, Text: &v1.EventText{Text: "delegated origin"}}})
		s.append(&Event{RecordID: 2, Index: 1, Message: v1.EventMessage{Kind: v1.EventKindBackgroundCommand, Ts: 1000, BackgroundCommand: &v1.EventBackgroundCommand{ID: "shell", Status: "running"}}})
		s.append(&Event{RecordID: 2, Index: 2, Message: v1.EventMessage{Kind: v1.EventKindSystem, Ts: 1000, System: &v1.EventSystem{Subtype: "compact_boundary"}}})
		s.append(&Event{RecordID: 2, Index: 3, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 1000, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "child", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		s.append(&Event{RecordID: 3, Message: v1.EventMessage{Kind: v1.EventKindBackgroundCommand, Ts: 2000, BackgroundCommand: &v1.EventBackgroundCommand{ID: "shell", Status: "completed"}}})
		s.append(&Event{RecordID: 4, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 2000, NativeSubagent: &v1.EventNativeSubagent{ID: "status-only", Status: v1.EventNativeSubagentStatusRunning}}})
		s.append(&Event{RecordID: 5, Message: v1.EventMessage{Kind: v1.EventKindSystem, Ts: 2000, System: &v1.EventSystem{Subtype: "compact_boundary"}}})
		s.append(&Event{RecordID: 6, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 3000, NativeSubagent: &v1.EventNativeSubagent{ID: "status-only", Status: v1.EventNativeSubagentStatusCompleted}}})
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
		s.append(&Event{RecordID: 7, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 4000, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "other", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
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
		s.append(&Event{RecordID: 1, Message: v1.EventMessage{Kind: v1.EventKindTextDelta, Ts: 1000, Source: v1.EventMessageSource{ThreadID: "root", DelegationToolUseID: "spawn"}, TextDelta: &v1.EventTextDelta{Text: "early"}}})
		for i, status := range []v1.EventNativeSubagentStatus{v1.EventNativeSubagentStatusRunning, v1.EventNativeSubagentStatusCompleted} {
			s.append(&Event{RecordID: uint64(i + 2), Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: int64(i+2) * 1000, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "child", ToolUseID: "spawn", Status: status}}})
		}
		_, meta := s.snapshot()
		if len(meta.Activities) != 1 || meta.Activities[0].AnchorAt.UnixMilli() != 1000 {
			t.Fatalf("late anchor = %#v", meta.Activities)
		}
		s.append(&Event{RecordID: 4, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 4000, Source: v1.EventMessageSource{ThreadID: "other-root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "other", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		_, scoped := s.snapshot()
		for _, a := range scoped.Activities {
			if a.ID == "child" && a.AnchorAt.UnixMilli() != 1000 {
				t.Fatalf("foreign namespace changed anchor = %#v", a)
			}
		}
		s.append(&Event{RecordID: 5, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Ts: 5000, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "ambiguous", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
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
	t.Run("hidden status does not extend invocation fallback", func(t *testing.T) {
		t.Parallel()
		s := newSectionIndex()
		messages := []v1.EventMessage{
			{Kind: v1.EventKindInit, Ts: 100, Init: &v1.EventInit{SessionID: "root"}},
			{Kind: v1.EventKindSystem, Ts: 120, System: &v1.EventSystem{Subtype: "active"}},
			{Kind: v1.EventKindUserInput, Ts: 140, UserInput: &v1.EventUserInput{Text: "continue"}},
			{Kind: v1.EventKindResult, Ts: 200, Result: &v1.EventResult{}},
			{Kind: v1.EventKindCommitSnapshot, Ts: 220, CommitSnapshot: &v1.EventCommitSnapshot{}},
			{Kind: v1.EventKindSystem, Ts: 240, System: &v1.EventSystem{Subtype: "compact_boundary"}},
		}
		for i, m := range messages {
			s.append(&Event{RecordID: uint64(i + 1), Message: m})
		}
		sections, _ := s.snapshot()
		if len(sections) != 2 || sections[0].DurationMS != 60 {
			t.Fatalf("fallback summary = %#v", sections)
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
			s.append(&Event{RecordID: uint64(i + 1), Message: m})
		}
		before, _ := s.snapshot()
		if before[0].TextCount != 2 {
			t.Fatalf("uncorrelated header = %#v", before[0])
		}
		s.append(&Event{RecordID: 6, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "first", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
		resolved, _ := s.snapshot()
		if resolved[0].TextCount != 1 {
			t.Fatalf("resolved header = %#v", resolved[0])
		}
		s.append(&Event{RecordID: 7, Message: v1.EventMessage{Kind: v1.EventKindNativeSubagent, Source: v1.EventMessageSource{ThreadID: "root"}, NativeSubagent: &v1.EventNativeSubagent{ID: "second", ToolUseID: "spawn", Status: v1.EventNativeSubagentStatusRunning}}})
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
			{Kind: v1.EventKindNativeSubagent, Ts: 100, NativeSubagent: &v1.EventNativeSubagent{ID: "child", ToolUseID: "spawn", Prompt: strings.Repeat("prompt", 100), Status: v1.EventNativeSubagentStatusRunning}},
			{Kind: v1.EventKindSkillLoad, SkillLoad: &v1.EventSkillLoad{Skill: "quality"}},
			{Kind: v1.EventKindSkillLoad, SkillLoad: &v1.EventSkillLoad{Skill: "quality", Path: "/quality/SKILL.md"}},
			{Kind: v1.EventKindResult, Ts: 300, Result: &v1.EventResult{Result: strings.Repeat("result", 100), Duration: 0.1, DurationAPI: 0.05, Usage: v1.EventUsage{InputTokens: 10}}},
			{Kind: v1.EventKindSystem, System: &v1.EventSystem{Subtype: "compact_boundary"}},
			{Kind: v1.EventKindNativeSubagent, Ts: 400, NativeSubagent: &v1.EventNativeSubagent{ID: "child", Status: v1.EventNativeSubagentStatusCompleted, Result: strings.Repeat("child result", 100)}},
		}
		for i, m := range events {
			s.append(&Event{RecordID: uint64(i + 1), Message: m})
		}
		sections, metadata := s.snapshot()
		if len(sections) != 2 || !reflect.DeepEqual(sections[0].Skills, []v1.EventSkillLoad{{Skill: "quality", Path: "/quality/SKILL.md"}}) {
			t.Fatalf("sections = %#v", sections)
		}
		if len(metadata.Tools) != 1 || metadata.Tools[0].Name != "Bash" || metadata.Tools[0].Calls != 1 || metadata.Tools[0].DurationMS != 100 {
			t.Fatalf("tool totals = %#v", metadata.Tools)
		}
		if len(metadata.Activities) != 1 || metadata.Activities[0].Status != "completed" || metadata.Activities[0].StartedAt == nil || metadata.Activities[0].EndedAt == nil {
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
}
