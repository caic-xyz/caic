// Usage badges showing API utilization with color-coded thresholds.
import { Show } from "solid-js";
import type { Accessor } from "solid-js";
import type { ClaudeExtraUsage, ClaudeUsageWindow, CodexRateLimitWindow, CodexUsage, UsageResp } from "@sdk/types.gen";
import Tooltip from "./Tooltip";
import styles from "./UsageBadges.module.css";

/** Grace period (ms) after resetsAt before the frontend zeroes utilization. */
const RESET_GRACE_MS = 60_000;

/** Return 0 utilization if the reset timestamp has passed (plus grace). */
function effectiveUtilization(w: ClaudeUsageWindow, now: number): number {
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

function Badge(props: { label: string; window: ClaudeUsageWindow; now: Accessor<number>; yellowAt: number; redAt: number }) {
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

function formatSeconds(seconds: number): string {
  if (seconds <= 0) return "now";
  const hours = Math.floor(seconds / 3600);
  const mins = Math.floor((seconds % 3600) / 60);
  if (hours >= 24) {
    const days = Math.floor(hours / 24);
    return `in ${days}d ${hours % 24}h`;
  }
  if (hours > 0) return `in ${hours}h ${mins}m`;
  return `in ${mins}m`;
}

function CodexWindowBadge(props: { label: string; window: CodexRateLimitWindow; yellowAt: number; redAt: number }) {
  const pct = () => Math.min(props.window.usedPercent, 100);
  const cls = () => (pct() >= props.redAt ? styles.red : pct() >= props.yellowAt ? styles.yellow : styles.green);
  return (
    <Tooltip text={`Resets ${formatSeconds(props.window.resetAfterSeconds)}`}>
      <span class={`${styles.badge} ${cls()}`}>
        {props.label}: {pct()}%
      </span>
    </Tooltip>
  );
}

function CodexBadges(props: { codex: CodexUsage }) {
  return (
    <>
      <Show when={props.codex.primary}>
        {(w) => <CodexWindowBadge label="Codex" window={w()} yellowAt={80} redAt={90} />}
      </Show>
      <Show when={props.codex.secondary}>
        {(w) => <CodexWindowBadge label="Codex 2" window={w()} yellowAt={90} redAt={95} />}
      </Show>
      <Show when={props.codex.credits.balance}>
        <Tooltip text={`Balance: $${props.codex.credits.balance}`}>
          <span class={`${styles.badge} ${props.codex.credits.hasCredits ? styles.green : styles.red}`}>
            Codex: ${props.codex.credits.balance}
          </span>
        </Tooltip>
      </Show>
    </>
  );
}

function ExtraBadge(props: { extra: ClaudeExtraUsage }) {
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
    <span class={styles.usageRow}>
      <Show when={props.usage()} keyed>
        {(u) => (
          <>
            <Show when={u.claude?.fiveHour}>
              {(w) => <Badge label="5h" window={w()} now={props.now} yellowAt={80} redAt={90} />}
            </Show>
            <Show when={u.claude?.sevenDay}>
              {(w) => <Badge label="7d" window={w()} now={props.now} yellowAt={90} redAt={95} />}
            </Show>
            <Show when={u.claude?.extraUsage}>
              {(extra) => <ExtraBadge extra={extra()} />}
            </Show>
            <Show when={u.codex}>
              {(c) => <CodexBadges codex={c()} />}
            </Show>
          </>
        )}
      </Show>
    </span>
  );
}
