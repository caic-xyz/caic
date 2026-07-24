// Tests for task quota countdown helpers.

import { describe, expect, it } from "vitest";

import type { Task } from "@sdk/types.gen";

import { QuotaRecoveryTracker, formatQuotaCountdown } from "./quota";

const now = Date.parse("2026-07-08T12:00:00Z");

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    harness: "claude",
    model: "claude-sonnet-4",
    ...overrides,
  } as Task;
}

describe("QuotaRecoveryTracker", () => {
  it("notifies once when a waiting quota-blocked task becomes eligible again", () => {
    const tracker = new QuotaRecoveryTracker();
    const task = makeTask({ state: "waiting", rateLimit: { blocked: true, window: "5h", resetsAt: "2026-07-08T12:42:00Z" } });
    const available = { ...task, rateLimit: undefined };

    expect(tracker.update([task])).toEqual([]);
    expect(tracker.update([available])).toEqual([available]);
    expect(tracker.update([available])).toEqual([]);
  });

  it("records a task that becomes waiting while its quota is exhausted", () => {
    const tracker = new QuotaRecoveryTracker();
    const task = makeTask({ state: "waiting", rateLimit: { blocked: true, window: "5h", resetsAt: "2026-07-08T12:42:00Z" } });
    const available = { ...task, rateLimit: undefined };

    expect(tracker.update([{ ...task, state: "running" }])).toEqual([]);
    expect(tracker.update([task])).toEqual([]);
    expect(tracker.update([available])).toEqual([available]);
  });

  it("does not notify when a task stops waiting before quota recovers", () => {
    const tracker = new QuotaRecoveryTracker();
    const task = makeTask({ state: "waiting", rateLimit: { blocked: true, window: "5h", resetsAt: "2026-07-08T12:42:00Z" } });

    tracker.update([task]);

    expect(tracker.update([{ ...task, state: "running", rateLimit: undefined }])).toEqual([]);
  });

  it("does not notify for a task that was already quota-available", () => {
    const tracker = new QuotaRecoveryTracker();
    const task = makeTask({ state: "waiting" });

    expect(tracker.update([task])).toEqual([]);
  });

  it("does not mark an unrelated waiting task as quota-blocked", () => {
    const tracker = new QuotaRecoveryTracker();
    const unrelated = makeTask({ state: "waiting" });

    expect(tracker.update([unrelated])).toEqual([]);
    expect(tracker.update([unrelated])).toEqual([]);
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
