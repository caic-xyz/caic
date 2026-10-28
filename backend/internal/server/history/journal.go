// Stores compact pricing observations with exact cache-write lifetimes without retaining transcript payloads.

package history

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/history/data"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tokens"
)

// Position identifies an output inside one physical raw record.
type Position = v1.HistoryPosition

// TurnEstimate is a mutable price for one canonical result event. Pricing must
// be served through a revalidated endpoint, never merged into immutable chunks.
type TurnEstimate struct {
	Position

	Model    string
	Cost     task.TurnCost
	TotalUSD float64
}

type costJournal struct {
	file   *os.File
	writer *bufio.Writer
	end    int64
}

func newCostJournal(dir string) (*costJournal, error) {
	f, err := openProjectionSpool(dir)
	if err != nil {
		return nil, err
	}
	return &costJournal{file: f, writer: bufio.NewWriterSize(f, 64<<10)}, nil
}

func (j *costJournal) append(fact *data.Fact) error {
	encoded, err := json.Marshal(fact)
	if err != nil {
		return err
	}
	if _, err := j.writer.Write(encoded); err != nil {
		return err
	}
	if err := j.writer.WriteByte('\n'); err != nil {
		return err
	}
	j.end += int64(len(encoded) + 1)
	return nil
}

func costFactFor(message agent.TimedMessage, position Position) *data.Fact {
	var fact *data.Fact
	switch m := message.Message.(type) {
	case *agent.ExitMessage:
		fact = &data.Fact{Kind: data.KindExit}
	case *agent.InitMessage:
		fact = &data.Fact{Kind: data.KindInit, Model: m.ReportedModel}
	case *agent.SystemMessage:
		fact = &data.Fact{Kind: data.KindSystem, Model: m.ReportedModel, Subtype: m.Subtype}
	case *agent.UsageMessage:
		fact = &data.Fact{Kind: data.KindUsage, Model: m.ReportedModel, Usage: usageToData(m.Usage), PerCall: m.PerCall, ModelDerived: m.ModelDerived, CumulativeCostUSD: m.CumulativeCostUSD, ReportedCostUSD: m.ReportedCostUSD}
	case *agent.ResultMessage:
		fact = &data.Fact{Kind: data.KindResult, Usage: usageToData(m.Usage), TotalCostUSD: m.TotalCostUSD}
	default:
		return nil
	}
	fact.RecordID = position.RecordID
	fact.Index = position.Index
	fact.At = message.ProducerTime
	fact.Source = data.Source(*message.Message.Provenance())
	return fact
}

func factMessage(f *data.Fact) (agent.Message, error) {
	p := agent.MessageProvenance{Source: agent.MessageSource(f.Source)}
	switch f.Kind {
	case data.KindExit:
		return &agent.ExitMessage{MessageProvenance: p}, nil
	case data.KindInit:
		return &agent.InitMessage{MessageProvenance: p, ReportedModel: f.Model}, nil
	case data.KindSystem:
		return &agent.SystemMessage{Source: agent.MessageSource(f.Source), Subtype: f.Subtype, ReportedModel: f.Model}, nil
	case data.KindUsage:
		return &agent.UsageMessage{MessageProvenance: p, Usage: usageFromData(f.Usage), ReportedModel: f.Model, PerCall: f.PerCall, ModelDerived: f.ModelDerived, CumulativeCostUSD: f.CumulativeCostUSD, ReportedCostUSD: f.ReportedCostUSD}, nil
	case data.KindResult:
		return &agent.ResultMessage{MessageProvenance: p, Usage: usageFromData(f.Usage), TotalCostUSD: f.TotalCostUSD}, nil
	default:
		return nil, fmt.Errorf("unknown history cost fact kind %q", f.Kind)
	}
}

// facts reads the captured committed journal prefix without decoding transcript data.
func (j *costJournal) facts(ctx context.Context, end int64, last uint64) iter.Seq2[data.Fact, error] {
	return func(yield func(data.Fact, error) bool) {
		decoder := json.NewDecoder(bufio.NewReaderSize(io.NewSectionReader(j.file, 0, end), 64<<10))
		for {
			if err := ctx.Err(); err != nil {
				yield(data.Fact{}, err)
				return
			}
			var fact data.Fact
			if err := decoder.Decode(&fact); err != nil {
				if !errors.Is(err, io.EOF) {
					yield(data.Fact{}, err)
				}
				return
			}
			if fact.RecordID > last {
				return
			}
			if !yield(fact, nil) {
				return
			}
		}
	}
}

func usageToData(u agent.Usage) data.Usage {
	out := data.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheReadInputTokens: u.CacheReadInputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens, CacheTTLSeconds: u.CacheTTLSeconds}
	if len(u.CacheWrites) > 0 {
		out.CacheWrites = make([]data.CacheWrite, len(u.CacheWrites))
		for i, b := range u.CacheWrites {
			out.CacheWrites[i] = data.CacheWrite{TTL: b.TTL, Tokens: b.Tokens}
		}
	}
	return out
}

func usageFromData(u data.Usage) agent.Usage {
	out := agent.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheReadInputTokens: u.CacheReadInputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens, CacheTTLSeconds: u.CacheTTLSeconds}
	if len(u.CacheWrites) > 0 {
		out.CacheWrites = make(tokens.CacheWrites, len(u.CacheWrites))
		for i, b := range u.CacheWrites {
			out.CacheWrites[i] = tokens.CacheWrite{TTL: b.TTL, Tokens: b.Tokens}
		}
	}
	return out
}
