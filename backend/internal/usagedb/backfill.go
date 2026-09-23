// Historical backfill stages usage days and repairs missing cost in existing days.

package usagedb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"
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

func (s *backfillStaging) append(row *UsageRow) error {
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
	data, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encode usage backfill row: %w", err)
	}
	if _, err := day.file.Write(append(data, '\n')); err != nil {
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

func backfillDayCosts(s *Store, name string, estimate func(*UsageRow) (float64, bool)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	path := filepath.Join(s.dir, name)
	data, err := os.ReadFile(path) //nolint:gosec // name is a validated day-file directory entry.
	if err != nil {
		return fmt.Errorf("read usage rollup day %s for missing cost: %w", name, err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	var changed []UsageRow
	for i, line := range lines {
		var row UsageRow
		if len(line) == 0 || json.Unmarshal(line, &row) != nil || row.Kind != rowKindUsage || row.CostUSD != 0 || row.CostEstimated {
			continue
		}
		if _, reported := s.reportedCostTasks[row.TaskID]; reported {
			continue
		}
		if pending := s.pending[row.TaskID]; pending != nil && pending.costInFlight != 0 {
			continue // a failed append still owns cost movement for this task
		}
		cost, ok := estimate(&row)
		if !ok || cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
			continue
		}
		row.CostUSD = cost
		row.CostEstimated = true
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil {
			return fmt.Errorf("decode usage fields for %s: %w", name, err)
		}
		fields["cost_usd"], err = json.Marshal(cost)
		if err != nil {
			return fmt.Errorf("encode estimated usage cost for %s: %w", name, err)
		}
		fields["cost_estimated"] = json.RawMessage("true")
		encoded, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encode estimated usage cost for %s: %w", name, err)
		}
		lines[i] = encoded
		changed = append(changed, row)
	}
	if len(changed) == 0 {
		return nil
	}
	staged, err := os.CreateTemp(s.dir, ".usage-cost-*.tmp")
	if err != nil {
		return fmt.Errorf("create usage cost staging file for %s: %w", name, err)
	}
	stagedPath := staged.Name()
	defer func() { _ = os.Remove(stagedPath) }()
	if _, err := staged.Write(bytes.Join(lines, []byte{'\n'})); err != nil {
		_ = staged.Close()
		return fmt.Errorf("write usage cost staging file for %s: %w", name, err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("close usage cost staging file for %s: %w", name, err)
	}
	day := name[:len(name)-len(".jsonl")]
	if f := s.files[day]; f != nil {
		delete(s.files, day)
		if err := f.Close(); err != nil {
			return fmt.Errorf("close usage rollup day %s before cost backfill: %w", day, err)
		}
	}
	if err := os.Rename(stagedPath, path); err != nil {
		return fmt.Errorf("publish estimated usage costs for %s: %w", day, err)
	}
	for i := range changed {
		row := &changed[i]
		d := s.dayAggregate(row.Day)
		d.CostUSD += row.CostUSD
		if row.TaskID != "" {
			s.flushedCost[row.TaskID] += row.CostUSD
		}
		if row.Model != "" {
			d.modelBucket(row.Model).CostUSD += row.CostUSD
		}
		if row.Harness != "" {
			d.harnessBucket(row.Harness).CostUSD += row.CostUSD
		}
	}
	return nil
}
