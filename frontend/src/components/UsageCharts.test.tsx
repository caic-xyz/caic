// Tests for the daily usage trend charts.

import { describe, it } from "node:test";
import { render } from "@solidjs/testing-library";
import { expect } from "@tests/expect";

import type { UsageDashboardDay } from "@sdk/types.gen";

import { usageDailySeries } from "../usageDashboard";
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

// hover dispatches the pointer event Plot listens to; the plot area starts at the
// left margin and ends before the right one.
function hover(chart: Element, fraction: number): void {
  const width = Number(chart.getAttribute("width"));
  chart.dispatchEvent(
    new MouseEvent("pointermove", { clientX: 52 + fraction * (width - 52 - 20), clientY: 90, bubbles: true }),
  );
}

// readoutText returns the pointer readout the crosshair draws above the chart.
function readoutText(charts: HTMLElement): string {
  return Array.from(charts.querySelectorAll('[aria-label="text"] text'), (node) => node.textContent ?? "").join("|");
}

// dayAnchors selects the day marks of both charts: Plot paints each mark group
// with the border stroke, which separates them from the axis and the grid.
function dayAnchors(charts: HTMLElement): SVGLineElement[] {
  return Array.from(charts.querySelectorAll<SVGLineElement>('g[stroke="var(--color-border)"] > line'));
}

function lineSpan(lines: readonly SVGLineElement[]): number {
  const [first] = lines;
  if (!first) return 0;
  return Number(first.getAttribute("y2")) - Number(first.getAttribute("y1"));
}

describe("UsageCharts", () => {
  it("marks the accumulating day as a provisional point in the trend", async () => {
    const { findByTestId } = render(() => (
      <UsageCharts points={usageDailySeries([day(utcDay(-1), 15_000, 0.5), day(utcDay(0), 3_000, 0.1)], "all", null)} />
    ));
    const charts = await findByTestId("usage-charts");

    // The provisional point is drawn hollow and named in its readout, so it is
    // not read as a completed fall in usage.
    for (const chart of Array.from(charts.querySelectorAll("svg"))) {
      hover(chart, 1);
      expect(readoutText(charts)).toContain("(in progress)");
    }
  });

  it("scales the token axis to the next unit past a billion tokens", async () => {
    const { findByTestId } = render(() => (
      <UsageCharts points={usageDailySeries([day(utcDay(-1), 1_500_000_000, 0.5)], "all", null)} />
    ));
    const charts = await findByTestId("usage-charts");

    expect(charts.textContent).toContain("Gt");
    expect(charts.textContent).not.toContain("1500.0Mt");
  });

  it("keeps a zero point for a day without usage", async () => {
    const { findByTestId } = render(() => (
      <UsageCharts points={usageDailySeries([day(utcDay(-2), 15_000, 0.5), day(utcDay(0), 3_000, 0.1)], "all", null)} />
    ));
    const charts = await findByTestId("usage-charts");

    // The zero-filled middle day is reachable, so a gap in usage reads as zero
    // rather than a missing point.
    for (const chart of Array.from(charts.querySelectorAll("svg"))) {
      hover(chart, 0.5);
      expect(readoutText(charts)).toContain(`${utcDay(-1).slice(5)} · 0t`);
    }
  });

  it("labels the first day of the range", async () => {
    const days: UsageDashboardDay[] = [];
    for (let offset = -13; offset <= 0; offset++) days.push(day(utcDay(offset), 15_000, 0.5));
    const { findByTestId } = render(() => <UsageCharts points={usageDailySeries(days, "all", null)} />);
    const charts = await findByTestId("usage-charts");

    const labels = Array.from(charts.querySelectorAll("svg text"), (node) => node.textContent ?? "").filter((text) =>
      /^\d\d-\d\d$/.test(text),
    );
    expect(labels).toContain(utcDay(-13).slice(5));
  });

  it("anchors every day of a readable range with a full-height rule", async () => {
    const days: UsageDashboardDay[] = [];
    for (let offset = -13; offset <= 0; offset++) days.push(day(utcDay(offset), 15_000, 0.5));
    const { findByTestId } = render(() => <UsageCharts points={usageDailySeries(days, "all", null)} />);
    const charts = await findByTestId("usage-charts");

    // Two charts, one anchor per day: the axis thins its labels, not the days.
    const anchors = dayAnchors(charts);
    expect(anchors).toHaveLength(28);
    expect(lineSpan(anchors)).toBeGreaterThan(100);
  });

  it("falls back to short day ticks once the days crowd together", async () => {
    const days: UsageDashboardDay[] = [];
    for (let offset = -89; offset <= 0; offset++) days.push(day(utcDay(offset), 15_000, 0.5));
    const { findByTestId } = render(() => <UsageCharts points={usageDailySeries(days, "all", null)} />);
    const charts = await findByTestId("usage-charts");

    const anchors = dayAnchors(charts);
    expect(anchors).toHaveLength(180);
    expect(lineSpan(anchors)).toBe(4);
  });

  it("thins the day axis across a long range", async () => {
    const days: UsageDashboardDay[] = [];
    for (let offset = -364; offset <= 0; offset++) days.push(day(utcDay(offset), 15_000, 0.5));
    const { findByTestId } = render(() => <UsageCharts points={usageDailySeries(days, "all", null)} />);
    const charts = await findByTestId("usage-charts");

    // Both charts share the day scale; a year of days must not draw a label per
    // day, which a point scale does because the domain has one value per day.
    const labels = Array.from(charts.querySelectorAll("svg text"), (node) => node.textContent ?? "").filter((text) =>
      /^\d\d-\d\d$/.test(text),
    );
    expect(labels.length).toBeGreaterThan(0);
    expect(labels.length).toBeLessThan(100);
  });

  it("keeps every point complete when the range ends before today", async () => {
    const { findByTestId } = render(() => (
      <UsageCharts
        points={usageDailySeries([day(utcDay(-2), 15_000, 0.5), day(utcDay(-1), 3_000, 0.1)], "all", null)}
      />
    ));
    const charts = await findByTestId("usage-charts");

    expect(charts.querySelectorAll('g[aria-label="dot"] circle')).toHaveLength(4);
    for (const chart of Array.from(charts.querySelectorAll("svg"))) {
      hover(chart, 1);
      expect(readoutText(charts)).not.toContain("(in progress)");
    }
  });
});
