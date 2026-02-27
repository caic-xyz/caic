// Compact summary card for a single task, used in the sidebar task list.
import { Show } from "solid-js";
import type { Accessor } from "solid-js";
import type { DiffStat } from "@sdk/types.gen";
import Tooltip from "./Tooltip";
import Button from "./Button";
import TailscaleIcon from "./tailscale.svg?solid";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import DeleteIcon from "@material-symbols/svg-400/outlined/delete.svg?solid";
import styles from "./TaskItemSummary.module.css";

export interface TaskItemSummaryProps {
  id: string;
  title: string;
  state: string;
  stateUpdatedAt: number;
  repo: string;
  repoURL?: string;
  baseBranch?: string;
  branch: string;
  harness?: string;
  model?: string;
  costUSD: number;
  duration: number;
  numTurns: number;
  activeInputTokens: number;
  activeCacheReadTokens: number;
  cumulativeInputTokens: number;
  cumulativeCacheCreationInputTokens: number;
  cumulativeCacheReadInputTokens: number;
  cumulativeOutputTokens: number;
  contextWindowLimit: number;
  containerUptimeMs?: number;
  diffStat?: DiffStat;
  error?: string;
  inPlanMode?: boolean;
  tailscale?: string;
  usb?: boolean;
  display?: boolean;
  selected: boolean;
  now: Accessor<number>;
  onClick: () => void;
  onTerminate?: () => void;
  terminateLoading?: boolean;
  onDiffClick?: () => void;
}

export default function TaskItemSummary(props: TaskItemSummaryProps) {
  return (
    <div
      data-task-id={props.id}
      tabIndex={0}
      onClick={() => props.onClick()}
      class={`${styles.card} ${props.selected ? styles.selected : ""}`}
    >
      <div class={styles.header}>
        <strong class={styles.title}>{props.title}</strong>
        <span class={styles.stateGroup}>
          <Show when={props.tailscale} keyed>
            {(ts) => ts.startsWith("https://")
              ? <a class={styles.featureIcon} href={ts} target="_blank" rel="noopener" title="Tailscale" onClick={(e) => e.stopPropagation()}><TailscaleIcon width="0.75rem" height="0.75rem" /></a>
              : <span class={styles.featureIcon} title="Tailscale"><TailscaleIcon width="0.75rem" height="0.75rem" /></span>
            }
          </Show>
          <Show when={props.usb}>
            <span class={styles.featureIcon} title="USB"><USBIcon width="0.75rem" height="0.75rem" /></span>
          </Show>
          <Show when={props.display}>
            <span class={styles.featureIcon} title="Display"><DisplayIcon width="0.75rem" height="0.75rem" /></span>
          </Show>
          <Show when={props.onTerminate && props.state !== "terminated" && props.state !== "failed"}>
            <span class={styles.terminateBtn} onClick={(e) => e.stopPropagation()}>
              <Button variant="red" loading={props.terminateLoading} disabled={props.terminateLoading || props.state === "terminating"} onClick={() => props.onTerminate?.()} title="Terminate" data-testid="terminate-task">
                <DeleteIcon width="0.85rem" height="0.85rem" />
              </Button>
            </span>
          </Show>
          <Show when={props.inPlanMode}>
            <span class={styles.planBadge} title="Plan mode">P</span>
          </Show>
          <span class={styles.badge} style={{ background: stateColor(props.state) }}>
            {props.state}
          </span>
        </span>
      </div>
      <Show when={props.repo || props.branch}>
        <div class={styles.metaRow}>
          <span class={styles.meta}>
            <Show when={props.repoURL} fallback={props.repo}>
              <a class={styles.repoLink} href={props.repoURL} target="_blank" rel="noopener" onClick={(e) => e.stopPropagation()}>{props.repo}</a>
            </Show>
            {props.repo && props.branch ? " · " : ""}
            <Show when={props.baseBranch}>
              <span class={styles.baseBranch}>{props.baseBranch}</span>
              <span class={styles.branchArrow}>→</span>
            </Show>
            <span class={styles.branchName}>
              <Show when={props.repoURL?.includes("github.com")} fallback={props.branch}>
                <a class={styles.repoLink} href={`${props.repoURL}/compare/${props.branch}?expand=1`} target="_blank" rel="noopener" onClick={(e) => e.stopPropagation()}>{props.branch}</a>
              </Show>
            </span>
          </span>
          <Show when={props.stateUpdatedAt > 0 && props.state !== "terminated"}>
            <StateDuration stateUpdatedAt={props.stateUpdatedAt} now={props.now} />
          </Show>
        </div>
      </Show>
      <Show when={props.harness || props.model}>
        <div class={styles.metaRow}>
          <span class={styles.meta}>
            {props.harness && props.harness !== "claude" ? props.harness + " · " : ""}{props.model}
            <Show when={props.activeInputTokens + props.activeCacheReadTokens > 0}>
              {" · "}
              <Tooltip text={`Accumulated: ${formatTokens(props.cumulativeCacheReadInputTokens)} cached + ${formatTokens(props.cumulativeInputTokens + props.cumulativeCacheCreationInputTokens)} in + ${formatTokens(props.cumulativeOutputTokens)} out`}>
                <span style={{ color: tokenColor(props.activeInputTokens + props.activeCacheReadTokens, props.contextWindowLimit) }}>
                  {formatTokens(props.activeInputTokens + props.activeCacheReadTokens)}/{formatTokens(props.contextWindowLimit)}
                </span>
              </Tooltip>
            </Show>
            <Show when={props.costUSD > 0}>
              {" · "}${props.costUSD.toFixed(2)}
            </Show>
          </span>
          <Show when={(props.containerUptimeMs ?? 0) > 0}>
            <span class={styles.duration}>{formatUptime(props.containerUptimeMs ?? 0)}</span>
          </Show>
        </div>
      </Show>
      <Show when={props.diffStat?.length ? props.diffStat : undefined} keyed>
        {(ds) => (
          <div
            class={`${styles.meta} ${props.onDiffClick ? styles.diffClickable : ""}`}
            onClick={(e) => { const fn = props.onDiffClick; if (fn) { e.stopPropagation(); fn(); } }}
          >
            {ds.length} file{ds.length !== 1 ? "s" : ""}
            {" "}
            <span class={styles.diffAdded}>+{ds.reduce((s, f) => s + f.added, 0)}</span>
            {" "}
            <span class={styles.diffDeleted}>-{ds.reduce((s, f) => s + f.deleted, 0)}</span>
          </div>
        )}
      </Show>
      <Show when={props.error}>
        <div class={styles.error}>{props.error}</div>
      </Show>
    </div>
  );
}

function StateDuration(props: { stateUpdatedAt: number; now: Accessor<number> }) {
  const elapsed = () => Math.max(0, props.now() - props.stateUpdatedAt * 1000);
  return <span class={styles.duration}>{formatElapsed(elapsed())}</span>;
}

function formatElapsed(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

function formatUptime(ms: number): string {
  const sec = Math.floor(ms / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ${sec % 60}s`;
  const hr = Math.floor(min / 60);
  return `${hr}h ${min % 60}m`;
}

function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}Mt`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(0)}kt`;
  return `${n}t`;
}

function tokenColor(current: number, limit: number): string {
  if (limit <= 0) return "inherit";
  const ratio = current / limit;
  if (ratio >= 0.9) return "#dc3545";
  if (ratio >= 0.75) return "#d4a017";
  return "inherit";
}

function stateColor(state: string): string {
  switch (state) {
    case "running":
      return "#d4edda";
    case "asking":
      return "#cce5ff";
    case "failed":
      return "#f8d7da";
    case "terminating":
      return "#fde2c8";
    case "terminated":
      return "#e2e3e5";
    default:
      return "#fff3cd";
  }
}
