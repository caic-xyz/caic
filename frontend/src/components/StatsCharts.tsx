// Compact task charts for resource history, token composition, and cumulative tool time.

import * as Plot from "@observablehq/plot";
import { createMemo, createSignal, Show } from "solid-js";

import type { EventStats } from "@sdk/types.gen";

import { formatBytes, formatTokens } from "../formatting";
import { deriveNetworkRates, type ToolTimingSummary } from "../taskStats";
import type { TurnTiming } from "../timing";
import { formatTimingDuration } from "../timing";
import ChartDataTable from "./ChartDataTable";
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
// Half the width of the longest pointer readout ("RX 1.0 KB/s"). The readout is
// centered on the focused sample, so it parks this far inside the frame rather
// than being clipped by the viewport at the edges of the sampled range.
const readoutHalfWidth = 34;

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
  return `${(share * 100).toFixed(1)}%`;
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

function formatExactBytes(value: number, perSecond: boolean): string {
  const unit = perSecond ? "B/s" : "B";
  const compact = `${formatBytes(value)}${perSecond ? "/s" : ""}`;
  return `${compact} (${String(value)} ${unit})`;
}

function formatExactSampleTime(ts: number): string {
  return new Date(ts).toISOString();
}

function tokenData(turns: readonly TurnTiming[], asShare: boolean): TokenDatum[] {
  return turns.flatMap((turn, i) => {
    const usage = turn.result.usage;
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
function drawTokenChart(turns: readonly TurnTiming[], width: number, asShare: boolean): Element {
  const data = tokenData(turns, asShare);
  return Plot.plot({
    width,
    height: 180,
    marginLeft: 48,
    marginBottom: 32,
    className: "caic-plot",
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "10px" },
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
  const domain = data.length > 0 ? [data[0].ts, data[data.length - 1].ts] : undefined;
  const available = data.filter((sample): sample is ResourceDatum & { value: number } => sample.value !== null);
  const showAxis = options.axisAnchor !== null && options.axisFormat !== null && options.maxValue !== null;
  const marginLeft = showAxis && options.axisAnchor === "left" ? 40 : 3;
  const marginRight = showAxis && options.axisAnchor === "right" ? 40 : 3;
  const span = domain ? domain[1] - domain[0] : 0;
  const readoutInset = span > 0 ? (readoutHalfWidth / (width - marginLeft - marginRight)) * span : 0;
  const readoutTs = (sample: ResourceDatum) =>
    Math.min(Math.max(sample.ts, (domain?.[0] ?? sample.ts) + readoutInset), (domain?.[1] ?? sample.ts) - readoutInset);
  // The crosshair follows the pointer in one dimension along the sampled range;
  // px and py stay bound to the sample so every pointer mark focuses the same one.
  const focus = Plot.pointerX({ px: "ts", py: "value" });
  return Plot.plot({
    width,
    height: options.height,
    margin: 3,
    marginBottom: showAxis ? 8 : 3,
    marginLeft,
    marginRight,
    marginTop: showAxis ? 8 : 3,
    className: "caic-plot",
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "10px" },
    x: { axis: null, domain },
    y: showAxis
      ? {
          axis: options.axisAnchor,
          domain: [0, options.maxValue],
          label: null,
          tickFormat: options.axisFormat,
          tickPadding: 2,
          ticks: [0, options.maxValue],
          tickSize: 2,
          zero: true,
        }
      : options.maxValue === null
        ? { axis: null, zero: true }
        : { axis: null, domain: [0, options.maxValue], zero: true },
    marks: [
      Plot.lineY(data, { x: "ts", y: "value", stroke: options.color, strokeWidth: 1.5 }),
      Plot.dot(available, { x: "ts", y: "value", fill: options.color, r: 1.8 }),
      Plot.ruleX(data, { ...focus, x: "ts", stroke: options.color, strokeOpacity: 0.45 }),
      // Samples with no measurement (disk before it reports, throughput before a
      // second sample) have no readout to show, so they leave the rule alone.
      Plot.text(data, {
        ...focus,
        x: readoutTs,
        frameAnchor: "top",
        dy: 2,
        textAnchor: "middle",
        fill: "var(--color-text-secondary)",
        stroke: "var(--color-bg-surface)",
        strokeWidth: 3,
        text: (sample: ResourceDatum) => (sample.value === null ? null : options.formatValue(sample.value)),
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
  const disk = () =>
    props.stats.map((sample) => ({
      ts: sample.ts,
      value: sample.diskUsed >= 0 ? sample.diskUsed : null,
    }));
  const networkRates = createMemo(() => deriveNetworkRates(props.stats));
  const rx = () => networkRates().map((sample) => ({ ts: sample.ts, value: sample.rxBytesPerSecond }));
  const tx = () => networkRates().map((sample) => ({ ts: sample.ts, value: sample.txBytesPerSecond }));
  const exactSampleRows = () =>
    props.stats
      .map((sample, index) => ({ rate: networkRates()[index], sample }))
      .reverse()
      .map(({ rate, sample }) => [
        formatExactSampleTime(sample.ts),
        `${String(sample.cpuPerc)}%`,
        `${String(sample.memPerc)}%`,
        rate.rxBytesPerSecond === null ? "—" : formatExactBytes(rate.rxBytesPerSecond, true),
        rate.txBytesPerSecond === null ? "—" : formatExactBytes(rate.txBytesPerSecond, true),
        sample.diskUsed < 0 ? "—" : formatExactBytes(sample.diskUsed, false),
      ]);

  return (
    <div class={styles.resourceGrid} data-testid="resource-charts">
      <div class={styles.resourceRow}>
        <div class={styles.resourceHeader}>
          <strong>CPU</strong>
          <span>
            {latest()?.cpuPerc.toFixed(1)}%<Show when={cpuObservedMax() > 100}> · max {String(cpuObservedMax())}%</Show>
          </span>
        </div>
        <PlotHost
          label="CPU utilization over time"
          draw={(width) =>
            drawResourceChart(cpu(), width, {
              axisAnchor: "left",
              axisFormat: (value) => `${Math.round(value)}%`,
              color: "var(--color-primary)",
              formatValue: (value) => `${value.toFixed(1)}%`,
              height: 58,
              label: "CPU utilization",
              maxValue: cpuScaleMax(),
            })
          }
        />
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
              axisAnchor: null,
              axisFormat: null,
              color: "var(--color-success)",
              formatValue: (value) => `${value.toFixed(1)}%`,
              height: 58,
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
                    height: 44,
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
                    height: 44,
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
          <span>{(latest()?.diskUsed ?? -1) >= 0 ? formatBytes(latest()?.diskUsed ?? 0) : "Unavailable"}</span>
        </div>
        <Show
          when={disk().some((sample) => sample.value !== null)}
          fallback={<div class={styles.resourceUnavailable}>No disk history available</div>}
        >
          <PlotHost
            label="Writable disk usage over time"
            draw={(width) =>
              drawResourceChart(disk(), width, {
                axisAnchor: null,
                axisFormat: null,
                color: "var(--color-warning-text)",
                formatValue: formatBytes,
                height: 58,
                label: "Writable disk usage",
                maxValue: null,
              })
            }
          />
        </Show>
      </div>
      <div class={styles.resourceRange}>{timeRange()}</div>
      <ChartDataTable
        caption="Exact samples"
        class={styles.sampleDetails}
        columns={["Time", "CPU", "Memory", "RX/s", "TX/s", "Disk"]}
        regionLabel="Exact resource samples"
        rows={exactSampleRows()}
      />
    </div>
  );
}

function drawToolChart(tools: readonly ToolTimingSummary[], width: number): Element {
  const height = Math.max(92, tools.length * 25 + 34);
  return Plot.plot({
    width,
    height,
    marginLeft: Math.min(150, Math.max(62, Math.max(...tools.map((tool) => tool.name.length)) * 7)),
    marginBottom: 28,
    className: "caic-plot",
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "10px" },
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
  turns: readonly TurnTiming[];
  tools: readonly ToolTimingSummary[];
}) {
  const stats = createMemo<readonly EventStats[]>((previous) => {
    const next = props.stats.slice(-maxResourceSamples);
    return next.length === previous.length && next[0] === previous[0] && next.at(-1) === previous.at(-1)
      ? previous
      : next;
  }, []);
  const turns = createMemo<readonly TurnTiming[]>((previous) => {
    const next = props.turns;
    return next.length === previous.length && next.at(-1)?.event === previous.at(-1)?.event ? previous : next;
  }, []);
  const [tokenShare, setTokenShare] = createSignal(false);
  const tokenLabel = () => (tokenShare() ? "Token share by turn" : "Token volume by turn");
  const tokenDescription = () =>
    tokenShare()
      ? "Each bar is one completed turn normalized to its own total, so the ratio between cache reads and the other categories stays visible."
      : "Each bar is one completed turn's tokens, stacked by category; cache reads usually dominate the total.";
  const tokenRows = () =>
    turns().map((turn, index) => {
      const usage = turn.result.usage;
      const total =
        usage.inputTokens + usage.cacheCreationInputTokens + usage.cacheReadInputTokens + usage.outputTokens;
      return [
        `Turn ${index + 1}`,
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
        <ResourceCharts stats={stats()} />
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
            columns={["Turn", "New input", "Cache write", "Cache read", "Output", "Total"]}
            regionLabel="Token counts by turn"
            rows={tokenRows()}
          />
        </div>
      </Show>
      <Show when={props.tools.length > 0}>
        <div class={styles.figure} data-testid="tool-time-chart">
          <div class={styles.title}>Tool time by kind</div>
          <PlotHost
            label="Cumulative tool time by tool kind"
            description="Completed calls; concurrent durations may overlap."
            draw={(width) => drawToolChart(props.tools, width)}
          />
          <div class={styles.note}>Completed calls; concurrent durations may overlap.</div>
          <ChartDataTable
            caption="Tool timings"
            columns={["Tool", "Calls", "Time"]}
            regionLabel="Cumulative tool time by kind"
            rows={props.tools.map((tool) => [tool.name, String(tool.calls), formatTimingDuration(tool.durationMs)])}
          />
        </div>
      </Show>
    </>
  );
}
