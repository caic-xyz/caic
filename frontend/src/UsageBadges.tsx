// Usage badges showing API utilization with color-coded thresholds.
import { Show } from "solid-js";
import type { Accessor } from "solid-js";
import type { UsageResp } from "@sdk/types.gen";
import styles from "./UsageBadges.module.css";

function formatReset(iso: string): string {
  const d = new Date(iso);
  const now = Date.now();
  const diffMs = d.getTime() - now;
  if (diffMs <= 0) return "now";
  const hours = Math.floor(diffMs / 3_600_000);
  const mins = Math.floor((diffMs % 3_600_000) / 60_000);
  if (hours >= 24) {
    const days = Math.floor(hours / 24);
    return `in ${days}d ${hours % 24}h`;
  }
  if (hours > 0) return `in ${hours}h ${mins}m`;
  return `in ${mins}m`;
}

function Badge(props: { label: string; utilization: number; resetsAt: string }) {
  const pct = () => Math.round(props.utilization);
  const cls = () => (pct() >= 80 ? styles.red : pct() >= 50 ? styles.yellow : styles.green);
  return (
    <span class={`${styles.badge} ${cls()}`} title={`Resets ${formatReset(props.resetsAt)}`}>
      {props.label}: {pct()}%
    </span>
  );
}

export default function UsageBadges(props: { usage: Accessor<UsageResp | null> }) {
  return (
    <Show when={props.usage()} keyed>
      {(u) => (
        <span class={styles.usageRow}>
          <Show when={u.fiveHour} keyed>
            {(w) => <Badge label="5h" utilization={w.utilization} resetsAt={w.resetsAt} />}
          </Show>
          <Show when={u.sevenDay} keyed>
            {(w) => <Badge label="Weekly" utilization={w.utilization} resetsAt={w.resetsAt} />}
          </Show>
        </span>
      )}
    </Show>
  );
}
