// Tests bounded resource samples and network rates across counter resets.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import { deriveNetworkRates } from "./taskStats";

describe("task stats", () => {
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
