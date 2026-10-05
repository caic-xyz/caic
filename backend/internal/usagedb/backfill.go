// Historical backfill stages usage days and repairs missing cost in existing days.

package usagedb

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
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
	"github.com/klauspost/compress/zstd"
)

const backfillSentinel = ".backfill.done"

// backfillStaging owns one temporary file and contribution state per day until
// every source row has been read successfully.
type backfillStaging struct {
	dir  string
	days map[string]*backfillDay
}

func newBackfillStaging(dir string) *backfillStaging {
	return &backfillStaging{dir: dir, days: make(map[string]*backfillDay)}
}

func (s *backfillStaging) append(row *data.UsageRow) error {
	day := s.days[row.Day]
	if day == nil {
		file, err := os.CreateTemp(s.dir, ".usage-backfill-*.tmp")
		if err != nil {
			return fmt.Errorf("create usage backfill staging file: %w", err)
		}
		day = &backfillDay{
			file:          file,
			path:          file.Name(),
			aggregate:     newDayAggregate(),
			watermarks:    make(map[string]time.Time),
			costs:         make(map[string]float64),
			reportedCosts: make(map[string]struct{}),
		}
		s.days[row.Day] = day
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encode usage backfill row: %w", err)
	}
	if _, err := day.file.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write usage backfill staging file: %w", err)
	}
	foldUsageRow(day.aggregate, row)
	if row.TaskID != "" {
		if row.Ts > 0 {
			at := row.Ts.AsTime()
			if at.After(day.watermarks[row.TaskID]) {
				day.watermarks[row.TaskID] = at
			}
		}
		day.costs[row.TaskID] += row.CostUSD
		if row.CostUSD != 0 && !row.CostEstimated {
			day.reportedCosts[row.TaskID] = struct{}{}
		}
	}
	return nil
}

func (s *backfillStaging) daysSorted() []string {
	days := make([]string, 0, len(s.days))
	for day := range s.days {
		days = append(days, day)
	}
	slices.Sort(days)
	return days
}

func (s *backfillStaging) close() error {
	var errs []error
	for _, day := range s.days {
		if day.file == nil {
			continue
		}
		if err := day.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close usage backfill staging file: %w", err))
		}
		day.file = nil
	}
	return errors.Join(errs...)
}

func (s *backfillStaging) cleanup() {
	for _, day := range s.days {
		if day.file != nil {
			_ = day.file.Close()
		}
		if day.path != "" {
			_ = os.Remove(day.path)
		}
	}
}

// backfillDay holds one staged day file and the in-memory contribution to
// install only after that file is atomically published.
type backfillDay struct {
	file          *os.File
	path          string
	aggregate     *dayAggregate
	watermarks    map[string]time.Time
	costs         map[string]float64
	reportedCosts map[string]struct{}
}

func writeBackfillSentinelTemp(dir string) (string, error) {
	f, err := os.CreateTemp(dir, ".usage-backfill-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create usage backfill sentinel staging file: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close usage backfill sentinel staging file: %w", err)
	}
	return path, nil
}

func backfillDayCosts(ctx context.Context, s *Store, name string, estimate func(*data.UsageRow) (float64, bool)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	path := filepath.Join(s.dir, name)
	if strings.HasSuffix(name, compressedDaySuffix) {
		if _, err := os.Stat(strings.TrimSuffix(path, ".zstd")); err == nil {
			return nil // the plain copy wins an interrupted compression
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat plain usage day for %s: %w", name, err)
		}
	}
	staged, err := os.CreateTemp(s.dir, ".usage-cost-*.tmp")
	if err != nil {
		return fmt.Errorf("create usage cost staging file for %s: %w", name, err)
	}
	stagedPath := staged.Name()
	defer func() { _ = os.Remove(stagedPath) }()
	var out io.Writer = staged
	var enc *zstd.Encoder
	if strings.HasSuffix(path, compressedDaySuffix) {
		enc, err = zstd.NewWriter(staged, zstd.WithEncoderConcurrency(1))
		if err != nil {
			return errors.Join(err, staged.Close())
		}
		out = enc
	}
	changes := newRecoveryState()
	changed := false
	transform := func(line []byte) ([]byte, error) {
		var row data.UsageRow
		decoded := json.Unmarshal(line, &row) == nil
		if !decoded {
			return line, nil
		}
		if row.Kind != rowKindUsage || row.CostUSD != 0 || row.CostEstimated {
			return line, nil
		}
		if _, reported := s.reportedCostTasks[row.TaskID]; reported {
			return line, nil
		}
		if pending := s.pending[row.TaskID]; pending != nil && pending.costInFlight != 0 {
			return line, nil // a failed append still owns cost movement for this task
		}
		cost, ok := estimate(&row)
		if !ok || cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
			return line, nil
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil {
			return nil, fmt.Errorf("decode usage fields for %s: %w", name, err)
		}
		costBytes, err := json.Marshal(cost)
		if err != nil {
			return nil, fmt.Errorf("encode estimated usage cost for %s: %w", name, err)
		}
		fields["cost_usd"] = costBytes
		fields["cost_estimated"] = json.RawMessage("true")
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("encode estimated usage cost for %s: %w", name, err)
		}
		changed = true
		changes.addUsageRow(&data.UsageRow{Day: row.Day, Model: row.Model, Harness: row.Harness, CostUSD: cost})
		changes.flushedCost[row.TaskID] += cost
		if bytes.HasSuffix(line, []byte{'\n'}) {
			encoded = append(encoded, '\n')
		}
		return encoded, nil
	}
	for line, readErr := range dayRecords(ctx, path) {
		if readErr != nil {
			err = readErr
			break
		}
		var encoded []byte
		encoded, err = transform(line)
		if err != nil {
			break
		}
		if _, err = out.Write(encoded); err != nil {
			break
		}
	}
	if enc != nil {
		err = errors.Join(err, enc.Close())
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && changed {
		err = staged.Sync()
	}
	err = errors.Join(err, staged.Close())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stage missing usage costs for %s: %w", name, err)
	}
	if !changed {
		return nil
	}
	day := strings.TrimSuffix(strings.TrimSuffix(name, ".zstd"), ".jsonl")
	if f := s.files[day]; f != nil {
		delete(s.files, day)
		if err := f.Close(); err != nil {
			return fmt.Errorf("close usage rollup day %s before cost backfill: %w", day, err)
		}
	}
	if err := os.Rename(stagedPath, path); err != nil {
		return fmt.Errorf("publish estimated usage costs for %s: %w", day, err)
	}
	s.mergeRecoveryState(changes)
	return nil
}
