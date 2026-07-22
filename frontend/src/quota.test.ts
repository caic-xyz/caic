// Tests for task quota countdown helpers.

import { describe, expect, it } from "vitest";

import type { ISOTimestamp, ProviderQuota, Task, UsageResp } from "@sdk/types.gen";

import { QuotaRecoveryTracker, formatQuotaCountdown, taskQuotaCountdown } from "./quota";

const now = Date.parse("2026-07-08T12:00:00Z");

function makeTask(overrides: Partial<Task> = {}): Pick<Task, "harness" | "model"> {
  return {
    harness: "claude",
    model: "claude-sonnet-4",
    ...overrides,
  } as Task;
}

function makeUsage(providers: ProviderQuota[]): UsageResp {
  return { providers, local: { windows: [] } };
}

function makeProvider(overrides: Partial<ProviderQuota> = {}): ProviderQuota {
  return {
    provider: "anthropic",
    label: "Anthropic",
    logoUrl: "",
    authKind: "oauth",
    usageUrl: "",
    ...overrides,
  };
}

describe("taskQuotaCountdown", () => {
  it("returns the reset countdown for a task's exhausted harness provider", () => {
    const usage = makeUsage([
      makeProvider({
        rateLimits: [{ window: "5h", usedPct: 100, resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp }],
      }),
    ]);

    expect(taskQuotaCountdown(makeTask(), usage, now)).toEqual({
      providerLabel: "Anthropic",
      window: "5h",
      resetsAt: "2026-07-08T12:42:00Z",
    });
  });

  it("does not return a countdown for a non-exhausted provider", () => {
    const usage = makeUsage([
      makeProvider({ rateLimits: [{ window: "5h", usedPct: 99, resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp }] }),
    ]);

    expect(taskQuotaCountdown(makeTask(), usage, now)).toBeUndefined();
  });

  it("matches codex harness quota", () => {
    const usage = makeUsage([
      makeProvider({ provider: "codex", label: "Codex", rateLimits: [{ window: "5h", usedPct: 100, resetsAt: "2026-07-08T13:00:00Z" as ISOTimestamp }] }),
    ]);

    expect(taskQuotaCountdown(makeTask({ harness: "codex", model: "gpt-5" }), usage, now)?.providerLabel).toBe("Codex");
  });

  it("uses the later reset when multiple quota windows are exhausted", () => {
    const usage = makeUsage([
      makeProvider({
        rateLimits: [
          { window: "5h", usedPct: 100, resetsAt: "2026-07-08T12:30:00Z" as ISOTimestamp },
          { window: "7d", usedPct: 100, resetsAt: "2026-07-09T12:00:00Z" as ISOTimestamp },
        ],
      }),
    ]);

    expect(taskQuotaCountdown(makeTask(), usage, now)?.window).toBe("7d");
  });
});

describe("QuotaRecoveryTracker", () => {
  it("notifies once when a waiting quota-blocked task becomes eligible again", () => {
    const tracker = new QuotaRecoveryTracker();
    const task = makeTask({ state: "waiting" });
    const exhausted = makeUsage([
      makeProvider({ rateLimits: [{ window: "5h", usedPct: 100, resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp }] }),
    ]);
    const available = makeUsage([
      makeProvider({ rateLimits: [{ window: "5h", usedPct: 42, resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp }] }),
    ]);

    expect(tracker.update([task as Task], exhausted, now)).toEqual([]);
    expect(tracker.update([task as Task], available, now)).toEqual([task]);
    expect(tracker.update([task as Task], available, now)).toEqual([]);
  });

  it("records a task that becomes waiting while its quota is exhausted", () => {
    const tracker = new QuotaRecoveryTracker();
    const task = makeTask({ state: "waiting" });
    const exhausted = makeUsage([
      makeProvider({ rateLimits: [{ window: "5h", usedPct: 100, resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp }] }),
    ]);
    const available = makeUsage([
      makeProvider({ rateLimits: [{ window: "5h", usedPct: 42, resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp }] }),
    ]);

    expect(tracker.update([{ ...task, state: "running" } as Task], exhausted, now)).toEqual([]);
    expect(tracker.update([task as Task], exhausted, now)).toEqual([]);
    expect(tracker.update([task as Task], available, now)).toEqual([task]);
  });

  it("does not notify when a task stops waiting before quota recovers", () => {
    const tracker = new QuotaRecoveryTracker();
    const task = makeTask({ state: "waiting" });
    const exhausted = makeUsage([
      makeProvider({ rateLimits: [{ window: "5h", usedPct: 100, resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp }] }),
    ]);
    const available = makeUsage([
      makeProvider({ rateLimits: [{ window: "5h", usedPct: 42, resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp }] }),
    ]);

    tracker.update([task as Task], exhausted, now);

    expect(tracker.update([{ ...task, state: "running" } as Task], available, now)).toEqual([]);
  });

  it("does not notify for a task that was already quota-available", () => {
    const tracker = new QuotaRecoveryTracker();
    const task = makeTask({ state: "waiting" });
    const available = makeUsage([
      makeProvider({ rateLimits: [{ window: "5h", usedPct: 42, resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp }] }),
    ]);

    expect(tracker.update([task as Task], available, now)).toEqual([]);
  });
});

describe("formatQuotaCountdown", () => {
  it("formats remaining minutes", () => {
    expect(formatQuotaCountdown("2026-07-08T12:01:01Z", now)).toBe("2m");
  });

  it("formats remaining hours", () => {
    expect(formatQuotaCountdown("2026-07-08T14:05:00Z", now)).toBe("2h 5m");
  });
});
