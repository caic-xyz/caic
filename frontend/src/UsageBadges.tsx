// Usage badges showing API utilization with color-coded thresholds.
import { Show } from "solid-js";
import type { Accessor } from "solid-js";
import type { ExtraUsage, UsageResp } from "@sdk/types.gen";
import Tooltip from "./Tooltip";
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
    <Tooltip text={`Resets ${formatReset(props.resetsAt)}`}>
      <span class={`${styles.badge} ${cls()}`}>
        {props.label}: {pct()}%
      </span>
    </Tooltip>
  );
}

function ExtraBadge(props: { extra: ExtraUsage }) {
  const pct = () => Math.round(props.extra.utilization);
  const cls = () => (pct() >= 80 ? styles.red : pct() >= 50 ? styles.yellow : styles.green);
  // API values are in cents; convert to dollars for display.
  const used = () => props.extra.usedCredits / 100;
  const limit = () => props.extra.monthlyLimit / 100;
  const title = () => `$${used().toFixed(2)} / $${limit().toFixed(2)}`;
  const hasData = () => props.extra.usedCredits !== 0 || props.extra.monthlyLimit !== 0;
  return (
    <Show when={hasData()}>
      <Tooltip text={props.extra.isEnabled ? title() : `Disabled — ${title()}`}>
        <span class={`${styles.badge} ${props.extra.isEnabled ? cls() : styles.disabled}`}>
          Extra: ${used().toFixed(0)} / ${limit().toFixed(0)}
        </span>
      </Tooltip>
    </Show>
  );
}

export default function UsageBadges(props: { usage: Accessor<UsageResp | null> }) {
  return (
    <Show when={props.usage()} keyed>
      {(u) => (
        <span class={styles.usageRow}>
          <Show when={u.fiveHour.resetsAt}>
            <Badge label="5h" utilization={u.fiveHour.utilization} resetsAt={u.fiveHour.resetsAt} />
          </Show>
          <Show when={u.sevenDay.resetsAt}>
            <Badge label="Weekly" utilization={u.sevenDay.utilization} resetsAt={u.sevenDay.resetsAt} />
          </Show>
          <ExtraBadge extra={u.extraUsage} />
        </span>
      )}
    </Show>
  );
}
