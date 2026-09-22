// Backfill staging streams neutral usage rows into complete, publishable daily files.

package usagedb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
			file:       file,
			path:       file.Name(),
			aggregate:  newDayAggregate(),
			watermarks: make(map[string]time.Time),
			costs:      make(map[string]float64),
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
	file       *os.File
	path       string
	aggregate  *dayAggregate
	watermarks map[string]time.Time
	costs      map[string]float64
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
