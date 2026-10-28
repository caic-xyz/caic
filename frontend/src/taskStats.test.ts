// Tests task analytics derivation from canonical event history.

import { describe, it } from "node:test";
import type { EventMessage } from "@sdk/types.gen";
import { expect } from "@tests/expect";

import { deriveNetworkRates, ToolTimingTracker } from "./taskStats";

function toolUse(id: string, name: string, ts: number): EventMessage {
  return { kind: "toolUse", ts, toolUse: { toolUseID: id, name, input: {} } };
}

function toolResult(id: string, ts: number, duration: number): EventMessage {
  return { kind: "toolResult", ts, toolResult: { toolUseID: id, duration } };
}

function deriveToolTimingSummaries(events: readonly EventMessage[]) {
  const tracker = new ToolTimingTracker();
  for (const event of events) tracker.append(event);
  return tracker.snapshot();
}

describe("task stats", () => {
  it("includes same-ID tools from multiple agents without mixing their start times", () => {
    const root = { threadID: "root", turnID: "turn" };
    const child = { threadID: "child", turnID: "turn", agentID: "native:child" };
    expect(
      deriveToolTimingSummaries([
        { ...toolUse("same", "Root", 1000), source: root },
        { ...toolUse("same", "Child", 2000), source: child },
        { ...toolResult("same", 3000, 0), source: root },
        { ...toolResult("same", 4000, 0), source: child },
      ]),
    ).toEqual([
      { name: "Child", calls: 1, durationMs: 2000 },
      { name: "Root", calls: 1, durationMs: 2000 },
    ]);
  });
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

  it("incrementally appends scalar timing without retaining messages", () => {
    const tracker = new ToolTimingTracker();
    tracker.append(toolUse("read", "Read", 1000));
    tracker.append(toolResult("read", 2000, 0));
    expect(tracker.snapshot()).toEqual([{ name: "Read", calls: 1, durationMs: 1000 }]);
    tracker.append(toolUse("bash", "Bash", 3000));
    tracker.append(toolResult("bash", 4000, 0.5));
    expect(tracker.snapshot()).toEqual([
      { name: "Read", calls: 1, durationMs: 1000 },
      { name: "Bash", calls: 1, durationMs: 500 },
    ]);
    const unchanged = tracker.snapshot();
    tracker.append({ kind: "text", ts: 5000, text: { text: "discarded payload" } });
    expect(tracker.snapshot()).toBe(unchanged);
    expect(JSON.stringify(tracker)).not.toContain("discarded payload");
  });

  it("derives network throughput without spanning counter resets", () => {
    const stats = [
      {
        ts: 1_000,
        cpuPerc: 0,
        memUsed: 0,
        memLimit: 0,
        memPerc: 0,
        netRx: 1_000,
        netTx: 500,
        blockRead: 0,
        blockWrite: 0,
        diskUsed: 0,
      },
      {
        ts: 3_000,
        cpuPerc: 0,
        memUsed: 0,
        memLimit: 0,
        memPerc: 0,
        netRx: 3_000,
        netTx: 1_500,
        blockRead: 0,
        blockWrite: 0,
        diskUsed: 0,
      },
      {
        ts: 4_000,
        cpuPerc: 0,
        memUsed: 0,
        memLimit: 0,
        memPerc: 0,
        netRx: 100,
        netTx: 50,
        blockRead: 0,
        blockWrite: 0,
        diskUsed: 0,
      },
    ];

    expect(deriveNetworkRates(stats)).toEqual([
      { ts: 1_000, rxBytesPerSecond: null, txBytesPerSecond: null },
      { ts: 3_000, rxBytesPerSecond: 1_000, txBytesPerSecond: 500 },
      { ts: 4_000, rxBytesPerSecond: null, txBytesPerSecond: null },
    ]);
  });
});
