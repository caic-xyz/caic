// Tests for the daily usage trend charts and their text alternative.

import { describe, it } from "node:test";
import { render, within } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { expect } from "@tests/expect";

import type { UsageDashboardDay } from "@sdk/types.gen";

import UsageCharts from "./UsageCharts";

function day(dayKey: string, tokens: number, costUSD: number): UsageDashboardDay {
  return {
    apiMs: 0,
    compactions: 0,
    costUSD,
    day: dayKey,
    erroredTurns: 0,
    harnesses: [],
    models: [],
    repos: [],
    skills: [],
    subagentSpawns: 0,
    subagentSpawnsBackground: 0,
    toolTimings: [],
    tools: [],
    tokens: {
      cacheReadTokens: tokens - Math.floor(tokens / 4),
      cacheWrite1hTokens: 0,
      cacheWrite5mTokens: 0,
      inputTokens: Math.floor(tokens / 4),
      outputTokens: 0,
      reasoningTokens: 0,
    },
    turns: 0,
    wallMs: 0,
  };
}

function utcDay(offsetDays: number): string {
  const date = new Date();
  date.setUTCDate(date.getUTCDate() + offsetDays);
  return date.toISOString().slice(0, 10);
}

describe("UsageCharts", () => {
  it("totals every token category for each day", async () => {
    const user = userEvent.setup();
    const { findByTestId } = render(() => <UsageCharts days={[day(utcDay(-1), 15_000, 0.5)]} />);
    const charts = await findByTestId("usage-charts");

    await user.click(within(charts).getByText("Daily totals (1)"));

    const table = within(charts).getByRole("table");
    expect(table).toHaveTextContent(utcDay(-1));
    expect(table).toHaveTextContent("15kt");
    expect(table).toHaveTextContent("$0.50");
  });

  it("marks the accumulating day as provisional in the trend and the totals", async () => {
    const { findByTestId } = render(() => (
      <UsageCharts days={[day(utcDay(-1), 15_000, 0.5), day(utcDay(0), 3_000, 0.1)]} />
    ));
    const charts = await findByTestId("usage-charts");

    expect(charts).toHaveTextContent("still accumulating");
    // The provisional point is drawn hollow and named in its readout, so it is
    // not read as a completed fall in usage.
    const titles = Array.from(charts.querySelectorAll("title"), (title) => title.textContent ?? "");
    expect(titles.some((title) => title.includes(`${utcDay(0)} (in progress)`))).toBe(true);

    await userEvent.setup().click(within(charts).getByText("Daily totals (2)"));
    expect(within(charts).getByRole("table")).toHaveTextContent(`${utcDay(0)} (in progress)`);
  });

  it("says nothing about an unfinished day when every day is complete", async () => {
    const { findByTestId } = render(() => (
      <UsageCharts days={[day(utcDay(-2), 15_000, 0.5), day(utcDay(-1), 3_000, 0.1)]} />
    ));
    const charts = await findByTestId("usage-charts");

    expect(charts).not.toHaveTextContent("still accumulating");
    expect(await charts.querySelectorAll("title")).toHaveLength(4);
  });
});
