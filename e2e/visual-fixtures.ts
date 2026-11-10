// Deterministic API fixtures for documentation resource charts and usage trends.
import { reportedUsageCost } from "../frontend/tests/usage";
import { ok } from "node:assert/strict";
import type {
  EventStats,
  ISOTimestamp,
  ServerHealth,
  SystemStatsResp,
  UsageDashboardResp,
  UsageDashboardDay,
} from "../sdk/caic/ts/v1/types.gen";

export const visualEpoch = Date.UTC(2026, 8, 2, 12);

/** Milliseconds between successive raw record positions in a capture. */
export const visualRecordStrideMs = 150;

/** Capture time of a raw record position, offset from the task start. */
export function visualPositionTime(startMs: number, position: { recordID: number; index: number }): number {
  return startMs + (position.recordID - 2) * visualRecordStrideMs + position.index;
}

/** Server health for the header level badge. The real server samples the host,
 *  so a capture would otherwise record whichever CPU level the machine had. */
export function serverHealth(): ServerHealth {
  return {
    time: new Date(visualEpoch).toISOString() as ISOTimestamp,
    cpuPerc: 34,
    cpuCount: 8,
    memUsed: 568 * 1024 * 1024,
    memTotal: 2 * 1024 * 1024 * 1024,
    cpuHigh: false,
    memHigh: false,
  };
}

export function resourceHistory(): EventStats[] {
  return Array.from({ length: 24 }, (_, index) => {
    const ts = visualEpoch + index * 5000;
    const cpu = [12, 18, 38, 52, 66, 71, 58, 44, 27, 16, 22, 34][index % 12];
    ok(cpu !== undefined);
    const memory = (384 + index * 8) * 1024 * 1024;
    return {
      ts,
      cpuCeiling: null,
      cpuQuota: null,
      runtimeCPUs: null,
      cpuPerc: cpu,
      memUsed: memory,
      memLimit: 2 * 1024 * 1024 * 1024,
      memPerc: (memory / (2 * 1024 * 1024 * 1024)) * 100,
      netRx: index * 12000,
      netTx: index * 8000,
      blockRead: index * 16000,
      blockWrite: index * 10000,
      diskUsed: 842 * 1024 * 1024,
    };
  });
}

/** Server statistics for the System page. The real endpoint samples the host,
 *  so a capture would record the machine that rendered it. */
export function systemStats(): SystemStatsResp {
  return {
    samples: resourceHistory().map((sample) => ({
      time: new Date(sample.ts).toISOString() as ISOTimestamp,
      cpus: Array.from({ length: 8 }, (_, index) => ({ id: `cpu${index}`, usagePerc: sample.cpuPerc / 8 })),
      memUsed: sample.memUsed,
      memTotal: sample.memLimit,
      memPerc: sample.memPerc,
      netRx: sample.netRx,
      netTx: sample.netTx,
      diskUsed: sample.diskUsed,
      diskTotal: 20 * 1024 * 1024 * 1024,
    })),
  };
}

export function usageHistory(): UsageDashboardResp {
  const days: UsageDashboardDay[] = Array.from({ length: 14 }, (_, index) => {
    const count = 4 + (index % 5);
    const tokens = {
      inputTokens: count * 2800,
      cacheWrite5mTokens: count * 8400,
      cacheWrite1hTokens: 0,
      cacheReadTokens: count * 42000,
      outputTokens: count * 6200,
      reasoningTokens: count * 1800,
    };
    const tools = [
      { name: "Read", count: count * 12 },
      { name: "Edit", count: count * 6 },
      { name: "Bash", count: count * 4 },
    ];
    const toolTimings = [
      {
        name: "Read",
        count: count * 12,
        durationMs: count * 1800,
        buckets: [
          { upperMs: 200, count: count * 9 },
          { upperMs: 500, count: count * 3 },
        ],
      },
      {
        name: "Edit",
        count: count * 6,
        durationMs: count * 1200,
        buckets: [
          { upperMs: 200, count: count * 4 },
          { upperMs: 500, count: count * 2 },
        ],
      },
      {
        name: "Bash",
        count: count * 4,
        durationMs: count * 24000,
        buckets: [
          { upperMs: 5000, count: count * 3 },
          { upperMs: 10000, count },
        ],
      },
    ];
    const repos = [
      { repo: "caic", tasks: count },
      { repo: "gomode", tasks: Math.ceil(count / 2) },
    ];
    const base = {
      tokens,
      turns: count * 3,
      erroredTurns: 0,
      apiMs: count * 48000,
      wallMs: count * 96000,
      compactions: index % 3,
      subagentSpawns: count,
      costUSD: count * 0.37,
      cost: reportedUsageCost(count * 0.37),
      tools,
      toolTimings,
      skills: [{ name: "code-review", count }],
      repos,
    };
    const models = [{ ...base, model: "opus-5.5", contextWindow: 200000 }];
    return {
      ...base,
      day: new Date(Date.UTC(2026, 7, 20 + index)).toISOString().slice(0, 10),
      subagentSpawnsBackground: 0,
      models,
      harnesses: [{ ...base, harness: "claude", models }],
    };
  });
  const first = days[0];
  ok(first);
  return { dataSince: first.day, days };
}
