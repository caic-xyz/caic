// Usage dashboard range filtering and rollup aggregation for the web UI.

import type { UsageDashboardCount, UsageDashboardDay, UsageDashboardTokens } from "@sdk/types.gen";

export type UsageRange = "7" | "30" | "90" | "all";

export interface UsageLeaderboardEntry {
  name: string;
  tokens: number;
  turns: number;
  costUSD: number;
  cacheHitRate?: number; // populated for harness entries
}

export interface UsageToolEntry {
  name: string;
  calls: number;
  timedCalls: number;
  durationMs: number;
}

export interface UsageDashboardSummary {
  tokens: UsageDashboardTokens;
  totalTokens: number;
  turns: number;
  erroredTurns: number;
  apiMs: number;
  wallMs: number;
  compactions: number;
  subagentSpawns: number;
  costUSD: number;
  skills: UsageDashboardCount[];
  models: UsageLeaderboardEntry[];
  harnesses: UsageLeaderboardEntry[];
  tools: UsageToolEntry[];
  repos: UsageDashboardCount[];
}

// UsageDrilldown filters every dashboard panel to one harness, one model, or
// a harness-by-model pair. Both fields set means the pair.
export interface UsageDrilldown {
  harness?: string;
  model?: string;
}

// drilldownFromHash parses a location hash fragment ("#harness=x&model=y")
// into a drill-down filter, or null when the fragment holds no filter.
export function drilldownFromHash(hash: string): UsageDrilldown | null {
  const params = new URLSearchParams(hash.startsWith("#") ? hash.slice(1) : hash);
  const harness = params.get("harness") ?? undefined;
  const model = params.get("model") ?? undefined;
  if (!harness && !model) return null;
  return { harness, model };
}

// hashFromDrilldown serializes a drill-down filter into a location hash
// fragment; the empty string clears the fragment.
export function hashFromDrilldown(drilldown: UsageDrilldown | null): string {
  if (!drilldown) return "";
  const params = new URLSearchParams();
  if (drilldown.harness) params.set("harness", drilldown.harness);
  if (drilldown.model) params.set("model", drilldown.model);
  const query = params.toString();
  return query ? `#${query}` : "";
}

// drilldownEqual reports whether two filters select the same subject.
export function drilldownEqual(a: UsageDrilldown | null, b: UsageDrilldown | null): boolean {
  return a?.harness === b?.harness && a?.model === b?.model;
}

// UsageDailyPoint is one day's tokens and cost for the trend charts, already
// resolved through any active drill-down.
export interface UsageDailyPoint {
  day: string;
  tokens: number;
  costUSD: number;
  inProgress: boolean;
}

// DayView is the slice of a rollup day the summary folds. Day totals, model
// drill-down entries, and harness drill-down entries all share this shape.
type DayView = Pick<
  UsageDashboardDay,
  | "tokens"
  | "turns"
  | "erroredTurns"
  | "apiMs"
  | "wallMs"
  | "compactions"
  | "subagentSpawns"
  | "costUSD"
  | "skills"
  | "repos"
  | "tools"
  | "toolTimings"
>;

// dayView resolves one day through the drill-down: the day itself when
// unfiltered, the matching model, harness, or harness-by-model entry when
// filtered, or null when the day has no usage for the selected subject.
function dayView(day: UsageDashboardDay, drilldown: UsageDrilldown | null): DayView | null {
  if (!drilldown) return day;
  if (drilldown.harness) {
    const harness = day.harnesses.find((entry) => entry.harness === drilldown.harness);
    if (!harness) return null;
    if (drilldown.model) return harness.models.find((entry) => entry.model === drilldown.model) ?? null;
    return harness;
  }
  if (drilldown.model) return day.models.find((entry) => entry.model === drilldown.model) ?? null;
  return day;
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

// summarizeUsageDays folds selected per-day rollups into dashboard metrics,
// optionally filtered to a harness, model, or harness-by-model drill-down.
// Repository counts are task-days: one task touching a repo on two days
// counts twice, which makes the range aggregation additive and explicit.
export function summarizeUsageDays(
  days: readonly UsageDashboardDay[],
  drilldown: UsageDrilldown | null = null,
): UsageDashboardSummary {
  const tokens = emptyTokens();
  const skills = new Map<string, number>();
  const repos = new Map<string, number>();
  const models = new Map<string, UsageLeaderboardEntry>();
  const harnesses = new Map<string, UsageLeaderboardEntry>();
  const harnessTokens = new Map<string, UsageDashboardTokens>();
  const tools = new Map<string, UsageToolEntry>();
  let turns = 0;
  let erroredTurns = 0;
  let apiMs = 0;
  let wallMs = 0;
  let compactions = 0;
  let subagentSpawns = 0;
  let costUSD = 0;

  for (const day of days) {
    const view = dayView(day, drilldown);
    if (!view) continue;
    addTokens(tokens, view.tokens);
    turns += view.turns;
    erroredTurns += view.erroredTurns;
    apiMs += view.apiMs;
    wallMs += view.wallMs;
    compactions += view.compactions;
    subagentSpawns += view.subagentSpawns;
    costUSD += view.costUSD;
    for (const skill of view.skills) skills.set(skill.name, (skills.get(skill.name) ?? 0) + skill.count);
    for (const repo of view.repos) repos.set(repo.repo, (repos.get(repo.repo) ?? 0) + repo.tasks);
    // Both leaderboards stay visible under any filter by reading the
    // harness-by-model cross product: a model filter ranks the harnesses that
    // used it with that model's numbers, and a harness filter ranks the
    // models used under it.
    if (drilldown?.harness) {
      const harness = day.harnesses.find((entry) => entry.harness === drilldown.harness);
      for (const model of harness?.models ?? []) {
        if (drilldown.model && model.model !== drilldown.model) continue;
        addLeaderboard(models, model.model, model.tokens, model.turns, model.costUSD);
      }
    } else {
      for (const model of day.models) {
        if (drilldown?.model && model.model !== drilldown.model) continue;
        addLeaderboard(models, model.model, model.tokens, model.turns, model.costUSD);
      }
    }
    if (drilldown?.model) {
      for (const harness of day.harnesses) {
        const pair = harness.models.find((entry) => entry.model === drilldown.model);
        if (!pair) continue;
        addLeaderboard(harnesses, harness.harness, pair.tokens, pair.turns, pair.costUSD);
        const combined = harnessTokens.get(harness.harness) ?? emptyTokens();
        addTokens(combined, pair.tokens);
        harnessTokens.set(harness.harness, combined);
      }
    } else {
      for (const harness of day.harnesses) {
        if (drilldown?.harness && harness.harness !== drilldown.harness) continue;
        addLeaderboard(harnesses, harness.harness, harness.tokens, harness.turns, harness.costUSD);
        const combined = harnessTokens.get(harness.harness) ?? emptyTokens();
        addTokens(combined, harness.tokens);
        harnessTokens.set(harness.harness, combined);
      }
    }
    for (const tool of view.tools) {
      const entry = tools.get(tool.name) ?? { name: tool.name, calls: 0, timedCalls: 0, durationMs: 0 };
      entry.calls += tool.count;
      tools.set(tool.name, entry);
    }
    for (const timing of view.toolTimings) {
      const entry = tools.get(timing.name) ?? { name: timing.name, calls: 0, timedCalls: 0, durationMs: 0 };
      entry.timedCalls += timing.count;
      entry.durationMs += timing.durationMs;
      tools.set(timing.name, entry);
    }
  }

  return {
    tokens,
    totalTokens: tokenTotal(tokens),
    turns,
    erroredTurns,
    apiMs,
    wallMs,
    compactions,
    subagentSpawns,
    costUSD,
    skills: counts([...skills].map(([name, count]) => ({ name, count }))),
    models: leaderboard(models),
    harnesses: leaderboard(harnesses).map((entry) => ({
      ...entry,
      cacheHitRate: usageCacheHitRate(harnessTokens.get(entry.name) ?? emptyTokens()),
    })),
    tools: [...tools.values()].sort(
      (a, b) => b.durationMs - a.durationMs || b.calls - a.calls || a.name.localeCompare(b.name),
    ),
    repos: counts([...repos].map(([name, count]) => ({ name, count }))),
  };
}

// usageDailySeries resolves each day through the drill-down into chart points;
// days without usage for the selected subject contribute nothing.
export function usageDailySeries(
  days: readonly UsageDashboardDay[],
  drilldown: UsageDrilldown | null = null,
): UsageDailyPoint[] {
  const today = new Date().toISOString().slice(0, 10);
  const points: UsageDailyPoint[] = [];
  for (const day of days) {
    const view = dayView(day, drilldown);
    if (!view) continue;
    points.push({
      day: day.day,
      inProgress: day.day === today,
      tokens: tokenTotal(view.tokens),
      costUSD: view.costUSD,
    });
  }
  return points;
}

// usageCacheHitRate returns cache reads as a share of all input context.
export function usageCacheHitRate(tokens: UsageDashboardTokens): number {
  const input = tokens.inputTokens + tokens.cacheWrite5mTokens + tokens.cacheWrite1hTokens + tokens.cacheReadTokens;
  return input === 0 ? 0 : tokens.cacheReadTokens / input;
}
