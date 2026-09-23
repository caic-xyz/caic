// Usage rollup projections stream neutral aggregate rows from retained task logs.

package taskslog

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

const usageDayFormat = "2006-01-02"

type usageHeaderResult struct {
	task *LoadedTask
	err  error
}

type usageStartResult struct {
	startedAt time.Time
	err       error
}

type usageLogPath struct {
	path      string
	startedAt time.Time
}

func storeUsageRows(s *Store, ctx context.Context, resolver WireResolver) iter.Seq2[usagedb.UsageRow, error] {
	return func(yield func(usagedb.UsageRow, error) bool) {
		if resolver == nil {
			yield(usagedb.UsageRow{}, errors.New("native wire resolver is required"))
			return
		}
		if err := ctx.Err(); err != nil {
			yield(usagedb.UsageRow{}, err)
			return
		}
		paths, err := usageLogPaths(ctx, s)
		if err != nil {
			yield(usagedb.UsageRow{}, fmt.Errorf("load retained task logs: %w", err))
			return
		}
		for start := 0; start < len(paths); start += maxParallelLogHeaderLoads {
			if err := ctx.Err(); err != nil {
				yield(usagedb.UsageRow{}, err)
				return
			}
			end := min(start+maxParallelLogHeaderLoads, len(paths))
			results := make([]usageHeaderResult, end-start)
			var wg sync.WaitGroup
			for i, path := range paths[start:end] {
				wg.Go(func() {
					if err := ctx.Err(); err != nil {
						results[i].err = err
						return
					}
					results[i].task, results[i].err = loadLogHeader(s.log, path, false)
				})
			}
			wg.Wait()
			for i, result := range results {
				if err := ctx.Err(); err != nil {
					yield(usagedb.UsageRow{}, err)
					return
				}
				if result.err != nil {
					s.log.WarnContext(ctx, "skip unreadable task log during usage backfill", "path", paths[start+i], "err", result.err)
					continue
				}
				result.task.SetWireResolver(resolver)
				for row, err := range result.task.UsageRows(ctx) {
					if err != nil {
						s.log.WarnContext(ctx, "skip unreadable task log during usage backfill", "path", result.task.LogPath(), "err", err)
						break
					}
					if !yield(row, nil) {
						return
					}
				}
			}
		}
		if err := ctx.Err(); err != nil {
			yield(usagedb.UsageRow{}, err)
		}
	}
}

func usageLogPaths(ctx context.Context, s *Store) ([]string, error) {
	paths, err := readOnlyLogPaths(s.LogDir)
	if err != nil {
		return nil, err
	}
	ordered := make([]usageLogPath, 0, len(paths))
	for start := 0; start < len(paths); start += maxParallelLogHeaderLoads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+maxParallelLogHeaderLoads, len(paths))
		results := make([]usageStartResult, end-start)
		var wg sync.WaitGroup
		for i, path := range paths[start:end] {
			wg.Go(func() { results[i].startedAt, results[i].err = loadUsageStartedAt(path) })
		}
		wg.Wait()
		for i, result := range results {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if result.err != nil {
				s.log.WarnContext(ctx, "skip unreadable task log during usage backfill", "path", paths[start+i], "err", result.err)
				continue
			}
			ordered = append(ordered, usageLogPath{path: paths[start+i], startedAt: result.startedAt})
		}
	}
	slices.SortFunc(ordered, func(a, b usageLogPath) int {
		if cmp := a.startedAt.Compare(b.startedAt); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.path, b.path)
	})
	paths = paths[:0]
	for _, path := range ordered {
		paths = append(paths, path.path)
	}
	return paths, nil
}

// loadUsageStartedAt reads the header timestamp needed to order a retained
// log stream. Its small initial scanner buffer avoids allocating a full scan
// buffer for every path during the metadata-only ordering pass.
func loadUsageStartedAt(path string) (startedAt time.Time, retErr error) {
	r, err := openLogReader(path)
	if err != nil {
		return time.Time{}, err
	}
	defer func() {
		if err := r.Close(); retErr == nil {
			retErr = err
		}
	}()
	scanner := newPhysicalLogScannerWithBuffer(r, path, 4<<10)
	meta, err := scanner.ReadHeader()
	if err != nil {
		return time.Time{}, err
	}
	return meta.StartedAt, nil
}

func taskUsageRows(lt *LoadedTask, ctx context.Context) iter.Seq2[usagedb.UsageRow, error] {
	return func(yield func(usagedb.UsageRow, error) bool) {
		if lt.LogVersion != agent.LogVersionV2 && lt.LogVersion != agent.LogVersionV3 || lt.TaskID == "" {
			return
		}
		type key struct {
			day   string
			model string
		}
		buckets := make(map[key]*usagedb.UsageRow)
		model := lt.ReportedModel
		if model == "" {
			model = lt.RequestedModel
		}
		repos := make([]string, len(lt.Repos))
		for i, repo := range lt.Repos {
			repos[i] = repo.Name
		}
		var skillReads agent.SkillReadTracker
		add := func(confirmed agent.Message, at time.Time) {
			// Claude records carry per-call model attribution. Codex, Pi, and
			// OpenCode logs only preserve a reliable session-level model.
			if lt.Harness == harness.Claude {
				model = usageClaudeModel(model, confirmed)
			}
			delta, ok := usageDelta(confirmed, lt.Harness)
			if !ok {
				return
			}
			day := at.UTC().Format(usageDayFormat)
			k := key{day: day, model: model}
			row := buckets[k]
			if row == nil {
				row = &usagedb.UsageRow{
					Kind:    "usage",
					Day:     day,
					TaskID:  lt.TaskID,
					Harness: string(lt.Harness),
					Repos:   slices.Clone(repos),
					Model:   model,
				}
				buckets[k] = row
			}
			row.Add(&delta)
			if ts := usagedb.NewTime(at); ts > row.Ts {
				row.Ts = ts
			}
		}
		for timed, err := range lt.StreamMessages(ctx) {
			if err != nil {
				yield(usagedb.UsageRow{}, err)
				return
			}
			if timed.ProducerTime.IsZero() {
				continue
			}
			count, reads := skillReads.Confirm(timed.Message)
			if count {
				add(timed.Message, timed.ProducerTime)
			}
			for _, read := range reads {
				add(read, timed.ProducerTime)
			}
		}
		keys := make([]key, 0, len(buckets))
		for k := range buckets {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b key) int {
			if a.day != b.day {
				return strings.Compare(a.day, b.day)
			}
			return strings.Compare(a.model, b.model)
		})
		for _, k := range keys {
			if !yield(*buckets[k], nil) {
				return
			}
		}
	}
}

func usageClaudeModel(current string, m agent.Message) string {
	switch m := m.(type) {
	case *agent.InitMessage:
		if m.ReportedModel != "" {
			return m.ReportedModel
		}
	case *agent.UsageMessage:
		if m.ReportedModel != "" {
			return m.ReportedModel
		}
	case *agent.SystemMessage:
		if m.Subtype == agent.SystemSubtypeModelRerouted && m.ReportedModel != "" {
			return m.ReportedModel
		}
	}
	return current
}

// usageDelta mirrors the durable, non-priced part of live rollup
// translation; see LoadedTask.UsageRows for the fields it cannot reconstruct.
func usageDelta(m agent.Message, h harness.Name) (usagedb.Delta, bool) {
	var d usagedb.Delta
	switch m := m.(type) {
	case *agent.UsageMessage:
		if h == harness.Pi {
			d.TokenBuckets = usageTokens(m.Usage)
		}
		d.ContextWindow = m.ContextWindow
	case *agent.ResultMessage:
		if h != harness.Pi {
			d.TokenBuckets = usageTokens(m.Usage)
		}
		d.Turns = 1
		if m.IsError {
			d.ErroredTurns = 1
		}
		d.APIMs = m.DurationAPIMs
		d.WallMs = m.DurationMs
		d.ContextWindow = m.ContextWindow
	case *agent.SystemMessage:
		if m.Subtype != "compact_boundary" {
			return usagedb.Delta{}, false
		}
		d.Compactions = 1
	case *agent.SkillReadMessage:
		if m.Skill == "" {
			return usagedb.Delta{}, false
		}
		d.SkillReads = map[string]int{m.Skill: 1}
	case *agent.ToolUseMessage:
		d.ToolCalls = map[string]int{m.Name: 1}
	case *agent.NativeSubagentMessage:
		d.Spawns = 1
		if m.Subagent.Background {
			d.SpawnsBackground = 1
		}
	default:
		return usagedb.Delta{}, false
	}
	return d, true
}

func usageTokens(u agent.Usage) usagedb.TokenBuckets {
	b := usagedb.TokenBuckets{
		Input:     int64(u.InputTokens),
		Output:    int64(u.OutputTokens),
		CacheRead: int64(u.CacheReadInputTokens),
		Reasoning: int64(u.ReasoningOutputTokens),
	}
	if u.CacheTTLSeconds >= 3600 {
		b.CacheWrite1h = int64(u.CacheCreationInputTokens)
	} else {
		b.CacheWrite5m = int64(u.CacheCreationInputTokens)
	}
	return b
}
