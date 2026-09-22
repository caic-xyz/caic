// Compact sidebar task card with per-repository Git and change-state markers.

import { For, Show, createEffect, createSignal, onMount, onCleanup } from "solid-js";
import type { Accessor } from "solid-js";
import { Portal } from "solid-js/web";
import { A } from "@solidjs/router";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import SudoIcon from "@material-symbols/svg-400/outlined/shield_person.svg?solid";
import DeleteIcon from "@material-symbols/svg-400/outlined/delete.svg?solid";
import RestoreIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";
import StopIcon from "@material-symbols/svg-400/outlined/stop_circle.svg?solid";
import TimerIcon from "@material-symbols/svg-400/outlined/timer.svg?solid";

import type {
  DiffStat,
  CIStatus,
  ForgeCheck,
  GitRepositoryState,
  RuntimeInstance,
  TaskRateLimit,
  TaskRepo,
  TaskState,
  SyncTarget,
} from "@sdk/types.gen";
import { SyncTargetDefault } from "@sdk/types.gen";

import CIDot from "./CIDot";
import RepoStateIcons, { diffStatState, repoStateLabel } from "./RepoStateIcons";
import TaskActionsMenu from "./TaskActionsMenu";
import Tooltip from "./Tooltip";
import TailscaleIcon from "./tailscale.svg?solid";
import TokenIcon from "./github.svg?solid";
import styles from "./TaskCard.module.css";
import { formatElapsed, formatTokens, tokenColor, stateColor, staleStateColor, isCacheStale } from "../formatting";
import { formatQuotaCountdown } from "../quota";
import { api } from "../api";

export interface TaskCardProps {
  id: string;
  title: string;
  forkedFromTaskID?: string;
  parentTaskID?: string;
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
  repoStates?: GitRepositoryState[];
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
  purgeModifierActive: boolean;
  actionLoading?: boolean;
  supportsCompact?: boolean;
  onFork?: () => void;
  onQuotaRecovery?: () => void;
  onError: (message: string) => void;
  /** Task number for voice mode display. Shown only when voice is connected. */
  voiceNumber?: number;
}

const terminalStates = new Set(["stopping", "stopped", "purging", "purged", "crashed", "failed"]);

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
  return window.confirm(`Stop runtime instance?\n\n${title}\nbranch: ${branch}`);
}

function confirmDirectPurge(title: string, branch: string): boolean {
  return window.confirm(`Purge runtime instance?\n\n${title}\nbranch: ${branch}`);
}

export default function TaskCard(props: TaskCardProps) {
  const isTerminal = () => terminalStates.has(props.state);
  const stale = () =>
    !terminalStates.has(props.state) && props.state !== "running" && isCacheStale(props.now(), props.cacheExpiresAt);
  const cacheExpiryText = () =>
    props.cacheTTLSeconds
      ? `Prompt cache likely expired (${formatElapsed(props.cacheTTLSeconds * 1000)} TTL) — continuing may use more tokens`
      : "Prompt cache likely expired — continuing may use more tokens";
  const [titleTruncated, setTitleTruncated] = createSignal(false);
  // subscriptionLabel names the subscription for harnesses whose cost is
  // API-equivalent pricing rather than actual spend; "" otherwise.
  const subscriptionLabel = (): string => {
    const h = props.harness;
    if (h === "claude") return "Claude Code subscription";
    if (h === "codex") return "Codex subscription";
    return "";
  };
  const [contextMenuPosition, setContextMenuPosition] = createSignal<{ x: number; y: number } | undefined>();
  const [menuActionPending, setMenuActionPending] = createSignal(false);
  // Compact Git state is pushed with the task over the task-list stream.
  const repoStates = () => props.repoStates ?? [];
  const hasRepositoryRuntime = () => Boolean(props.runtime?.id) && props.state !== "purged";
  let cardRef: HTMLDivElement | undefined;
  let titleRef: HTMLElement | undefined; // eslint-disable-line no-unassigned-vars -- assigned by SolidJS ref
  let contextMenuRef: HTMLDivElement | undefined;

  const repositoryState = (repo: TaskRepo) => {
    // Without a runtime there is no Git state to push; purged tasks keep none.
    if (!hasRepositoryRuntime()) {
      return undefined;
    }
    return repoStates().find((state) => state.name === repo.name);
  };
  const repoStateRows = () =>
    (props.repos ?? []).map((repo, index) => {
      const state =
        repositoryState(repo) ??
        ((!hasRepositoryRuntime() || repoStates().length === 0) && index === 0
          ? diffStatState(props.diffStat)
          : undefined);
      return {
        name: repo.name,
        branch: state?.branch || repo.branch,
        state,
      };
    });
  const hasMultipleRepos = () => (props.repos?.length ?? 0) > 1;
  const repoStateText = (repo: { name: string; branch: string }) =>
    hasMultipleRepos() ? [repo.name, repo.branch].filter(Boolean).join(" · ") : repo.branch || repo.name;

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

  async function runMenuAction(name: "sync" | "compact", action: () => Promise<void>) {
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
      const response = await api.syncTask(taskId, {
        force: false,
        ...(target ? { target } : {}),
      });
      if (response.status !== "blocked") return;
      const issues = response.safetyIssues?.map((issue) => `${issue.file}: ${issue.detail}`).join("; ");
      throw new Error(issues ? `blocked: ${issues}` : "blocked by safety checks");
    });
  }

  function doCompact() {
    const taskId = props.id;
    void runMenuAction("compact", async () => {
      await api.compactContext(taskId, {});
    });
  }

  onMount(() => {
    const check = () => {
      if (titleRef) setTitleTruncated(titleRef.scrollWidth > titleRef.clientWidth);
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
        ref={(el) => {
          cardRef = el;
        }}
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
          <Tooltip text={props.title} class={styles.titleWrapper} disabled={!titleTruncated()}>
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
            {/* Stopped/crashed: expose revive + purge only after selecting the card. */}
            <Show when={props.selected && (props.state === "stopped" || props.state === "crashed")}>
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
            {/* Active states: stop button. Shift-click or double-click/tap skips stop and goes straight to purge. */}
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
                    if (e.detail > 1) return;
                    if (e.shiftKey && props.onPurge) {
                      props.onPurge();
                    } else if (props.state === "running") {
                      if (confirmStopTask(props.title, props.repos?.[0]?.branch ?? "")) props.onStop?.();
                    } else {
                      props.onStop?.();
                    }
                  }}
                  onDblClick={(e) => {
                    e.stopPropagation();
                    if (props.onPurge && confirmDirectPurge(props.title, props.repos?.[0]?.branch ?? "")) {
                      props.onPurge();
                    }
                  }}
                  aria-label={props.purgeModifierActive ? "Purge" : "Stop"}
                  title={props.purgeModifierActive ? "Purge" : "Stop (hold Shift or double-click to purge)"}
                  data-testid="stop-task"
                >
                  <Show
                    when={props.actionLoading}
                    fallback={
                      props.purgeModifierActive ? (
                        <DeleteIcon width="0.85rem" height="0.85rem" data-testid="purge-task-icon" aria-hidden="true" />
                      ) : (
                        <StopIcon width="0.85rem" height="0.85rem" data-testid="stop-task-icon" aria-hidden="true" />
                      )
                    }
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

        <Show when={props.forkedFromTaskID !== props.parentTaskID ? props.forkedFromTaskID : undefined} keyed>
          {(parentID) => (
            <div class={styles.originRow}>
              <span class={styles.originGlyph} aria-hidden="true">
                ↳
              </span>
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
        <Show when={props.parentTaskID} keyed>
          {(parentID) => (
            <div class={styles.originRow}>
              <span class={styles.originGlyph} aria-hidden="true">
                ↳
              </span>
              <span>child of</span>
              <A
                class={styles.originLink}
                href={`/task/@${parentID}`}
                title={`Open parent task ${parentID}`}
                onClick={(event) => event.stopPropagation()}
              >
                {parentID}
              </A>
            </div>
          )}
        </Show>

        {/* Line 2: [timer times] [PR] [CI] [state badge] */}
        {(() => {
          const timePair = () => (
            <Show when={(!isTerminal() && props.stateUpdatedAt) || props.duration > 0}>
              <span class={styles.timePair}>
                <TimerIcon width="0.65rem" height="0.65rem" class={styles.timerIcon} />
                <Show when={!isTerminal() && props.stateUpdatedAt}>
                  <StateDuration stateUpdatedAt={props.stateUpdatedAt} now={props.now} />
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
                {(status) => <CIDot status={status as CIStatus} checks={props.ciChecks} />}
              </Show>
              <Tooltip text={cacheExpiryText()} disabled={!stale()}>
                <span
                  class={styles.badge}
                  data-testid="state-badge"
                  style={{ "--badge-bg": stale() ? staleStateColor(props.state) : stateColor(props.state) }}
                >
                  {props.state}
                </span>
              </Tooltip>
            </>
          );
          return (
            <div class={styles.metaRow}>
              {timePair()}
              <span class={styles.statusBadges}>{statusBadges()}</span>
            </div>
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
                  <Tooltip
                    text={`${props.rateLimit?.window} quota resets at ${new Date(props.rateLimit?.resetsAt ?? "").toLocaleString()}`}
                  >
                    <span class={styles.quotaCountdown} data-testid="quota-countdown">
                      out of quota · resets in {formatQuotaCountdown(props.rateLimit?.resetsAt ?? "", props.now())}
                    </span>
                  </Tooltip>
                </>
              </Show>
              <Show when={props.activeInputTokens + props.activeCacheReadTokens > 0}>
                {" · "}
                <Tooltip
                  text={`Accumulated: ${formatTokens(props.cumulativeCacheReadInputTokens)} cached + ${formatTokens(props.cumulativeInputTokens + props.cumulativeCacheCreationInputTokens)} in + ${formatTokens(props.cumulativeOutputTokens)} out`}
                >
                  <span
                    class={styles.tokens}
                    data-testid="task-card-tokens"
                    style={{
                      "--tokens-color": tokenColor(
                        props.activeInputTokens + props.activeCacheReadTokens,
                        props.contextWindowLimit,
                      ),
                    }}
                  >
                    {formatTokens(props.activeInputTokens + props.activeCacheReadTokens)}
                    <Show when={props.contextWindowLimit > 0}>/{formatTokens(props.contextWindowLimit)}</Show>
                  </span>
                </Tooltip>
              </Show>
              <Show when={props.costUSD > 0}>
                {/* The separator stays a sibling of the price: Tooltip's
                    inline-flex wrapper blockifies its child, and CSS strips
                    a leading space at the start of a line box. */}
                {" · "}
                <Show when={subscriptionLabel()}>
                  <Tooltip text={`API-equivalent cost — ${subscriptionLabel()}`}>
                    <span>${props.costUSD.toFixed(2)}</span>
                  </Tooltip>
                </Show>
                <Show when={!subscriptionLabel()}>${props.costUSD.toFixed(2)}</Show>
              </Show>
            </span>
          </div>
        </Show>

        <Show when={repoStateRows().length > 0}>
          <div class={styles.metaRow}>
            <span class={styles.repoStates} data-testid="task-card-repo-states">
              <For each={repoStateRows()}>
                {(repo) => (
                  <span class={styles.repoState} data-testid="task-card-repo-state">
                    <span class={styles.repoStateLabel}>{repoStateText(repo)}</span>
                    <Show when={repoStateLabel(repo.state)}>
                      <span class={styles.repoStateSummary}>
                        <RepoStateIcons state={repo.state} />
                      </span>
                    </Show>
                  </span>
                )}
              </For>
            </span>
          </div>
        </Show>

        <Show when={props.rateLimit?.blocked && props.repos?.[0]?.name ? props.onQuotaRecovery : undefined} keyed>
          {(recover) => (
            <button
              type="button"
              class={styles.quotaRecovery}
              data-testid="quota-recovery-card"
              onClick={(event) => {
                event.stopPropagation();
                recover();
              }}
            >
              Continue in new agent
            </button>
          )}
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
              position={position}
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
              onStop={() => {
                setContextMenuPosition(undefined);
                props.onStop?.();
              }}
              onRevive={() => {
                setContextMenuPosition(undefined);
                props.onRevive?.();
              }}
              onPurge={() => {
                setContextMenuPosition(undefined);
                props.onPurge?.();
              }}
              onCompact={doCompact}
              onFork={() => {
                setContextMenuPosition(undefined);
                props.onFork?.();
              }}
            />
          )}
        </Show>
      </Portal>
    </>
  );
}

function StateDuration(props: { stateUpdatedAt: string; now: Accessor<number> }) {
  const elapsed = () => Math.max(0, props.now() - new Date(props.stateUpdatedAt).getTime());
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
      const turnMs = props.turnStartedAt ? new Date(props.turnStartedAt).getTime() : 0;
      const start = turnMs > 0 ? turnMs : new Date(props.stateUpdatedAt).getTime();
      return base + Math.max(0, props.now() - start);
    }
    return base;
  };
  return <span>{formatElapsed(thinkMs())}</span>;
}
