// UsageCharts renders daily token and cost trends from drill-down-resolved
// chart points.

import * as Plot from "@observablehq/plot";

import { formatCost, formatTokens } from "../formatting";
import type { UsageDailyPoint } from "../usageDashboard";
import PlotHost from "./PlotHost";
import styles from "./UsageCharts.module.css";

interface TrendOptions {
  value: (item: UsageDailyPoint) => number;
  color: string;
  format: (value: number) => string;
  area: boolean;
}

// The completed days draw the trend; the accumulating day extends it with a
// dashed segment so its provisional value stays visible without joining the trend.
function trendMarks(data: readonly UsageDailyPoint[], options: TrendOptions): Plot.Markish[] {
  const complete = data.filter((item) => !item.inProgress);
  const partial = data.find((item) => item.inProgress);
  const last = complete.at(-1);
  const title = (item: UsageDailyPoint) =>
    `${item.day}${item.inProgress ? " (in progress)" : ""}: ${options.format(options.value(item))}`;
  const x = (item: UsageDailyPoint) => utcDay(item.day);
  const marks: Plot.Markish[] = [];
  if (options.area) {
    marks.push(Plot.areaY(complete, { x, y: options.value, fill: options.color, fillOpacity: 0.2 }));
  }
  marks.push(Plot.lineY(complete, { x, y: options.value, stroke: options.color, strokeWidth: 2 }));
  marks.push(Plot.dot(complete, { x, y: options.value, fill: options.color, r: 2.5, title }));
  if (partial && last) {
    marks.push(
      Plot.lineY([last, partial], {
        x,
        y: options.value,
        stroke: options.color,
        strokeWidth: 2,
        strokeDasharray: "3 3",
        strokeOpacity: 0.6,
      }),
    );
  }
  if (partial) {
    marks.push(
      Plot.dot([partial], {
        x,
        y: options.value,
        fill: "var(--color-bg-surface)",
        stroke: options.color,
        strokeWidth: 1.5,
        r: 2.5,
        title,
      }),
    );
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

function drawDailyTrend(
  data: readonly UsageDailyPoint[],
  width: number,
  options: TrendOptions & { y: string; tickFormat: (value: number) => string },
): Element {
  return Plot.plot({
    width,
    height: plotHeight,
    marginTop: plotMarginTop,
    marginRight: plotMarginRight,
    marginBottom: plotMarginBottom,
    marginLeft: plotMarginLeft,
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "11px" },
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
    y: { grid: true, label: options.y, labelArrow: false, tickFormat: options.tickFormat, zero: true },
    marks: [dayAnchors(data, width), ...trendMarks(data, options)],
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

export default function UsageCharts(props: { points: readonly UsageDailyPoint[]; subject?: string }) {
  const data = () => props.points;
  const accumulating = () => data().some((item) => item.inProgress);
  const scope = () => (props.subject ? ` for ${props.subject}` : "");
  const description = (subject: string) =>
    `${subject} per UTC day for the selected range${scope()}.${
      accumulating() ? " The current day is still accumulating, so its point is provisional." : ""
    }`;

  return (
    <div data-testid="usage-charts">
      <div class={styles.grid}>
        <section class={styles.figure}>
          <h2>Tokens per day</h2>
          <PlotHost
            label="Total tokens per day"
            description={description("Total tokens")}
            draw={(width) =>
              drawDailyTrend(data(), width, {
                area: true,
                color: "var(--color-primary)",
                format: formatTokens,
                tickFormat: formatTokens,
                value: (item) => item.tokens,
                y: "Tokens",
              })
            }
          />
        </section>
        <section class={styles.figure}>
          <h2>Cost per day</h2>
          <PlotHost
            label="Reported cost per day in US dollars"
            description={description("Reported cost in US dollars")}
            draw={(width) =>
              drawDailyTrend(data(), width, {
                area: false,
                color: "var(--color-plan)",
                format: formatCost,
                tickFormat: formatCost,
                value: (item) => item.costUSD,
                y: "USD",
              })
            }
          />
        </section>
      </div>
    </div>
  );
}
