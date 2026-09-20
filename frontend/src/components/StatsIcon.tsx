// StatsIcon links from a task header to its full usage and performance view.

import { Show } from "solid-js";
import { A } from "@solidjs/router";

import type { EventStats } from "@sdk/types.gen";

import { formatTokens } from "../formatting";
import type { TaskUsageSummary } from "./StatsDetail";
import styles from "./StatsIcon.module.css";

function formatUSD(usd: number): string {
  return `$${usd.toFixed(usd < 0.01 ? 4 : 2)}`;
}

function totalTokens(usage: TaskUsageSummary): number {
  return usage.inputTokens + usage.cacheWriteInputTokens + usage.cacheReadInputTokens + usage.outputTokens;
}

function barColor(ratio: number): string {
  if (ratio >= 0.85) return "var(--color-danger)";
  if (ratio >= 0.5) return "var(--color-warning-text)";
  return "var(--color-success)";
}

function netColor(bytes: number): string {
  if (bytes >= 1e9) return "var(--color-danger)";
  if (bytes >= 100e6) return "var(--color-warning-text)";
  return "var(--color-success)";
}

function diskColor(bytes: number): string {
  if (bytes >= 10e9) return "var(--color-danger)";
  if (bytes >= 5e9) return "var(--color-warning-text)";
  return "var(--color-success)";
}

export default function StatsIcon(props: { href: string; stats: EventStats[]; usage: TaskUsageSummary }) {
  const latest = () => props.stats.at(-1);
  const maxNet = () => {
    let max = 1;
    for (const stat of props.stats) max = Math.max(max, stat.netRx + stat.netTx);
    return max;
  };
  const maxDisk = () => {
    let max = 1;
    for (const stat of props.stats) max = Math.max(max, stat.diskUsed);
    return max;
  };
  const cpuRatio = () => Math.min(1, (latest()?.cpuPerc ?? 0) / 100);
  const memRatio = () => Math.min(1, (latest()?.memPerc ?? 0) / 100);
  const netRatio = () => {
    const stat = latest();
    return stat ? Math.min(1, (stat.netRx + stat.netTx) / maxNet()) : 0;
  };
  const diskRatio = () => {
    const stat = latest();
    return stat ? Math.min(1, Math.max(0, stat.diskUsed) / maxDisk()) : 0;
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
      <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
        <rect
          x="0"
          y={8 - Math.round(cpuRatio() * 8)}
          width="6"
          height={Math.round(cpuRatio() * 8)}
          rx="1"
          fill={hasStats() ? barColor(cpuRatio()) : "var(--color-border)"}
        />
        <rect
          x="10"
          y={8 - Math.round(memRatio() * 8)}
          width="6"
          height={Math.round(memRatio() * 8)}
          rx="1"
          fill={hasStats() ? barColor(memRatio()) : "var(--color-border)"}
        />
        <rect
          x="0"
          y={9 + (8 - Math.round(netRatio() * 8))}
          width="6"
          height={Math.round(netRatio() * 8)}
          rx="1"
          fill={hasStats() ? netColor((latest()?.netRx ?? 0) + (latest()?.netTx ?? 0)) : "var(--color-border)"}
        />
        <rect
          x="10"
          y={9 + (8 - Math.round(diskRatio() * 8))}
          width="6"
          height={Math.round(diskRatio() * 8)}
          rx="1"
          fill={hasStats() ? diskColor(latest()?.diskUsed ?? 0) : "var(--color-border)"}
        />
      </svg>
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
