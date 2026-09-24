// StatsIcon links from a task header to its full usage and performance view.

import { Show } from "solid-js";
import { A } from "@solidjs/router";

import type { EventStats } from "@sdk/types.gen";

import { formatBytes, formatTokens } from "../formatting";
import type { TaskUsageSummary } from "./StatsDetail";
import styles from "./StatsIcon.module.css";

function formatUSD(usd: number): string {
  return `$${usd.toFixed(usd < 0.01 ? 4 : 2)}`;
}

function totalTokens(usage: TaskUsageSummary): number {
  return usage.inputTokens + usage.cacheWriteInputTokens + usage.cacheReadInputTokens + usage.outputTokens;
}

// A bar fills toward its metric's ceiling, and the same fraction decides its
// color, so height and color always agree. Fixed ceilings keep the glyph
// comparable between tasks and keep its shape stable as a task's history grows.
const netCeilingBytes = 1e9;
const diskCeilingBytes = 10e9;

function clampRatio(value: number): number {
  return Math.min(1, Math.max(0, value));
}

// A bar turns amber at half its ceiling and red at 85% of it.
function barClass(ratio: number): string {
  if (ratio >= 0.85) return styles.barDanger;
  if (ratio >= 0.5) return styles.barWarning;
  return styles.barSuccess;
}

// The glyph is decorative art, so its state travels to assistive technology as text.
function resourceAnnouncement(stat: EventStats | undefined): string {
  if (!stat) return "No resource samples yet.";
  const disk = stat.diskUsed >= 0 ? formatBytes(stat.diskUsed) : "unavailable";
  return `${stat.cpuPerc.toFixed(1)}% CPU, ${stat.memPerc.toFixed(1)}% memory, ${formatBytes(
    stat.netRx + stat.netTx,
  )} transferred, ${disk} disk used.`;
}

export default function StatsIcon(props: { href: string; stats: EventStats[]; usage: TaskUsageSummary }) {
  const latest = () => props.stats.at(-1);
  const cpuRatio = () => clampRatio((latest()?.cpuPerc ?? 0) / 100);
  const memRatio = () => clampRatio((latest()?.memPerc ?? 0) / 100);
  const netRatio = () => {
    const stat = latest();
    return stat ? clampRatio((stat.netRx + stat.netTx) / netCeilingBytes) : 0;
  };
  const diskRatio = () => {
    const stat = latest();
    return stat ? clampRatio(stat.diskUsed / diskCeilingBytes) : 0;
  };
  const hasStats = () => props.stats.length > 0;
  const tokens = () => totalTokens(props.usage);

  return (
    <A
      class={styles.iconLink}
      href={props.href}
      title="Task usage and performance statistics"
      aria-label="Task statistics"
    >
      <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true" data-generated-svg="">
        <rect
          x="0"
          y={8 - Math.round(cpuRatio() * 8)}
          width="6"
          height={Math.round(cpuRatio() * 8)}
          rx="1"
          class={hasStats() ? barClass(cpuRatio()) : styles.barIdle}
        />
        <rect
          x="10"
          y={8 - Math.round(memRatio() * 8)}
          width="6"
          height={Math.round(memRatio() * 8)}
          rx="1"
          class={hasStats() ? barClass(memRatio()) : styles.barIdle}
        />
        <rect
          x="0"
          y={9 + (8 - Math.round(netRatio() * 8))}
          width="6"
          height={Math.round(netRatio() * 8)}
          rx="1"
          class={hasStats() ? barClass(netRatio()) : styles.barIdle}
        />
        <rect
          x="10"
          y={9 + (8 - Math.round(diskRatio() * 8))}
          width="6"
          height={Math.round(diskRatio() * 8)}
          rx="1"
          class={hasStats() ? barClass(diskRatio()) : styles.barIdle}
        />
      </svg>
      <span class={styles.visuallyHidden}>{resourceAnnouncement(latest())}</span>
      <Show when={tokens() > 0}>
        <span class={styles.iconSummary}>
          {formatTokens(tokens())}
          <Show when={props.usage.costUSD > 0}>
            <span class={styles.iconSummarySeparator}> · </span>
            {formatUSD(props.usage.costUSD)}
          </Show>
        </span>
      </Show>
    </A>
  );
}
