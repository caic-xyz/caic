// Orders harness choices for quota recovery without claiming unknown capacity.

import type { HarnessInfo, QuotaProvider, UsageResp } from "@sdk/types.gen";

export type QuotaTargetStatus = "available" | "exhausted" | "unknown";

export interface QuotaTarget {
  harness: HarnessInfo;
  status: QuotaTargetStatus;
  label: string;
  recommended: boolean;
}

interface Candidate {
  harness: HarnessInfo;
  status: QuotaTargetStatus;
  sameExhaustedGroup: boolean;
  originalIndex: number;
}

/** Orders recovery harnesses by known viability while retaining every choice. */
export function quotaRecoveryTargets(
  harnesses: HarnessInfo[],
  usage: UsageResp | null,
  exhaustedGroup: QuotaProvider | undefined,
  preferredHarness: string,
  now: number,
): QuotaTarget[] {
  const candidates = harnesses.map((harness, originalIndex): Candidate => {
    const sameExhaustedGroup = exhaustedGroup !== undefined
      && harness.quotaGroup === exhaustedGroup;
    return {
      harness,
      status: sameExhaustedGroup
        ? "exhausted"
        : quotaGroupStatus(harness.quotaGroup, usage, now),
      sameExhaustedGroup,
      originalIndex,
    };
  });
  candidates.sort((a, b) => {
    const statusOrder = quotaStatusRank(a.status) - quotaStatusRank(b.status);
    if (statusOrder !== 0) return statusOrder;
    if (a.status === "available") {
      const preferredOrder = Number(b.harness.name === preferredHarness)
        - Number(a.harness.name === preferredHarness);
      if (preferredOrder !== 0) return preferredOrder;
    }
    return a.originalIndex - b.originalIndex;
  });

  const recommendedIndex = candidates.findIndex((candidate) => candidate.status === "available");
  return candidates.map((candidate, index) => {
    const recommended = index === recommendedIndex;
    return {
      harness: candidate.harness,
      status: candidate.status,
      label: quotaTargetLabel(candidate.status, candidate.sameExhaustedGroup, recommended),
      recommended,
    };
  });
}

function quotaGroupStatus(
  group: QuotaProvider | undefined,
  usage: UsageResp | null,
  now: number,
): QuotaTargetStatus {
  if (group === undefined) return "unknown";
  const provider = usage?.providers?.find((candidate) => candidate.provider === group);
  if (provider?.fetchStatus !== "fresh") return "unknown";
  const limits = provider.rateLimits;
  if (!limits || limits.length === 0) return "unknown";
  if (limits.some((limit) => limit.usedPct >= 100 && (!limit.resetsAt || Date.parse(limit.resetsAt) > now))) {
    return "exhausted";
  }
  if (limits.some((limit) => limit.usedPct >= 100)) return "unknown";
  return "available";
}

function quotaStatusRank(status: QuotaTargetStatus): number {
  switch (status) {
    case "available": return 0;
    case "unknown": return 1;
    case "exhausted": return 2;
  }
}

function quotaTargetLabel(status: QuotaTargetStatus, sameExhaustedGroup: boolean, recommended: boolean): string {
  if (sameExhaustedGroup) return "Same exhausted quota";
  switch (status) {
    case "available": return recommended ? "Available · Recommended" : "Available";
    case "exhausted": return "Out of quota";
    case "unknown": return "Quota status unknown";
  }
}
