// Compact task charts for resource history, token composition, and cumulative tool time.

import * as Plot from "@observablehq/plot";
import { createEffect, createMemo, createSignal, For, onCleanup, onMount, Show } from "solid-js";

import type { EventStats } from "@sdk/types.gen";

import { formatTokens } from "../formatting";
import { deriveNetworkRates, type ToolTimingSummary } from "../taskStats";
import type { TurnTiming } from "../timing";
import { formatTimingDuration } from "../timing";
import styles from "./StatsCharts.module.css";

interface TokenDatum {
  turn: string;
  category: string;
  tokens: number;
}

interface ResourceDatum {
  ts: number;
  value: number | null;
}

interface ResourceChartOptions {
  axisAnchor: "left" | "right" | null;
  axisFormat: ((value: number) => string) | null;
  color: string;
  formatValue: (value: number) => string;
  height: number;
  label: string;
  maxValue: number | null;
}

const tokenCategories = ["New input", "Cache write", "Cache read", "Output"];
const maxResourceSamples = 120;

function formatBytes(bytes: number): string {
  if (bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.max(0, Math.min(Math.floor(Math.log2(bytes) / 10), units.length - 1));
  const value = bytes / Math.pow(1024, i);
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[i]}`;
}

function formatSampleTime(ts: number): string {
  return new Date(ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function formatThroughput(bytesPerSecond: number): string {
  return `${formatBytes(bytesPerSecond)}/s`;
}

function throughputScaleMax(data: readonly ResourceDatum[]): number {
  const max = Math.max(0, ...data.flatMap((sample) => sample.value === null ? [] : [sample.value]));
  if (max === 0) return 1;
  const unit = Math.pow(1024, Math.max(0, Math.floor(Math.log2(max) / 10)));
  const normalized = max / unit;
  const step = normalized < 10 ? 0.1 : 1;
  return Math.ceil(normalized / step) * step * unit;
}

function formatExactBytes(value: number, perSecond: boolean): string {
  const unit = perSecond ? "B/s" : "B";
  const compact = `${formatBytes(value)}${perSecond ? "/s" : ""}`;
  return `${compact} (${String(value)} ${unit})`;
}

function formatExactSampleTime(ts: number): string {
  return new Date(ts).toISOString();
}

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

function drawResourceChart(
  data: readonly ResourceDatum[],
  width: number,
  options: ResourceChartOptions,
): Element {
  const domain = data.length > 0 ? [data[0].ts, data[data.length - 1].ts] : undefined;
  const available = data.filter((sample): sample is ResourceDatum & { value: number } => sample.value !== null);
  const showAxis = options.axisAnchor !== null && options.axisFormat !== null && options.maxValue !== null;
  return Plot.plot({
    width,
    height: options.height,
    margin: 3,
    marginBottom: showAxis ? 8 : 3,
    marginLeft: showAxis && options.axisAnchor === "left" ? 40 : 3,
    marginRight: showAxis && options.axisAnchor === "right" ? 40 : 3,
    marginTop: showAxis ? 8 : 3,
    ariaLabel: `${options.label} over time`,
    className: "caic-plot",
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "10px" },
    x: { axis: null, domain },
    y: showAxis
      ? { axis: options.axisAnchor, domain: [0, options.maxValue], label: null, tickFormat: options.axisFormat, tickPadding: 2, ticks: [0, options.maxValue], tickSize: 2, zero: true }
      : options.maxValue === null ? { axis: null, zero: true } : { axis: null, domain: [0, options.maxValue], zero: true },
    marks: [
      Plot.lineY(data, { x: "ts", y: "value", stroke: options.color, strokeWidth: 1.5 }),
      Plot.dot(available, {
        x: "ts",
        y: "value",
        fill: options.color,
        r: 1.8,
        title: (d: ResourceDatum & { value: number }) => `${formatSampleTime(d.ts)} · ${options.formatValue(d.value)}`,
      }),
    ],
  });
}

function ResourceCharts(props: { stats: readonly EventStats[] }) {
  const latest = () => props.stats.at(-1);
  const cpuObservedMax = () => Math.max(...props.stats.map((sample) => sample.cpuPerc));
  const cpuScaleMax = () => Math.max(100, Math.ceil(cpuObservedMax()));
  const timeRange = () => {
    const first = props.stats[0];
    const last = props.stats.at(-1);
    if (!first || !last) return "";
    if (first === last) return `1 sample · ${formatSampleTime(last.ts)}`;
    return `${props.stats.length} samples · ${formatSampleTime(first.ts)}–${formatSampleTime(last.ts)}`;
  };
  const cpu = () => props.stats.map((sample) => ({ ts: sample.ts, value: sample.cpuPerc }));
  const memory = () => props.stats.map((sample) => ({ ts: sample.ts, value: sample.memPerc }));
  const disk = () => props.stats.map((sample) => ({ ts: sample.ts, value: sample.diskUsed >= 0 ? sample.diskUsed : null }));
  const networkRates = createMemo(() => deriveNetworkRates(props.stats));
  const rx = () => networkRates().map((sample) => ({ ts: sample.ts, value: sample.rxBytesPerSecond }));
  const tx = () => networkRates().map((sample) => ({ ts: sample.ts, value: sample.txBytesPerSecond }));

  return (
      <div class={styles.resourceGrid} data-testid="resource-charts">
      <div class={styles.resourceRow}>
        <div class={styles.resourceHeader}>
          <strong>CPU</strong>
          <span>
            {latest()?.cpuPerc.toFixed(1)}%
            <Show when={cpuObservedMax() > 100}> · max {String(cpuObservedMax())}%</Show>
          </span>
        </div>
        <PlotHost
          label="CPU utilization over time"
          draw={(width) => drawResourceChart(cpu(), width, {
            axisAnchor: "left",
            axisFormat: (value) => `${Math.round(value)}%`,
            color: "var(--color-primary)",
            formatValue: (value) => `${value.toFixed(1)}%`,
            height: 58,
            label: "CPU utilization",
            maxValue: cpuScaleMax(),
          })}
        />
      </div>
      <div class={styles.resourceRow}>
        <div class={styles.resourceHeader}>
          <strong>Memory</strong>
          <span>{formatBytes(latest()?.memUsed ?? 0)} / {formatBytes(latest()?.memLimit ?? 0)}</span>
        </div>
        <PlotHost label="Memory utilization over time" draw={(width) => drawResourceChart(memory(), width, {
          axisAnchor: null,
          axisFormat: null,
          color: "var(--color-success)",
          formatValue: (value) => `${value.toFixed(1)}%`,
          height: 58,
          label: "Memory utilization",
          maxValue: 100,
        })} />
      </div>
      <div class={styles.resourceRow}>
        <div class={styles.resourceHeader} title="Cumulative network totals">
          <strong>Network</strong>
          <span>
            <span class={styles.networkRx}>RX</span> {formatBytes(latest()?.netRx ?? 0)} · <span class={styles.networkTx}>TX</span> {formatBytes(latest()?.netTx ?? 0)}
          </span>
        </div>
        <Show
          when={networkRates().some((sample) => sample.rxBytesPerSecond !== null || sample.txBytesPerSecond !== null)}
          fallback={<div class={styles.resourceUnavailable}>Waiting for another sample</div>}
        >
          <div class={styles.networkCharts}>
            <div class={styles.networkSeries}>
              <div class={styles.networkSeriesHeader}>
                <strong class={styles.networkRx}>RX/s</strong>
                <span>{networkRates().at(-1)?.rxBytesPerSecond === null ? "—" : formatThroughput(networkRates().at(-1)?.rxBytesPerSecond ?? 0)}</span>
              </div>
              <PlotHost label="Network receive throughput over time" draw={(width) => drawResourceChart(rx(), width, {
                axisAnchor: "left",
                axisFormat: formatThroughput,
                color: "var(--color-primary)",
                formatValue: (value) => `RX ${formatThroughput(value)}`,
                height: 44,
                label: "Network receive throughput",
                maxValue: throughputScaleMax(rx()),
              })} />
            </div>
            <div class={styles.networkSeries}>
              <div class={`${styles.networkSeriesHeader} ${styles.networkSeriesHeaderTx}`}>
                <strong class={styles.networkTx}>TX/s</strong>
                <span>{networkRates().at(-1)?.txBytesPerSecond === null ? "—" : formatThroughput(networkRates().at(-1)?.txBytesPerSecond ?? 0)}</span>
              </div>
              <PlotHost label="Network transmit throughput over time" draw={(width) => drawResourceChart(tx(), width, {
                axisAnchor: "right",
                axisFormat: formatThroughput,
                color: "var(--color-plan)",
                formatValue: (value) => `TX ${formatThroughput(value)}`,
                height: 44,
                label: "Network transmit throughput",
                maxValue: throughputScaleMax(tx()),
              })} />
            </div>
          </div>
        </Show>
      </div>
      <div class={styles.resourceRow}>
        <div class={styles.resourceHeader}>
          <strong>Disk</strong>
          <span>{(latest()?.diskUsed ?? -1) >= 0 ? formatBytes(latest()?.diskUsed ?? 0) : "Unavailable"}</span>
        </div>
        <Show
          when={disk().some((sample) => sample.value !== null)}
          fallback={<div class={styles.resourceUnavailable}>No disk history available</div>}
        >
          <PlotHost label="Writable disk usage over time" draw={(width) => drawResourceChart(disk(), width, {
            axisAnchor: null,
            axisFormat: null,
            color: "var(--color-warning-text)",
            formatValue: formatBytes,
            height: 58,
            label: "Writable disk usage",
            maxValue: null,
          })} />
        </Show>
      </div>
      <div class={styles.resourceRange}>{timeRange()}</div>
      <details class={styles.sampleDetails}>
        <summary>Exact samples ({props.stats.length})</summary>
        {/* eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- overflow region must be keyboard-scrollable */}
        <div aria-label="Exact resource samples" class={styles.sampleTableWrap} role="region" tabIndex={0}>
          <table class={styles.sampleTable}>
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">CPU</th>
                <th scope="col">Memory</th>
                <th scope="col">RX/s</th>
                <th scope="col">TX/s</th>
                <th scope="col">Disk</th>
              </tr>
            </thead>
            <tbody>
              <For each={props.stats.map((sample, index) => ({ sample, rate: networkRates()[index] })).reverse()}>
                {({ sample, rate }) => (
                  <tr>
                    <td>{formatExactSampleTime(sample.ts)}</td>
                    <td>{String(sample.cpuPerc)}%</td>
                    <td>{String(sample.memPerc)}%</td>
                    <td>{rate.rxBytesPerSecond === null ? "—" : formatExactBytes(rate.rxBytesPerSecond, true)}</td>
                    <td>{rate.txBytesPerSecond === null ? "—" : formatExactBytes(rate.txBytesPerSecond, true)}</td>
                    <td>{sample.diskUsed < 0 ? "—" : formatExactBytes(sample.diskUsed, false)}</td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </details>
    </div>
  );
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

export default function StatsCharts(props: { stats: readonly EventStats[]; turns: readonly TurnTiming[]; tools: readonly ToolTimingSummary[] }) {
  const stats = createMemo<readonly EventStats[]>((previous) => {
    const next = props.stats.slice(-maxResourceSamples);
    return next.length === previous.length && next[0] === previous[0] && next.at(-1) === previous.at(-1) ? previous : next;
  }, []);
  const turns = createMemo<readonly TurnTiming[]>((previous) => {
    const next = props.turns;
    return next.length === previous.length && next.at(-1)?.event === previous.at(-1)?.event ? previous : next;
  }, []);

  return (
    <>
      <Show when={stats().length > 0}>
        <ResourceCharts stats={stats()} />
      </Show>
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
