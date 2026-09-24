// UsageCharts renders daily token and cost trends from selected rollup days.

import * as Plot from "@observablehq/plot";
import { Show } from "solid-js";

import type { UsageDashboardDay } from "@sdk/types.gen";

import { formatCost, formatTokens } from "../formatting";
import ChartDataTable from "./ChartDataTable";
import PlotHost from "./PlotHost";
import styles from "./UsageCharts.module.css";

interface DailyUsageDatum {
  day: string;
  tokens: number;
  costUSD: number;
  inProgress: boolean;
}

// The rollup already holds the current UTC day, which is still accumulating. Its
// total is drawn from a dashed segment with a hollow marker and stated in the
// note, so a partial day cannot be read as a completed fall in usage.
function currentUTCDay(): string {
  return new Date().toISOString().slice(0, 10);
}

function dailyUsage(days: readonly UsageDashboardDay[]): DailyUsageDatum[] {
  const today = currentUTCDay();
  return days.map((day) => ({
    day: day.day,
    inProgress: day.day === today,
    tokens:
      day.tokens.inputTokens +
      day.tokens.cacheWrite5mTokens +
      day.tokens.cacheWrite1hTokens +
      day.tokens.cacheReadTokens +
      day.tokens.outputTokens,
    costUSD: day.costUSD,
  }));
}

interface TrendOptions {
  value: (item: DailyUsageDatum) => number;
  color: string;
  format: (value: number) => string;
  area: boolean;
}

// The completed days draw the trend; the accumulating day extends it with a
// dashed segment so its provisional value stays visible without joining the trend.
function trendMarks(data: readonly DailyUsageDatum[], options: TrendOptions): Plot.Markish[] {
  const complete = data.filter((item) => !item.inProgress);
  const partial = data.find((item) => item.inProgress);
  const last = complete.at(-1);
  const title = (item: DailyUsageDatum) =>
    `${item.day}${item.inProgress ? " (in progress)" : ""}: ${options.format(options.value(item))}`;
  const x = (item: DailyUsageDatum) => item.day;
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

function drawDailyTrend(
  data: readonly DailyUsageDatum[],
  width: number,
  options: TrendOptions & { y: string; tickFormat: (value: number) => string },
): Element {
  return Plot.plot({
    width,
    height: 180,
    marginLeft: 52,
    marginBottom: 32,
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: "11px" },
    x: { type: "point", domain: data.map((item) => item.day), label: null, tickFormat: (day) => String(day).slice(5) },
    y: { grid: true, label: options.y, tickFormat: options.tickFormat, zero: true },
    marks: trendMarks(data, options),
  });
}

export default function UsageCharts(props: { days: readonly UsageDashboardDay[] }) {
  const data = () => dailyUsage(props.days);
  const accumulating = () => data().some((item) => item.inProgress);
  const description = (subject: string) =>
    `${subject} per UTC day for the selected range.${
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
      <Show when={accumulating()}>
        <p class={styles.note}>The current UTC day is still accumulating; its point is provisional.</p>
      </Show>
      <ChartDataTable
        caption="Daily totals"
        columns={["Day", "Tokens", "Cost"]}
        regionLabel="Daily usage totals"
        rows={data().map((item) => [
          item.inProgress ? `${item.day} (in progress)` : item.day,
          formatTokens(item.tokens),
          formatCost(item.costUSD),
        ])}
      />
    </div>
  );
}
