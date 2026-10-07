// Deterministic API fixtures for documentation resource charts and usage trends.
import type { EventMessage, UsageDashboardResp, UsageDashboardDay } from "../sdk/caic/ts/v1/types.gen";

export const visualEpoch = Date.UTC(2026, 8, 2, 12);

export function resourceHistory(): EventMessage[] {
  return Array.from({ length: 24 }, (_, index) => {
    const ts = visualEpoch + index * 5000;
    const cpu = [12, 18, 38, 52, 66, 71, 58, 44, 27, 16, 22, 34][index % 12];
    const memory = (384 + index * 8) * 1024 * 1024;
    return {
      kind: "stats",
      ts,
      stats: {
        ts,
        cpuPerc: cpu,
        memUsed: memory,
        memLimit: 2 * 1024 * 1024 * 1024,
        memPerc: (memory / (2 * 1024 * 1024 * 1024)) * 100,
        netRx: index * 12000,
        netTx: index * 8000,
        blockRead: index * 16000,
        blockWrite: index * 10000,
        diskUsed: 842 * 1024 * 1024,
      },
    };
  });
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
      { name: "Read", count: count * 12, durationMs: count * 1800 },
      { name: "Edit", count: count * 6, durationMs: count * 1200 },
      { name: "Bash", count: count * 4, durationMs: count * 24000 },
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
  return { dataSince: days[0].day, days };
}
