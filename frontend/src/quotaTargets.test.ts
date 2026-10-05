// Tests quota-aware harness ordering and conservative availability labels.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import type { HarnessInfo, ISOTimestamp, QuotaProvider, UsageResp } from "@sdk/types.gen";
import { quotaRecoveryTargets } from "./quotaTargets";

const now = Date.parse("2026-09-11T12:00:00Z");

function harness(name: HarnessInfo["name"], quotaGroup?: QuotaProvider): HarnessInfo {
  return {
    name,
    models: [],
    supportsImages: false,
    supportsCompact: false,
    supportsModelRefresh: false,
    ...(quotaGroup ? { quotaGroup } : {}),
  };
}

function usage(
  groups: Array<{
    provider: QuotaProvider;
    utilization: number;
    resetsAt?: string;
    fetchStatus?: "fresh" | "stale" | "error";
  }>,
) {
  return {
    local: { windows: [] },
    providers: groups.map((group) => ({
      provider: group.provider,
      label: group.provider,
      logoUrl: "",
      authKind: "oauth",
      usageUrl: "",
      fetchStatus: group.fetchStatus ?? "fresh",
      rateLimits: [
        {
          label: "primary",
          window: "primary",
          utilization: group.utilization,
          ...(group.resetsAt ? { resetsAt: group.resetsAt as ISOTimestamp } : {}),
        },
      ],
    })),
  } satisfies UsageResp;
}

describe("quotaRecoveryTargets", () => {
  it("puts a preferred viable alternative first and the shared exhausted group last", () => {
    const targets = quotaRecoveryTargets(
      [harness("claude", "claudecode"), harness("codex", "codex"), harness("opencode", "openrouter"), harness("pi")],
      usage([
        { provider: "claudecode", utilization: 0.1 },
        { provider: "codex", utilization: 0.25 },
        { provider: "openrouter", utilization: 0.1 },
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

  for (const [gemini, thirdParty] of [
    [1, 0],
    [0, 1],
    [0, 0],
    [1, 1],
  ]) {
    it(`does not infer Antigravity harness capacity without a model (${gemini}, ${thirdParty})`, () => {
      const data = usage([{ provider: "antigravity", utilization: 0 }]);
      data.providers[0].rateLimits = [
        { label: "7d", window: "gemini-weekly", utilization: gemini },
        { label: "5h", window: "gemini-5h", utilization: gemini },
        { label: "3p-7d", window: "3p-weekly", utilization: thirdParty },
        { label: "3p-5h", window: "3p-5h", utilization: thirdParty },
      ];
      const [target] = quotaRecoveryTargets(
        [harness("antigravity", "antigravity")],
        data,
        "antigravity",
        "antigravity",
        now,
      );
      expect(target.status).toBe("unknown");
      expect(target.label).toBe("Quota status unknown");
      expect(target.recommended).toBe(false);
    });
  }

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
      usage([{ provider: "codex", utilization: 1, resetsAt: "2026-09-11T11:00:00Z" }]),
      "claudecode",
      "codex",
      now,
    );

    expect(target.status).toBe("unknown");
    expect(target.label).toBe("Quota status unknown");
  });

  for (const fetchStatus of ["stale", "error"] as const) {
    it(`treats ${fetchStatus} provider data as unknown`, () => {
      const [target] = quotaRecoveryTargets(
        [harness("codex", "codex")],
        usage([{ provider: "codex", utilization: 0.25, fetchStatus }]),
        "claudecode",
        "codex",
        now,
      );

      expect(target.status).toBe("unknown");
      expect(target.recommended).toBe(false);
      expect(target.label).toBe("Quota status unknown");
    });
  }
});
