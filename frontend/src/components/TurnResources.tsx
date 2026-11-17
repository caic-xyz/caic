// TurnResources draws the task's retained CPU and memory samples within one turn as lanes on the turn timeline's axis, with exact values.

import { createMemo, For, Show } from "solid-js";

import type { EventStats } from "@sdk/types.gen";
import { formatExactSeconds, formatTotalDuration } from "../timing";
import ChartDataTable from "./ChartDataTable";
import styles from "./TurnResources.module.css";

const columns = ["At", "CPU", "Memory", "Memory used"] as const;

interface Series {
  key: "cpu" | "memory";
  name: string;
  summary: string;
  // Each point is a share of the axis and of the series' explicit domain.
  points: readonly { x: number; y: number }[];
}

// coverage states why a turn draws no samples, or that it draws only part of
// its window because older samples left the full ring of retained samples;
// null otherwise, and for a turn older than every retained sample. A ring that
// is not full has dropped nothing: sampling simply had not reached the turn's
// start. An unknown capacity, zero, drops nothing.
function coverage(
  stats: readonly EventStats[],
  retained: number,
  start: number,
  end: number,
  shown: number,
): string | null {
  const first = stats[0];
  const last = stats.at(-1);
  if (!first || !last) return "No resource samples are retained for this task.";
  const dropped = retained > 0 && stats.length >= retained && first.ts > start;
  if (shown === 0) {
    if (dropped && first.ts > end) return null;
    if (last.ts < start) return "Resource samples stopped before this turn.";
    return "No resource sample falls within this turn.";
  }
  if (dropped)
    return `Samples start ${formatTotalDuration(first.ts - start)} into the turn; earlier ones are no longer retained.`;
  return null;
}

function percent(value: number): string {
  return `${value.toFixed(1)}%`;
}

export default function TurnResources(props: {
  stats: readonly EventStats[];
  // retained is the server's resource ring capacity; zero is unknown.
  retained: number;
  // startMs is the turn start, in milliseconds since the epoch.
  startMs: number;
  durationMs: number;
  // tickShare is the timeline's gridline step as a share of the axis.
  tickShare: number;
}) {
  const end = () => props.startMs + props.durationMs;
  const samples = createMemo(() => props.stats.filter((sample) => sample.ts >= props.startMs && sample.ts <= end()));
  const note = () => coverage(props.stats, props.retained, props.startMs, end(), samples().length);
  const share = (ts: number) => (props.durationMs > 0 ? (ts - props.startMs) / props.durationMs : 0);
  const series = createMemo<Series[]>(() => {
    const shown = samples();
    // CPU follows the task resource chart: 100% is one busy CPU, and the
    // scale reaches the configured ceiling when one is known.
    const cpuPeak = Math.max(0, ...shown.map((sample) => sample.cpuPerc));
    const ceiling = shown.at(-1)?.cpuCeiling ?? 0;
    const cpuMax = Math.max(100, Math.ceil(cpuPeak), ceiling * 100);
    const memoryPeak = Math.max(0, ...shown.map((sample) => sample.memPerc));
    return [
      {
        key: "cpu",
        name: "CPU",
        summary: `peak ${percent(cpuPeak)} · scale 0–${cpuMax}%, 100% = 1 CPU`,
        points: shown.map((sample) => ({ x: share(sample.ts), y: sample.cpuPerc / cpuMax })),
      },
      {
        key: "memory",
        name: "Memory",
        summary: `peak ${percent(memoryPeak)} of limit · scale 0–100%`,
        points: shown.map((sample) => ({ x: share(sample.ts), y: sample.memPerc / 100 })),
      },
    ];
  });
  const rows = () =>
    samples().map((sample) => [
      formatExactSeconds(sample.ts - props.startMs),
      percent(sample.cpuPerc),
      percent(sample.memPerc),
      `${(sample.memUsed / 2 ** 20).toFixed(1)} MiB`,
    ]);

  return (
    <div class={styles.resources} data-testid="turn-resources">
      <Show when={samples().length > 0}>
        <div class={styles.lanes} aria-hidden="true">
          <For each={series()}>
            {(item) => (
              <div class={styles.lane} data-series={item.key} data-testid="turn-resource-lane">
                <div class={styles.label}>
                  {item.name} <span class={styles.summary}>{item.summary}</span>
                </div>
                <div class={styles.track} style={{ "--tick-share": props.tickShare }}>
                  <svg class={styles.line} viewBox="0 0 1000 100" preserveAspectRatio="none" data-generated-svg="">
                    <polyline points={item.points.map((p) => `${p.x * 1000},${(1 - p.y) * 100}`).join(" ")} />
                  </svg>
                  <For each={item.points}>
                    {(point) => <span class={styles.dot} style={{ "--x": point.x, "--y": point.y }} />}
                  </For>
                </div>
              </div>
            )}
          </For>
        </div>
      </Show>
      <Show when={note()}>
        {(text) => (
          <p class={styles.note} data-testid="turn-resources-note">
            {text()}
          </p>
        )}
      </Show>
      <Show when={samples().length > 0}>
        <ChartDataTable
          caption="Resource samples, CPU 100% per CPU"
          regionLabel="Resource samples in this turn"
          columns={columns}
          rows={rows()}
        />
      </Show>
    </div>
  );
}
