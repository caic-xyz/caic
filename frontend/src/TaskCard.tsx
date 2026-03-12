// Compact card for a single task, used in the sidebar task list.
import { Show, createSignal, onMount, onCleanup } from "solid-js";
import type { Accessor } from "solid-js";
import type { DiffStat } from "@sdk/types.gen";
import Tooltip from "./Tooltip";
import TailscaleIcon from "./tailscale.svg?solid";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import DeleteIcon from "@material-symbols/svg-400/outlined/delete.svg?solid";
import RestoreIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";
import TimerIcon from "@material-symbols/svg-400/outlined/timer.svg?solid";
import styles from "./TaskCard.module.css";
import { formatElapsed, formatTokens, tokenColor, stateColor } from "./formatting";

export interface TaskCardProps {
  id: string;
  title: string;
  state: string;
  stateUpdatedAt: number;
  repo: string;
  remoteURL?: string;
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
  startedAt?: number;
  turnStartedAt?: number;
  diffStat?: DiffStat;
  error?: string;
  inPlanMode?: boolean;
  tailscale?: string;
  usb?: boolean;
  display?: boolean;
  selected: boolean;
  now: Accessor<number>;
  onClick: () => void;
  onStop?: () => void;
  onPurge?: () => void;
  onRevive?: () => void;
  actionLoading?: boolean;
  onDiffClick?: () => void;
}

const terminalStates = new Set(["stopping", "stopped", "purging", "purged", "failed"]);

export default function TaskCard(props: TaskCardProps) {
  const isTerminal = () => terminalStates.has(props.state);
  const [titleTruncated, setTitleTruncated] = createSignal(false);
  let titleRef: HTMLElement | undefined; // eslint-disable-line no-unassigned-vars -- assigned by SolidJS ref

  onMount(() => {
    const check = () => { if (titleRef) setTitleTruncated(titleRef.scrollWidth > titleRef.clientWidth); };
    check();
    if (titleRef) {
      const ro = new ResizeObserver(check);
      ro.observe(titleRef);
      onCleanup(() => ro.disconnect());
    }
  });

  return (
    <div
      data-task-id={props.id}
      role="button"
      tabIndex={0}
      onClick={() => props.onClick()}
      onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); props.onClick(); } }}
      class={`${styles.card} ${props.selected ? styles.selected : ""}`}
    >
      {/* Line 1: title + feature icons + plan badge + purge (no state badge) */}
      <div class={styles.header}>
        <Tooltip text={props.title} class={styles.titleWrapper} disabled={!titleTruncated()}>
          <strong ref={titleRef} class={styles.title}>{props.title}</strong>
        </Tooltip>
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
          {/* Stopped: revive + purge buttons */}
          <Show when={props.state === "stopped"}>
            <Show when={props.onRevive}>
              <span class={styles.purgeBtn}>
                <button
                  class={styles.purgeIcon}
                  disabled={props.actionLoading}
                  onClick={(e) => { e.stopPropagation(); props.onRevive?.(); }}
                  title="Revive"
                  data-testid="revive-task"
                >
                  <Show when={props.actionLoading} fallback={<RestoreIcon width="0.85rem" height="0.85rem" />}>
                    <span class={styles.purgeSpinner} />
                  </Show>
                </button>
              </span>
            </Show>
            <Show when={props.onPurge}>
              <span class={styles.purgeBtn}>
                <button
                  class={styles.purgeIcon}
                  disabled={props.actionLoading}
                  onClick={(e) => { e.stopPropagation(); if (window.confirm(`Purge container?\n\n${props.title}\nrepo: ${props.repo}\nbranch: ${props.branch}`)) props.onPurge?.(); }}
                  title="Purge"
                  data-testid="purge-task"
                >
                  <DeleteIcon width="0.85rem" height="0.85rem" />
                </button>
              </span>
            </Show>
          </Show>
          {/* Active states: stop button (trash can) */}
          <Show when={props.state !== "stopped" && props.onStop && !terminalStates.has(props.state)}>
            <span class={styles.purgeBtn}>
              <button
                class={styles.purgeIcon}
                disabled={props.actionLoading}
                onClick={(e) => {
                  e.stopPropagation();
                  if (props.state === "running") {
                    if (window.confirm(`Stop container?\n\n${props.title}\nrepo: ${props.repo}\nbranch: ${props.branch}`)) props.onStop?.();
                  } else {
                    props.onStop?.();
                  }
                }}
                title="Stop"
                data-testid="stop-task"
              >
                <Show when={props.actionLoading} fallback={<DeleteIcon width="0.85rem" height="0.85rem" />}>
                  <span class={styles.purgeSpinner} />
                </Show>
              </button>
            </span>
          </Show>
          <Show when={props.inPlanMode}>
            <span class={styles.planBadge} title="Plan mode">P</span>
          </Show>
        </span>
      </div>

      {/* Line 2: base→branch | [timer times] [state badge] */}
      <div class={styles.metaRow}>
        <span class={styles.meta}>
          <Show when={props.branch}>
            <Show when={props.baseBranch && props.branch}>
              <span class={styles.baseBranch}>{props.baseBranch}</span>
              <span class={styles.branchArrow}>→</span>
            </Show>
            <span class={styles.branchName}>{props.branch}</span>
          </Show>
        </span>
        <span class={styles.stateGroup}>
          <Show when={(!isTerminal() && props.stateUpdatedAt > 0) || props.duration > 0}>
            <span class={styles.timePair}>
              <TimerIcon width="0.65rem" height="0.65rem" class={styles.timerIcon} />
              <Show when={!isTerminal() && props.stateUpdatedAt > 0}>
                <StateDuration stateUpdatedAt={props.stateUpdatedAt} now={props.now} />
                <Show when={props.duration > 0 || props.state === "running"}>
                  <span class={styles.timeSep}>/</span>
                </Show>
              </Show>
              <Show when={props.duration > 0 || props.state === "running"}>
                <ThinkTime duration={props.duration} state={props.state} stateUpdatedAt={props.stateUpdatedAt} turnStartedAt={props.turnStartedAt} now={props.now} />
              </Show>
            </span>
          </Show>
          <span class={styles.badge} style={{ background: stateColor(props.state) }}>
            {props.state}
          </span>
        </span>
      </div>

      {/* Line 3: model · tokens · cost */}
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
        </div>
      </Show>

      {/* Line 4 (optional): diff */}
      <Show when={props.diffStat?.length ? props.diffStat : undefined} keyed>
        {(ds) => {
          const content = () => <>
            {ds.length} file{ds.length !== 1 ? "s" : ""}
            {" "}
            <span class={styles.diffAdded}>+{ds.reduce((s, f) => s + f.added, 0)}</span>
            {" "}
            <span class={styles.diffDeleted}>-{ds.reduce((s, f) => s + f.deleted, 0)}</span>
          </>;
          return (
            <Show when={props.onDiffClick} fallback={<div class={styles.meta}>{content()}</div>}>
              {(fn) => (
                <div
                  class={`${styles.meta} ${styles.diffClickable}`}
                  role="button"
                  tabIndex={0}
                  onClick={(e) => { e.stopPropagation(); fn()(); }}
                  onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); e.stopPropagation(); fn()(); } }}
                >
                  {content()}
                </div>
              )}
            </Show>
          );
        }}
      </Show>
      <Show when={props.error}>
        <div class={styles.error}>{props.error}</div>
      </Show>
    </div>
  );
}

function StateDuration(props: { stateUpdatedAt: number; now: Accessor<number> }) {
  const elapsed = () => Math.max(0, props.now() - props.stateUpdatedAt * 1000);
  return <span>{formatElapsed(elapsed())}</span>;
}

function ThinkTime(props: { duration: number; state: string; stateUpdatedAt: number; turnStartedAt?: number; now: Accessor<number> }) {
  const thinkMs = () => {
    const base = props.duration * 1000;
    if (props.state === "running") {
      const turnStart = (props.turnStartedAt ?? 0) > 0 ? (props.turnStartedAt as number) : props.stateUpdatedAt;
      return base + Math.max(0, props.now() - turnStart * 1000);
    }
    return base;
  };
  return <span>{formatElapsed(thinkMs())}</span>;
}
