// UsageCharts renders daily token and cost trends from drill-down-resolved
// chart points.

import * as Plot from "@observablehq/plot";
import { Show } from "solid-js";

import { formatCost, formatTokens } from "../formatting";
import type { UsageDailyPoint } from "../usageDashboard";
import ChartDataTable from "./ChartDataTable";
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
  const x = (item: UsageDailyPoint) => item.day;
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
  data: readonly UsageDailyPoint[],
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
