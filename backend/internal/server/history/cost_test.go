// Tests pricing-input fidelity through the projected history's compact cost journal.

package history

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/server/history/data"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tokens"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

type journalPricer struct{}

func (journalPricer) ModelPrice(agent.QuotaProvider, string, time.Time) (usage.ModelPrice, bool) {
	return usage.ModelPrice{InputPerMTok: 2, OutputPerMTok: 10, LongContext: usage.LongContextPrice{InputTokensAbove: 272_000, InputMultiplier: 2, OutputMultiplier: 1.5}}, true
}

func testSourcePricingInputs(t *testing.T) {
	t.Parallel()
	t.Run("derived model retains per-request context pricing", func(t *testing.T) {
		t.Parallel()
		source, _ := costFixture(t, harness.Codex,
			[]agent.Message{&agent.UsageMessage{PerCall: true, ModelDerived: true, Usage: agent.Usage{InputTokens: 300_000, OutputTokens: 1_000}}},
			[]agent.Message{&agent.ResultMessage{Usage: agent.Usage{InputTokens: 300_000, OutputTokens: 1_000}}},
		)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var costs []TurnEstimate
		for cost, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, journalPricer{}) {
			if err != nil {
				t.Fatal(err)
			}
			costs = append(costs, cost)
		}
		if len(costs) != 1 || math.Abs(costs[0].Cost.USD-1.215) > 1e-9 {
			t.Fatalf("request cost = %+v, want 1.215", costs)
		}
	})
	t.Run("root process exit resets request coverage", func(t *testing.T) {
		t.Parallel()
		source, _ := costFixture(t, harness.Codex,
			[]agent.Message{&agent.UsageMessage{ReportedModel: "priced", Usage: agent.Usage{InputTokens: 100_000}}},
			[]agent.Message{&agent.ExitMessage{}},
			[]agent.Message{&agent.ResultMessage{Usage: agent.Usage{InputTokens: 100_000}}},
		)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var costs []TurnEstimate
		for cost, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, journalPricer{}) {
			if err != nil {
				t.Fatal(err)
			}
			costs = append(costs, cost)
		}
		if len(costs) != 1 || math.Abs(costs[0].TotalUSD-0.4) > 1e-9 {
			t.Fatalf("cost after process exit = %+v, want cumulative 0.4", costs)
		}
	})
	t.Run("native request parser preserves per-call pricing", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t,
			`{"t":"caic_meta","version":3,"harness":"codex","model":"priced","prompt":"pricing"}`,
			`{"t":"agent","ts":1.000,"msg":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"root","model":"priced"}}}}`,
			`{"t":"agent","ts":2.000,"msg":{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"root","turnId":"turn","tokenUsage":{"total":{"inputTokens":300000,"outputTokens":1000},"last":{"inputTokens":300000,"outputTokens":1000}}}}}`,
			`{"t":"agent","ts":3.000,"msg":{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"root","turn":{"id":"turn","status":"completed"}}}}`,
		)
		source, err := NewSource(t.Context(), path, backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for cost, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, journalPricer{}) {
			if err != nil {
				t.Fatal(err)
			}
			count++
			if cost.RecordID != 4 || math.Abs(cost.Cost.USD-1.215) > 1e-9 {
				t.Fatalf("native request cost = %+v", cost)
			}
		}
		if count != 1 {
			t.Fatalf("result costs=%d, want1", count)
		}
	})
	t.Run("child requests retain root attribution and lifecycle", func(t *testing.T) {
		t.Parallel()
		child := agent.MessageSource{ThreadID: "child", RootThreadID: "root"}
		source, _ := costFixture(t, harness.Codex,
			[]agent.Message{&agent.InitMessage{ReportedModel: "root-model"}},
			[]agent.Message{&agent.InitMessage{Source: child, ReportedModel: "child-model"}},
			[]agent.Message{&agent.UsageMessage{Source: child, ReportedModel: "child-model", ModelDerived: true, PerCall: true, Usage: agent.Usage{InputTokens: 300_000}}},
			[]agent.Message{&agent.UsageMessage{ModelDerived: true, PerCall: true, Usage: agent.Usage{InputTokens: 100_000}}},
			[]agent.Message{&agent.ExitMessage{Source: child}},
			[]agent.Message{&agent.ResultMessage{Source: child, Usage: agent.Usage{InputTokens: 999}}},
			[]agent.Message{&agent.ResultMessage{Usage: agent.Usage{InputTokens: 400_000}}},
		)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var costs []TurnEstimate
		for cost, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, journalPricer{}) {
			if err != nil {
				t.Fatal(err)
			}
			costs = append(costs, cost)
		}
		if len(costs) != 1 || costs[0].Model != "root-model" || math.Abs(costs[0].Cost.USD-1.4) > 1e-9 {
			t.Fatalf("child request fold=%+v", costs)
		}
		if costs[0].RecordID != 8 {
			t.Fatalf("child result became a turn: %+v", costs)
		}
	})
	t.Run("corrupt or unsupported pricing journal surfaces an error", func(t *testing.T) {
		t.Parallel()
		source, _ := costFixture(t, harness.Codex, []agent.Message{&agent.UsageMessage{Usage: agent.Usage{InputTokens: 1}}})
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := source.journal.writer.Flush(); err != nil {
			t.Fatal(err)
		}
		// The private spool is corrupted after normal construction, exercising
		// a read failure rather than permitting invalid production observations.
		replacement := data.Fact{RecordID: 2, Kind: "other"}
		raw, err := json.Marshal(&replacement)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.journal.file.WriteAt(append(raw, '\n'), 0); err != nil {
			t.Fatal(err)
		}
		source.journal.end = int64(len(raw) + 1)
		failed := false
		for _, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, nil) {
			if err == nil || !strings.Contains(err.Error(), "unknown history cost fact kind") {
				t.Fatalf("invalid fact accepted: %v", err)
			}
			failed = true
		}
		if !failed {
			t.Fatal("unsupported pricing fact produced no error")
		}
	})
}

func costFixture(t testing.TB, h harness.Name, groups ...[]agent.Message) (*Source, *atomic.Int64) {
	lines := make([]string, 1, 1+len(groups))
	lines[0] = fmt.Sprintf(`{"t":"caic_meta","version":3,"harness":%q,"model":"initial-model","prompt":"context"}`, h)
	for i := range groups {
		lines = append(lines, fmt.Sprintf(`{"t":"agent","ts":%d.000,"msg":%d}`, i+2, i))
	}
	path := writeFixture(t, lines...)
	calls := &atomic.Int64{}
	resolver := agent.Backends{h: &agenttest.FakeBackend{HarnessName: h, WireFactory: func() agent.WireFormat {
		return &agenttest.Wire{Parse: func(data []byte) ([]agent.Message, error) {
			calls.Add(1)
			var index int
			if err := json.Unmarshal(data, &index); err != nil {
				return nil, err
			}
			return groups[index], nil
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
	return source, calls
}

type contextPricer struct{ rate float64 }

func (p contextPricer) ModelPrice(agent.QuotaProvider, string, time.Time) (usage.ModelPrice, bool) {
	return usage.ModelPrice{InputPerMTok: p.rate}, true
}

func testSourceTurnCosts(t *testing.T) {
	t.Parallel()
	t.Run("input fidelity", testSourcePricingInputs)
	t.Run("cache duration pricing", testSourceCachePricing)
	for _, h := range []harness.Name{harness.Claude, harness.Codex, harness.OpenCode, harness.Pi} {
		t.Run(string(h), func(t *testing.T) {
			t.Parallel()
			cumulative, reported := 2.5, 1.25
			groups := [][]agent.Message{{&agent.InitMessage{ReportedModel: "first"}}, {&agent.UsageMessage{ReportedModel: "first", Usage: agent.Usage{InputTokens: 1000000}, ReportedCostUSD: &reported, CumulativeCostUSD: &cumulative}}, {&agent.ResultMessage{TotalCostUSD: 2.5, Usage: agent.Usage{InputTokens: 1000000}}}, {&agent.SystemMessage{Subtype: agent.SystemSubtypeModelRerouted, ReportedModel: "second"}}, {&agent.UsageMessage{ReportedModel: "second", ModelDerived: true, Usage: agent.Usage{InputTokens: 2000000}}}, {&agent.ResultMessage{TotalCostUSD: 4, Usage: agent.Usage{InputTokens: 2000000}}}, {&agent.SystemMessage{Subtype: "context_cleared"}}, {&agent.ResultMessage{TotalCostUSD: 0.5, Usage: agent.Usage{InputTokens: 500000}}}}
			source, calls := costFixture(t, h, groups...)
			snapshot, err := source.Refresh(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			parsed := calls.Load()
			body, err := readRange(source, t.Context(), snapshot, 1, snapshot.Through)
			if err != nil {
				t.Fatal(err)
			}
			for _, price := range []usage.ModelPricer{nil, contextPricer{rate: 3}, contextPricer{rate: 7}} {
				tracker := task.CostTracker{Harness: h, Model: "initial-model"}
				var want []TurnEstimate
				for i, messages := range groups {
					for j, message := range messages {
						cost, known := tracker.Observe(message, time.Unix(int64(i+2), 0), price)
						if _, ok := message.(*agent.ResultMessage); ok {
							if !known {
								cost = task.TurnCost{}
							}
							want = append(want, TurnEstimate{RecordID: uint64(i + 2), Index: uint32(j), Model: tracker.Model, Cost: cost, TotalUSD: tracker.TotalUSD})
						}
					}
				}
				var got []TurnEstimate
				for cost, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, price) {
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, cost)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("cost facts diverged from authoritative full fold:\ngot=%+v\nwant=%+v", got, want)
				}
			}
			if calls.Load() != parsed {
				t.Fatal("repricing replayed native task history")
			}
			again, err := readRange(source, t.Context(), snapshot, 1, snapshot.Through)
			if err != nil || again != body {
				t.Fatal("mutable pricing changed immutable chunk", err)
			}
		})
	}
}

func TestTurnEstimateModel(t *testing.T) {
	t.Parallel()
	source, _ := costFixture(t, harness.Codex, []agent.Message{&agent.InitMessage{ReportedModel: "first"}}, []agent.Message{&agent.SystemMessage{Subtype: agent.SystemSubtypeModelRerouted, ReportedModel: "second"}}, []agent.Message{&agent.ResultMessage{}})
	snapshot, err := source.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for estimate, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if estimate.Cost.Source != "" {
			t.Fatalf("fixture pricing unexpectedly known: %+v", estimate)
		}
		if estimate.Model != "second" {
			t.Fatalf("authoritative reroute missing with unknown pricing: %+v", estimate)
		}
	}
}

type cacheJournalPricer struct{ scale float64 }

func (p cacheJournalPricer) ModelPrice(agent.QuotaProvider, string, time.Time) (usage.ModelPrice, bool) {
	return usage.ModelPrice{CacheWritePerMTok: 3 * p.scale, CacheWritePrices: []usage.CacheWritePrice{{TTLSeconds: 300, PerMTok: 5 * p.scale}, {TTLSeconds: 3600, PerMTok: 9 * p.scale}}}, true
}

func testSourceCachePricing(t *testing.T) {
	t.Parallel()
	t.Run("native Claude mixed and unknown cache durations", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t,
			`{"t":"caic_meta","version":3,"harness":"claude","model":"priced","prompt":"pricing"}`,
			`{"t":"agent","ts":2.125,"msg":{"type":"assistant","message":{"model":"priced","id":"request","role":"assistant","content":[],"usage":{"cache_creation_input_tokens":350000,"cache_creation":{"ephemeral_5m_input_tokens":100000,"ephemeral_1h_input_tokens":200000}}}}}`,
			`{"t":"agent","ts":3.000,"msg":{"type":"result","subtype":"success","total_cost_usd":99,"usage":{"cache_creation_input_tokens":350000}}}`,
		)
		source, err := NewSource(t.Context(), path, backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, scale := range []float64{1, 2} {
			count := 0
			for cost, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, cacheJournalPricer{scale: scale}) {
				if err != nil {
					t.Fatal(err)
				}
				count++
				if cost.RecordID != 3 || cost.Cost.Source != task.CostEstimated || math.Abs(cost.Cost.USD-2.45*scale) > 1e-9 {
					t.Fatalf("native mixed-duration cost=%+v, want %v", cost, 2.45*scale)
				}
			}
			if count != 1 {
				t.Fatalf("priced results=%d, want1", count)
			}
		}
		count := 0
		for fact, err := range source.journal.facts(t.Context(), source.journal.end, snapshot.Through) {
			if err != nil {
				t.Fatal(err)
			}
			if fact.Kind != data.KindUsage {
				continue
			}
			count++
			message, err := factMessage(&fact)
			if err != nil {
				t.Fatal(err)
			}
			want := tokens.CacheWrites{{Tokens: 50000}, {TTL: 5 * time.Minute, Tokens: 100000}, {TTL: time.Hour, Tokens: 200000}}
			m, ok := message.(*agent.UsageMessage)
			if !ok {
				t.Fatalf("usage fact decoded as %T", message)
			}
			if got := m.Usage.CacheWriteBuckets(); !reflect.DeepEqual(got, want) {
				t.Fatalf("journal cache durations=%+v, want %+v", got, want)
			}
			if !fact.At.Equal(time.Unix(2, 125000000)) {
				t.Fatalf("producer time=%v", fact.At)
			}
		}
		if count != 1 {
			t.Fatalf("journal usage facts=%d, want1", count)
		}
	})
	t.Run("result residual subtracts covered duration buckets", func(t *testing.T) {
		t.Parallel()
		source, calls := costFixture(t, harness.OpenCode,
			[]agent.Message{&agent.UsageMessage{PerCall: true, ReportedModel: "priced", Usage: agent.Usage{CacheCreationInputTokens: 100000, CacheTTLSeconds: 3600}}},
			[]agent.Message{&agent.ResultMessage{Usage: agent.Usage{CacheCreationInputTokens: 350000, CacheTTLSeconds: 300, CacheWrites: tokens.CacheWrites{{Tokens: 50000}, {TTL: 5 * time.Minute, Tokens: 100000}, {TTL: time.Hour, Tokens: 200000}}}}},
		)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		parsed := calls.Load()
		for _, scale := range []float64{1, 2} {
			count := 0
			for cost, err := range source.TurnCosts(t.Context(), snapshot, snapshot.Through, cacheJournalPricer{scale: scale}) {
				if err != nil {
					t.Fatal(err)
				}
				count++
				if cost.Cost.Source != task.CostEstimated || math.Abs(cost.TotalUSD-2.45*scale) > 1e-9 {
					t.Fatalf("residual duration cost=%+v, want %v", cost, 2.45*scale)
				}
			}
			if count != 1 {
				t.Fatalf("priced results=%d, want1", count)
			}
		}
		if calls.Load() != parsed {
			t.Fatal("repricing replayed native task history")
		}
	})
}
