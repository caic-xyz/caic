// Rollup sink tests: spy-pinned fold forwarding plus real-store replay resume through the task folds.

package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

// rollupSpy records every RollupSink call so the forwarding subtests can
// assert what the task folds forwarded. It stays in-package: this in-package
// test cannot import a tasktest package that itself imports task.
type rollupSpy struct {
	Observes []rollupObserve
	Quotas   []usagedb.QuotaChange
	Closed   int
}

// Observe records the call.
func (s *rollupSpy) Observe(meta usagedb.TaskMeta, e *usagedb.Event) {
	s.Observes = append(s.Observes, rollupObserve{Meta: meta, Event: *e})
}

// ObserveQuota records the change.
func (s *rollupSpy) ObserveQuota(c *usagedb.QuotaChange) { s.Quotas = append(s.Quotas, *c) }

// Close records the close.
func (s *rollupSpy) Close() error { s.Closed++; return nil }

type rollupObserve struct {
	Meta  usagedb.TaskMeta
	Event usagedb.Event
}

func TestTaskRollupForwarding(t *testing.T) {
	t.Parallel()

	t.Run("live", func(t *testing.T) {
		t.Parallel()
		sink := &rollupSpy{}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "requested-model", "")
		tk.Rollup = sink

		// Init captures the reported model; a model-less usage message must
		// attribute to it. Uninteresting messages (the init) forward nothing.
		tk.addMessage(t.Context(), &agent.InitMessage{ReportedModel: "claude-opus"}, false)
		tk.addMessage(t.Context(), &agent.UsageMessage{
			ReportedModel: "claude-opus",
			Usage:         agent.Usage{InputTokens: 10, OutputTokens: 5},
		}, false)
		tk.addMessage(t.Context(), &agent.UsageMessage{Usage: agent.Usage{OutputTokens: 3}}, false)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 0.5,
			NumTurns:     1,
		}, false)

		if len(sink.Observes) != 3 {
			t.Fatalf("observes = %d, want 3", len(sink.Observes))
		}
		for i, o := range sink.Observes {
			if o.Meta.TaskID != tk.ID || o.Meta.Harness != "claude" || o.Meta.RequestedModel != "requested-model" {
				t.Errorf("observe %d meta = %+v", i, o.Meta)
			}
			if len(o.Meta.Repos) != 0 {
				t.Errorf("observe %d repos = %v, want empty", i, o.Meta.Repos)
			}
			if o.Event.At.IsZero() {
				t.Errorf("observe %d producer time must be materialized", i)
			}
		}
		if model := sink.Observes[0].Event.Model; model != "claude-opus" {
			t.Errorf("stamped usage model = %q, want claude-opus", model)
		}
		if model := sink.Observes[1].Event.Model; model != "claude-opus" {
			t.Errorf("model-less usage model = %q, want active model claude-opus", model)
		}
		if !sink.Observes[2].Event.TurnBoundary {
			t.Errorf("result must mark the turn boundary")
		}
		// The result's cost snapshot must equal the task's live cost after
		// its fold (harness-reported total plus the cache-read surcharge).
		if cost := sink.Observes[2].Event.CostUSD; cost <= 0 {
			t.Errorf("result cost snapshot = %v, want the folded live cost", cost)
		} else if live, _, _, _, _ := tk.LiveStats(); cost != live {
			t.Errorf("result cost snapshot = %v, want live cost %v", cost, live)
		}
		if sink.Closed != 0 {
			t.Errorf("sink must not be closed by the task: %d", sink.Closed)
		}
	})

	t.Run("restore", func(t *testing.T) {
		t.Parallel()
		sink := &rollupSpy{}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "requested-model", "")
		tk.Rollup = sink
		timed := atOffset(0)
		tk.SeedTimelineEntries([]agent.TimedMessage{
			{Message: &agent.InitMessage{ReportedModel: "gpt-5.6"}, ProducerTime: timed},
			{Message: &agent.UsageMessage{ReportedModel: "gpt-5.6", ModelDerived: true, Usage: agent.Usage{OutputTokens: 9}}, ProducerTime: timed.Add(time.Second)},
			{Message: &agent.ResultMessage{MessageType: "result", NumTurns: 1}, ProducerTime: timed.Add(2 * time.Second)},
			// Timestamp-less entry (adopted task): forwarded with a zero time
			// so the sink applies its no-watermark rule.
			{Message: &agent.ToolUseMessage{Name: "Edit"}},
		})

		if len(sink.Observes) != 3 {
			t.Fatalf("observes = %d, want 3", len(sink.Observes))
		}
		for i, want := range []time.Time{timed.Add(time.Second), timed.Add(2 * time.Second), {}} {
			if !sink.Observes[i].Event.At.Equal(want) {
				t.Errorf("observe %d at = %v, want %v", i, sink.Observes[i].Event.At, want)
			}
		}
		if model := sink.Observes[0].Event.Model; model != "gpt-5.6" {
			t.Errorf("stamped usage model = %q, want gpt-5.6", model)
		}
		if model := sink.Observes[1].Event.Model; model != "gpt-5.6" {
			t.Errorf("result model = %q, want active model gpt-5.6", model)
		}
		if calls := sink.Observes[2].Event.Delta.ToolCalls["Edit"]; calls != 1 {
			t.Errorf("tool calls = %v, want one Edit", sink.Observes[2].Event.Delta.ToolCalls)
		}
		// Restore folds a codex result without a pricer, so cost stays zero.
		if cost := sink.Observes[1].Event.CostUSD; cost != 0 {
			t.Errorf("restore cost snapshot = %v, want 0", cost)
		}
	})

	t.Run("quota", func(t *testing.T) {
		t.Parallel()
		sink := &rollupSpy{}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "m", "")
		tk.Rollup = sink
		tk.addMessage(t.Context(), &agent.RateLimitMessage{
			Status: agent.RateLimitStatusAllowed, QuotaProvider: agent.QuotaProviderAnthropic,
		}, false)
		if len(sink.Quotas) != 1 {
			t.Fatalf("quota changes = %d, want 1", len(sink.Quotas))
		}
		if got := sink.Quotas[0]; got.Provider != "anthropic" || got.Status != "allowed" {
			t.Errorf("quota change = %+v", got)
		}
	})

	t.Run("discard default", func(t *testing.T) {
		t.Parallel()
		// NewTask always wires DiscardRollup, so folding is safe without a
		// real sink: one task through the live path, one through restore.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "m", "")
		if _, ok := tk.Rollup.(DiscardRollup); !ok {
			t.Fatalf("default Rollup = %T, want DiscardRollup", tk.Rollup)
		}
		tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)
		tk2 := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "m", "")
		tk2.SeedTimelineEntries([]agent.TimedMessage{{Message: &agent.ResultMessage{MessageType: "result"}}})
	})
}

func TestTaskRollupTranslation(t *testing.T) {
	t.Parallel()
	at := atOffset(0)

	t.Run("token source is disjoint per harness", func(t *testing.T) {
		t.Parallel()
		// Claude's wire carries the same per-call usage twice (assistant
		// record pair) plus a message_delta and the turn-total result; the
		// turn total must count exactly once.
		claude := []agent.Message{
			&agent.UsageMessage{ReportedModel: "claude-opus", Usage: agent.Usage{InputTokens: 10, CacheCreationInputTokens: 400, CacheReadInputTokens: 900, OutputTokens: 50, CacheTTLSeconds: 3600}},
			&agent.UsageMessage{ReportedModel: "claude-opus", Usage: agent.Usage{InputTokens: 10, CacheCreationInputTokens: 400, CacheReadInputTokens: 900, OutputTokens: 50, CacheTTLSeconds: 3600}},
			&agent.UsageMessage{ReportedModel: "claude-opus", ModelDerived: true, Usage: agent.Usage{InputTokens: 10, CacheCreationInputTokens: 400, CacheReadInputTokens: 900, OutputTokens: 50}},
			&agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 42, CacheCreationInputTokens: 8191, CacheReadInputTokens: 87823, OutputTokens: 746, CacheTTLSeconds: 3600}, NumTurns: 1},
		}
		var total usagedb.TokenBuckets
		for _, m := range claude {
			e, ok := rollupEvent(m, at, "claude-opus", 0, harness.Claude)
			if !ok {
				t.Fatalf("%T must translate", m)
			}
			total.Input += e.Delta.Input
			total.CacheWrite1h += e.Delta.CacheWrite1h
			total.CacheRead += e.Delta.CacheRead
			total.Output += e.Delta.Output
		}
		want := usagedb.TokenBuckets{Input: 42, CacheWrite1h: 8191, CacheRead: 87823, Output: 746}
		if total != want {
			t.Errorf("claude tokens = %+v, want %+v (result total counted once)", total, want)
		}

		// Pi's turn total arrives as the turn-end usage; its result carries
		// only the last call and must not add tokens.
		e, ok := rollupEvent(&agent.UsageMessage{ReportedModel: "zai/glm-5.3", Usage: agent.Usage{InputTokens: 100, OutputTokens: 30}}, at, "zai/glm-5.3", 0, harness.Pi)
		if !ok || e.Delta.Output != 30 {
			t.Fatalf("pi usage tokens = %+v, ok = %v", e.Delta.TokenBuckets, ok)
		}
		e, ok = rollupEvent(&agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 100, OutputTokens: 30}, NumTurns: 1}, at, "zai/glm-5.3", 0, harness.Pi)
		if !ok || e.Delta.TokenBuckets != (usagedb.TokenBuckets{}) {
			t.Errorf("pi result must carry no tokens, got %+v", e.Delta.TokenBuckets)
		}

		// OpenCode has no per-call records; its result is the only source.
		e, _ = rollupEvent(&agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 7, OutputTokens: 9}, NumTurns: 1}, at, "m", 0, harness.OpenCode)
		if e.Delta.TokenBuckets != (usagedb.TokenBuckets{Input: 7, Output: 9}) {
			t.Errorf("opencode result tokens = %+v", e.Delta.TokenBuckets)
		}
	})

	t.Run("usage splits cache writes by ttl", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			ttl    int
			want5m int64
			want1h int64
		}{
			{name: "unknown ttl", want5m: 400},
			{name: "five minute ttl", ttl: 300, want5m: 400},
			{name: "one hour ttl", ttl: 3600, want1h: 400},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				e, ok := rollupEvent(&agent.UsageMessage{
					Usage: agent.Usage{InputTokens: 10, CacheCreationInputTokens: 400, CacheReadInputTokens: 900, OutputTokens: 50, CacheTTLSeconds: tc.ttl},
				}, at, "m", 0, harness.Pi)
				if !ok {
					t.Fatal("usage message must translate")
				}
				want := usagedb.TokenBuckets{Input: 10, CacheWrite5m: tc.want5m, CacheWrite1h: tc.want1h, CacheRead: 900, Output: 50}
				if e.Delta.TokenBuckets != want {
					t.Errorf("tokens = %+v, want %+v", e.Delta.TokenBuckets, want)
				}
			})
		}
	})

	t.Run("counts and skips", func(t *testing.T) {
		t.Parallel()
		// Interesting messages translate; everything else is skipped.
		interesting := []agent.Message{
			&agent.ResultMessage{MessageType: "result", NumTurns: 1, IsError: true},
			&agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary"},
			&agent.SkillReadMessage{Skill: "review"},
			&agent.ToolUseMessage{Name: "Edit"},
			&agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{Background: true}},
		}
		for _, m := range interesting {
			if _, ok := rollupEvent(m, at, "m", 0, harness.Claude); !ok {
				t.Errorf("%T must translate", m)
			}
		}
		skipped := []agent.Message{
			&agent.InitMessage{ReportedModel: "m"},
			&agent.TextMessage{Text: "hi"},
			&agent.SystemMessage{MessageType: "system", Subtype: "model_rerouted"},
			&agent.SkillReadMessage{},
		}
		for _, m := range skipped {
			if _, ok := rollupEvent(m, at, "m", 0, harness.Claude); ok {
				t.Errorf("%T must be skipped", m)
			}
		}
	})
}

func TestTaskRollupResume(t *testing.T) {
	t.Parallel()

	t.Run("restart dedupes replay and keeps tail", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		id := ksid.NewID()
		history := []agent.TimedMessage{
			{Message: &agent.InitMessage{ReportedModel: "m"}, ProducerTime: atOffset(0)},
			{Message: &agent.UsageMessage{ReportedModel: "m"}, ProducerTime: atOffset(1)},
			{Message: &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{OutputTokens: 100}, NumTurns: 1}, ProducerTime: atOffset(2)},
		}

		s1 := newRollupStore(t, dir)
		tk := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "m", "")
		tk.Rollup = s1
		tk.SeedTimelineEntries(history)
		if err := s1.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		before := readUsageRows(t, dir, "2026-02-05")
		if len(before) != 1 {
			t.Fatalf("rows after first run = %d, want 1", len(before))
		}

		// A restarted server replays the same history: fully flushed, so
		// nothing may be written again.
		s2 := newRollupStore(t, dir)
		tk2 := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "m", "")
		tk2.Rollup = s2
		tk2.SeedTimelineEntries(history)
		if rows := readUsageRows(t, dir, "2026-02-05"); len(rows) != len(before) {
			t.Fatalf("rows after duplicate replay = %d, want %d", len(rows), len(before))
		}

		// The unflushed tail (produced after the watermark) must be ingested.
		tail := []agent.TimedMessage{
			{Message: &agent.UsageMessage{ReportedModel: "m"}, ProducerTime: atOffset(3)},
			{Message: &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{OutputTokens: 30}, NumTurns: 1}, ProducerTime: atOffset(4)},
		}
		tk3 := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "m", "")
		tk3.Rollup = s2
		tk3.SeedTimelineEntries(tail)
		rows := readUsageRows(t, dir, "2026-02-05")
		if len(rows) != 2 {
			t.Fatalf("rows after tail = %d, want 2", len(rows))
		}
		if rows[1].Output != 30 {
			t.Errorf("tail row = %+v", rows[1])
		}
		if days := s2.Days(); days[0].Turns != 2 || days[0].Tokens.Output != 130 {
			t.Errorf("days = %+v", days)
		}
	})

	t.Run("timestampless adopted replay", func(t *testing.T) {
		t.Parallel()
		// Timestamp-less events have no ordering identity: synthetic rows
		// never advance the watermark, so one replay pass is complete (no
		// loss after a turn boundary), but re-replaying the same
		// timestamp-less history duplicates its rows. The trade-off is
		// documented on Store.Observe.
		dir := t.TempDir()
		id := ksid.NewID()
		noTime := []agent.TimedMessage{
			{Message: &agent.UsageMessage{ReportedModel: "m"}},
			{Message: &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{OutputTokens: 7}}},
			{Message: &agent.ToolUseMessage{Name: "Edit"}},
			{Message: &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{OutputTokens: 3}}},
		}

		s := newRollupStore(t, dir)
		tk := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "m", "")
		tk.Rollup = s
		tk.SeedTimelineEntries(noTime)
		rows := readUsageRows(t, dir, time.Now().UTC().Format("2006-01-02"))
		if len(rows) != 2 {
			t.Fatalf("adopted rows = %d, want 2 (both turns, nothing lost at the boundary)", len(rows))
		}
		if rows[0].Output != 7 || rows[1].Output != 3 {
			t.Errorf("rows = %+v / %+v", rows[0], rows[1])
		}

		// Re-replaying the same timestamp-less history duplicates its rows:
		// the degenerate cost of identity-less events.
		tk2 := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "m", "")
		tk2.Rollup = s
		tk2.SeedTimelineEntries(noTime)
		if rows = readUsageRows(t, dir, time.Now().UTC().Format("2006-01-02")); len(rows) != 4 {
			t.Errorf("rows after re-replay = %d, want 4 (documented duplication)", len(rows))
		}
	})
}

func newRollupStore(t *testing.T, dir string) *usagedb.Store {
	s, err := usagedb.New(usagedb.Config{Log: testLogger(), Dir: dir})
	if err != nil {
		t.Fatalf("usagedb.New: %v", err)
	}
	return s
}

// readUsageRows parses every usage row in the store's day file for day.
func readUsageRows(t *testing.T, dir, day string) []usagedb.UsageRow {
	data, err := os.ReadFile(filepath.Join(dir, day+".jsonl")) //nolint:gosec // test fixture path built from t.TempDir().
	if err != nil {
		t.Fatalf("read day file %s: %v", day, err)
	}
	var rows []usagedb.UsageRow
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row usagedb.UsageRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse row: %v", err)
		}
		if row.Kind == "usage" {
			rows = append(rows, row)
		}
	}
	return rows
}

// atOffset returns the replay base time (2026-02-05 10:00:00 UTC) plus sec.
func atOffset(sec int) time.Time {
	return time.Date(2026, time.February, 5, 10, 0, sec, 0, time.UTC)
}
