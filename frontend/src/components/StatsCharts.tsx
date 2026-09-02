// Compact task analytics charts for token composition and cumulative tool time.

import * as Plot from "@observablehq/plot";
import { createEffect, createMemo, createSignal, onCleanup, onMount, Show } from "solid-js";

import { formatTokens } from "../formatting";
import type { ToolTimingSummary } from "../taskStats";
import type { TurnTiming } from "../timing";
import { formatTimingDuration } from "../timing";
import styles from "./StatsCharts.module.css";

interface TokenDatum {
  turn: string;
  category: string;
  tokens: number;
}

const tokenCategories = ["New input", "Cache write", "Cache read", "Output"];

function PlotHost(props: { label: string; draw: (width: number) => Element }) {
  const [width, setWidth] = createSignal(390);
  let host: HTMLDivElement | undefined; // eslint-disable-line no-unassigned-vars -- assigned by SolidJS ref

  onMount(() => {
    if (!host) return;
    const resize = () => setWidth(Math.max(280, Math.floor(host?.getBoundingClientRect().width ?? 390)));
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

function tokenData(turns: readonly TurnTiming[]): TokenDatum[] {
  return turns.flatMap((turn, i) => {
    const usage = turn.result.usage;
    return [
      { turn: String(i + 1), category: "New input", tokens: usage.inputTokens },
      { turn: String(i + 1), category: "Cache write", tokens: usage.cacheCreationInputTokens },
      { turn: String(i + 1), category: "Cache read", tokens: usage.cacheReadInputTokens },
      { turn: String(i + 1), category: "Output", tokens: usage.outputTokens },
    ];
  });
}

function drawTokenChart(turns: readonly TurnTiming[], width: number): Element {
  const data = tokenData(turns);
  return Plot.plot({
    width,
    height: 180,
    marginLeft: 48,
    marginBottom: 32,
    ariaLabel: "Token volume by turn",
    className: "caic-plot",
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "10px" },
    x: { type: "band", label: "Turn", padding: 0.25, domain: turns.map((_, i) => String(i + 1)) },
    y: { label: "Tokens", grid: true, tickFormat: formatTokens },
    color: {
      domain: tokenCategories,
      range: ["var(--color-warning-border)", "var(--color-primary)", "var(--color-success)", "var(--color-plan)"],
      legend: true,
    },
    marks: [
      Plot.barY(data, {
        x: "turn",
        y: "tokens",
        fill: "category",
        title: (d: TokenDatum) => `Turn ${d.turn} · ${d.category}: ${formatTokens(d.tokens)}`,
      }),
      Plot.ruleY([0]),
    ],
  });
}

function drawToolChart(tools: readonly ToolTimingSummary[], width: number): Element {
  const height = Math.max(92, tools.length * 25 + 34);
  return Plot.plot({
    width,
    height,
    marginLeft: Math.min(150, Math.max(62, Math.max(...tools.map((tool) => tool.name.length)) * 7)),
    marginBottom: 28,
    ariaLabel: "Cumulative tool time by tool kind",
    className: "caic-plot",
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "10px" },
    x: { label: "Cumulative time", grid: true, tickFormat: formatTimingDuration },
    y: {
      label: null,
      domain: tools.map((tool) => tool.name),
      tickFormat: (name) => String(name).length > 22 ? `${String(name).slice(0, 21)}…` : String(name),
    },
    marks: [
      Plot.barX(tools, {
        x: "durationMs",
        y: "name",
        fill: "var(--color-primary)",
        title: (d: ToolTimingSummary) => `${d.name}: ${formatTimingDuration(d.durationMs)} across ${d.calls} ${d.calls === 1 ? "call" : "calls"}`,
      }),
      Plot.ruleX([0]),
    ],
  });
}

export default function StatsCharts(props: { turns: readonly TurnTiming[]; tools: readonly ToolTimingSummary[] }) {
  const turns = createMemo<readonly TurnTiming[]>((previous) => {
    const next = props.turns;
    return next.length === previous.length && next.at(-1)?.event === previous.at(-1)?.event ? previous : next;
  }, []);

  return (
    <>
      <Show when={turns().length > 0}>
        <div class={styles.figure} data-testid="turn-token-chart">
          <div class={styles.title}>Tokens by turn</div>
          <PlotHost label="Token volume by turn" draw={(width) => drawTokenChart(turns(), width)} />
        </div>
      </Show>
      <Show when={props.tools.length > 0}>
        <div class={styles.figure} data-testid="tool-time-chart">
          <div class={styles.title}>Tool time by kind</div>
          <PlotHost label="Cumulative tool time by tool kind" draw={(width) => drawToolChart(props.tools, width)} />
          <div class={styles.note}>Completed calls; concurrent durations may overlap.</div>
        </div>
      </Show>
    </>
  );
}
