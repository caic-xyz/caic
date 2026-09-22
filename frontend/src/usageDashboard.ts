// Usage dashboard range filtering and rollup aggregation for the web UI.

import type { UsageDashboardCount, UsageDashboardDay, UsageDashboardTokens } from "@sdk/types.gen";

export type UsageRange = "7" | "30" | "90" | "all";

export interface UsageLeaderboardEntry {
  name: string;
  tokens: number;
  turns: number;
  costUSD: number;
}

export interface UsageDashboardSummary {
  tokens: UsageDashboardTokens;
  totalTokens: number;
  turns: number;
  costUSD: number;
  skills: UsageDashboardCount[];
  models: UsageLeaderboardEntry[];
  harnesses: UsageLeaderboardEntry[];
  repos: UsageDashboardCount[];
}

function emptyTokens(): UsageDashboardTokens {
  return {
    inputTokens: 0,
    cacheWrite5mTokens: 0,
    cacheWrite1hTokens: 0,
    cacheReadTokens: 0,
    outputTokens: 0,
    reasoningTokens: 0,
  };
}

function addTokens(total: UsageDashboardTokens, tokens: UsageDashboardTokens): void {
  total.inputTokens += tokens.inputTokens;
  total.cacheWrite5mTokens += tokens.cacheWrite5mTokens;
  total.cacheWrite1hTokens += tokens.cacheWrite1hTokens;
  total.cacheReadTokens += tokens.cacheReadTokens;
  total.outputTokens += tokens.outputTokens;
  total.reasoningTokens += tokens.reasoningTokens;
}

function tokenTotal(tokens: UsageDashboardTokens): number {
  return (
    tokens.inputTokens +
    tokens.cacheWrite5mTokens +
    tokens.cacheWrite1hTokens +
    tokens.cacheReadTokens +
    tokens.outputTokens
  );
}

function counts(entries: readonly UsageDashboardCount[]): UsageDashboardCount[] {
  return [...entries].sort((a, b) => b.count - a.count || a.name.localeCompare(b.name));
}

function leaderboard(entries: Map<string, UsageLeaderboardEntry>): UsageLeaderboardEntry[] {
  return [...entries.values()].sort((a, b) => b.tokens - a.tokens || a.name.localeCompare(b.name));
}

function addLeaderboard(
  entries: Map<string, UsageLeaderboardEntry>,
  name: string,
  tokens: UsageDashboardTokens,
  turns: number,
  costUSD: number,
): void {
  const entry = entries.get(name) ?? { name, tokens: 0, turns: 0, costUSD: 0 };
  entry.tokens += tokenTotal(tokens);
  entry.turns += turns;
  entry.costUSD += costUSD;
  entries.set(name, entry);
}

// usageDaysForRange returns the most recent calendar-day window, based on the
// latest rollup day rather than the viewer's local clock.
export function usageDaysForRange(days: readonly UsageDashboardDay[], range: UsageRange): UsageDashboardDay[] {
  if (range === "all" || days.length === 0) return [...days];
  const latest = days.at(-1)?.day;
  if (!latest) return [];
  const end = new Date(`${latest}T00:00:00Z`);
  end.setUTCDate(end.getUTCDate() - Number(range) + 1);
  const start = end.toISOString().slice(0, 10);
  return days.filter((day) => day.day >= start);
}

// usageRangeFrom accepts only the range values represented by the UI control.
export function usageRangeFrom(value: string): UsageRange {
  return value === "7" || value === "30" || value === "90" ? value : "all";
}

// summarizeUsageDays folds selected per-day rollups into dashboard metrics.
// Repository counts are task-days: one task touching a repo on two days counts
// twice, which makes the range aggregation additive and explicit.
export function summarizeUsageDays(days: readonly UsageDashboardDay[]): UsageDashboardSummary {
  const tokens = emptyTokens();
  const skills = new Map<string, number>();
  const repos = new Map<string, number>();
  const models = new Map<string, UsageLeaderboardEntry>();
  const harnesses = new Map<string, UsageLeaderboardEntry>();
  let turns = 0;
  let costUSD = 0;

  for (const day of days) {
    addTokens(tokens, day.tokens);
    turns += day.turns;
    costUSD += day.costUSD;
    for (const skill of day.skills) skills.set(skill.name, (skills.get(skill.name) ?? 0) + skill.count);
    for (const repo of day.repos) repos.set(repo.repo, (repos.get(repo.repo) ?? 0) + repo.tasks);
    for (const model of day.models) addLeaderboard(models, model.model, model.tokens, model.turns, model.costUSD);
    for (const harness of day.harnesses)
      addLeaderboard(harnesses, harness.harness, harness.tokens, harness.turns, harness.costUSD);
  }

  return {
    tokens,
    totalTokens: tokenTotal(tokens),
    turns,
    costUSD,
    skills: counts([...skills].map(([name, count]) => ({ name, count }))),
    models: leaderboard(models),
    harnesses: leaderboard(harnesses),
    repos: counts([...repos].map(([name, count]) => ({ name, count }))),
  };
}

// usageCacheHitRate returns cache reads as a share of all input context.
export function usageCacheHitRate(tokens: UsageDashboardTokens): number {
  const input = tokens.inputTokens + tokens.cacheWrite5mTokens + tokens.cacheWrite1hTokens + tokens.cacheReadTokens;
  return input === 0 ? 0 : tokens.cacheReadTokens / input;
}
