// Tests usage dashboard range selection and additive rollup aggregation.

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import type { UsageDashboardDay } from "@sdk/types.gen";

import { summarizeUsageDays, usageCacheHitRate, usageDailySeries, usageDaysForRange } from "./usageDashboard";

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
        toolTimings: [
          {
            name: "Read",
            count: 2,
            durationMs: 1500,
            buckets: [
              { upperMs: 500, count: 1 },
              { upperMs: 1_000, count: 1 },
            ],
          },
        ],
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
    expect(summary.tools).toEqual([{ name: "Read", calls: 3, timedCalls: 2, durationMs: 1500, medianMs: 500 }]);
    expect(summary.costUSD).toBeCloseTo(0.3);
    expect(summary.repos).toEqual([{ name: "caic", count: 3 }]);
    expect(summary.skills).toEqual([{ name: "review", count: 4 }]);
    expect(summary.models[0]).toMatchObject({ name: "model-a", tokens: 70, turns: 2 });
    expect(usageCacheHitRate(summary.tokens)).toBeCloseTo(2 / 3);
    assert.ok(summary.harnesses[0] !== undefined);
    expect(summary.harnesses[0].cacheHitRate).toBeCloseTo(2 / 3);
  });

  it("estimates the tool median from merged duration buckets", () => {
    const summary = summarizeUsageDays([
      day("2026-01-24", {
        toolTimings: [
          {
            name: "Read",
            count: 4,
            durationMs: 400,
            buckets: [
              { upperMs: 100, count: 3 },
              { upperMs: 5_000, count: 1 },
            ],
          },
        ],
      }),
      day("2026-01-25", {
        toolTimings: [{ name: "Read", count: 1, durationMs: 5_000, buckets: [{ upperMs: 5_000, count: 1 }] }],
      }),
    ]);

    // The mean sits at 1.08s, pulled up by one slow call; the median sits in the
    // first bucket, whose interpolation tops out at its 100ms bound.
    expect(summary.tools[0]).toMatchObject({ name: "Read", timedCalls: 5, durationMs: 5_400, medianMs: 100 });
  });

  it("leaves the median unknown when no call kept a distribution", () => {
    const summary = summarizeUsageDays([
      day("2026-01-24", { toolTimings: [{ name: "Read", count: 2, durationMs: 1_000, buckets: [] }] }),
    ]);

    expect(summary.tools[0]).toMatchObject({ name: "Read", timedCalls: 2, medianMs: null });
  });

  it("weights harness cache hit rate by input tokens across days", () => {
    const first = day("2026-01-24");
    assert.ok(first.harnesses[0] !== undefined);
    const second = day("2026-01-25", {
      harnesses: [
        {
          ...first.harnesses[0],
          tokens: { ...first.harnesses[0].tokens, inputTokens: 70, cacheReadTokens: 0 },
        },
      ],
    });
    const summary = summarizeUsageDays([first, second]);
    assert.ok(summary.harnesses[0] !== undefined);
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
      toolTimings: [{ name: "Read", count: 2, durationMs: 1200, buckets: [] }],
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
          toolTimings: [{ name: "Read", count: 2, durationMs: 1200, buckets: [] }],
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
    expect(summary.tools).toEqual([{ name: "Read", calls: 4, timedCalls: 2, durationMs: 1200, medianMs: null }]);
    expect(summary.skills).toEqual([{ name: "review", count: 1 }]);
    expect(summary.repos).toEqual([{ name: "caic", count: 1 }]);
    expect(summary.models.map((entry) => entry.name)).toEqual(["model-a"]);
    // The harness-by-model cross product ranks the harnesses that used the
    // model with that model's numbers instead of the harness totals.
    expect(summary.harnesses.map((entry) => entry.name)).toEqual(["claude"]);
    assert.ok(summary.harnesses[0] !== undefined);
    expect(summary.harnesses[0].tokens).toBe(100);
  });

  it("filters every panel to the selected harness and keeps the model list", () => {
    const summary = summarizeUsageDays([drillDay("2026-01-24")], { harness: "claude" });

    expect(summary.totalTokens).toBe(100);
    expect(summary.turns).toBe(2);
    expect(summary.skills).toEqual([{ name: "review", count: 1 }]);
    expect(summary.harnesses.map((entry) => entry.name)).toEqual(["claude"]);
    assert.ok(summary.harnesses[0] !== undefined);
    expect(summary.harnesses[0].tokens).toBe(100);
    // Models used under the harness stay visible through the cross product.
    expect(summary.models.map((entry) => entry.name)).toEqual(["model-a"]);
    assert.ok(summary.models[0] !== undefined);
    expect(summary.models[0].tokens).toBe(100);
  });

  it("filters every panel to a harness-by-model pair", () => {
    const summary = summarizeUsageDays([drillDay("2026-01-24")], { harness: "claude", model: "model-a" });

    expect(summary.turns).toBe(2);
    expect(summary.skills).toEqual([{ name: "review", count: 1 }]);
    expect(summary.models.map((entry) => entry.name)).toEqual(["model-a"]);
    expect(summary.harnesses.map((entry) => entry.name)).toEqual(["claude"]);
  });

  it("attributes compactions to the drilled model and harness", () => {
    const summary = summarizeUsageDays([drillDay("2026-01-24")], { model: "model-a" });

    expect(summary.compactions).toBe(1);
    expect(summary.models[0]).toMatchObject({ name: "model-a", compactions: 1 });
    expect(summary.harnesses[0]).toMatchObject({ name: "claude", compactions: 1 });
  });

  it("drops days without the selected subject from the summary", () => {
    const summary = summarizeUsageDays([day("2026-01-23", { models: [], harnesses: [] }), drillDay("2026-01-24")], {
      model: "model-a",
    });

    expect(summary.turns).toBe(2);
    expect(summary.models.map((entry) => entry.name)).toEqual(["model-a"]);
  });

  it("sums an unknown subject to empty counters", () => {
    const summary = summarizeUsageDays([drillDay("2026-01-24")], { model: "missing" });

    expect(summary.totalTokens).toBe(0);
    expect(summary.models).toEqual([]);
    expect(summary.skills).toEqual([]);
  });

  it("resolves daily chart points through the drill-down", () => {
    const days = [drillDay("2026-01-24"), drillDay("2026-01-25")];

    expect(usageDailySeries(days, "all", null)).toEqual([
      { day: "2026-01-24", tokens: 100, costUSD: 0.9, inProgress: false },
      { day: "2026-01-25", tokens: 100, costUSD: 0.9, inProgress: false },
    ]);
    expect(usageDailySeries(days, "all", { harness: "claude" })).toEqual([
      { day: "2026-01-24", tokens: 100, costUSD: 0.2, inProgress: false },
      { day: "2026-01-25", tokens: 100, costUSD: 0.2, inProgress: false },
    ]);
  });

  it("zeroes a recorded day without the selected subject", () => {
    const days = [drillDay("2026-01-24"), day("2026-01-25", { models: [], harnesses: [] }), drillDay("2026-01-26")];

    expect(usageDailySeries(days, "all", { model: "model-a" }).map((point) => [point.day, point.tokens])).toEqual([
      ["2026-01-24", 100],
      ["2026-01-25", 0],
      ["2026-01-26", 100],
    ]);
  });

  it("keeps one chart point per day of the range, zeroing the missing ones", () => {
    const days = [day("2026-01-01"), day("2026-01-04"), day("2026-01-10")];

    expect(usageDailySeries(days, "all", null).map((point) => [point.day, point.tokens])).toEqual([
      ["2026-01-01", 35],
      ["2026-01-02", 0],
      ["2026-01-03", 0],
      ["2026-01-04", 35],
      ["2026-01-05", 0],
      ["2026-01-06", 0],
      ["2026-01-07", 0],
      ["2026-01-08", 0],
      ["2026-01-09", 0],
      ["2026-01-10", 35],
    ]);
    // The range window counts back from the latest recorded day and never
    // reaches before the first one.
    expect(usageDailySeries(days, "7", null).map((point) => point.day)).toEqual([
      "2026-01-04",
      "2026-01-05",
      "2026-01-06",
      "2026-01-07",
      "2026-01-08",
      "2026-01-09",
      "2026-01-10",
    ]);
    expect(usageDailySeries(days.slice(2), "7", null).map((point) => point.day)).toEqual(["2026-01-10"]);
  });
});

describe("arbitrary cache-write lifetimes", () => {
  it("counts generic buckets once while accepting old dashboard fields", () => {
    const first = day("2026-01-24");
    first.tokens.cacheWrites = [
      { ttlSeconds: 0, tokens: 3 },
      { ttlSeconds: 7200, tokens: 40 },
    ];
    first.tokens.cacheWrite5mTokens = 3;
    const second = day("2026-01-25");
    second.tokens.cacheWrite1hTokens = 7;
    const summary = summarizeUsageDays([first, second]);
    expect(summary.totalTokens).toBe(120);
    expect(usageCacheHitRate(summary.tokens)).toBeCloseTo(40 / 110);
    expect(summary.tokens.cacheWrites?.find((b) => b.ttlSeconds === 7200)?.tokens).toBe(40);
    expect(first.tokens.cacheWrites[1]?.tokens).toBe(40);
  });
});
