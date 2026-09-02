// Compact card for a single task, used in the sidebar task list.

import { For, Show, createEffect, createSignal, onMount, onCleanup } from "solid-js";
import type { Accessor } from "solid-js";
import { Portal } from "solid-js/web";
import { A } from "@solidjs/router";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import SudoIcon from "@material-symbols/svg-400/outlined/shield_person.svg?solid";
import DeleteIcon from "@material-symbols/svg-400/outlined/delete.svg?solid";
import RestoreIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";
import TimerIcon from "@material-symbols/svg-400/outlined/timer.svg?solid";

import type {
  DiffStat,
  CIStatus,
  ForgeCheck,
  RuntimeInstance,
  TaskRateLimit,
  TaskRepo,
  TaskState,
  SyncTarget,
} from "@sdk/types.gen";
import { SyncTargetDefault } from "@sdk/types.gen";

import { compactContext, syncTask } from "../api";
import CIDot from "./CIDot";
import TaskActionsMenu from "./TaskActionsMenu";
import Tooltip from "./Tooltip";
import TailscaleIcon from "./tailscale.svg?solid";
import TokenIcon from "./github.svg?solid";
import styles from "./TaskCard.module.css";
import {
  formatElapsed,
  formatTokens,
  tokenColor,
  stateColor,
  staleStateColor,
  isCacheStale,
} from "../formatting";
import { formatQuotaCountdown } from "../quota";

export interface TaskCardProps {
  id: string;
  title: string;
  forkedFromTaskID?: string;
  state: TaskState;
  stateUpdatedAt: string;
  repos?: TaskRepo[];
  harness?: string;
  model?: string;
  effort?: string;
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
  cacheTTLSeconds?: number;
  cacheExpiresAt?: string;
  turnStartedAt?: string;
  diffStat?: DiffStat;
  error?: string;
  inPlanMode?: boolean;
  runtime?: RuntimeInstance;
  gitHubToken?: boolean;
  forgePR?: number;
  ciStatus?: CIStatus;
  ciChecks?: ForgeCheck[];
  autoFixPR?: boolean;
  rateLimit?: TaskRateLimit;
  selected: boolean;
  tabIndex: number;
  now: Accessor<number>;
  onClick: () => void;
  onStop?: () => void;
  onPurge?: () => void;
  onRevive?: () => void;
  actionLoading?: boolean;
  onDiffClick?: () => void;
  supportsCompact?: boolean;
  onFork?: () => void;
  onError: (message: string) => void;
  /** Task number for voice mode display. Shown only when voice is connected. */
  voiceNumber?: number;
}

const terminalStates = new Set([
  "stopping",
  "stopped",
  "purging",
  "purged",
  "crashed",
  "failed",
]);

const actionMenuActiveStates = new Set([
  "running",
  "branching",
  "provisioning",
  "starting",
  "waiting",
  "asking",
  "has_plan",
  "purging",
]);

const actionMenuWaitingStates = new Set(["waiting", "asking", "has_plan"]);

function confirmStopTask(title: string, branch: string): boolean {
  return window.confirm(
    `Stop runtime instance?\n\n${title}\nbranch: ${branch}`,
  );
}

export default function TaskCard(props: TaskCardProps) {
  const isTerminal = () => terminalStates.has(props.state);
  const stale = () =>
    !terminalStates.has(props.state) &&
    props.state !== "running" &&
    isCacheStale(props.now(), props.cacheExpiresAt);
  const cacheExpiryText = () => props.cacheTTLSeconds
    ? `Prompt cache likely expired (${formatElapsed(props.cacheTTLSeconds * 1000)} TTL) — continuing may use more tokens`
    : "Prompt cache likely expired — continuing may use more tokens";
  const [titleTruncated, setTitleTruncated] = createSignal(false);
  const [contextMenuPosition, setContextMenuPosition] = createSignal<
    { x: number; y: number } | undefined
  >();
  const [menuActionPending, setMenuActionPending] = createSignal(false);
  let cardRef: HTMLDivElement | undefined;
  let titleRef: HTMLElement | undefined; // eslint-disable-line no-unassigned-vars -- assigned by SolidJS ref
  let contextMenuRef: HTMLDivElement | undefined;

  createEffect(() => {
    if (!contextMenuPosition()) return;
    const close = () => setContextMenuPosition(undefined);
    const closeOnOutsidePointer = (event: PointerEvent) => {
      if (!contextMenuRef?.contains(event.target as Node)) close();
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      event.stopImmediatePropagation();
      close();
      cardRef?.focus();
    };
    document.addEventListener("pointerdown", closeOnOutsidePointer, true);
    document.addEventListener("keydown", closeOnEscape, true);
    window.addEventListener("blur", close);
    window.addEventListener("resize", close);
    onCleanup(() => {
      document.removeEventListener("pointerdown", closeOnOutsidePointer, true);
      document.removeEventListener("keydown", closeOnEscape, true);
      window.removeEventListener("blur", close);
      window.removeEventListener("resize", close);
    });
  });

  async function runMenuAction(
    name: "sync" | "compact",
    action: () => Promise<void>,
  ) {
    setContextMenuPosition(undefined);
    if (menuActionPending()) return;
    setMenuActionPending(true);
    try {
      await action();
    } catch (error) {
      const message = error instanceof Error ? error.message : "Unknown error";
      props.onError(`${name} failed: ${message}`);
    } finally {
      setMenuActionPending(false);
    }
  }

  function doSync(target?: SyncTarget) {
    const taskId = props.id;
    void runMenuAction("sync", async () => {
      const response = await syncTask(taskId, {
        force: false,
        ...(target ? { target } : {}),
      });
      if (response.status !== "blocked") return;
      const issues = response.safetyIssues
        ?.map((issue) => `${issue.file}: ${issue.detail}`)
        .join("; ");
      throw new Error(issues ? `blocked: ${issues}` : "blocked by safety checks");
    });
  }

  function doCompact() {
    const taskId = props.id;
    void runMenuAction("compact", async () => {
      await compactContext(taskId, {});
    });
  }

  onMount(() => {
    const check = () => {
      if (titleRef)
        setTitleTruncated(titleRef.scrollWidth > titleRef.clientWidth);
    };
    check();
    if (titleRef) {
      const ro = new ResizeObserver(check);
      ro.observe(titleRef);
      onCleanup(() => ro.disconnect());
    }
  });

  return (
    <>
    <div
      ref={(el) => { cardRef = el; }}
      data-task-id={props.id}
      role="button"
      tabIndex={props.tabIndex}
      onClick={() => props.onClick()}
      onContextMenu={(event) => {
        event.preventDefault();
        event.stopPropagation();
        if (props.actionLoading || menuActionPending()) return;
        setContextMenuPosition({ x: event.clientX, y: event.clientY });
      }}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          props.onClick();
        }
      }}
      class={`${styles.card} ${props.selected ? styles.selected : ""}`}
    >
      {/* Line 1: title + feature icons + plan badge + purge (no state badge) */}
      <div class={styles.header}>
        <Show when={props.voiceNumber !== undefined}>
          <span class={styles.voiceNumber}>#{props.voiceNumber}</span>
        </Show>
        <Tooltip
          text={props.title}
          class={styles.titleWrapper}
          disabled={!titleTruncated()}
        >
          <strong ref={titleRef} class={styles.title}>
            {props.title}
          </strong>
        </Tooltip>
        <span class={styles.stateGroup}>
          <Show when={props.runtime?.tailscale} keyed>
            {(ts) =>
              ts.startsWith("https://") ? (
                <a
                  class={styles.featureIconBadge}
                  href={ts}
                  target="_blank"
                  rel="noopener"
                  title="Tailscale"
                  onClick={(e) => e.stopPropagation()}
                >
                  <TailscaleIcon width="0.7rem" height="0.7rem" />
                </a>
              ) : (
                <span class={styles.featureIconBadge} title="Tailscale">
                  <TailscaleIcon width="0.7rem" height="0.7rem" />
                </span>
              )
            }
          </Show>
          <Show when={props.runtime?.usb}>
            <span class={styles.featureBadge} title="USB">
              USB
            </span>
          </Show>
          <Show when={props.runtime?.display}>
            <span class={styles.featureIconBadge} title="Display">
              <DisplayIcon width="0.7rem" height="0.7rem" />
            </span>
          </Show>
          <Show when={props.runtime?.sudo}>
            <span class={styles.featureIconBadge} title="Sudo">
              <SudoIcon width="0.7rem" height="0.7rem" />
            </span>
          </Show>
          <Show when={props.gitHubToken}>
            <span class={styles.featureIconBadge} title="GitHub token">
              <TokenIcon width="0.7rem" height="0.7rem" />
            </span>
          </Show>
          {/* Stopped/crashed: revive + purge buttons */}
          <Show when={props.state === "stopped" || props.state === "crashed"}>
            <Show when={props.onRevive}>
              <span class={styles.reviveBtn}>
                <button
                  class={styles.reviveIcon}
                  disabled={props.actionLoading}
                  onClick={(e) => {
                    e.stopPropagation();
                    props.onRevive?.();
                  }}
                  title="Revive"
                  data-testid="revive-task"
                >
                  <Show
                    when={props.actionLoading}
                    fallback={<RestoreIcon width="0.85rem" height="0.85rem" />}
                  >
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
                  onClick={(e) => {
                    e.stopPropagation();
                    props.onPurge?.();
                  }}
                  title="Purge"
                  data-testid="purge-task"
                >
                  <DeleteIcon width="0.85rem" height="0.85rem" />
                </button>
              </span>
            </Show>
          </Show>
          {/* Active states: stop button (trash can). Shift-click or double-click/tap skips stop and goes straight to purge. */}
          <Show
            when={
              props.state !== "stopped" &&
              props.state !== "crashed" &&
              props.onStop &&
              !terminalStates.has(props.state)
            }
          >
            <span class={styles.purgeBtn}>
              <button
                class={styles.purgeIcon}
                disabled={props.actionLoading}
                onClick={(e) => {
                  e.stopPropagation();
                  if (e.shiftKey && props.onPurge) {
                    props.onPurge();
                  } else if (props.state === "running") {
                    if (
                      confirmStopTask(
                        props.title,
                        props.repos?.[0]?.branch ?? "",
                      )
                    )
                      props.onStop?.();
                  } else {
                    props.onStop?.();
                  }
                }}
                onDblClick={(e) => {
                  e.stopPropagation();
                  props.onPurge?.();
                }}
                title="Stop (shift-click or double-click to purge)"
                data-testid="stop-task"
              >
                <Show
                  when={props.actionLoading}
                  fallback={<DeleteIcon width="0.85rem" height="0.85rem" />}
                >
                  <span class={styles.purgeSpinner} />
                </Show>
              </button>
            </span>
          </Show>
          <Show when={props.inPlanMode}>
            <span class={styles.planBadge} title="Plan mode">
              P
            </span>
          </Show>
        </span>
      </div>

      <Show when={props.forkedFromTaskID} keyed>
        {(parentID) => (
          <div class={styles.originRow}>
            <span class={styles.originGlyph} aria-hidden="true">↳</span>
            <span>forked from</span>
            <A
              class={styles.originLink}
              href={`/task/@${parentID}`}
              title={`Open origin task ${parentID}`}
              onClick={(event) => event.stopPropagation()}
            >
              {parentID}
            </A>
          </div>
        )}
      </Show>

      {/* Line 2: base→branch | [timer times] [PR] [CI] [state badge] */}
      {(() => {
        const multiRepo = (props.repos?.length ?? 0) > 1;
        const timePair = () => (
          <Show
            when={(!isTerminal() && props.stateUpdatedAt) || props.duration > 0}
          >
            <span class={styles.timePair}>
              <TimerIcon
                width="0.65rem"
                height="0.65rem"
                class={styles.timerIcon}
              />
              <Show when={!isTerminal() && props.stateUpdatedAt}>
                <StateDuration
                  stateUpdatedAt={props.stateUpdatedAt}
                  now={props.now}
                />
                <Show when={props.duration > 0 || props.state === "running"}>
                  <span class={styles.timeSep}>/</span>
                </Show>
              </Show>
              <Show when={props.duration > 0 || props.state === "running"}>
                <ThinkTime
                  duration={props.duration}
                  state={props.state}
                  stateUpdatedAt={props.stateUpdatedAt}
                  turnStartedAt={props.turnStartedAt}
                  now={props.now}
                />
              </Show>
            </span>
          </Show>
        );
        const statusBadges = () => (
          <>
            <Show when={props.forgePR}>
              <span class={styles.prBadge} title={`PR #${props.forgePR}`}>
                PR
              </span>
            </Show>
            <Show when={props.autoFixPR && props.forgePR}>
              <span class={styles.autoBadge} title="Auto-fix PR enabled">
                auto
              </span>
            </Show>
            <Show when={props.forgePR && props.ciStatus} keyed>
              {(status) => (
                <CIDot status={status as CIStatus} checks={props.ciChecks} />
              )}
            </Show>
            <Tooltip
              text={cacheExpiryText()}
              disabled={!stale()}
            >
              <span
                class={styles.badge}
                data-testid="state-badge"
                style={{
                  background: stale()
                    ? staleStateColor(props.state)
                    : stateColor(props.state),
                }}
              >
                {props.state}
              </span>
            </Tooltip>
          </>
        );
        const repoSpan = (
          r: { baseBranch?: string; branch: string; name: string },
          showName: boolean,
        ) => {
          if (!r.branch)
            return (
              <Show when={showName}>
                <span class={styles.repoName}>{r.name}</span>
              </Show>
            );
          return (
            <>
              <Show when={r.baseBranch && r.branch}>
                <span class={styles.baseBranch}>{r.baseBranch}</span>
                <span class={styles.branchArrow}>→</span>
              </Show>
              <span class={styles.branchName}>{r.branch}</span>
              <Show when={showName}>
                <span class={styles.repoName}>{r.name}</span>
              </Show>
            </>
          );
        };
        return (
          <>
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
                <span class={styles.stateGroup}>{statusBadges()}</span>
              </div>
              <For each={props.repos?.slice(1)}>
                {(r, i) => {
                  const isLast = () => i() === (props.repos?.length ?? 0) - 2;
                  return (
                    <div class={styles.metaRow}>
                      <span class={styles.branchMeta}>{repoSpan(r, true)}</span>
                      <Show when={isLast()}>
                        <span class={styles.stateGroup}>{timePair()}</span>
                      </Show>
                    </div>
                  );
                }}
              </For>
            </Show>
          </>
        );
      })()}

      {/* Line 3: harness · model · effort · tokens · cost */}
      <Show when={props.harness || props.model}>
        <div class={styles.metaRow}>
          <span class={styles.meta}>
            {(() => {
              const parts: string[] = [];
              if (props.harness) parts.push(props.harness);
              if (props.model) parts.push(props.model);
              if (props.effort) parts.push(props.effort);
              return parts.join(" · ");
            })()}
            <Show when={props.rateLimit?.blocked}>
              <>
                {" · "}
                <Tooltip text={`${props.rateLimit?.window} quota resets at ${new Date(props.rateLimit?.resetsAt ?? "").toLocaleString()}`}>
                  <span class={styles.quotaCountdown} data-testid="quota-countdown">
                    out of quota · resets in {formatQuotaCountdown(props.rateLimit?.resetsAt ?? "", props.now())}
                  </span>
                </Tooltip>
              </>
            </Show>
            <Show
              when={props.activeInputTokens + props.activeCacheReadTokens > 0}
            >
              {" · "}
              <Tooltip
                text={`Accumulated: ${formatTokens(props.cumulativeCacheReadInputTokens)} cached + ${formatTokens(props.cumulativeInputTokens + props.cumulativeCacheCreationInputTokens)} in + ${formatTokens(props.cumulativeOutputTokens)} out`}
              >
                <span
                  style={{
                    color: tokenColor(
                      props.activeInputTokens + props.activeCacheReadTokens,
                      props.contextWindowLimit,
                    ),
                  }}
                >
                  {formatTokens(
                    props.activeInputTokens + props.activeCacheReadTokens,
                  )}
                  /{formatTokens(props.contextWindowLimit)}
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
          const content = () => (
            <>
              {ds.length} file{ds.length !== 1 ? "s" : ""}{" "}
              <span class={styles.diffAdded}>
                +{ds.reduce((s, f) => s + f.added, 0)}
              </span>{" "}
              <span class={styles.diffDeleted}>
                -{ds.reduce((s, f) => s + f.deleted, 0)}
              </span>
            </>
          );
          return (
            <Show
              when={props.onDiffClick}
              fallback={<div class={styles.meta}>{content()}</div>}
            >
              {(fn) => (
                <div
                  class={`${styles.meta} ${styles.diffClickable}`}
                  role="button"
                  tabIndex={0}
                  onClick={(e) => {
                    e.stopPropagation();
                    fn()();
                  }}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      e.stopPropagation();
                      fn()();
                    }
                  }}
                >
                  {content()}
                </div>
              )}
            </Show>
          );
        }}
      </Show>
      <Show when={props.error}>
        <div class={styles.errorSummary}>{props.error}</div>
      </Show>
    </div>
    <Portal>
      <Show when={contextMenuPosition()} keyed>
        {(position) => (
          <TaskActionsMenu
            class={styles.contextMenu}
            style={{ left: `${position.x}px`, top: `${position.y}px` }}
            menuRef={(element) => {
              contextMenuRef = element;
              const bounds = element.getBoundingClientRect();
              const x = Math.max(8, Math.min(position.x, window.innerWidth - bounds.width - 8));
              const y = Math.max(8, Math.min(position.y, window.innerHeight - bounds.height - 8));
              if (x !== position.x || y !== position.y) setContextMenuPosition({ x, y });
            }}
            forge={props.repos?.[0]?.forge}
            forgePR={props.forgePR}
            baseBranch={props.repos?.[0]?.baseBranch ?? "main"}
            active={actionMenuActiveStates.has(props.state)}
            recoverable={props.state === "stopped" || props.state === "crashed"}
            waiting={actionMenuWaitingStates.has(props.state)}
            purging={props.state === "purging"}
            supportsCompact={props.supportsCompact}
            canFork={!!props.onFork && !!props.repos?.[0]?.name}
            onSync={() => doSync()}
            onSyncDefault={() => doSync(SyncTargetDefault)}
            onStop={() => { setContextMenuPosition(undefined); props.onStop?.(); }}
            onRevive={() => { setContextMenuPosition(undefined); props.onRevive?.(); }}
            onPurge={() => {
              setContextMenuPosition(undefined);
              props.onPurge?.();
            }}
            onCompact={doCompact}
            onFork={() => { setContextMenuPosition(undefined); props.onFork?.(); }}
          />
        )}
      </Show>
    </Portal>
    </>
  );
}

function StateDuration(props: {
  stateUpdatedAt: string;
  now: Accessor<number>;
}) {
  const elapsed = () =>
    Math.max(0, props.now() - new Date(props.stateUpdatedAt).getTime());
  return <span>{formatElapsed(elapsed())}</span>;
}

function ThinkTime(props: {
  duration: number;
  state: TaskState;
  stateUpdatedAt: string;
  turnStartedAt?: string;
  now: Accessor<number>;
}) {
  const thinkMs = () => {
    const base = props.duration * 1000;
    if (props.state === "running") {
      const turnMs = props.turnStartedAt
        ? new Date(props.turnStartedAt).getTime()
        : 0;
      const start =
        turnMs > 0 ? turnMs : new Date(props.stateUpdatedAt).getTime();
      return base + Math.max(0, props.now() - start);
    }
    return base;
  };
  return <span>{formatElapsed(thinkMs())}</span>;
}
