// Tests for task turn durations and user-response wait derivation.

import { describe, expect, it } from "vitest";

import type { EventMessage } from "@sdk/types.gen";

import { deriveTaskTimings, formatTimingDuration, IncrementalEventTimingTracker, IncrementalTaskTimingTracker } from "./timing";

function result(ts: number, duration: number): EventMessage {
  return {
    kind: "result",
    ts,
    result: {
      subtype: "success",
      isError: false,
      result: "done",
      totalCostUSD: 0,
      duration,
      durationAPI: duration / 2,
      numTurns: 1,
      usage: {
        inputTokens: 1,
        outputTokens: 1,
        cacheCreationInputTokens: 0,
        cacheReadInputTokens: 0,
        model: "test",
      },
    },
  };
}

function input(ts: number, text: string): EventMessage {
  return { kind: "userInput", ts, userInput: { text } };
}

describe("deriveTaskTimings", () => {
  it("associates each completed turn with its first following user response", () => {
    const firstResult = result(10_000, 2);
    const firstInput = input(25_500, "continue");
    const secondResult = result(30_000, 3);
    const secondInput = input(31_250, "again");

    const timings = deriveTaskTimings([
      input(1_000, "initial prompt"),
      firstResult,
      { kind: "system", ts: 12_000, system: { subtype: "idle" } },
      firstInput,
      secondResult,
      secondInput,
    ]);

    expect(timings.turns.map((turn) => turn.waitMs)).toEqual([15_500, 1_250]);
    expect(timings.userWaitMs.get(firstInput)).toBe(15_500);
    expect(timings.userWaitMs.get(secondInput)).toBe(1_250);
  });

  it("tracks the previous visual event for single-event block durations", () => {
    const initialInput = input(1_000, "initial prompt");
    const assistantText: EventMessage = { kind: "text", ts: 2_500, text: { text: "done" } };

    const timings = deriveTaskTimings([
      initialInput,
      { kind: "system", ts: 2_000, system: { subtype: "idle" } },
      assistantText,
    ]);

    expect(timings.previousEventTs.get(assistantText)).toBe(1_000);
  });

  it("leaves unfinished waits and initial prompts without a duration", () => {
    const initialInput = input(1_000, "initial prompt");
    const completed = result(2_000, 1);
    const timings = deriveTaskTimings([initialInput, completed]);

    expect(timings.turns[0].waitMs).toBeNull();
    expect(timings.userWaitMs.has(initialInput)).toBe(false);
  });

  it("does not present same-timestamp inputs as a measured wait", () => {
    const userInput = input(1_000, "continue");
    const timings = deriveTaskTimings([result(1_000, 1), userInput]);

    expect(timings.turns[0].waitMs).toBeNull();
    expect(timings.userWaitMs.has(userInput)).toBe(false);
  });

  it("consumes a completed turn only once", () => {
    const firstInput = input(2_000, "first");
    const duplicateInput = input(3_000, "duplicate");
    const timings = deriveTaskTimings([result(1_000, 1), firstInput, duplicateInput]);

    expect(timings.userWaitMs.get(firstInput)).toBe(1_000);
    expect(timings.userWaitMs.has(duplicateInput)).toBe(false);
  });
});

describe("IncrementalEventTimingTracker", () => {
  it("processes only appended events in a large streaming block", () => {
    const tracker = new IncrementalEventTimingTracker();
    const first = input(1_000, "prompt");
    const events = [first, ...Array.from({ length: 65_000 }, (_, index): EventMessage => ({
      kind: "textDelta",
      ts: index + 2_000,
      textDelta: { text: "delta" },
    }))];
    expect(tracker.derive(events).range).toEqual({ start: 1_000, end: 66_999 });

    Object.defineProperty(first, "ts", {
      configurable: true,
      get: () => { throw new Error("retained event was rescanned"); },
    });
    events.push({ kind: "text", ts: 70_000, text: { text: "done" } });

    expect(tracker.derive(events).range).toEqual({ start: 1_000, end: 70_000 });
  });

  it("rebuilds when a group is replaced rather than appended", () => {
    const tracker = new IncrementalEventTimingTracker();
    tracker.derive([input(1_000, "old"), result(2_000, 1)]);

    const replacement = { kind: "text", ts: 10_000, text: { text: "new" } } satisfies EventMessage;
    const timing = tracker.derive([replacement]);

    expect(timing.range).toEqual({ start: 10_000, end: 10_000 });
    expect(timing.resultEvent).toBeNull();
    expect(timing.firstTimedEvent).toBe(replacement);
  });
});

describe("IncrementalTaskTimingTracker", () => {
  it("derives waits across appended batches", () => {
    const tracker = new IncrementalTaskTimingTracker();
    const messages = [input(1_000, "prompt"), result(5_000, 4)];

    expect(tracker.derive(messages, false).turns[0].waitMs).toBeNull();
    const response = input(8_500, "continue");
    messages.push(response);
    const timings = tracker.derive(messages, false);

    expect(timings.turns).toHaveLength(1);
    expect(timings.turns[0].waitMs).toBe(3_500);
    expect(timings.userWaitMs.get(response)).toBe(3_500);
  });

  it("discards retained state when the timeline epoch resets", () => {
    const tracker = new IncrementalTaskTimingTracker();
    tracker.derive([result(1_000, 1), input(2_000, "old")], false);

    const newResult = result(10_000, 2);
    const timings = tracker.derive([newResult], true);

    expect(timings.turns).toHaveLength(1);
    expect(timings.turns[0].event).toBe(newResult);
    expect(timings.turns[0].waitMs).toBeNull();
    expect(timings.userWaitMs.size).toBe(0);
  });
});

describe("formatTimingDuration", () => {
  it("keeps sub-500ms timings precise and rounds longer durations to clock notation", () => {
    expect(formatTimingDuration(125)).toBe("125ms");
    expect(formatTimingDuration(499)).toBe("499ms");
    expect(formatTimingDuration(500)).toBe("0:01");
    expect(formatTimingDuration(1_250)).toBe("0:01");
    expect(formatTimingDuration(59_500)).toBe("1:00");
    expect(formatTimingDuration(65_000)).toBe("1:05");
    expect(formatTimingDuration(3_599_500)).toBe("1:00:00");
    expect(formatTimingDuration(7_260_000)).toBe("2:01:00");
  });
});

describe("timing derivation performance", () => {
  it("scans a large replay within one frame budget", () => {
    const messages = Array.from({ length: 65_000 }, (_, index): EventMessage => ({
      kind: "system",
      ts: index + 1,
      system: { subtype: "idle" },
    }));
    messages[32_000] = result(32_001, 1);
    messages[64_999] = input(65_000, "continue");

    const start = performance.now();
    const timings = deriveTaskTimings(messages);
    const elapsed = performance.now() - start;

    expect(timings.turns[0].waitMs).toBe(32_999);
    expect(elapsed).toBeLessThan(16);
  });
});
