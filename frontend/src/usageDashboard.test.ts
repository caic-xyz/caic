// Tests usage dashboard range selection and additive rollup aggregation.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import type { UsageDashboardDay } from "@sdk/types.gen";

import {
  drilldownEqual,
  drilldownFromHash,
  hashFromDrilldown,
  summarizeUsageDays,
  usageCacheHitRate,
  usageDailySeries,
  usageDaysForRange,
} from "./usageDashboard";

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
        erroredTurns: 0,
        apiMs: 0,
        wallMs: 0,
        compactions: 0,
        subagentSpawns: 0,
        tools: [],
        toolTimings: [],
        skills: [],
        repos: [],
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
        erroredTurns: 0,
        apiMs: 0,
        wallMs: 0,
        compactions: 0,
        subagentSpawns: 0,
        tools: [],
        toolTimings: [],
        skills: [],
        repos: [],
        models: [],
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

describe("summarizeUsageDays drill-down", () => {
  // One day whose model and harness entries carry the full drill-down fold.
  function drillDay(value: string, overrides: Partial<UsageDashboardDay> = {}): UsageDashboardDay {
    const tokens = {
      inputTokens: 40,
      cacheWrite5mTokens: 0,
      cacheWrite1hTokens: 0,
      cacheReadTokens: 40,
      outputTokens: 20,
      reasoningTokens: 0,
    };
    const pair: UsageDashboardDay["models"][number] = {
      model: "model-a",
      tokens,
      turns: 2,
      erroredTurns: 1,
      apiMs: 500,
      wallMs: 900,
      compactions: 1,
      subagentSpawns: 3,
      costUSD: 0.2,
      contextWindow: 200,
      tools: [{ name: "Read", count: 4 }],
      toolTimings: [{ name: "Read", count: 2, durationMs: 1200 }],
      skills: [{ name: "review", count: 1 }],
      repos: [{ repo: "caic", tasks: 1 }],
    };
    return day(value, {
      tokens,
      turns: 9,
      costUSD: 0.9,
      models: [
        pair,
        {
          model: "other-model",
          tokens,
          turns: 7,
          erroredTurns: 0,
          apiMs: 0,
          wallMs: 0,
          compactions: 0,
          subagentSpawns: 0,
          costUSD: 0.7,
          contextWindow: 100,
          tools: [],
          toolTimings: [],
          skills: [],
          repos: [],
        },
      ],
      harnesses: [
        {
          harness: "claude",
          tokens,
          turns: 2,
          erroredTurns: 1,
          apiMs: 500,
          wallMs: 900,
          compactions: 1,
          subagentSpawns: 3,
          costUSD: 0.2,
          tools: [{ name: "Read", count: 4 }],
          toolTimings: [{ name: "Read", count: 2, durationMs: 1200 }],
          skills: [{ name: "review", count: 1 }],
          repos: [{ repo: "caic", tasks: 1 }],
          models: [pair],
        },
      ],
      ...overrides,
    });
  }

  it("filters every panel to the selected model", () => {
    const summary = summarizeUsageDays([drillDay("2026-01-24")], { model: "model-a" });

    expect(summary.totalTokens).toBe(100);
    expect(summary.turns).toBe(2);
    expect(summary.erroredTurns).toBe(1);
    expect(summary.apiMs).toBe(500);
    expect(summary.wallMs).toBe(900);
    expect(summary.compactions).toBe(1);
    expect(summary.subagentSpawns).toBe(3);
    expect(summary.costUSD).toBeCloseTo(0.2);
    expect(summary.tools).toEqual([{ name: "Read", calls: 4, timedCalls: 2, durationMs: 1200 }]);
    expect(summary.skills).toEqual([{ name: "review", count: 1 }]);
    expect(summary.repos).toEqual([{ name: "caic", count: 1 }]);
    expect(summary.models.map((entry) => entry.name)).toEqual(["model-a"]);
    // The harness-by-model cross product ranks the harnesses that used the
    // model with that model's numbers instead of the harness totals.
    expect(summary.harnesses.map((entry) => entry.name)).toEqual(["claude"]);
    expect(summary.harnesses[0].tokens).toBe(100);
  });

  it("filters every panel to the selected harness and keeps the model list", () => {
    const summary = summarizeUsageDays([drillDay("2026-01-24")], { harness: "claude" });

    expect(summary.totalTokens).toBe(100);
    expect(summary.turns).toBe(2);
    expect(summary.skills).toEqual([{ name: "review", count: 1 }]);
    expect(summary.harnesses.map((entry) => entry.name)).toEqual(["claude"]);
    expect(summary.harnesses[0].tokens).toBe(100);
    // Models used under the harness stay visible through the cross product.
    expect(summary.models.map((entry) => entry.name)).toEqual(["model-a"]);
    expect(summary.models[0].tokens).toBe(100);
  });

  it("filters every panel to a harness-by-model pair", () => {
    const summary = summarizeUsageDays([drillDay("2026-01-24")], { harness: "claude", model: "model-a" });

    expect(summary.turns).toBe(2);
    expect(summary.skills).toEqual([{ name: "review", count: 1 }]);
    expect(summary.models.map((entry) => entry.name)).toEqual(["model-a"]);
    expect(summary.harnesses.map((entry) => entry.name)).toEqual(["claude"]);
  });

  it("drops days without the selected subject", () => {
    const summary = summarizeUsageDays([day("2026-01-23", { models: [], harnesses: [] }), drillDay("2026-01-24")], {
      model: "model-a",
    });

    expect(summary.turns).toBe(2);
    expect(summary.models.map((entry) => entry.name)).toEqual(["model-a"]);
    expect(
      usageDailySeries([day("2026-01-23", { models: [], harnesses: [] }), drillDay("2026-01-24")], {
        model: "model-a",
      }),
    ).toEqual([{ day: "2026-01-24", tokens: 100, costUSD: 0.2, inProgress: false }]);
  });

  it("sums an unknown subject to empty counters", () => {
    const summary = summarizeUsageDays([drillDay("2026-01-24")], { model: "missing" });

    expect(summary.totalTokens).toBe(0);
    expect(summary.models).toEqual([]);
    expect(summary.skills).toEqual([]);
  });

  it("resolves daily chart points through the drill-down", () => {
    const days = [drillDay("2026-01-24"), drillDay("2026-01-25")];

    expect(usageDailySeries(days)).toEqual([
      { day: "2026-01-24", tokens: 100, costUSD: 0.9, inProgress: false },
      { day: "2026-01-25", tokens: 100, costUSD: 0.9, inProgress: false },
    ]);
    expect(usageDailySeries(days, { harness: "claude" })).toEqual([
      { day: "2026-01-24", tokens: 100, costUSD: 0.2, inProgress: false },
      { day: "2026-01-25", tokens: 100, costUSD: 0.2, inProgress: false },
    ]);
  });

  it("round-trips the drill-down through the location hash", () => {
    expect(hashFromDrilldown(null)).toBe("");
    expect(hashFromDrilldown({ model: "model-a" })).toBe("#model=model-a");
    expect(hashFromDrilldown({ harness: "claude" })).toBe("#harness=claude");
    expect(hashFromDrilldown({ harness: "claude", model: "model-a" })).toBe("#harness=claude&model=model-a");

    expect(drilldownFromHash("")).toBe(null);
    expect(drilldownFromHash("#model=model-a")).toEqual({ model: "model-a" });
    expect(drilldownFromHash("#harness=claude")).toEqual({ harness: "claude" });
    expect(drilldownFromHash("#harness=claude&model=model-a")).toEqual({ harness: "claude", model: "model-a" });
    // Field order in the fragment does not matter.
    expect(drilldownFromHash("#model=model-a&harness=claude")).toEqual({ harness: "claude", model: "model-a" });

    expect(drilldownEqual({ model: "a" }, { model: "a" })).toBe(true);
    expect(drilldownEqual({ model: "a" }, { model: "a", harness: "h" })).toBe(false);
    expect(drilldownEqual(null, null)).toBe(true);
  });
});
