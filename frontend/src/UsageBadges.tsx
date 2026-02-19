// Usage badges showing API utilization with color-coded thresholds.
import { Show } from "solid-js";
import type { Accessor } from "solid-js";
import type { ExtraUsage, UsageResp, UsageWindow } from "@sdk/types.gen";
import Tooltip from "./Tooltip";
import styles from "./UsageBadges.module.css";

/** Grace period (ms) after resetsAt before the frontend zeroes utilization. */
const RESET_GRACE_MS = 60_000;

/** Return 0 utilization if the reset timestamp has passed (plus grace). */
function effectiveUtilization(w: UsageWindow, now: number): number {
  const resetMs = new Date(w.resetsAt).getTime();
  if (now > resetMs + RESET_GRACE_MS) return 0;
  return w.utilization;
}

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

function Badge(props: { label: string; window: UsageWindow; now: Accessor<number>; yellowAt: number; redAt: number }) {
  const pct = () => Math.round(effectiveUtilization(props.window, props.now()));
  const cls = () => (pct() >= props.redAt ? styles.red : pct() >= props.yellowAt ? styles.yellow : styles.green);
  return (
    <Tooltip text={`Resets ${formatReset(props.window.resetsAt)}`}>
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

export default function UsageBadges(props: { usage: Accessor<UsageResp | null>; now: Accessor<number> }) {
  return (
    <Show when={props.usage()} keyed>
      {(u) => (
        <span class={styles.usageRow}>
          <Show when={u.fiveHour.resetsAt}>
            <Badge label="5h" window={u.fiveHour} now={props.now} yellowAt={80} redAt={90} />
          </Show>
          <Show when={u.sevenDay.resetsAt}>
            <Badge label="7d" window={u.sevenDay} now={props.now} yellowAt={90} redAt={95} />
          </Show>
          <ExtraBadge extra={u.extraUsage} />
        </span>
      )}
    </Show>
  );
}
