// Tests quota-aware harness ordering and conservative availability labels.

import { describe, expect, it } from "vitest";

import type { HarnessInfo, ISOTimestamp, QuotaProvider, UsageResp } from "@sdk/types.gen";
import { quotaRecoveryTargets } from "./quotaTargets";

const now = Date.parse("2026-09-11T12:00:00Z");

function harness(name: HarnessInfo["name"], quotaGroup?: QuotaProvider): HarnessInfo {
  return {
    name,
    models: [],
    supportsImages: false,
    supportsCompact: false,
    ...(quotaGroup ? { quotaGroup } : {}),
  };
}

function usage(groups: Array<{ provider: QuotaProvider; usedPct: number; resetsAt?: string; fetchStatus?: "fresh" | "stale" | "error" }>): UsageResp {
  return {
    local: { windows: [] },
    providers: groups.map((group) => ({
      provider: group.provider,
      label: group.provider,
      logoUrl: "",
      authKind: "oauth",
      usageUrl: "",
      fetchStatus: group.fetchStatus ?? "fresh",
      rateLimits: [{
        window: "primary",
        usedPct: group.usedPct,
        ...(group.resetsAt ? { resetsAt: group.resetsAt as ISOTimestamp } : {}),
      }],
    })),
  };
}

describe("quotaRecoveryTargets", () => {
  it("puts a preferred viable alternative first and the shared exhausted group last", () => {
    const targets = quotaRecoveryTargets(
      [
        harness("claude", "claudecode"),
        harness("codex", "codex"),
        harness("opencode", "openrouter"),
        harness("pi"),
      ],
      usage([
        { provider: "claudecode", usedPct: 10 },
        { provider: "codex", usedPct: 25 },
        { provider: "openrouter", usedPct: 10 },
      ]),
      "claudecode",
      "opencode",
      now,
    );

    expect(targets.map((target) => target.harness.name)).toEqual(["opencode", "codex", "pi", "claude"]);
    expect(targets.map((target) => target.label)).toEqual([
      "Available · Recommended",
      "Available",
      "Quota status unknown",
      "Same exhausted quota",
    ]);
  });

  it("keeps unknown candidates selectable without describing them as available", () => {
    const targets = quotaRecoveryTargets(
      [harness("pi"), harness("opencode", "openrouter")],
      usage([]),
      "claudecode",
      "pi",
      now,
    );

    expect(targets).toHaveLength(2);
    expect(targets.every((target) => target.status === "unknown")).toBe(true);
    expect(targets.every((target) => target.recommended === false)).toBe(true);
    expect(targets.every((target) => !target.label.includes("Available"))).toBe(true);
  });

  it("does not claim an expired exhausted snapshot is available", () => {
    const [target] = quotaRecoveryTargets(
      [harness("codex", "codex")],
      usage([{ provider: "codex", usedPct: 100, resetsAt: "2026-09-11T11:00:00Z" }]),
      "claudecode",
      "codex",
      now,
    );

    expect(target.status).toBe("unknown");
    expect(target.label).toBe("Quota status unknown");
  });

  it.each(["stale", "error"] as const)("treats %s provider data as unknown", (fetchStatus) => {
    const [target] = quotaRecoveryTargets(
      [harness("codex", "codex")],
      usage([{ provider: "codex", usedPct: 25, fetchStatus }]),
      "claudecode",
      "codex",
      now,
    );

    expect(target.status).toBe("unknown");
    expect(target.recommended).toBe(false);
    expect(target.label).toBe("Quota status unknown");
  });
});
