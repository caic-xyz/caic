// Compact summary card for a single task, used in the sidebar task list.
import { Show } from "solid-js";
import type { Accessor } from "solid-js";
import styles from "./TaskItemSummary.module.css";

export interface TaskItemSummaryProps {
  task: string;
  state: string;
  stateUpdatedAt: number;
  repo: string;
  branch: string;
  model?: string;
  claudeCodeVersion?: string;
  costUSD: number;
  durationMs: number;
  numTurns: number;
  containerUptimeMs?: number;
  error?: string;
  selected: boolean;
  now: Accessor<number>;
  onClick: () => void;
}

export default function TaskItemSummary(props: TaskItemSummaryProps) {
  return (
    <div
      onClick={() => props.onClick()}
      class={`${styles.card} ${props.selected ? styles.selected : ""}`}
    >
      <div class={styles.header}>
        <strong class={styles.title}>{props.task}</strong>
        <span class={styles.stateGroup}>
          <span class={styles.badge} style={{ background: stateColor(props.state) }}>
            {props.state}
          </span>
          <Show when={props.stateUpdatedAt > 0 && props.state !== "terminated"}>
            <StateDuration stateUpdatedAt={props.stateUpdatedAt} now={props.now} />
          </Show>
        </span>
      </div>
      <Show when={props.repo}>
        <div class={styles.meta}>{props.repo}</div>
      </Show>
      <Show when={props.branch}>
        <div class={styles.branch}>{props.branch}</div>
      </Show>
      <Show when={props.model}>
        <div class={styles.meta}>{props.model}{props.claudeCodeVersion ? ` · ${props.claudeCodeVersion}` : ""}</div>
      </Show>
      <Show when={props.costUSD > 0}>
        <span class={styles.cost}>
          ${props.costUSD.toFixed(4)} &middot; {(props.durationMs / 1000).toFixed(1)}s &middot; {props.numTurns} turns
        </span>
      </Show>
      <Show when={(props.containerUptimeMs ?? 0) > 0}>
        <span class={styles.cost}>
          container {formatUptime(props.containerUptimeMs ?? 0)}
        </span>
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
