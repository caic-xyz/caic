// Tests task analytics derivation from canonical event history.

import type { EventMessage } from "@sdk/types.gen";
import { describe, expect, it } from "vitest";

import { deriveNetworkRates, deriveToolTimingSummaries, IncrementalToolTimingTracker } from "./taskStats";

function toolUse(id: string, name: string, ts: number): EventMessage {
  return { kind: "toolUse", ts, toolUse: { toolUseID: id, name, input: {} } };
}

function toolResult(id: string, ts: number, duration: number): EventMessage {
  return { kind: "toolResult", ts, toolResult: { toolUseID: id, duration } };
}

describe("task stats", () => {
  it("aggregates completed tool time by tool name", () => {
    const events = [
      toolUse("read-1", "Read", 1_000),
      toolResult("read-1", 1_250, 0),
      toolUse("bash-1", "Bash", 2_000),
      toolResult("bash-1", 9_000, 4),
      toolUse("read-2", "Read", 10_000),
      toolResult("read-2", 10_750, 0),
      toolUse("pending", "Write", 11_000),
    ];

    expect(deriveToolTimingSummaries(events)).toEqual([
      { name: "Bash", calls: 1, durationMs: 4_000 },
      { name: "Read", calls: 2, durationMs: 1_000 },
    ]);
  });

  it("incrementally extends summaries and resets when history is replaced", () => {
    const tracker = new IncrementalToolTimingTracker();
    const first = [toolUse("read", "Read", 1_000), toolResult("read", 2_000, 0)];
    expect(tracker.derive(first)).toEqual([{ name: "Read", calls: 1, durationMs: 1_000 }]);

    const extended = [...first, toolUse("bash", "Bash", 3_000), toolResult("bash", 4_000, 0.5)];
    expect(tracker.derive(extended)).toEqual([
      { name: "Read", calls: 1, durationMs: 1_000 },
      { name: "Bash", calls: 1, durationMs: 500 },
    ]);
    const unchanged = tracker.derive(extended);
    expect(tracker.derive([...extended, { kind: "text", ts: 5_000, text: { text: "done" } }])).toBe(unchanged);
    expect(tracker.derive([toolUse("write", "Write", 5_000), toolResult("write", 5_250, 0)]))
      .toEqual([{ name: "Write", calls: 1, durationMs: 250 }]);
  });

  it("derives network throughput without spanning counter resets", () => {
    const stats = [
      { ts: 1_000, cpuPerc: 0, memUsed: 0, memLimit: 0, memPerc: 0, netRx: 1_000, netTx: 500, blockRead: 0, blockWrite: 0, diskUsed: 0 },
      { ts: 3_000, cpuPerc: 0, memUsed: 0, memLimit: 0, memPerc: 0, netRx: 3_000, netTx: 1_500, blockRead: 0, blockWrite: 0, diskUsed: 0 },
      { ts: 4_000, cpuPerc: 0, memUsed: 0, memLimit: 0, memPerc: 0, netRx: 100, netTx: 50, blockRead: 0, blockWrite: 0, diskUsed: 0 },
    ];

    expect(deriveNetworkRates(stats)).toEqual([
      { ts: 1_000, rxBytesPerSecond: null, txBytesPerSecond: null },
      { ts: 3_000, rxBytesPerSecond: 1_000, txBytesPerSecond: 500 },
      { ts: 4_000, rxBytesPerSecond: null, txBytesPerSecond: null },
    ]);
  });
});
