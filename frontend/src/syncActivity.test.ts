// Tests folding push records into one anchored entry per attempt, live appends, and reload rebuilds.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import type { EventMessage, EventSyncActivity } from "@sdk/types.gen";

import { SyncActivityTracker } from "./syncActivity";

function record(ts: number, syncActivity: EventSyncActivity): EventMessage {
  return { kind: "syncActivity", ts, syncActivity };
}

describe("SyncActivityTracker", () => {
  it("folds the records of one attempt into one entry", () => {
    const entries = new SyncActivityTracker().derive([
      record(10, { syncID: "a", phase: "start", target: "origin", branch: "feature" }),
      record(11, { syncID: "a", phase: "output", output: "hook one\n" }),
      record(12, { syncID: "a", phase: "output", output: "hook two\n" }),
      record(13, { syncID: "a", phase: "end", status: "failed", error: "rejected", durationMS: 3000 }),
    ]);
    expect(entries).toEqual([
      {
        id: "a",
        branch: "feature",
        target: "origin",
        output: "hook one\nhook two\n",
        status: "failed",
        error: "rejected",
        durationMs: 3000,
        anchorTs: 10,
      },
    ]);
  });

  it("keeps concurrent attempts separate and an unfinished one running", () => {
    const entries = new SyncActivityTracker().derive([
      record(1, { syncID: "a", phase: "start", branch: "one" }),
      record(2, { syncID: "b", phase: "start", branch: "two" }),
      record(3, { syncID: "a", phase: "end", status: "pushed", durationMS: 1 }),
    ]);
    expect(entries.map((entry) => [entry.id, entry.status])).toEqual([
      ["a", "pushed"],
      ["b", null],
    ]);
  });

  it("changes only the entry a new record touches", () => {
    const tracker = new SyncActivityTracker();
    const messages = [
      record(1, { syncID: "a", phase: "start" }),
      record(2, { syncID: "a", phase: "end", status: "pushed" }),
      record(3, { syncID: "b", phase: "start" }),
    ];
    const before = tracker.derive(messages);
    expect(tracker.derive(messages)).toBe(before);
    const after = tracker.derive([...messages, record(4, { syncID: "b", phase: "output", output: "x" })]);
    expect(after[0]).toBe(before[0]);
    expect(after[1]?.output).toBe("x");
  });

  it("rebuilds when the history is replaced", () => {
    const tracker = new SyncActivityTracker();
    tracker.derive([record(1, { syncID: "a", phase: "start" })]);
    const entries = tracker.derive([record(5, { syncID: "b", phase: "start" })]);
    expect(entries.map((entry) => entry.id)).toEqual(["b"]);
  });
});
