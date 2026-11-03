// Shared resource history charts plus task token composition and cumulative tool-time charts.

import * as Plot from "@observablehq/plot";
import { createMemo, createSignal, Show } from "solid-js";

import type { EventStats } from "@sdk/types.gen";

import { basePlot, crosshairMarks, labelMargin, tickMargin } from "../chartPlot";
import { formatBytes, formatTokens } from "../formatting";
import { deriveNetworkRates, type ToolTimingSummary } from "../taskStats";
import type { HistoryTurn } from "@sdk/types.gen";
import { formatTimingDuration } from "../timing";
import ChartDataTable, { DataTable } from "./ChartDataTable";
import { ToggleChip } from "./FormControls";
import PlotHost from "./PlotHost";
import styles from "./StatsCharts.module.css";

interface TokenDatum {
  turn: string;
  category: string;
  count: number;
  share: number;
  value: number;
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

function formatSampleTime(ts: number): string {
  return new Date(ts).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function formatThroughput(bytesPerSecond: number): string {
  return `${formatBytes(bytesPerSecond)}/s`;
}

function formatShare(share: number): string {
  return formatPercent(share * 100);
}

function formatPercent(value: number): string {
  return `${value.toFixed(1)}%`;
}

function formatShareTick(share: number): string {
  return `${Math.round(share * 100)}%`;
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

function tokenData(turns: readonly HistoryTurn[], asShare: boolean): TokenDatum[] {
  return turns.flatMap((turn, i) => {
    const usage = turn.usage;
    const counts: readonly (readonly [string, number])[] = [
      ["New input", usage.inputTokens],
      ["Cache write", usage.cacheCreationInputTokens],
      ["Cache read", usage.cacheReadInputTokens],
      ["Output", usage.outputTokens],
    ];
    const total = counts.reduce((sum, [, count]) => sum + count, 0);
    return counts.map(([category, count]) => {
      const share = total > 0 ? count / total : 0;
      return { category, count, share, turn: String(i + 1), value: asShare ? share : count };
    });
  });
}

// Cache reads usually outweigh the other categories by an order of magnitude, so
// the absolute stack flattens them into the baseline; the share view keeps the
// composition between them readable.
function drawTokenChart(turns: readonly HistoryTurn[], width: number, asShare: boolean): Element {
  const data = tokenData(turns, asShare);
  return Plot.plot({
    ...basePlot(width, 180),
    marginLeft: 48,
    marginBottom: 32,
    x: { type: "band", label: "Turn", padding: 0.25, domain: turns.map((_, i) => String(i + 1)) },
    y: asShare
      ? { label: null, grid: true, domain: [0, 1], ticks: [0, 0.5, 1], tickFormat: formatShareTick }
      : { label: "Tokens", grid: true, tickFormat: formatTokens },
    color: {
      domain: tokenCategories,
      range: ["var(--color-warning-border)", "var(--color-primary)", "var(--color-success)", "var(--color-plan)"],
      legend: true,
    },
    marks: [
      Plot.barY(data, {
        x: "turn",
        y: "value",
        fill: "category",
        title: (d: TokenDatum) => `Turn ${d.turn} · ${d.category}: ${formatTokens(d.count)} (${formatShare(d.share)})`,
      }),
      Plot.ruleY([0]),
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
  const timeRange = () => {
    const first = props.stats[0];
    const last = props.stats.at(-1);
    if (!first || !last) return "";
    if (first === last) return `1 sample · ${formatSampleTime(last.ts)}`;
    return `${props.stats.length} samples · ${formatSampleTime(first.ts)}–${formatSampleTime(last.ts)}`;
  };
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
              color: "var(--color-primary)",
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
            {latest()?.cpuQuota === undefined
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
              color: "var(--color-success)",
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
      <div class={styles.resourceRange}>{timeRange()}</div>
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
      <h3 class={styles.samplesTitle}>Exact samples</h3>
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

function drawToolChart(tools: readonly ToolTimingSummary[], width: number): Element {
  const height = Math.max(92, tools.length * 25 + 34);
  return Plot.plot({
    ...basePlot(width, height),
    marginLeft: Math.min(150, Math.max(62, Math.max(...tools.map((tool) => labelMargin(tool.name))))),
    marginBottom: 28,
    x: { label: "Cumulative time", grid: true, tickFormat: formatTimingDuration },
    y: {
      label: null,
      domain: tools.map((tool) => tool.name),
      tickFormat: (name) => (String(name).length > 22 ? `${String(name).slice(0, 21)}…` : String(name)),
    },
    marks: [
      Plot.barX(tools, {
        x: "durationMs",
        y: "name",
        fill: "var(--color-primary)",
        title: (d: ToolTimingSummary) =>
          `${d.name}: ${formatTimingDuration(d.durationMs)} across ${d.calls} ${d.calls === 1 ? "call" : "calls"}`,
      }),
      Plot.ruleX([0]),
    ],
  });
}

export default function StatsCharts(props: {
  stats: readonly EventStats[];
  turns: readonly HistoryTurn[];
  tools: readonly ToolTimingSummary[];
}) {
  const stats = createMemo<readonly EventStats[]>((previous) => {
    const next = props.stats.slice(-maxResourceSamples);
    return next.length === previous.length && next[0] === previous[0] && next.at(-1) === previous.at(-1)
      ? previous
      : next;
  }, []);
  const turns = createMemo(() => props.turns);
  const tools = createMemo(() => props.tools);
  const [tokenShare, setTokenShare] = createSignal(false);
  const tokenLabel = () => (tokenShare() ? "Token share by turn" : "Token volume by turn");
  const tokenDescription = () =>
    tokenShare()
      ? "Each bar is one completed turn normalized to its own total, so the ratio between cache reads and the other categories stays visible."
      : "Each bar is one completed turn's tokens, stacked by category; cache reads usually dominate the total.";
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
        <ResourceCharts stats={stats()} scope="task" />
      </Show>
      <Show when={turns().length > 0}>
        <div class={styles.figure} data-testid="turn-token-chart">
          <div class={styles.figureHeader}>
            <div class={styles.title}>Tokens by turn</div>
            <ToggleChip
              checked={tokenShare()}
              title="Show each turn as a share of its own tokens"
              onChange={setTokenShare}
            >
              Share of turn
            </ToggleChip>
          </div>
          <PlotHost
            label={tokenLabel()}
            description={tokenDescription()}
            draw={(width) => drawTokenChart(turns(), width, tokenShare())}
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
          <div class={styles.title}>Tool time by kind</div>
          <PlotHost
            label="Cumulative tool time by tool kind"
            description="Completed calls; concurrent durations may overlap."
            draw={(width) => drawToolChart(tools(), width)}
          />
          <div class={styles.note}>Completed calls; concurrent durations may overlap.</div>
          <ChartDataTable
            caption="Tool timings"
            columns={["Tool", "Calls", "Time"]}
            regionLabel="Cumulative tool time by kind"
            rows={tools().map((tool) => [tool.name, String(tool.calls), formatTimingDuration(tool.durationMs)])}
          />
        </div>
      </Show>
    </>
  );
}
