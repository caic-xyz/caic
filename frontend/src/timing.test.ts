// Tests for task turn durations and user-response wait derivation.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import type { EventMessage } from "@sdk/types.gen";

import { formatTimingDuration, IncrementalEventTimingTracker, VisualTimingTracker } from "./timing";

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
        reportedModel: "test",
      },
    },
  };
}

function input(ts: number, text: string): EventMessage {
  return { kind: "userInput", ts, userInput: { text } };
}

describe("IncrementalEventTimingTracker", () => {
  it("processes only appended events in a large streaming block", () => {
    const tracker = new IncrementalEventTimingTracker();
    const first = input(1_000, "prompt");
    const events = [
      first,
      ...Array.from({ length: 65_000 }, (_, index): EventMessage => ({
        kind: "textDelta",
        ts: index + 2_000,
        textDelta: { text: "delta" },
      })),
    ];
    expect(tracker.derive(events).range).toEqual({ start: 1_000, end: 66_999 });

    Object.defineProperty(first, "ts", {
      configurable: true,
      get: () => {
        throw new Error("retained event was rescanned");
      },
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

describe("VisualTimingTracker", () => {
  it("uses the numeric preceding context timestamp without fabricating an event", () => {
    const event = input(5000, "carry");
    const previous = new VisualTimingTracker().derive([event], true, 1000);
    expect(previous.get(event)).toBe(1000);
  });
  it("skips child observations and only scans appended events", () => {
    const tracker = new VisualTimingTracker();
    const first = input(1000, "root");
    const events: EventMessage[] = [first, { ...result(2000, 1), source: { agentID: "child" } }];
    tracker.derive(events, false, 0);
    Object.defineProperty(first, "ts", {
      get: () => {
        throw new Error("rescanned prior event");
      },
    });
    const next = input(3000, "next");
    events.push(next);
    expect(tracker.derive(events, false, 0).get(next)).toBe(1000);
  });
  it("drops preceding references on context replacement", () => {
    const tracker = new VisualTimingTracker();
    const prior = input(1000, "old");
    tracker.derive([prior, result(2000, 1)], false, 0);
    const next = input(10000, "new");
    const values = tracker.derive([next], true, 9000);
    expect(values.has(prior)).toBe(false);
    expect(values.get(next)).toBe(9000);
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
