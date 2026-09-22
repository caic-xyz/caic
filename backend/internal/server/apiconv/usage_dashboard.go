// Usage dashboard conversion from rollup aggregates to API DTOs.

package apiconv

import (
	"slices"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

// UsageDashboard converts sorted durable-rollup day snapshots to the public
// dashboard contract. It preserves zero-valued breakdowns as empty arrays so
// clients can iterate them without null checks.
func UsageDashboard(days []usagedb.DayRollup) v1.UsageDashboardResp {
	out := v1.UsageDashboardResp{Days: make([]v1.UsageDashboardDay, len(days))}
	if len(days) > 0 {
		out.DataSince = days[0].Day
	}
	for i := range days {
		out.Days[i] = usageDashboardDay(&days[i])
	}
	return out
}

func usageDashboardDay(day *usagedb.DayRollup) v1.UsageDashboardDay {
	return v1.UsageDashboardDay{
		Day:                      day.Day,
		Tokens:                   usageDashboardTokens(day.Tokens),
		Turns:                    day.Turns,
		ErroredTurns:             day.ErroredTurns,
		APIMs:                    day.APIMs,
		WallMs:                   day.WallMs,
		Compactions:              day.Compactions,
		SubagentSpawns:           day.SubagentSpawns,
		SubagentSpawnsBackground: day.SubagentSpawnsBackground,
		CostUSD:                  day.CostUSD,
		Models:                   usageDashboardModels(day.Models),
		Harnesses:                usageDashboardHarnesses(day.Harnesses),
		Repos:                    usageDashboardRepos(day.Repos),
		Skills:                   usageDashboardCounts(day.Skills),
		Tools:                    usageDashboardCounts(day.Tools),
	}
}

func usageDashboardTokens(tokens usagedb.TokenBuckets) v1.UsageDashboardTokens {
	return v1.UsageDashboardTokens{
		Input:        tokens.Input,
		CacheWrite5m: tokens.CacheWrite5m,
		CacheWrite1h: tokens.CacheWrite1h,
		CacheRead:    tokens.CacheRead,
		Output:       tokens.Output,
		Reasoning:    tokens.Reasoning,
	}
}

func usageDashboardModels(models map[string]usagedb.ModelRollup) []v1.UsageDashboardModel {
	names := sortedNames(models)
	out := make([]v1.UsageDashboardModel, len(names))
	for i, name := range names {
		model := models[name]
		out[i] = v1.UsageDashboardModel{Model: name, Tokens: usageDashboardTokens(model.Tokens), Turns: model.Turns, CostUSD: model.CostUSD, ContextWindow: model.ContextWindow}
	}
	return out
}

func usageDashboardHarnesses(harnesses map[string]usagedb.HarnessRollup) []v1.UsageDashboardHarness {
	names := sortedNames(harnesses)
	out := make([]v1.UsageDashboardHarness, len(names))
	for i, name := range names {
		harness := harnesses[name]
		out[i] = v1.UsageDashboardHarness{Harness: name, Tokens: usageDashboardTokens(harness.Tokens), Turns: harness.Turns, CostUSD: harness.CostUSD}
	}
	return out
}

func usageDashboardRepos(repos map[string]int) []v1.UsageDashboardRepo {
	names := sortedNames(repos)
	out := make([]v1.UsageDashboardRepo, len(names))
	for i, name := range names {
		out[i] = v1.UsageDashboardRepo{Repo: name, Tasks: repos[name]}
	}
	return out
}

func usageDashboardCounts(counts map[string]int) []v1.UsageDashboardCount {
	names := sortedNames(counts)
	out := make([]v1.UsageDashboardCount, len(names))
	for i, name := range names {
		out[i] = v1.UsageDashboardCount{Name: name, Count: counts[name]}
	}
	return out
}

func sortedNames[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
