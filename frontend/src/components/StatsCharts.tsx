// Shared resource history charts plus the task's per-turn token lanes and its tool-time breakdown.

import * as Plot from "@observablehq/plot";
import { createMemo, Show } from "solid-js";

import type { EventStats } from "@sdk/types.gen";

import { basePlot, crosshairMarks, labelMargin, tickMargin } from "../chartPlot";
import { formatBytes, formatTokens } from "../formatting";
import { deriveNetworkRates, type ToolTimingSummary } from "../taskStats";
import type { HistoryTurn } from "@sdk/types.gen";
import ChartDataTable, { DataTable } from "./ChartDataTable";
import PlotHost from "./PlotHost";
import TurnTools from "./TurnTools";
import styles from "./StatsCharts.module.css";

interface TokenDatum {
  turn: string;
  category: string;
  count: number;
  // level is count as a share of its category's peak, so each lane has its own scale.
  level: number;
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

// resourceTimeTicks picks the ages, in seconds, that keep a label: a round step
// small enough to fit the chart width, newest first.
function resourceTimeTicks(spanMs: number, width: number): number[] {
  const steps = [10, 30, 60, 120, 300, 600];
  const spanSeconds = Math.max(spanMs / 1000, 1);
  const maxLabels = Math.max(2, Math.floor(width / 56));
  const step = steps.find((candidate) => spanSeconds / candidate <= maxLabels) ?? steps[steps.length - 1] ?? 600;
  const ticks: number[] = [];
  for (let age = 0; age <= spanSeconds; age += step) ticks.push(age);
  return ticks;
}

// formatSampleAge labels one tick as its distance back from the newest sample.
function formatSampleAge(ageSeconds: number): string {
  if (ageSeconds <= 0) return "now";
  if (ageSeconds < 60) return `-${Math.round(ageSeconds)}s`;
  const minutes = Math.floor(ageSeconds / 60);
  const seconds = Math.round(ageSeconds % 60);
  return seconds ? `-${minutes}m${String(seconds).padStart(2, "0")}s` : `-${minutes}m`;
}

function formatThroughput(bytesPerSecond: number): string {
  return `${formatBytes(bytesPerSecond)}/s`;
}

function formatPercent(value: number): string {
  return `${value.toFixed(1)}%`;
}

function throughputScaleMax(data: readonly ResourceDatum[]): number {
  const max = Math.max(0, ...data.flatMap((sample) => (sample.value === null ? [] : [sample.value])));
  if (max === 0) return 1;
  const unit = Math.pow(1024, Math.max(0, Math.floor(Math.log2(max) / 10)));
  const normalized = max / unit;
  const step = normalized < 10 ? 0.1 : 1;
  return Math.ceil(normalized / step) * step * unit;
}

function formatExactSampleTime(ts: number): string {
  return new Date(ts).toLocaleString();
}

const tokenCounts = (turn: HistoryTurn): readonly (readonly [string, number])[] => [
  ["New input", turn.usage.inputTokens],
  ["Cache write", turn.usage.cacheCreationInputTokens],
  ["Cache read", turn.usage.cacheReadInputTokens],
  ["Output", turn.usage.outputTokens],
];

function tokenPeaks(turns: readonly HistoryTurn[]): Map<string, number> {
  const peaks = new Map(tokenCategories.map((category) => [category, 0]));
  for (const turn of turns)
    for (const [category, count] of tokenCounts(turn)) peaks.set(category, Math.max(peaks.get(category) ?? 0, count));
  return peaks;
}

function tokenData(turns: readonly HistoryTurn[], peaks: ReadonlyMap<string, number>): TokenDatum[] {
  return turns.flatMap((turn, i) =>
    tokenCounts(turn).map(([category, count]) => {
      const peak = peaks.get(category) ?? 0;
      return { category, count, level: peak > 0 ? count / peak : 0, turn: String(i + 1) };
    }),
  );
}

// turnTicks keeps every turn label that fits: a round step such as 5 or 10,
// and the first turn when the step leaves room beside it.
function turnTicks(count: number, width: number): string[] {
  const labelWidth = String(count).length * 6 + 8;
  const fits = Math.max(1, Math.floor(width / labelWidth));
  const step = [1, 2, 5, 10, 20, 25, 50, 100, 200, 500].find((candidate) => count / candidate <= fits) ?? 1000;
  const ticks: string[] = [];
  for (let n = 1; n <= count; n++) if ((n === 1 && step >= 5) || n % step === 0) ticks.push(String(n));
  return ticks;
}

// Cache reads outweigh the other categories by orders of magnitude, so one
// stacked scale flattens them into its baseline. Each category gets its own
// lane scaled to its peak, which its label states, on one shared turn axis.
function drawTokenChart(turns: readonly HistoryTurn[], width: number): Element {
  const peaks = tokenPeaks(turns);
  const data = tokenData(turns, peaks);
  const marginLeft = 8;
  const marginRight = 8;
  const laneHeight = 52;
  return Plot.plot({
    ...basePlot(width, tokenCategories.length * laneHeight + 30),
    marginLeft,
    marginRight,
    marginTop: 0,
    marginBottom: 24,
    x: {
      type: "band",
      label: null,
      padding: 0.2,
      domain: turns.map((_, i) => String(i + 1)),
      ticks: turnTicks(turns.length, width - marginLeft - marginRight),
      tickSize: 2,
    },
    // The headroom above 1 keeps the tallest bar clear of the lane label.
    y: { axis: null, domain: [0, 1.3] },
    fy: { axis: null, domain: tokenCategories, padding: 0.12 },
    color: {
      domain: tokenCategories,
      range: [
        "var(--color-chart-new-input)",
        "var(--color-chart-cache-write)",
        "var(--color-chart-cache-read)",
        "var(--color-chart-output)",
      ],
    },
    marks: [
      Plot.barY(data, {
        x: "turn",
        y: "level",
        fy: "category",
        fill: "category",
        title: (d: TokenDatum) => `Turn ${d.turn} · ${d.category}: ${formatTokens(d.count)}`,
      }),
      Plot.ruleY([0], { stroke: "var(--color-border)" }),
      Plot.text(tokenCategories, {
        fy: (category: string) => category,
        frameAnchor: "top-left",
        dy: 3,
        fill: "var(--color-text-secondary)",
        text: (category: string) => `${category} · peak ${formatTokens(peaks.get(category) ?? 0)}`,
      }),
    ],
  });
}

function drawResourceChart(data: readonly ResourceDatum[], width: number, options: ResourceChartOptions): Element {
  const first = data[0];
  const last = data.at(-1);
  const domain: [number, number] | undefined = first && last ? [first.ts, last.ts] : undefined;
  const available = data.filter((sample): sample is ResourceDatum & { value: number } => sample.value !== null);
  const valueAxis = options.axisFormat !== null && options.maxValue !== null;
  const showAxis = options.axisAnchor !== null && valueAxis;
  const axisMargin =
    options.axisFormat !== null && options.maxValue !== null
      ? Math.max(40, labelMargin(options.axisFormat(0)), labelMargin(options.axisFormat(options.maxValue)))
      : 3;
  const span = domain ? domain[1] - domain[0] : 0;
  const valueMarginLeft = showAxis && options.axisAnchor === "left" ? axisMargin : 3;
  const valueMarginRight = showAxis && options.axisAnchor === "right" ? axisMargin : 3;
  const timeTicks = domain ? resourceTimeTicks(span, width - valueMarginLeft - valueMarginRight) : [];
  // A tick label is centered on its tick, so the end labels need half a label of
  // room beyond the plot area or the frame clips them.
  const timeLabels = timeTicks.map(formatSampleAge);
  const marginLeft = Math.max(valueMarginLeft, tickMargin(timeLabels[0]));
  const marginRight = Math.max(valueMarginRight, tickMargin(timeLabels.at(-1)));
  const crosshair = crosshairMarks(
    data.map((sample) => ({
      x: sample.ts,
      value: sample.value,
      label: sample.value === null ? "" : options.formatValue(sample.value),
    })),
    { color: options.color, plotWidth: width - marginLeft - marginRight },
  );
  return Plot.plot({
    ...basePlot(width, options.height),
    margin: 3,
    marginBottom: 22,
    marginLeft,
    marginRight,
    marginTop: showAxis ? 8 : 3,
    x: {
      domain,
      // The newest sample is the right edge, so ages count back from it. The
      // scale label would echo the channel name, which the ages already explain.
      label: null,
      labelArrow: false,
      ticks: timeTicks.map((age) => (domain ? domain[1] - age * 1000 : age)),
      tickFormat: (ts: number) => formatSampleAge(domain ? (domain[1] - ts) / 1000 : 0),
      tickPadding: 2,
      tickSize: 2,
    },
    y: showAxis
      ? {
          axis: options.axisAnchor,
          domain: [0, options.maxValue ?? 0],
          label: null,
          grid: true,
          tickFormat: options.axisFormat,
          tickPadding: 2,
          ticks: [0, (options.maxValue ?? 0) / 2, options.maxValue ?? 0],
          tickSize: 2,
          zero: true,
        }
      : options.maxValue === null
        ? { axis: null, zero: true }
        : { axis: null, domain: [0, options.maxValue], zero: true },
    marks: [
      Plot.lineY(data, { x: "ts", y: "value", stroke: options.color, strokeWidth: 1.5 }),
      Plot.dot(available, { x: "ts", y: "value", fill: options.color, r: 1.8 }),
      // Samples with no measurement (disk before it reports, throughput before a
      // second sample) leave the rule alone and show no readout.
      ...crosshair,
    ],
  });
}

export interface ResourceSample extends EventStats {
  cpuCount?: number;
  diskTotal?: number;
}

export function ResourceCharts(props: {
  stats: readonly ResourceSample[];
  scope: "machine" | "task";
  // A page that renders the sample table itself, after later sections, turns it
  // off here instead of showing it twice.
  samples?: boolean;
}) {
  const latest = () => props.stats.at(-1);
  // A page has room to draw the same charts taller than a pane does.
  const heights = () => (props.scope === "machine" ? { value: 140, network: 104 } : { value: 76, network: 62 });
  const cpuObservedMax = () => Math.max(...props.stats.map((sample) => sample.cpuPerc));
  const cpuScaleMax = () =>
    Math.max(
      100,
      Math.ceil(cpuObservedMax()),
      ...props.stats.map((sample) => (sample.cpuCount ?? sample.cpuCeiling ?? 0) * 100),
    );
  const cpuCount = () => latest()?.cpuCount ?? null;
  const cpu = () =>
    windowAnchored(
      props.stats.map((sample) => ({ ts: sample.ts, value: sample.cpuPerc })),
      props.scope,
    );
  const memory = () =>
    windowAnchored(
      props.stats.map((sample) => ({ ts: sample.ts, value: sample.memPerc })),
      props.scope,
    );
  const disk = () =>
    windowAnchored(
      props.stats.map((sample) => ({
        ts: sample.ts,
        value: sample.diskUsed >= 0 ? sample.diskUsed : null,
      })),
      props.scope,
    );
  const networkRates = createMemo(() => deriveNetworkRates(props.stats));
  const rx = () =>
    windowAnchored(
      networkRates().map((sample) => ({ ts: sample.ts, value: sample.rxBytesPerSecond })),
      props.scope,
    );
  const tx = () =>
    windowAnchored(
      networkRates().map((sample) => ({ ts: sample.ts, value: sample.txBytesPerSecond })),
      props.scope,
    );
  return (
    <div class={styles.resourceGrid} data-testid="resource-charts">
      <div class={styles.resourceRow}>
        <div class={styles.resourceHeader}>
          <strong>CPU</strong>
          <span>
            {latest()?.cpuPerc.toFixed(1)}% · {(Number(latest()?.cpuPerc ?? 0) / 100).toFixed(2)}
            <Show when={cpuCount() !== null}> / {cpuCount()}</Show> CPUs
          </span>
        </div>
        <PlotHost
          label="CPU utilization over time"
          description="100% is one fully busy logical CPU. The scale includes the machine CPU count or configured task ceiling. Task ceilings exclude inherited limits and process affinity."
          draw={(width) =>
            drawResourceChart(cpu(), width, {
              axisAnchor: "left",
              axisFormat: (value) => `${Math.round(value)}%`,
              color: "var(--color-chart-cpu)",
              formatValue: (value) => `${value.toFixed(1)}%`,
              height: heights().value,
              label: "CPU utilization",
              maxValue: cpuScaleMax(),
            })
          }
        />
        <div class={styles.note}>
          100% = 1 logical CPU
          <Show when={cpuCount()}>
            {(capacity) => <> · {((latest()?.cpuPerc ?? 0) / capacity()).toFixed(1)}% of capacity</>}
          </Show>
          <Show when={props.scope === "task"}>
            {" · effective capacity unknown"}
            <Show when={latest()?.cpuCeiling}>{(ceiling) => <> · configured ceiling ≤ {ceiling()} CPUs</>}</Show>
          </Show>
        </div>
        <Show when={props.scope === "task"}>
          <div class={styles.note}>
            Runtime: {latest()?.runtimeCPUs ?? "unknown"} CPUs ·{" "}
            {(latest()?.cpuQuota ?? null) === null
              ? "quota unknown"
              : latest()?.cpuQuota === 0
                ? "no explicit quota"
                : `quota ${latest()?.cpuQuota} CPUs`}
          </div>
          <div class={styles.note}>Inherited CPU limits and process affinity are unreported.</div>
          <Show when={latest()?.cpuError}>
            {(warning) => (
              <details class={styles.note}>
                <summary>CPU metadata warning</summary>
                {warning()}
              </details>
            )}
          </Show>
        </Show>
      </div>
      <div class={styles.resourceRow}>
        <div class={styles.resourceHeader}>
          <strong>Memory</strong>
          <span>
            {formatBytes(latest()?.memUsed ?? 0)} / {formatBytes(latest()?.memLimit ?? 0)}
          </span>
        </div>
        <PlotHost
          label="Memory utilization over time"
          draw={(width) =>
            drawResourceChart(memory(), width, {
              axisAnchor: "left",
              axisFormat: (value) => `${Math.round(value)}%`,
              color: "var(--color-chart-memory)",
              formatValue: (value) => `${value.toFixed(1)}%`,
              height: heights().value,
              label: "Memory utilization",
              maxValue: 100,
            })
          }
        />
      </div>
      <div class={styles.resourceRow}>
        <div class={styles.resourceHeader} title="Cumulative network totals">
          <strong>Network</strong>
          <span>
            <span class={styles.networkRx}>RX</span> {formatBytes(latest()?.netRx ?? 0)} ·{" "}
            <span class={styles.networkTx}>TX</span> {formatBytes(latest()?.netTx ?? 0)}
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
                <span>
                  {networkRates().at(-1)?.rxBytesPerSecond === null
                    ? "—"
                    : formatThroughput(networkRates().at(-1)?.rxBytesPerSecond ?? 0)}
                </span>
              </div>
              <PlotHost
                label="Network receive throughput over time"
                draw={(width) =>
                  drawResourceChart(rx(), width, {
                    axisAnchor: "left",
                    axisFormat: formatThroughput,
                    color: "var(--color-primary)",
                    formatValue: (value) => `RX ${formatThroughput(value)}`,
                    height: heights().network,
                    label: "Network receive throughput",
                    maxValue: throughputScaleMax(rx()),
                  })
                }
              />
            </div>
            <div class={styles.networkSeries}>
              <div class={`${styles.networkSeriesHeader} ${styles.networkSeriesHeaderTx}`}>
                <strong class={styles.networkTx}>TX/s</strong>
                <span>
                  {networkRates().at(-1)?.txBytesPerSecond === null
                    ? "—"
                    : formatThroughput(networkRates().at(-1)?.txBytesPerSecond ?? 0)}
                </span>
              </div>
              <PlotHost
                label="Network transmit throughput over time"
                draw={(width) =>
                  drawResourceChart(tx(), width, {
                    axisAnchor: "right",
                    axisFormat: formatThroughput,
                    color: "var(--color-plan)",
                    formatValue: (value) => `TX ${formatThroughput(value)}`,
                    height: heights().network,
                    label: "Network transmit throughput",
                    maxValue: throughputScaleMax(tx()),
                  })
                }
              />
            </div>
          </div>
        </Show>
      </div>
      <div class={styles.resourceRow}>
        <div class={styles.resourceHeader}>
          <strong>Disk</strong>
          <span>
            {(latest()?.diskUsed ?? -1) >= 0 ? formatBytes(latest()?.diskUsed ?? 0) : "Unavailable"}
            <Show when={(latest()?.diskUsed ?? -1) >= 0 && latest()?.diskTotal !== undefined}>
              {" / "}
              {formatBytes(latest()?.diskTotal ?? 0)}
            </Show>
          </span>
        </div>
        <Show
          when={disk().some((sample) => sample.value !== null)}
          fallback={<div class={styles.resourceUnavailable}>No disk history available</div>}
        >
          <PlotHost
            label="Writable disk usage over time"
            draw={(width) =>
              drawResourceChart(disk(), width, {
                // The used bytes sit in a narrow band under a much larger total,
                // so this chart scales to the data and puts the total in its
                // caption instead of flattening the trend against a fixed axis.
                axisAnchor: null,
                axisFormat: null,
                color: "var(--color-warning-text)",
                formatValue: formatBytes,
                height: heights().value,
                label: "Writable disk usage",
                maxValue: null,
              })
            }
          />
        </Show>
      </div>
      <Show when={props.samples !== false}>
        <ResourceSamples scope={props.scope} stats={props.stats} />
      </Show>
    </div>
  );
}

// machineWindowMs is the machine ring: sixty samples five seconds apart. The
// server publishes its first sample five seconds after it starts, so anchoring
// the series at the start of that window keeps the plot from rescaling with
// every sample while the server is fresh. The anchor carries no value, so it
// draws nothing and the table keeps only measured rows.
const machineWindowMs = 60 * 5_000;

function windowAnchored(data: readonly ResourceDatum[], scope: "machine" | "task"): readonly ResourceDatum[] {
  const first = data[0];
  const last = data.at(-1);
  if (scope !== "machine" || !first || !last || last.ts - first.ts >= machineWindowMs) return data;
  return [{ ts: last.ts - machineWindowMs, value: null }, ...data];
}

// ResourceSamples lists every sample behind the resource charts. A page that
// places the table itself, below other sections, turns it off on the charts.
// A task pane is too narrow for the machine-only columns: its throughput charts
// carry the rates, and its CPU ceiling, quota, and runtime barely move.
export function ResourceSamples(props: { stats: readonly ResourceSample[]; scope: "machine" | "task" }) {
  const machine = () => props.scope === "machine";
  const networkRates = createMemo(() => deriveNetworkRates(props.stats));
  const rows = () =>
    props.stats
      .map((sample, index) => {
        const rate = networkRates()[index];
        if (!rate) throw new Error("Resource sample has no matching network rate");
        return { rate, sample };
      })
      .reverse()
      .map(({ rate, sample }) => [
        formatExactSampleTime(sample.ts),
        formatPercent(sample.cpuPerc),
        ...(machine() ? [sample.cpuCount === undefined ? "Unknown" : String(sample.cpuCount)] : []),
        formatPercent(sample.memPerc),
        ...(machine()
          ? [
              rate.rxBytesPerSecond === null ? "—" : formatThroughput(rate.rxBytesPerSecond),
              rate.txBytesPerSecond === null ? "—" : formatThroughput(rate.txBytesPerSecond),
            ]
          : []),
        sample.diskUsed < 0 ? "—" : formatBytes(sample.diskUsed),
      ]);

  return (
    <section class={styles.samples} data-testid="exact-samples">
      <DataTable
        columns={[
          "Time",
          // The parenthetical explains the multi-CPU scale, which the machine
          // view needs beside its CPU count; a task sample spans one ceiling.
          machine() ? "CPU (100% per CPU)" : "CPU",
          ...(machine() ? ["CPUs"] : []),
          "Memory",
          ...(machine() ? ["RX/s", "TX/s"] : []),
          "Disk",
        ]}
        fit={props.scope === "task"}
        regionLabel="Exact resource samples"
        rows={rows()}
      />
    </section>
  );
}

export default function StatsCharts(props: {
  stats: readonly EventStats[];
  turns: readonly HistoryTurn[];
  tools: readonly ToolTimingSummary[];
  // A page that renders the sample table in its own block turns it off here.
  samples?: boolean;
}) {
  const stats = createMemo<readonly EventStats[]>((previous) => {
    const next = props.stats.slice(-maxResourceSamples);
    return next.length === previous.length && next[0] === previous[0] && next.at(-1) === previous.at(-1)
      ? previous
      : next;
  }, []);
  const turns = createMemo(() => props.turns);
  const tools = createMemo(() => props.tools);
  const tokenRows = () =>
    turns().map((turn, index) => {
      const usage = turn.usage;
      const total =
        usage.inputTokens + usage.cacheCreationInputTokens + usage.cacheReadInputTokens + usage.outputTokens;
      return [
        `Turn ${index + 1}`,
        turn.model || "Unknown",
        formatTokens(usage.inputTokens),
        formatTokens(usage.cacheCreationInputTokens),
        formatTokens(usage.cacheReadInputTokens),
        formatTokens(usage.outputTokens),
        formatTokens(total),
      ];
    });

  return (
    <>
      <Show when={stats().length > 0}>
        <ResourceCharts samples={props.samples} stats={stats()} scope="task" />
      </Show>
      <Show when={turns().length > 0}>
        <div class={styles.figure} data-testid="turn-token-chart">
          <div class={styles.title}>Tokens by turn</div>
          <PlotHost
            label="Tokens by turn"
            description="One lane per token category, each scaled to its own peak, on a shared turn axis."
            draw={(width) => drawTokenChart(turns(), width)}
          />
          <ChartDataTable
            caption="Turn tokens"
            columns={["Turn", "Model", "New input", "Cache write", "Cache read", "Output", "Total"]}
            regionLabel="Token counts by turn"
            rows={tokenRows()}
          />
        </div>
      </Show>
      <Show when={tools().length > 0}>
        <div class={styles.figure} data-testid="tool-time-chart">
          <TurnTools tools={tools()} />
        </div>
      </Show>
    </>
  );
}
