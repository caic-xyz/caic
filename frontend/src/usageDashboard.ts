// Usage dashboard aggregation preserves additive cost provenance across ranges and drill-downs.

import type { UsageDashboardCost, UsageDashboardCount, UsageDashboardDay, UsageDashboardTokens } from "@sdk/types.gen";
import { formatCost } from "./formatting";

function emptyUsageCost(): UsageDashboardCost {
  return { reportedUSD: 0, estimatedUSD: 0, reportedParts: 0, estimatedParts: 0, unknownParts: 0 };
}

function addUsageCost(total: UsageDashboardCost, cost: UsageDashboardCost): void {
  total.reportedUSD += cost.reportedUSD;
  total.estimatedUSD += cost.estimatedUSD;
  total.reportedParts += cost.reportedParts;
  total.estimatedParts += cost.estimatedParts;
  total.unknownParts += cost.unknownParts;
}

export function usageCostSource(cost: UsageDashboardCost): "estimated" | "mixed" | "partial" | "reported" | "unknown" {
  const reported = cost.reportedParts !== 0;
  const estimated = cost.estimatedParts !== 0;
  if (cost.unknownParts !== 0) return reported || estimated ? "partial" : "unknown";
  if (reported && estimated) return "mixed";
  if (reported) return "reported";
  return estimated ? "estimated" : "unknown";
}

export function formatUsageCost(usd: number, cost: UsageDashboardCost): string {
  const source = usageCostSource(cost);
  if (source === "unknown") return usd === 0 ? "Unknown" : `${formatCost(usd)} · source unknown`;
  if (source === "partial") {
    const known = cost.reportedUSD + cost.estimatedUSD;
    // Old rows can retain numeric amounts without proving their source.
    if (Math.abs(usd - known) > 1e-9) return `${formatCost(usd)} · incomplete (${formatCost(known)} known)`;
    return `${formatCost(usd)} known subtotal · incomplete`;
  }
  return `${formatCost(usd)} · ${source === "mixed" ? "reported + estimated" : source}`;
}

export type UsageRange = "7" | "30" | "90" | "all";

export interface UsageLeaderboardEntry {
  name: string;
  tokens: number;
  turns: number;
  compactions: number;
  costUSD: number;
  cost: UsageDashboardCost;
  cacheHitRate?: number; // populated for harness entries
}

export interface UsageToolEntry {
  name: string;
  calls: number;
  timedCalls: number;
  durationMs: number;
  // medianMs is null when no call in the range kept a duration distribution.
  medianMs: number | null;
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
  cost: UsageDashboardCost;
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

// UsageDailyPoint is one day's tokens and cost for the trend charts, already
// resolved through any active drill-down.
export interface UsageDailyPoint {
  day: string;
  tokens: number;
  costUSD: number;
  cost: UsageDashboardCost;
  turns: number;
  compactions: number;
  subagentSpawns: number;
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
  | "cost"
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
    const harness = day.harnesses?.find((entry) => entry.harness === drilldown.harness);
    if (!harness) return null;
    if (drilldown.model) return harness.models?.find((entry) => entry.model === drilldown.model) ?? null;
    return harness;
  }
  if (drilldown.model) return day.models?.find((entry) => entry.model === drilldown.model) ?? null;
  return day;
}

function emptyTokens(): UsageDashboardTokens {
  return {
    inputTokens: 0,
    cacheWrites: [],
    cacheWrite5mTokens: 0,
    cacheWrite1hTokens: 0,
    cacheReadTokens: 0,
    outputTokens: 0,
    reasoningTokens: 0,
  };
}

function addTokens(total: UsageDashboardTokens, tokens: UsageDashboardTokens): void {
  const buckets = cacheWriteBuckets(total);
  for (const b of cacheWriteBuckets(tokens)) {
    const existing = buckets.find((v) => v.ttlSeconds === b.ttlSeconds);
    if (existing) existing.tokens += b.tokens;
    else buckets.push({ ...b });
  }
  total.cacheWrites = buckets;
  total.inputTokens += tokens.inputTokens;
  total.cacheWrite5mTokens += tokens.cacheWrite5mTokens;
  total.cacheWrite1hTokens += tokens.cacheWrite1hTokens;
  total.cacheReadTokens += tokens.cacheReadTokens;
  total.outputTokens += tokens.outputTokens;
  total.reasoningTokens += tokens.reasoningTokens;
}

// cacheWriteBuckets also accepts dashboards served by versions without durations.
function cacheWriteBuckets(tokens: UsageDashboardTokens): { ttlSeconds: number; tokens: number }[] {
  if (tokens.cacheWrites) return tokens.cacheWrites.map((b) => ({ ...b }));
  return [
    { ttlSeconds: 300, tokens: tokens.cacheWrite5mTokens },
    { ttlSeconds: 3600, tokens: tokens.cacheWrite1hTokens },
  ];
}

function cacheWriteTotal(tokens: UsageDashboardTokens): number {
  return tokens.cacheWrites
    ? tokens.cacheWrites.reduce((sum, b) => sum + b.tokens, 0)
    : tokens.cacheWrite5mTokens + tokens.cacheWrite1hTokens;
}

function tokenTotal(tokens: UsageDashboardTokens): number {
  return tokens.inputTokens + cacheWriteTotal(tokens) + tokens.cacheReadTokens + tokens.outputTokens;
}

function counts(entries: readonly UsageDashboardCount[]): UsageDashboardCount[] {
  return [...entries].sort((a, b) => b.count - a.count || a.name.localeCompare(b.name));
}

function newToolEntry(name: string): UsageToolEntry {
  return { name, calls: 0, timedCalls: 0, durationMs: 0, medianMs: null };
}

// usageToolMedianMs estimates a tool's median duration from merged buckets. Each
// bucket holds the calls above the previous bound and at most its own, and a
// rank inside one bucket is interpolated linearly, so the estimate stays within
// one bucket of the measured median.
function usageToolMedianMs(buckets: ReadonlyMap<number, number>): number | null {
  const bounds = [...buckets.keys()].sort((a, b) => a - b);
  const total = bounds.reduce((sum, bound) => sum + (buckets.get(bound) ?? 0), 0);
  if (total === 0) return null;
  const rank = Math.ceil(total / 2);
  let lower = 0;
  let before = 0;
  for (const bound of bounds) {
    const count = buckets.get(bound) ?? 0;
    if (count === 0) continue;
    if (before + count >= rank) return lower + ((rank - before) / count) * (bound - lower);
    before += count;
    lower = bound;
  }
  return lower;
}

function leaderboard(entries: Map<string, UsageLeaderboardEntry>): UsageLeaderboardEntry[] {
  return [...entries.values()].sort((a, b) => b.tokens - a.tokens || a.name.localeCompare(b.name));
}

function addLeaderboard(
  entries: Map<string, UsageLeaderboardEntry>,
  name: string,
  tokens: UsageDashboardTokens,
  turns: number,
  compactions: number,
  costUSD: number,
  cost: UsageDashboardCost,
): void {
  const entry = entries.get(name) ?? { name, tokens: 0, turns: 0, compactions: 0, costUSD: 0, cost: emptyUsageCost() };
  entry.tokens += tokenTotal(tokens);
  entry.turns += turns;
  entry.compactions += compactions;
  entry.costUSD += costUSD;
  addUsageCost(entry.cost, cost);
  entries.set(name, entry);
}

// usageWindowStart returns the first UTC day of the selected range. The window
// counts back from the latest rollup day rather than the viewer's local clock,
// and never reaches before the first recorded day.
function usageWindowStart(days: readonly UsageDashboardDay[], range: UsageRange): string | null {
  const first = days[0]?.day;
  const latest = days.at(-1)?.day;
  if (!first || !latest) return null;
  if (range === "all") return first;
  const end = new Date(`${latest}T00:00:00Z`);
  end.setUTCDate(end.getUTCDate() - Number(range) + 1);
  const start = end.toISOString().slice(0, 10);
  return start > first ? start : first;
}

// utcDays lists every UTC day key from start through end inclusive.
function utcDays(start: string, end: string): string[] {
  const span = Math.round((Date.parse(`${end}T00:00:00Z`) - Date.parse(`${start}T00:00:00Z`)) / 86_400_000) + 1;
  const first = new Date(`${start}T00:00:00Z`);
  return Array.from({ length: Math.max(span, 0) }, (_, offset) => {
    const date = new Date(first);
    date.setUTCDate(date.getUTCDate() + offset);
    return date.toISOString().slice(0, 10);
  });
}

// usageDaysForRange returns the recorded rollup days inside the selected range.
export function usageDaysForRange(days: readonly UsageDashboardDay[], range: UsageRange): UsageDashboardDay[] {
  const start = usageWindowStart(days, range);
  return start === null ? [] : days.filter((day) => day.day >= start);
}

// usageRangeFrom accepts known ranges and defaults stale URL values to 30 days.
export function usageRangeFrom(value: string): UsageRange {
  return value === "7" || value === "90" || value === "all" ? value : "30";
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
  const toolBuckets = new Map<string, Map<number, number>>();
  let turns = 0;
  let erroredTurns = 0;
  let apiMs = 0;
  let wallMs = 0;
  let compactions = 0;
  let subagentSpawns = 0;
  let costUSD = 0;
  const cost = emptyUsageCost();

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
    addUsageCost(cost, view.cost);
    for (const skill of view.skills ?? []) skills.set(skill.name, (skills.get(skill.name) ?? 0) + skill.count);
    for (const repo of view.repos ?? []) repos.set(repo.repo, (repos.get(repo.repo) ?? 0) + repo.tasks);
    // Both leaderboards stay visible under any filter by reading the
    // harness-by-model cross product: a model filter ranks the harnesses that
    // used it with that model's numbers, and a harness filter ranks the
    // models used under it.
    if (drilldown?.harness) {
      const harness = day.harnesses?.find((entry) => entry.harness === drilldown.harness);
      for (const model of harness?.models ?? []) {
        if (drilldown.model && model.model !== drilldown.model) continue;
        addLeaderboard(models, model.model, model.tokens, model.turns, model.compactions, model.costUSD, model.cost);
      }
    } else {
      for (const model of day.models ?? []) {
        if (drilldown?.model && model.model !== drilldown.model) continue;
        addLeaderboard(models, model.model, model.tokens, model.turns, model.compactions, model.costUSD, model.cost);
      }
    }
    if (drilldown?.model) {
      for (const harness of day.harnesses ?? []) {
        const pair = harness.models?.find((entry) => entry.model === drilldown.model);
        if (!pair) continue;
        addLeaderboard(harnesses, harness.harness, pair.tokens, pair.turns, pair.compactions, pair.costUSD, pair.cost);
        const combined = harnessTokens.get(harness.harness) ?? emptyTokens();
        addTokens(combined, pair.tokens);
        harnessTokens.set(harness.harness, combined);
      }
    } else {
      for (const harness of day.harnesses ?? []) {
        if (drilldown?.harness && harness.harness !== drilldown.harness) continue;
        addLeaderboard(
          harnesses,
          harness.harness,
          harness.tokens,
          harness.turns,
          harness.compactions,
          harness.costUSD,
          harness.cost,
        );
        const combined = harnessTokens.get(harness.harness) ?? emptyTokens();
        addTokens(combined, harness.tokens);
        harnessTokens.set(harness.harness, combined);
      }
    }
    for (const tool of view.tools ?? []) {
      const entry = tools.get(tool.name) ?? newToolEntry(tool.name);
      entry.calls += tool.count;
      tools.set(tool.name, entry);
    }
    for (const timing of view.toolTimings ?? []) {
      const entry = tools.get(timing.name) ?? newToolEntry(timing.name);
      entry.timedCalls += timing.count;
      entry.durationMs += timing.durationMs;
      const buckets = toolBuckets.get(timing.name) ?? new Map<number, number>();
      for (const bucket of timing.buckets ?? []) {
        buckets.set(bucket.upperMs, (buckets.get(bucket.upperMs) ?? 0) + bucket.count);
      }
      toolBuckets.set(timing.name, buckets);
      tools.set(timing.name, entry);
    }
  }

  for (const [name, entry] of tools) {
    entry.medianMs = usageToolMedianMs(toolBuckets.get(name) ?? new Map<number, number>());
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
    cost,
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

// usageDailySeries resolves one chart point per UTC day of the selected range,
// through any active drill-down. A day without a rollup row, or without usage
// for the drilled subject, contributes a zero point so the trend keeps its
// daily spacing instead of joining two distant days.
export function usageDailySeries(
  days: readonly UsageDashboardDay[],
  range: UsageRange,
  drilldown: UsageDrilldown | null,
): UsageDailyPoint[] {
  const start = usageWindowStart(days, range);
  const latest = days.at(-1)?.day;
  if (start === null || !latest) return [];
  const byDay = new Map(days.map((day) => [day.day, day]));
  const today = new Date().toISOString().slice(0, 10);
  return utcDays(start, latest).map((key) => {
    const day = byDay.get(key);
    const view = day ? dayView(day, drilldown) : null;
    return {
      day: key,
      inProgress: key === today,
      tokens: view ? tokenTotal(view.tokens) : 0,
      costUSD: view ? view.costUSD : 0,
      cost: view ? view.cost : emptyUsageCost(),
      turns: view ? view.turns : 0,
      compactions: view ? view.compactions : 0,
      subagentSpawns: view ? view.subagentSpawns : 0,
    };
  });
}

// usageCacheHitRate returns cache reads as a share of all input context.
export function usageCacheHitRate(tokens: UsageDashboardTokens): number {
  const input = tokens.inputTokens + cacheWriteTotal(tokens) + tokens.cacheReadTokens;
  return input === 0 ? 0 : tokens.cacheReadTokens / input;
}
