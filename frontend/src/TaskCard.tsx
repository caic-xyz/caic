// Compact card for a single task, used in the sidebar task list.
import { For, Show, createSignal, onMount, onCleanup } from "solid-js";
import type { Accessor } from "solid-js";
import type { DiffStat, CIStatus, ForgeCheck, TaskRepo } from "@sdk/types.gen";
import CIDot from "./CIDot";
import Tooltip from "./Tooltip";
import TailscaleIcon from "./tailscale.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import DeleteIcon from "@material-symbols/svg-400/outlined/delete.svg?solid";
import RestoreIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";
import TimerIcon from "@material-symbols/svg-400/outlined/timer.svg?solid";
import styles from "./TaskCard.module.css";
import { formatElapsed, formatTokens, tokenColor, stateColor, staleStateColor, isCacheStale } from "./formatting";

export interface TaskCardProps {
  id: string;
  title: string;
  state: string;
  stateUpdatedAt: number;
  repos?: TaskRepo[];
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
  forgePR?: number;
  ciStatus?: CIStatus;
  ciChecks?: ForgeCheck[];
  autoFixPR?: boolean;
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
  const stale = () => props.state !== "running" && isCacheStale(props.stateUpdatedAt, props.now());
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
              ? <a class={styles.featureIconBadge} href={ts} target="_blank" rel="noopener" title="Tailscale" onClick={(e) => e.stopPropagation()}><TailscaleIcon width="0.7rem" height="0.7rem" /></a>
              : <span class={styles.featureIconBadge} title="Tailscale"><TailscaleIcon width="0.7rem" height="0.7rem" /></span>
            }
          </Show>
          <Show when={props.usb}>
            <span class={styles.featureBadge} title="USB">USB</span>
          </Show>
          <Show when={props.display}>
            <span class={styles.featureIconBadge} title="Display"><DisplayIcon width="0.7rem" height="0.7rem" /></span>
          </Show>
          {/* Stopped: revive + purge buttons */}
          <Show when={props.state === "stopped"}>
            <Show when={props.onRevive}>
              <span class={styles.reviveBtn}>
                <button
                  class={styles.reviveIcon}
                  disabled={props.actionLoading}
                  onClick={(e) => { e.stopPropagation(); props.onRevive?.(); }}
                  title="Revive"
                  data-testid="revive-task"
                >
                  <Show when={props.actionLoading} fallback={<RestoreIcon width="0.85rem" height="0.85rem" />}>
                    <span class={styles.reviveSpinner} />
                  </Show>
                </button>
              </span>
            </Show>
            <Show when={props.onPurge}>
              <span class={styles.purgeBtn}>
                <button
                  class={styles.purgeIcon}
                  disabled={props.actionLoading}
                  onClick={(e) => { e.stopPropagation(); if (window.confirm(`Purge container?\n\n${props.title}\nbranch: ${props.repos?.[0]?.branch ?? ""}`)) props.onPurge?.(); }}
                  title="Purge"
                  data-testid="purge-task"
                >
                  <DeleteIcon width="0.85rem" height="0.85rem" />
                </button>
              </span>
            </Show>
          </Show>
          {/* Active states: stop button (trash can). Shift-click or double-click/tap skips stop and goes straight to purge. */}
          <Show when={props.state !== "stopped" && props.onStop && !terminalStates.has(props.state)}>
            <span class={styles.purgeBtn}>
              <button
                class={styles.purgeIcon}
                disabled={props.actionLoading}
                onClick={(e) => {
                  e.stopPropagation();
                  if (e.shiftKey && props.onPurge) {
                    if (window.confirm(`Purge container?\n\n${props.title}\nbranch: ${props.repos?.[0]?.branch ?? ""}`)) props.onPurge();
                  } else if (props.state === "running") {
                    if (window.confirm(`Stop container?\n\n${props.title}\nbranch: ${props.repos?.[0]?.branch ?? ""}`)) props.onStop?.();
                  } else {
                    props.onStop?.();
                  }
                }}
                onDblClick={(e) => {
                  e.stopPropagation();
                  if (props.onPurge && window.confirm(`Purge container?\n\n${props.title}\nbranch: ${props.repos?.[0]?.branch ?? ""}`)) props.onPurge();
                }}
                title="Stop (shift-click or double-click to purge)"
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

      {/* Line 2: base→branch | [timer times] [PR] [CI] [state badge] */}
      {(() => {
        const multiRepo = (props.repos?.length ?? 0) > 1;
        const timePair = () => (
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
        );
        const statusBadges = () => <>
          <Show when={props.forgePR}>
            <span class={styles.prBadge} title={`PR #${props.forgePR}`}>PR</span>
          </Show>
          <Show when={props.autoFixPR && props.forgePR}>
            <span class={styles.autoBadge} title="Auto-fix PR enabled">auto</span>
          </Show>
          <Show when={props.forgePR && props.ciStatus} keyed>
            {(status) => <CIDot status={status as CIStatus} checks={props.ciChecks} />}
          </Show>
          <Tooltip text="Prompt cache likely expired — continuing may use more tokens" disabled={!stale()}>
            <span class={styles.badge} style={{ background: stale() ? staleStateColor(props.state) : stateColor(props.state) }}>
              {props.state}
            </span>
          </Tooltip>
        </>;
        const repoSpan = (r: { baseBranch?: string; branch: string; name: string }, showName: boolean) => <>
          <Show when={r.baseBranch && r.branch}>
            <span class={styles.baseBranch}>{r.baseBranch}</span>
            <span class={styles.branchArrow}>→</span>
          </Show>
          <span class={styles.branchName}>{r.branch}</span>
          <Show when={showName}>
            <span class={styles.repoName}>{r.name}</span>
          </Show>
        </>;
        return <>
          <Show when={!multiRepo}>
            {/* Single repo: branch + timing + badges on same row */}
            <div class={styles.metaRow}>
              <span class={styles.branchMeta}>
                <Show when={props.repos?.[0]} keyed>
                  {(primary) => repoSpan(primary, false)}
                </Show>
              </span>
              <span class={styles.stateGroup}>
                {timePair()}
                {statusBadges()}
              </span>
            </div>
          </Show>
          <Show when={multiRepo}>
            {/* Multi repo: first repo + badges, middle repos plain, last repo + timing */}
            <div class={styles.metaRow}>
              <span class={styles.branchMeta}>
                <Show when={props.repos?.[0]} keyed>
                  {(primary) => repoSpan(primary, true)}
                </Show>
              </span>
              <span class={styles.stateGroup}>
                {statusBadges()}
              </span>
            </div>
            <For each={props.repos?.slice(1)}>
              {(r, i) => {
                const isLast = () => i() === (props.repos?.length ?? 0) - 2;
                return (
                  <div class={styles.metaRow}>
                    <span class={styles.branchMeta}>
                      {repoSpan(r, true)}
                    </span>
                    <Show when={isLast()}>
                      <span class={styles.stateGroup}>
                        {timePair()}
                      </span>
                    </Show>
                  </div>
                );
              }}
            </For>
          </Show>
        </>;
      })()}

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
