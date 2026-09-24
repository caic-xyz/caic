// Tests usage dashboard range selection and additive rollup aggregation.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import type { UsageDashboardDay } from "@sdk/types.gen";

import { summarizeUsageDays, usageCacheHitRate, usageDaysForRange } from "./usageDashboard";

function day(value: string, overrides: Partial<UsageDashboardDay> = {}): UsageDashboardDay {
  return {
    day: value,
    tokens: {
      inputTokens: 10,
      cacheWrite5mTokens: 0,
      cacheWrite1hTokens: 0,
      cacheReadTokens: 20,
      outputTokens: 5,
      reasoningTokens: 2,
    },
    turns: 1,
    erroredTurns: 0,
    apiMs: 0,
    wallMs: 0,
    compactions: 0,
    subagentSpawns: 0,
    subagentSpawnsBackground: 0,
    costUSD: 0.1,
    models: [
      {
        model: "model-a",
        tokens: {
          inputTokens: 10,
          cacheWrite5mTokens: 0,
          cacheWrite1hTokens: 0,
          cacheReadTokens: 20,
          outputTokens: 5,
          reasoningTokens: 2,
        },
        turns: 1,
        costUSD: 0.1,
        contextWindow: 100,
      },
    ],
    harnesses: [
      {
        harness: "claude",
        tokens: {
          inputTokens: 10,
          cacheWrite5mTokens: 0,
          cacheWrite1hTokens: 0,
          cacheReadTokens: 20,
          outputTokens: 5,
          reasoningTokens: 2,
        },
        turns: 1,
        costUSD: 0.1,
      },
    ],
    repos: [{ repo: "caic", tasks: 1 }],
    skills: [{ name: "review", count: 1 }],
    tools: [],
    ...overrides,
    toolTimings: overrides.toolTimings ?? [],
  };
}

describe("usageDaysForRange", () => {
  it("uses the latest rollup day instead of the browser clock", () => {
    const days = [day("2026-01-01"), day("2026-01-24"), day("2026-01-25")];

    expect(usageDaysForRange(days, "7").map((entry) => entry.day)).toEqual(["2026-01-24", "2026-01-25"]);
    expect(usageDaysForRange(days, "all")).toEqual(days);
  });
});

describe("summarizeUsageDays", () => {
  it("adds daily counters and labels repository totals as task-days", () => {
    const summary = summarizeUsageDays([
      day("2026-01-24"),
      day("2026-01-25", {
        apiMs: 1250,
        wallMs: 3000,
        compactions: 2,
        erroredTurns: 1,
        tools: [{ name: "Read", count: 3 }],
        toolTimings: [{ name: "Read", count: 2, durationMs: 1500 }],
        costUSD: 0.2,
        repos: [{ repo: "caic", tasks: 2 }],
        skills: [{ name: "review", count: 3 }],
      }),
    ]);

    expect(summary.totalTokens).toBe(70);
    expect(summary.turns).toBe(2);
    expect(summary.apiMs).toBe(1250);
    expect(summary.wallMs).toBe(3000);
    expect(summary.compactions).toBe(2);
    expect(summary.erroredTurns).toBe(1);
    expect(summary.tools).toEqual([{ name: "Read", calls: 3, timedCalls: 2, durationMs: 1500 }]);
    expect(summary.costUSD).toBeCloseTo(0.3);
    expect(summary.repos).toEqual([{ name: "caic", count: 3 }]);
    expect(summary.skills).toEqual([{ name: "review", count: 4 }]);
    expect(summary.models[0]).toMatchObject({ name: "model-a", tokens: 70, turns: 2 });
    expect(usageCacheHitRate(summary.tokens)).toBeCloseTo(2 / 3);
    expect(summary.harnesses[0].cacheHitRate).toBeCloseTo(2 / 3);
  });

  it("weights harness cache hit rate by input tokens across days", () => {
    const first = day("2026-01-24");
    const second = day("2026-01-25", {
      harnesses: [
        {
          ...first.harnesses[0],
          tokens: { ...first.harnesses[0].tokens, inputTokens: 70, cacheReadTokens: 0 },
        },
      ],
    });
    const summary = summarizeUsageDays([first, second]);
    expect(summary.harnesses[0].cacheHitRate).toBeCloseTo(20 / 100);
  });
});
