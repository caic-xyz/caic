// Shared quota helpers for matching task harnesses to provider usage snapshots.

import type { ProviderQuota, QuotaRateLimit, Task, UsageResp } from "@sdk/types.gen";

export interface QuotaCountdown {
  providerLabel: string;
  window: string;
  resetsAt: string;
}

const providerAliases: Record<string, string[]> = {
  claude: ["anthropic"],
  codex: ["codex", "openai"],
  opencode: ["opencode"],
  pi: ["pi"],
};

function addCandidate(out: Set<string>, value: string | undefined) {
  const normalized = value?.trim().toLowerCase();
  if (normalized) out.add(normalized);
}

function addModelCandidates(out: Set<string>, model: string | undefined) {
  const normalized = model?.trim().toLowerCase();
  if (!normalized) return;

  const prefix = normalized.split(/[/:]/, 1)[0];
  addCandidate(out, prefix);

  if (normalized.startsWith("claude-") || normalized.includes("/claude-")) addCandidate(out, "anthropic");
  if (normalized.startsWith("gpt-") || normalized.startsWith("o1") || normalized.startsWith("o3") || normalized.startsWith("o4")) addCandidate(out, "openai");
  if (normalized.startsWith("deepseek")) addCandidate(out, "deepseek");
  if (normalized.startsWith("gemini")) addCandidate(out, "gemini");
  if (normalized.includes("mimo")) addCandidate(out, "xiaomi");
}

function taskProviderCandidates(task: Pick<Task, "harness" | "model">): Set<string> {
  const out = new Set<string>();
  addCandidate(out, task.harness);
  for (const alias of providerAliases[task.harness] ?? []) addCandidate(out, alias);
  addModelCandidates(out, task.model);
  return out;
}

type ExhaustedRateLimit = QuotaRateLimit & { resetsAt: string };

function exhaustedRateLimits(provider: ProviderQuota, now: number): ExhaustedRateLimit[] {
  return (provider.rateLimits ?? []).filter((limit): limit is ExhaustedRateLimit => {
    const resetAt = Date.parse(limit.resetsAt ?? "");
    return limit.usedPct >= 100 && Number.isFinite(resetAt) && resetAt > now;
  });
}

/** Returns the blocking quota reset countdown for this task, if its provider is exhausted. */
export function taskQuotaCountdown(task: Pick<Task, "harness" | "model">, usage: UsageResp | null, now: number): QuotaCountdown | undefined {
  const candidates = taskProviderCandidates(task);
  const providers = usage?.providers ?? [];
  for (const provider of providers) {
    if (!candidates.has(provider.provider.toLowerCase())) continue;
    const exhausted = exhaustedRateLimits(provider, now);
    if (exhausted.length === 0) continue;

    const blocking = exhausted.reduce((latest, limit) => {
      const latestReset = Date.parse(latest.resetsAt);
      const limitReset = Date.parse(limit.resetsAt);
      return limitReset > latestReset ? limit : latest;
    });
    return {
      providerLabel: provider.label,
      window: blocking.window,
      resetsAt: blocking.resetsAt,
    };
  }
  return undefined;
}

/** Tracks quota-blocked waiting tasks and reports their one-time recovery transition. */
export class QuotaRecoveryTracker {
  private blockedTaskIDs = new Set<string>();

  /**
   * Records the current quota state and returns waiting tasks whose quota just became available.
   * Tasks that leave waiting are not reported: they are no longer blocked on quota.
   */
  update(tasks: Task[], usage: UsageResp | null, now: number): Task[] {
    const nextBlockedTaskIDs = new Set<string>();
    const recovered: Task[] = [];

    for (const task of tasks) {
      const blocked = task.state === "waiting" && taskQuotaCountdown(task, usage, now) !== undefined;
      if (blocked) {
        nextBlockedTaskIDs.add(task.id);
      } else if (task.state === "waiting" && this.blockedTaskIDs.has(task.id)) {
        recovered.push(task);
      }
    }

    this.blockedTaskIDs = nextBlockedTaskIDs;
    return recovered;
  }
}

export function formatQuotaCountdown(resetsAt: string, now: number): string {
  const diffMs = Date.parse(resetsAt) - now;
  if (!Number.isFinite(diffMs) || diffMs <= 0) return "now";

  const totalMinutes = Math.ceil(diffMs / 60_000);
  if (totalMinutes < 60) return `${totalMinutes}m`;

  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours < 24) return `${hours}h ${minutes}m`;

  const days = Math.floor(hours / 24);
  return `${days}d ${hours % 24}h`;
}
