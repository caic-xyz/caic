// Tests host and task level thresholds and host-pressure attribution to the contributing tasks.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import type { EventStats, ServerHealth } from "@sdk/types.gen";

import { hostPressure, taskLevel } from "./resourceLevel";

function sample(cpuPerc: number, memUsed: number): EventStats {
  return {
    ts: 0,
    cpuPerc,
    cpuCeiling: 16,
    cpuQuota: 0,
    runtimeCPUs: 16,
    memUsed,
    memLimit: 1000,
    memPerc: 0,
    netRx: 0,
    netTx: 0,
    blockRead: 0,
    blockWrite: 0,
    diskUsed: -1,
  };
}

const health: ServerHealth = {
  cpuPerc: 760,
  cpuCount: 8,
  memUsed: 850,
  memTotal: 1000,
  cpuLevel: "high",
  memLevel: "warning",
  cpuHigh: true,
  memHigh: true,
};

const GiB = 1024 ** 3;

describe("hostPressure", () => {
  it("reports contributors only while the host is at warning or high level", () => {
    const samples = [sample(200, 100), sample(50, 300)];
    expect(hostPressure(health, true, samples)).toEqual({
      cpu: { level: "high", busiest: 200, floor: 50 },
      memory: { level: "warning", busiest: 300, floor: 100 },
    });
    expect(hostPressure({ ...health, cpuLevel: "normal", memLevel: "normal" }, true, samples)).toEqual({
      cpu: null,
      memory: null,
    });
  });

  it("extends contributors through comparable heavy tasks and stops at the small tail", () => {
    const host = { ...health, memUsed: 29 * GiB, memTotal: 32 * GiB, memLevel: "high" } as const;
    const memory = (gib: number[]) =>
      hostPressure(
        host,
        true,
        gib.map((g) => sample(0, g * GiB)),
      ).memory;
    // The runner-up needs only 5% of 32 GiB; 2.1 GiB is also at least half of 2.5 GiB; 0.2 GiB is neither.
    expect(memory([0.2, 12, 2.1, 0.15, 2.5])).toEqual({ level: "high", busiest: 12 * GiB, floor: 2.1 * GiB });
    // Past the runner-up, a task under half of the previous contributor ends the chain.
    expect(memory([12, 4, 1.9])).toEqual({ level: "high", busiest: 12 * GiB, floor: 4 * GiB });
    // A comparable runner-up below 5% of capacity is noise.
    expect(memory([1.5, 1])).toEqual({ level: "high", busiest: 1.5 * GiB, floor: 1.5 * GiB });
  });

  it("drops pressure when the host reading or task usage is unavailable", () => {
    const none = { cpu: null, memory: null };
    const samples = [sample(200, 100)];
    expect(hostPressure(null, true, samples)).toEqual(none);
    expect(hostPressure(health, false, samples)).toEqual(none);
    expect(hostPressure({ ...health, error: "sampling failed" }, true, samples)).toEqual(none);
    expect(hostPressure({ ...health, cpuCount: 0, memTotal: 0 }, true, samples)).toEqual(none);
    expect(hostPressure(health, true, [])).toEqual(none);
    expect(hostPressure(health, true, [sample(0, 0)])).toEqual(none);
  });
});

describe("taskLevel", () => {
  it("uses container thresholds without host pressure", () => {
    expect(taskLevel(null, 100, null)).toBe("unavailable");
    expect(taskLevel(49.9, 100, null)).toBe("normal");
    expect(taskLevel(50, 100, null)).toBe("warning");
    expect(taskLevel(75, 100, null)).toBe("high");
  });

  it("gives the busiest task the host level and the other contributors warning", () => {
    const high = { level: "high", busiest: 200, floor: 120 } as const;
    expect(taskLevel(5, 200, high)).toBe("high");
    expect(taskLevel(5, 150, high)).toBe("warning");
    expect(taskLevel(5, 120, high)).toBe("warning");
    expect(taskLevel(5, 119, high)).toBe("normal");
    const warning = { level: "warning", busiest: 200, floor: 200 } as const;
    expect(taskLevel(5, 200, warning)).toBe("warning");
    expect(taskLevel(5, 180, warning)).toBe("normal");
    // A task without known capacity still shows its host contribution.
    expect(taskLevel(null, 200, warning)).toBe("warning");
    // Host pressure never lowers a container-relative level.
    expect(taskLevel(80, 10, warning)).toBe("high");
  });
});
