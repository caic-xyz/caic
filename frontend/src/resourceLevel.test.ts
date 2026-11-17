// Tests host and task level thresholds and host-pressure attribution to the busiest tasks.

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
  cpuHigh: true,
  memHigh: true,
};

describe("hostPressure", () => {
  it("reports the busiest fresh task only while the host is at warning or high level", () => {
    const samples = [sample(200, 100), sample(50, 300)];
    expect(hostPressure(health, true, samples)).toEqual({
      cpu: { hostPercent: 95, busiest: 200 },
      memory: { hostPercent: 85, busiest: 300 },
    });
    expect(hostPressure({ ...health, cpuPerc: 632, memUsed: 799 }, true, samples)).toEqual({
      cpu: null,
      memory: null,
    });
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

  it("gives the busiest task the host level and scales the range for the others", () => {
    const high = { hostPercent: 95, busiest: 200 };
    expect(taskLevel(5, 200, high)).toBe("high");
    // 95% × 190/200 = 90.25%: still high.
    expect(taskLevel(5, 190, high)).toBe("high");
    // 95% × 180/200 = 85.5%: warning.
    expect(taskLevel(5, 180, high)).toBe("warning");
    // 95% × 160/200 = 76%: below the host warning threshold.
    expect(taskLevel(5, 160, high)).toBe("normal");
    const warning = { hostPercent: 85, busiest: 200 };
    expect(taskLevel(5, 200, warning)).toBe("warning");
    expect(taskLevel(5, 180, warning)).toBe("normal");
    // A task without known capacity still shows its host contribution.
    expect(taskLevel(null, 200, warning)).toBe("warning");
    // Host pressure never lowers a container-relative level.
    expect(taskLevel(80, 10, warning)).toBe("high");
  });
});
