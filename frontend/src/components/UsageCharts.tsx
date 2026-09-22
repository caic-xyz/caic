// UsageCharts renders daily token and cost trends from selected rollup days.

import * as Plot from "@observablehq/plot";
import { createEffect, createSignal, onCleanup, onMount } from "solid-js";

import type { UsageDashboardDay } from "@sdk/types.gen";

import { formatCost, formatTokens } from "../formatting";
import styles from "./UsageCharts.module.css";

interface DailyUsageDatum {
  day: string;
  tokens: number;
  costUSD: number;
}

function dailyUsage(days: readonly UsageDashboardDay[]): DailyUsageDatum[] {
  return days.map((day) => ({
    day: day.day,
    tokens:
      day.tokens.inputTokens +
      day.tokens.cacheWrite5mTokens +
      day.tokens.cacheWrite1hTokens +
      day.tokens.cacheReadTokens +
      day.tokens.outputTokens,
    costUSD: day.costUSD,
  }));
}

function PlotHost(props: { label: string; draw: (width: number) => Element }) {
  const [width, setWidth] = createSignal(480);
  // eslint-disable-next-line no-unassigned-vars -- assigned by SolidJS ref
  let host: HTMLDivElement | undefined;

  onMount(() => {
    if (!host) return;
    const resize = () => setWidth(Math.max(280, Math.floor(host?.getBoundingClientRect().width ?? 480)));
    resize();
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(resize);
    observer?.observe(host);
    window.addEventListener("resize", resize);
    onCleanup(() => {
      observer?.disconnect();
      window.removeEventListener("resize", resize);
    });
  });

  createEffect(() => {
    const plot = props.draw(width());
    plot.setAttribute("aria-label", props.label);
    host?.replaceChildren(plot);
  });

  return <div class={styles.chart} ref={host} />;
}

function drawDailyTokens(data: readonly DailyUsageDatum[], width: number): Element {
  return Plot.plot({
    width,
    height: 180,
    marginLeft: 52,
    marginBottom: 32,
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "11px" },
    x: { type: "point", domain: data.map((item) => item.day), label: null, tickFormat: (day) => String(day).slice(5) },
    y: { grid: true, label: "Tokens", tickFormat: formatTokens, zero: true },
    marks: [
      Plot.areaY(data, { x: "day", y: "tokens", fill: "var(--color-primary)", fillOpacity: 0.2 }),
      Plot.lineY(data, { x: "day", y: "tokens", stroke: "var(--color-primary)", strokeWidth: 2 }),
      Plot.dot(data, {
        x: "day",
        y: "tokens",
        fill: "var(--color-primary)",
        r: 2.5,
        title: (item) => `${item.day}: ${formatTokens(item.tokens)}`,
      }),
    ],
  });
}

function drawDailyCost(data: readonly DailyUsageDatum[], width: number): Element {
  return Plot.plot({
    width,
    height: 180,
    marginLeft: 52,
    marginBottom: 32,
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "11px" },
    x: { type: "point", domain: data.map((item) => item.day), label: null, tickFormat: (day) => String(day).slice(5) },
    y: { grid: true, label: "USD", tickFormat: formatCost, zero: true },
    marks: [
      Plot.lineY(data, { x: "day", y: "costUSD", stroke: "var(--color-plan)", strokeWidth: 2 }),
      Plot.dot(data, {
        x: "day",
        y: "costUSD",
        fill: "var(--color-plan)",
        r: 2.5,
        title: (item) => `${item.day}: ${formatCost(item.costUSD)}`,
      }),
    ],
  });
}

export default function UsageCharts(props: { days: readonly UsageDashboardDay[] }) {
  const data = () => dailyUsage(props.days);

  return (
    <div class={styles.grid} data-testid="usage-charts">
      <section class={styles.figure}>
        <h2>Tokens per day</h2>
        <PlotHost label="Total tokens per day" draw={(width) => drawDailyTokens(data(), width)} />
      </section>
      <section class={styles.figure}>
        <h2>Cost per day</h2>
        <PlotHost label="Reported cost per day in US dollars" draw={(width) => drawDailyCost(data(), width)} />
      </section>
    </div>
  );
}
