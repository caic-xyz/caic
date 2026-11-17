// Stores compact pricing and per-call usage observations with exact cache-write lifetimes without retaining transcript payloads.

package history

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"strconv"

	taskslogv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/history/data"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tokens"
)

// TurnEstimate is a mutable price for one canonical result event. Pricing must
// be served through a revalidated endpoint, never merged into immutable chunks.
type TurnEstimate struct {
	v1.HistoryPosition

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

func costFactFor(message agent.TimedMessage, position v1.HistoryPosition) *data.Fact {
	var fact *data.Fact
	switch m := message.Message.(type) {
	case *agent.ExitMessage:
		fact = &data.Fact{Kind: data.KindExit}
	case *agent.InitMessage:
		fact = &data.Fact{Kind: data.KindInit, Model: m.ReportedModel}
	case *agent.SessionMetadataMessage:
		if m.ReportedModel == "" {
			return nil
		}
		fact = &data.Fact{Kind: data.KindMetadata, Model: m.ReportedModel}
	case *agent.SystemMessage:
		fact = &data.Fact{Kind: data.KindSystem, Model: m.ReportedModel, Subtype: m.Subtype}
	case *agent.UsageMessage:
		fact = &data.Fact{Kind: data.KindUsage, Model: m.ReportedModel, Usage: usageToData(m.Usage), PerCall: m.PerCall, ModelDerived: m.ModelDerived, CumulativeCostUSD: m.CumulativeCostUSD, ReportedCostUSD: m.ReportedCostUSD, ContextWindow: m.ContextWindow}
	case *agent.ResultMessage:
		fact = &data.Fact{Kind: data.KindResult, Usage: usageToData(m.Usage), TotalCostUSD: m.TotalCostUSD, CostReported: m.CostReported, ContextWindow: m.ContextWindow}
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
	p := taskslogv3.MessageProvenance{Source: taskslogv3.MessageSource(f.Source)}
	switch f.Kind {
	case data.KindExit:
		return &agent.ExitMessage{MessageProvenance: p}, nil
	case data.KindInit:
		return &agent.InitMessage{MessageProvenance: p, ReportedModel: f.Model}, nil
	case data.KindMetadata:
		return &agent.SessionMetadataMessage{MessageProvenance: p, ReportedModel: f.Model}, nil
	case data.KindSystem:
		return &agent.SystemMessage{Source: taskslogv3.MessageSource(f.Source), Subtype: f.Subtype, ReportedModel: f.Model}, nil
	case data.KindUsage:
		return &agent.UsageMessage{MessageProvenance: p, Usage: usageFromData(f.Usage), ReportedModel: f.Model, PerCall: f.PerCall, ModelDerived: f.ModelDerived, CumulativeCostUSD: f.CumulativeCostUSD, ReportedCostUSD: f.ReportedCostUSD}, nil
	case data.KindResult:
		return &agent.ResultMessage{MessageProvenance: p, Usage: usageFromData(f.Usage), TotalCostUSD: f.TotalCostUSD, CostReported: f.CostReported}, nil
	default:
		return nil, fmt.Errorf("unknown history cost fact kind %q", f.Kind)
	}
}

// facts reads the captured committed journal prefix from offset start, a line
// start, without decoding transcript data.
func (j *costJournal) facts(ctx context.Context, start, end int64, last uint64) iter.Seq2[data.Fact, error] {
	return func(yield func(data.Fact, error) bool) {
		decoder := json.NewDecoder(bufio.NewReaderSize(io.NewSectionReader(j.file, start, end-start), 64<<10))
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

// seekScanBytes bounds the committed prefix that seek leaves to a linear scan.
const seekScanBytes = 4 << 10

// recordPrefix starts every encoded fact: data.Fact keeps RecordID first.
var recordPrefix = []byte(`{"recordID":`)

// seek returns a line start in the committed prefix end at or before the first
// fact of record first or later. Facts are appended in record order, so a
// binary search over line starts leaves at most seekScanBytes of earlier facts
// for the caller to skip, independently of the task's length.
func (j *costJournal) seek(end int64, first uint64) (int64, error) {
	lo, hi := int64(0), end
	var buf [512]byte
	for hi-lo > seekScanBytes {
		// The line containing mid ends at its first newline; one read usually
		// holds the next line's record ID too.
		at := lo + (hi-lo)/2
		var line []byte
		for line == nil {
			n, err := j.file.ReadAt(buf[:min(int64(len(buf)), hi-at)], at)
			if n == 0 && err != nil {
				return 0, err
			}
			if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
				at += int64(i) + 1
				line = buf[i+1 : n]
			} else {
				at += int64(n)
			}
			if at >= hi {
				// No line starts in the upper half; the scan covers it.
				return lo, nil
			}
		}
		if bytes.IndexByte(line, ',') < 0 {
			n, err := j.file.ReadAt(buf[:min(int64(len(buf)), hi-at)], at)
			if n == 0 && err != nil {
				return 0, err
			}
			line = buf[:n]
		}
		id, err := lineRecordID(line)
		if err != nil {
			return 0, err
		}
		if id < first {
			lo = at
		} else {
			hi = at
		}
	}
	return lo, nil
}

// lineRecordID parses the record ID that starts an encoded fact.
func lineRecordID(line []byte) (uint64, error) {
	digits, ok := bytes.CutPrefix(line, recordPrefix)
	if !ok {
		return 0, errors.New("invalid history cost journal line")
	}
	if i := bytes.IndexByte(digits, ','); i >= 0 {
		digits = digits[:i]
	}
	id, err := strconv.ParseUint(string(digits), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid history cost journal record: %w", err)
	}
	return id, nil
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
