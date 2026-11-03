// UsageCharts renders daily token and cost trends from drill-down-resolved
// chart points.

import * as Plot from "@observablehq/plot";

import { basePlot, crosshairMarks } from "../chartPlot";
import { formatCost, formatTokens } from "../formatting";
import type { UsageDailyPoint } from "../usageDashboard";
import PlotHost from "./PlotHost";
import styles from "./UsageCharts.module.css";

interface TrendSeries {
  label: string;
  value: (item: UsageDailyPoint) => number;
  color: string;
  format: (value: number) => string;
}

interface TrendOptions {
  series: TrendSeries[];
  y: string;
  // The single-series charts fill under their line.
  area: boolean;
}

// The completed days draw the trend; the accumulating day extends it with a
// dashed segment so its provisional value stays visible without joining the trend.
function trendMarks(data: readonly UsageDailyPoint[], options: TrendOptions): Plot.Markish[] {
  const complete = data.filter((item) => !item.inProgress);
  const partial = data.find((item) => item.inProgress);
  const last = complete.at(-1);
  const x = (item: UsageDailyPoint) => utcDay(item.day);
  const rows = options.series.flatMap((series) =>
    complete.map((item) => ({ ts: x(item), series: series.label, value: series.value(item) })),
  );
  const marks: Plot.Markish[] = [];
  const only = options.series.length === 1 ? options.series[0] : undefined;
  if (only && options.area) {
    marks.push(Plot.areaY(complete, { x, y: only.value, fill: only.color, fillOpacity: 0.2 }));
  }
  // z groups the series explicitly; Plot otherwise infers it from the values.
  marks.push(Plot.lineY(rows, { x: "ts", y: "value", z: "series", stroke: "series", strokeWidth: 2 }));
  if (only) {
    marks.push(Plot.dot(complete, { x, y: only.value, fill: only.color, r: 2.5 }));
  }
  for (const series of options.series) {
    if (partial && last) {
      marks.push(
        Plot.lineY([last, partial], {
          x,
          y: series.value,
          stroke: series.color,
          strokeWidth: 2,
          strokeDasharray: "3 3",
          strokeOpacity: 0.6,
        }),
      );
    }
    if (partial && only) {
      marks.push(
        Plot.dot([partial], {
          x,
          y: only.value,
          fill: "var(--color-bg-surface)",
          stroke: only.color,
          strokeWidth: 1.5,
          r: 2.5,
        }),
      );
    }
  }
  return marks;
}

const plotHeight = 180;
const plotMarginTop = 20;
const plotMarginRight = 20;
const plotMarginBottom = 32;
const plotMarginLeft = 52;

// A day keeps a full-height anchor while it is at least this wide; a denser
// range falls back to a short tick below the axis so the plot area stays clear.
const minDaySpacing = 8;
const dayTickLength = 4;

function utcDay(day: string): Date {
  return new Date(`${day}T00:00:00Z`);
}

// dayAnchors marks every day of the range: vertical rules through the plot while
// the days stay readable, short ticks under the axis once they crowd together.
function dayAnchors(data: readonly UsageDailyPoint[], width: number): Plot.Markish {
  const x = (item: UsageDailyPoint) => utcDay(item.day);
  const plotWidth = width - plotMarginLeft - plotMarginRight;
  const spacing = plotWidth / Math.max(data.length - 1, 1);
  if (spacing >= minDaySpacing) {
    return Plot.ruleX(data, { x, stroke: "var(--color-border)", strokeOpacity: 0.45 });
  }
  return Plot.tickX(data, {
    x,
    stroke: "var(--color-border)",
    strokeOpacity: 0.6,
    insetTop: plotHeight - plotMarginTop - plotMarginBottom - dayTickLength,
  });
}

// readoutLabel names every series for one day, so a hover reads both lines.
function readoutLabel(series: readonly TrendSeries[], item: UsageDailyPoint): string {
  const values = series
    .map((entry) =>
      series.length === 1
        ? entry.format(entry.value(item))
        : `${entry.format(entry.value(item))} ${entry.label.toLowerCase()}`,
    )
    .join(" · ");
  return `${formatDayTick(utcDay(item.day))} · ${values}${item.inProgress ? " (in progress)" : ""}`;
}

function drawDailyTrend(data: readonly UsageDailyPoint[], width: number, options: TrendOptions): Element {
  const primary = options.series[0];
  if (!primary) throw new Error("A daily trend needs one series");
  const legend = options.series.length > 1;
  return Plot.plot({
    ...basePlot(width, plotHeight),
    marginTop: plotMarginTop,
    marginRight: plotMarginRight,
    marginBottom: plotMarginBottom,
    marginLeft: plotMarginLeft,
    // A UTC day scale keeps the daily spacing the series guarantees. The ticks
    // are explicit so the range never starts on an unlabelled day, unlike the
    // interval d3 picks on its own.
    x: {
      type: "utc",
      domain: data.map((item) => utcDay(item.day)),
      label: null,
      ticks: dayTicks(data, width),
      tickFormat: formatDayTick,
    },
    // The direction arrow repeats the axis label, which already names the unit.
    y: { grid: true, label: options.y, labelArrow: false, tickFormat: primary.format, zero: true },
    // A second series needs its name on screen; the axis cannot carry two.
    color: {
      domain: options.series.map((series) => series.label),
      range: options.series.map((series) => series.color),
      legend,
    },
    marks: [
      dayAnchors(data, width),
      ...trendMarks(data, options),
      // The same pointer readout the resource charts use, so one hover gesture
      // reads a point in every chart the app draws.
      ...crosshairMarks(
        data.map((item) => ({
          x: utcDay(item.day).getTime(),
          value: primary.value(item),
          label: readoutLabel(options.series, item),
        })),
        { color: primary.color, plotWidth: width - plotMarginLeft - plotMarginRight },
      ),
    ],
  });
}

// dayTicks lists the days that keep a label: the first day of the range, then
// every step that fits the width, plus the last day when it does not crowd the
// previous label.
function dayTicks(data: readonly UsageDailyPoint[], width: number): Date[] {
  const first = data[0];
  if (!first) return [];
  const plotWidth = width - plotMarginLeft - plotMarginRight;
  const spacing = plotWidth / Math.max(data.length - 1, 1);
  const maxLabels = Math.max(2, Math.floor(plotWidth / 48));
  const step = Math.max(1, Math.ceil(data.length / maxLabels));
  const ticks: Date[] = [];
  for (let index = 0; index < data.length; index += step) {
    ticks.push(utcDay(data[index]?.day ?? first.day));
  }
  const last = data.at(-1);
  const previous = ticks.at(-1);
  if (last && previous && utcDay(last.day).getTime() !== previous.getTime()) {
    const gap = utcDay(last.day).getTime() - previous.getTime();
    if (gap >= 40 * (86_400_000 / spacing)) ticks.push(utcDay(last.day));
  }
  return ticks;
}

// formatDayTick labels one UTC day as MM-DD.
function formatDayTick(value: Date): string {
  const month = String(value.getUTCMonth() + 1).padStart(2, "0");
  const day = String(value.getUTCDate()).padStart(2, "0");
  return `${month}-${day}`;
}

export default function UsageCharts(props: {
  points: readonly UsageDailyPoint[];
  // The totals the page sums over the same range, shown beside each title.
  totals: { tokens: number; costUSD: number; turns: number; subagentSpawns: number };
  subject?: string;
}) {
  const data = () => props.points;
  const accumulating = () => data().some((item) => item.inProgress);
  const scope = () => (props.subject ? ` for ${props.subject}` : "");
  const description = (subject: string) =>
    `${subject} per UTC day for the selected range${scope()}.${
      accumulating() ? " The current day is still accumulating, so its point is provisional." : ""
    }`;
  const count = (value: number) => String(Math.round(value));
  const tokens: TrendSeries = {
    label: "Tokens",
    value: (item) => item.tokens,
    color: "var(--color-primary)",
    format: formatTokens,
  };
  const cost: TrendSeries = {
    label: "Cost",
    value: (item) => item.costUSD,
    color: "var(--color-plan)",
    format: formatCost,
  };
  const turns: TrendSeries[] = [
    { label: "Turns", value: (item) => item.turns, color: "var(--color-primary)", format: count },
    { label: "Compactions", value: (item) => item.compactions, color: "var(--color-plan)", format: count },
  ];
  const spawns: TrendSeries = {
    label: "Subagents",
    value: (item) => item.subagentSpawns,
    color: "var(--color-success)",
    format: count,
  };

  return (
    <div data-testid="usage-charts">
      <div class={styles.grid}>
        <section class={styles.figure}>
          <header class={styles.header}>
            <h2>Tokens per day</h2>
            <strong aria-label={`Total ${formatTokens(props.totals.tokens)}`} title="Total over the selected range">
              {formatTokens(props.totals.tokens)}
            </strong>
          </header>
          <PlotHost
            label="Total tokens per day"
            description={description("Total tokens")}
            draw={(width) => drawDailyTrend(data(), width, { area: true, series: [tokens], y: "Tokens" })}
          />
        </section>
        <section class={styles.figure}>
          <header class={styles.header}>
            <h2>Cost per day</h2>
            <strong aria-label={`Total ${formatCost(props.totals.costUSD)}`} title="Total over the selected range">
              {formatCost(props.totals.costUSD)}
            </strong>
          </header>
          <PlotHost
            label="Reported cost per day in US dollars"
            description={description("Reported cost in US dollars")}
            draw={(width) => drawDailyTrend(data(), width, { area: false, series: [cost], y: "USD" })}
          />
        </section>
        <section class={styles.figure}>
          <header class={styles.header}>
            <h2>Turns per day</h2>
            <strong aria-label={`Total ${count(props.totals.turns)}`} title="Total over the selected range">
              {count(props.totals.turns)}
            </strong>
          </header>
          <PlotHost
            label="Turns and compactions per day"
            description={description("Turns, with compactions drawn beside them")}
            // One scale carries both counts, so it names the unit, not a series.
            draw={(width) => drawDailyTrend(data(), width, { area: false, series: turns, y: "Count" })}
          />
        </section>
        <section class={styles.figure}>
          <header class={styles.header}>
            <h2>Subagents per day</h2>
            <strong aria-label={`Total ${count(props.totals.subagentSpawns)}`} title="Total over the selected range">
              {count(props.totals.subagentSpawns)}
            </strong>
          </header>
          <PlotHost
            label="Subagent spawns per day"
            description={description("Subagent spawns")}
            draw={(width) => drawDailyTrend(data(), width, { area: false, series: [spawns], y: "Subagents" })}
          />
        </section>
      </div>
    </div>
  );
}
