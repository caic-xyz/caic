// TaskDetail renders current-context history, server-summarized archived disclosures, stable scrolling, and task actions.

import {
  createSignal,
  createMemo,
  createEffect,
  For,
  Index,
  Show,
  onCleanup,
  onMount,
  Switch,
  Match,
  type Accessor,
  type JSX,
  untrack,
} from "solid-js";
import { A, useLocation } from "@solidjs/router";
import CloseIcon from "@material-symbols/svg-400/outlined/close.svg?solid";
import CopyIcon from "@material-symbols/svg-400/outlined/content_copy.svg?solid";
import CheckIcon from "@material-symbols/svg-400/outlined/check.svg?solid";
import InfoIcon from "@material-symbols/svg-400/outlined/info.svg?solid";
import SkillIcon from "@material-symbols/svg-400/outlined/menu_book.svg?solid";
import ProcessesIcon from "@material-symbols/svg-400/outlined/account_tree.svg?solid";
import LoadingIcon from "@material-symbols/svg-400/outlined/progress_activity.svg?solid";
import SendIcon from "@material-symbols/svg-400/outlined/send.svg?solid";

import type {
  EventMessage,
  HistoryActivity,
  HistoryEvent,
  HistorySection,
  HistoryTurn,
  TaskPlan,
  EventResult,
  AskQuestion,
  EventAsk,
  EventTextDelta,
  SafetyIssue,
  ImageConstraints,
  SyncTarget,
  DiffFileStat,
  ForgeCheck,
  EventSkillLoad,
  EventToolInputView,
  EventSubagentSpawn,
  TaskRateLimit,
  GitRepositoryState,
} from "@sdk/types.gen";
import { RootConversationProjection } from "../rootConversation";
import type { DraftImage } from "../images";
import { SyncTargetDefault } from "@sdk/types.gen";

import { useHostMode } from "@maruel/gomode/web/HostMode";
import { notifications } from "@maruel/gomode/web/notifications";

import {
  isChildEvent,
  toolIdentity,
  messageIdentity,
  isSessionBoundary,
  buildTurnItems,
  groupMessages,
  aggregateToolGroups,
  groupTurns,
  rateLimitPercentage,
  toolCallDurationMs,
  toolCallDurations,
  toolCountSummary,
  turnSummary,
  type MsgItem,
  type MessageGroup,
  type Turn,
} from "../grouping";
import { displayedAtRoot as historyRoot } from "../historyRouting";
import { createHistoryDisclosure } from "../historyDisclosure";
import { compareHistoryPosition, historyPositionKey } from "../historyPosition";
import { createLiveDuration } from "../liveDuration";
import { useTaskTimeline } from "../TaskTimelineContext";
import { createHistoryScroll } from "../historyScroll";
import { attachmentThumbnailWidth, taskAttachmentURL, taskAttachmentViewURL, taskViewURL } from "../runtimePath";
import { formatBytes, formatElapsed, formatTokens, toolCallDetail } from "../formatting";
import { formatQuotaCountdown, quotaDismissalKey } from "../quota";
import { VisualTimingTracker, formatTimingDuration } from "../timing";
import type { ToolCall } from "../grouping";
import AutoResizeTextarea from "./AutoResizeTextarea";
import PromptInput from "./PromptInput";
import { bindPromptSubmitShortcut } from "./promptSubmitShortcut";
import Button from "./Button";
import Markdown, { MarkdownTaskContext } from "./Markdown";
import UnifiedDiffBlock from "./UnifiedDiffBlock";
import ProgressPanel from "./ProgressPanel";
import NativeAgents, { BackgroundCommands, BackgroundCommandStatusChip, NativeActivityStatus } from "./NativeSubagents";
import { assignNativeAnchors } from "../nativeSubagents";
import StatsIcon from "./StatsIcon";
import ActionButton from "./ActionButton";
import TimingIcon from "./TimingIcon";
import TurnInvocationIcon, { SessionInvocationIcon } from "./TurnInvocationIcon";
import WidgetCard from "./WidgetCard";
import Dropdown from "./Dropdown";
import TaskActionsMenu from "./TaskActionsMenu";
import RepoStateIcons, { diffStatState, repoStateLabel } from "./RepoStateIcons";
import { prefetchTaskDiff } from "../diffCache";
import styles from "./TaskDetail.module.css";
import { api } from "../api";
import { taskViewMode } from "../taskView";

// Module-level store for <details> open/closed state (tool calls, thinking blocks).
// Keys: toolUseID, "group:<firstToolUseID>", "thinking:<firstEventTs>".
// Survives component remounts on task switching.
export const detailsOpenState = new Map<string, boolean>();

// Per-task expansion state for collapsed past turns and sessions.
// Turn keys: "<sessionKey>:turn:<firstEventTs>".
// Session keys: "session:<firstEventTs>".
const expandedTurnsByTask = new Map<string, Set<string>>();
const expandedSessionsByTask = new Map<string, Set<string>>();

// A setup that appears to last longer than this is reconstructed history with
// unavailable producer timestamps, not a credible container startup duration.
const maxCredibleSetupDurationMs = 60 * 60 * 1000;

interface Props {
  taskId: string;
  taskState: string;
  autoFocusPrompt: boolean;
  startedAt?: string;
  title?: string;
  error?: string;
  initialPrompt?: string;
  inPlanMode?: boolean;
  currentPlan: TaskPlan | null;
  repo: string;
  remoteURL?: string;
  forge?: string;
  branch: string;
  baseBranch: string;
  forgeOwner?: string;
  forgeRepo?: string;
  forgePR?: number;
  ciStatus?: string;
  ciChecks?: ForgeCheck[];
  harness: string;
  model?: string;
  costUSD?: number;
  cumulativeInputTokens?: number;
  cumulativeOutputTokens?: number;
  cumulativeCacheCreationInputTokens?: number;
  cumulativeCacheReadInputTokens?: number;
  stoppedDiskUsedBytes: number;
  diffStat?: DiffFileStat[];
  repoStates?: GitRepositoryState[];
  vncPort?: number;
  sudoPassword?: string;
  supportsImages?: boolean;
  supportsCompact?: boolean;
  rateLimit?: TaskRateLimit;
  now: number;
  onStop: (id: string) => void;
  onPurge: (id: string) => void;
  onRevive: (id: string) => void;
  onFork?: (id: string) => void;
  onQuotaRecovery?: (id: string) => void;
  quotaDismissed?: boolean;
  onDismissQuotaWarning?: (id: string) => void;
  parentTaskID?: string;
  childTasks: { id: string; title: string }[];
  onClose: () => void;
  onError: (message: string) => void;
  inputDraft: string;
  onInputDraft: (value: string) => void;
  inputImages: DraftImage[];
  imageConstraints: ImageConstraints | null;
  imageGeneration: number;
  onAddImages: (blobs: Blob[]) => void;
  onRemoveImage: (image: DraftImage) => void;
  onSendInput: () => Promise<void>;
  onCancelInputConversion: () => void;
}

type CIStatus = "pending" | "success" | "failure";

const CI_STATUS_CLASS: Record<CIStatus, string> = {
  pending: styles.ciStatus_pending,
  success: styles.ciStatus_success,
  failure: styles.ciStatus_failure,
};

const CI_STATUS_LABEL: Record<CIStatus, string> = {
  pending: "CI: pending",
  success: "CI: passed",
  failure: "CI: failed",
};

function ciLabel(status: CIStatus, checks?: ForgeCheck[]): string {
  if (!checks || checks.length === 0) return CI_STATUS_LABEL[status];
  if (status === "pending" || status === "failure") {
    const done = checks.filter((c) => c.status === "completed").length;
    if (done < checks.length) return `CI: ${done}/${checks.length}`;
  }
  return CI_STATUS_LABEL[status];
}

function checkDuration(c: ForgeCheck, now: number): string {
  const start = c.startedAt ? new Date(c.startedAt).getTime() : c.queuedAt ? new Date(c.queuedAt).getTime() : 0;
  if (!start) return "";
  const end = c.completedAt ? new Date(c.completedAt).getTime() : now;
  return formatElapsed(end - start);
}

function checkStatusLabel(c: ForgeCheck): string {
  if (c.status === "completed") {
    if (c.conclusion === "success" || c.conclusion === "neutral" || c.conclusion === "skipped") return "passed";
    return c.conclusion || "failed";
  }
  if (c.status === "in_progress") return "running";
  return "queued";
}

function checkJobURL(c: ForgeCheck, forge?: string): string | undefined {
  if (forge === "gitlab") return `https://gitlab.com/${c.owner}/${c.repo}/-/jobs/${c.jobID}`;
  if (c.runID && c.jobID) return `https://github.com/${c.owner}/${c.repo}/actions/runs/${c.runID}/job/${c.jobID}`;
  return undefined;
}

function ciActionsURL(remoteURL?: string, forge?: string): string | undefined {
  if (!remoteURL) return undefined;
  return forge === "gitlab" ? `${remoteURL}/-/pipelines` : `${remoteURL}/actions`;
}

export default function TaskDetail(props: Props) {
  const hostMode = useHostMode();
  const location = useLocation();
  const taskPath = () => location.pathname.split("/").slice(0, 3).join("/");
  const timeline = useTaskTimeline();
  const messages = timeline.messages;
  const metadata = () => timeline.manifest()?.metadata;
  const sections = () => timeline.manifest()?.sections ?? [];
  const currentSection = () => sections().at(-1) ?? null;
  const archivedIDs = createMemo(() =>
    sections()
      .slice(0, -1)
      .map((section) => section.id),
  );
  const [openActivityIDs, setOpenActivityIDs] = createSignal<ReadonlySet<string>>(new Set());
  const incarnation = createMemo(() => `${props.taskId}:${timeline.manifest()?.bootstrap.generation ?? ""}`);
  createEffect(() => {
    incarnation();
    setOpenActivityIDs(new Set<string>());
  });
  const pinActivity = (id: string, open: boolean) => {
    const activity = metadata()?.activities.find((value) => value.kind === "nativeSubagent" && value.id === id);
    const home = activity ? activity.sectionID : null;
    const focused =
      document.activeElement instanceof HTMLElement
        ? document.activeElement.closest<HTMLElement>("[data-native-id]")
        : null;
    const restore =
      !open && activity?.status !== "running" && home !== currentSection()?.id && focused?.dataset.nativeId === id;
    setOpenActivityIDs((previous) => {
      const next = new Set(previous);
      if (open) next.add(id);
      else next.delete(id);
      return next;
    });
    if (restore && home)
      queueMicrotask(() => {
        if (!messageAreaRef?.isConnected) return;
        messageAreaRef
          .querySelector<HTMLElement>(`[data-anchor-key="${CSS.escape(`session:${home}`)}"] button`)
          ?.focus();
      });
  };
  const currentActivities = () =>
    (metadata()?.activities ?? []).filter(
      (activity) =>
        activity.status === "running" ||
        (activity.kind === "nativeSubagent" && openActivityIDs().has(activity.id)) ||
        activity.sectionID === currentSection()?.id,
    );
  const nativeActivities = createMemo(() =>
    currentActivities().filter((activity) => activity.kind === "nativeSubagent"),
  );
  const displayedAtRoot = (event: EventMessage) => historyRoot(event, metadata()?.routing ?? []);
  const [sending, setSending] = createSignal(false);
  const [pendingAction, setPendingAction] = createSignal<"sync" | "restart" | "compact" | null>(null);
  const [actionError, setActionError] = createSignal<string | null>(null);
  const [safetyIssues, setSafetyIssues] = createSignal<SafetyIssue[]>([]);
  const [dismissedKey, setDismissedKey] = createSignal<string | null>(null);
  const isQuotaDismissed = () =>
    props.quotaDismissed || (dismissedKey() !== null && dismissedKey() === quotaDismissalKey(props.rateLimit));

  const handleDismissQuota = () => {
    setDismissedKey(quotaDismissalKey(props.rateLimit));
    props.onDismissQuotaWarning?.(props.taskId);
  };
  const [contextMenuOpen, setContextMenuOpen] = createSignal(false);
  const [fixingPR, setFixingPR] = createSignal(false);
  // Compact Git state is pushed with the task over the task-list stream.
  const repoStates = () => props.repoStates ?? [];
  const [elideHeaderGitStats, setElideHeaderGitStats] = createSignal(false);
  let headerRef: HTMLDivElement | undefined;
  let headerMetaRef: HTMLSpanElement | undefined;
  let headerGitStatsFrame: number | undefined;

  function isEllipsized(element: HTMLElement | undefined): boolean {
    return !!element && element.scrollWidth > element.clientWidth;
  }

  function headerWraps(): boolean {
    if (!headerRef) return false;
    const headerItems = Array.from(headerRef.children).filter(
      (child): child is HTMLElement => child instanceof HTMLElement && getComputedStyle(child).display !== "none",
    );
    const first = headerItems[0]?.getBoundingClientRect();
    const firstCenter = first && first.top + first.height / 2;
    return (
      firstCenter !== undefined &&
      headerItems.some((item) => {
        const box = item.getBoundingClientRect();
        return Math.abs(box.top + box.height / 2 - firstCenter) > 1;
      })
    );
  }

  function scheduleHeaderGitStatsElision() {
    if (headerGitStatsFrame !== undefined) cancelAnimationFrame(headerGitStatsFrame);
    setElideHeaderGitStats(false);
    headerGitStatsFrame = requestAnimationFrame(() => {
      headerGitStatsFrame = undefined;
      setElideHeaderGitStats(isEllipsized(headerMetaRef) || headerWraps());
    });
  }

  onMount(() => {
    scheduleHeaderGitStatsElision();
    window.addEventListener("resize", scheduleHeaderGitStatsElision);
    onCleanup(() => {
      window.removeEventListener("resize", scheduleHeaderGitStatsElision);
      if (headerGitStatsFrame !== undefined) cancelAnimationFrame(headerGitStatsFrame);
    });
  });

  createEffect(() => {
    const hasRepositoryState =
      repoStates().some((state) => repoStateLabel(state)) || repoStateLabel(diffStatState(props.diffStat));
    if (!hasRepositoryState) {
      setElideHeaderGitStats(false);
      return;
    }
    scheduleHeaderGitStatsElision();
  });

  // The prompt may appear after the task fetch; focus it whenever its ref attaches.
  // Touch-primary devices skip this to avoid opening the software keyboard.
  function setPromptRef(element: HTMLElement) {
    if (!props.autoFocusPrompt || window.matchMedia("(hover: none) and (pointer: coarse)").matches) return;
    requestAnimationFrame(() => {
      if (element.isConnected && taskViewMode(location.pathname, location.search) === "closed") element.focus();
    });
  }

  function handlePromptNavigation(event: KeyboardEvent) {
    if (event.key === "Escape") {
      if (taskViewMode(location.pathname, location.search) !== "closed") return;
      event.preventDefault();
      event.stopPropagation();
      props.onClose();
      requestAnimationFrame(() => {
        document.querySelector<HTMLElement>("[data-testid='prompt-input']")?.focus();
      });
      return;
    }
    if (event.key !== "Tab" || !event.shiftKey) return;
    const currentCard = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]")).find(
      (candidate) => candidate.dataset.taskId === props.taskId,
    );
    if (
      !currentCard ||
      currentCard.getClientRects().length === 0 ||
      getComputedStyle(currentCard).visibility === "hidden" ||
      currentCard.closest("[inert]")
    )
      return;
    event.preventDefault();
    event.stopPropagation();
    // Task updates can replace the card while this handler runs. Focus the
    // current card after Solid has applied that update.
    requestAnimationFrame(() => {
      const card = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]")).find(
        (candidate) => candidate.dataset.taskId === props.taskId,
      );
      card?.focus();
    });
  }

  onMount(() => {
    // Escape dismisses the task detail (navigates to /).
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.defaultPrevented || taskViewMode(location.pathname, location.search) !== "closed")
        return;
      const target = e.target instanceof HTMLElement ? e.target : null;
      if (target?.closest("[data-testid='task-detail-prompt']")) return;
      props.onClose();
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  let messageAreaRef: HTMLDivElement | undefined;
  const historyScroll = createHistoryScroll(
    () => messageAreaRef,
    () => props.taskId,
    timeline.epoch,
    () => [messages(), metadata()?.activities, props.currentPlan],
  );

  // Expansion state for collapsed past turns and sessions. Persisted per task across remounts.
  const [expandedTurnKeys, setExpandedTurnKeys] = createSignal<ReadonlySet<string>>(
    expandedTurnsByTask.get(props.taskId) ?? new Set<string>(), // eslint-disable-line solid/reactivity -- initial value; createEffect below syncs on taskId changes
  );
  const [expandedSessionKeys, setExpandedSessionKeys] = createSignal<ReadonlySet<string>>(
    expandedSessionsByTask.get(props.taskId) ?? new Set<string>(), // eslint-disable-line solid/reactivity -- initial value
  );
  // Sync expansion state when taskId changes.
  createEffect(() => {
    setExpandedTurnKeys(expandedTurnsByTask.get(props.taskId) ?? new Set<string>());
    setExpandedSessionKeys(expandedSessionsByTask.get(props.taskId) ?? new Set<string>());
  });
  // Expanding/collapsing a past turn or session replaces its row with a different
  // number of rows, shifting every row after it. Since those rows are matched by
  // array position (not stable identity), Solid destroys and recreates them, which
  // defeats the browser's native scroll anchoring. Re-anchor manually: remember the
  // clicked row's viewport offset, then after the DOM updates, find the row with the
  // same data-anchor-key and correct scrollTop so it lands back at that offset.
  function anchoredToggleTurn(e: MouseEvent, key: string) {
    const anchorKey = `turn:${key}`;
    const beforeTop = (e.currentTarget as HTMLElement | null)?.getBoundingClientRect().top;
    toggleTurn(key);
    historyScroll.restoreAnchor(anchorKey, beforeTop);
  }
  function anchoredToggleSession(e: MouseEvent, key: string) {
    const anchorKey = `session:${key}`;
    const beforeTop = (e.currentTarget as HTMLElement | null)?.getBoundingClientRect().top;
    toggleSession(key);
    historyScroll.restoreAnchor(anchorKey, beforeTop);
  }
  function toggleTurn(key: string) {
    setExpandedTurnKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      expandedTurnsByTask.set(props.taskId, next);
      return next;
    });
  }
  function toggleSession(key: string) {
    setExpandedSessionKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      expandedSessionsByTask.set(props.taskId, next);
      return next;
    });
  }

  // Project root ownership and completed/live contexts atomically before
  // presentation consumes them. Closed contexts keep their object identities.
  const rootProjection = new RootConversationProjection();
  let derivedTaskId = "";
  let derivedEpoch = -1;
  let derivedRouting = -1;
  const conversation = createMemo(() => {
    const taskId = props.taskId;
    const epoch = timeline.epoch();
    const routing = metadata()?.routingRevision ?? 0;
    const reset = taskId !== derivedTaskId || epoch !== derivedEpoch || routing !== derivedRouting;
    derivedTaskId = taskId;
    derivedEpoch = epoch;
    derivedRouting = routing;
    return rootProjection.derive(messages(), displayedAtRoot, reset);
  });

  // Fold task timing incrementally so live flushes process only appended events.
  const timingTracker = new VisualTimingTracker();
  let timingTaskId = "";
  let timingEpoch = -1;
  const previousVisualTimes = createMemo(() => {
    const taskId = props.taskId;
    const epoch = timeline.epoch();
    const reset = taskId !== timingTaskId || epoch !== timingEpoch;
    timingTaskId = taskId;
    timingEpoch = epoch;
    const first = currentSection()?.body?.first;
    const previous = first ? metadata()?.turns.findLast((turn) => compareHistoryPosition(turn, first) < 0) : null;
    return timingTracker.derive(messages(), reset, previous ? Date.parse(previous.at) : 0);
  });
  const numericTurns = createMemo(
    () => new Map((metadata()?.turns ?? []).map((turn) => [historyPositionKey(turn), turn])),
  );
  const turnForEvent = (event: EventMessage) => {
    const position = timeline.position(event);
    return position ? numericTurns().get(historyPositionKey(position)) : undefined;
  };
  const numericWaits = createMemo(
    () =>
      new Map(
        (metadata()?.turns ?? []).flatMap((turn) =>
          turn.waitInput && turn.waitMs !== undefined
            ? [[historyPositionKey(turn.waitInput), turn.waitMs] as const]
            : [],
        ),
      ),
  );
  let inputEpoch = -1;
  let inputOffset = 0;
  const loadedInputs = createMemo<readonly EventMessage[]>((previous) => {
    const epoch = timeline.epoch();
    const history = messages();
    if (inputEpoch !== epoch || history.length < inputOffset) {
      inputOffset = 0;
      inputEpoch = epoch;
      previous = [];
    }
    const added: EventMessage[] = [];
    for (; inputOffset < history.length; inputOffset++) {
      const message = history[inputOffset];
      if (message?.userInput) added.push(message);
    }
    return added.length ? [...previous, ...added] : previous;
  }, []);
  const userWaits = createMemo(() => {
    const values = new Map<EventMessage, number>();
    const waits = numericWaits();
    for (const message of loadedInputs()) {
      const position = timeline.position(message);
      const wait = position ? waits.get(historyPositionKey(position)) : undefined;
      if (wait !== undefined) values.set(message, wait);
    }
    return values;
  });
  const turnTiming = (turn: Turn) => {
    const event = turn.groups.flatMap((group) => group.events).find((item) => item.result === turn.result);
    return event ? turnForEvent(event) : undefined;
  };
  const statsHistory = timeline.stats;
  const hasInitialPromptEvent = () => !timeline.loading() && (metadata()?.hasInitialPrompt ?? false);
  const currentSessionCompletedTurns = createMemo(() => conversation().turns);
  const currentBoundary = createMemo<HistoryEvent | null>((previous) => {
    const next = currentSection()?.boundary ?? null;
    return previous && next && historyPositionKey(previous) === historyPositionKey(next) ? previous : next;
  }, null);
  const currentSessionBoundaryEvent = () => currentBoundary()?.message ?? null;
  const currentSessionKey = createMemo(() => `session:${currentSection()?.id ?? "current"}`);
  const currentGroups = createMemo(() => conversation().groups);

  // Whether a live turn is currently streaming. Mirrors Android's hasLiveTurn.
  const hasLiveTurn = createMemo(() => currentGroups().length > 0);
  // Elidable turns (all completed turns except the last, which is always expanded).
  // Cached in its own memo so slice(0, -1) only runs on turn completion, matching
  // Android's remember(currentSessionCompletedTurns) { dropLast(1) } pattern.
  const elidableTurns = createMemo(() => {
    const completedTurns = currentSessionCompletedTurns();
    return completedTurns.slice(0, -1);
  });
  // Completed turn items (elidable): stable during streaming.
  const completedTurnItems = createMemo(() => buildTurnItems(elidableTurns(), expandedTurnKeys(), currentSessionKey()));
  // Last completed turn is always expanded.
  const lastTurnItems = createMemo((): MsgItem[] => {
    const completedTurns = currentSessionCompletedTurns();
    const last = completedTurns[completedTurns.length - 1];
    if (!last) return [];
    // Match Android's keyPrefix strategy: when a live turn exists, use "lg" to avoid
    // key collisions with live items. When idle, use "g" so the LazyColumn can reuse
    // composables if the same turn transitions between live and last-expanded states.
    const initialKeyPrefix = hasLiveTurn() ? "lg" : "g";
    return last.groups.map((g, j) => ({
      kind: "group" as const,
      group: g,
      isLive: false,
      key: g.messageKey ? `message:${g.messageKey}` : `${initialKeyPrefix}:${j}`,
    }));
  });
  // Session boundary item for the current session.
  const sessionBoundaryItems = createMemo((): MsgItem[] => {
    const boundaryEv = currentSessionBoundaryEvent();
    return boundaryEv
      ? [
          {
            kind: "sessionBoundary",
            event: boundaryEv,
            key: "cur-sess-boundary",
          },
        ]
      : [];
  });
  // Live turn groups: incomplete turn groups flushed on structural boundaries.
  const liveItems = createMemo((): MsgItem[] => {
    const liveGroups = currentGroups();
    return liveGroups
      .filter(
        (group) =>
          !(
            group.kind === "other" &&
            group.events.length === 1 &&
            group.events[0]?.kind === "system" &&
            isSessionBoundary(group.events[0])
          ),
      )
      .map((g, j) => {
        return { kind: "group", group: g, isLive: true, key: g.messageKey ? `message:${g.messageKey}` : `g:${j}` };
      });
  });
  // Current boundary, collapsed completed turns, expanded latest turn, and live groups.
  const transcriptItems = createMemo((): MsgItem[] => [
    ...sessionBoundaryItems(),
    ...completedTurnItems(),
    ...lastTurnItems(),
    ...liveItems(),
  ]);

  // Last ask group: only the most recent ask is interactive.
  // Flatten all groups from all sessions + current live turn for ask detection.
  const allGroups = createMemo(() => {
    const sessionGroups = currentSessionCompletedTurns().flatMap((turn) => turn.groups);
    return [...sessionGroups, ...currentGroups()];
  });
  const lastAskGroup = createMemo((): MessageGroup | null => {
    const groups = allGroups();
    for (let i = groups.length - 1; i >= 0; i--) {
      const group = groups[i];
      const event = group?.events[0];
      if (group?.kind === "ask" && event && !isChildEvent(event)) return group;
    }
    return null;
  });

  let inputActive = true;
  onCleanup(() => {
    inputActive = false;
    timingTracker.derive([], true, 0);
    rootProjection.clear();
    props.onCancelInputConversion();
  });
  async function sendInput() {
    if (sending() || (!props.inputDraft.trim() && props.inputImages.length === 0)) return;
    notifications.requestNotificationPermission({ enabled: hostMode.browserNotificationsEnabled() });
    setSending(true);
    setActionError(null);
    try {
      await props.onSendInput();
    } finally {
      if (inputActive) setSending(false);
    }
  }

  async function sendAskAnswer(text: string) {
    setSending(true);
    try {
      await api.sendInput(props.taskId, { prompt: { text } });
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Unknown error";
      setActionError(`send failed: ${msg}`);
      setTimeout(() => setActionError(null), 5000);
    } finally {
      setSending(false);
    }
  }

  const hasUnansweredAsk = () => {
    const group = lastAskGroup();
    return group !== null && group.answerText === undefined;
  };

  // Detail SSE can show an ask before the task-list SSE patches the task state.
  const isPendingAsk = () => props.taskState === "pending" && hasUnansweredAsk();

  const isActive = () => {
    const s = props.taskState;
    return (
      s === "running" ||
      s === "branching" ||
      s === "provisioning" ||
      s === "starting" ||
      s === "waiting" ||
      s === "asking" ||
      s === "has_plan" ||
      s === "purging" ||
      isPendingAsk()
    );
  };

  const isWaiting = () =>
    props.taskState === "waiting" || props.taskState === "asking" || props.taskState === "has_plan" || isPendingAsk();
  const isRecoverable = () => props.taskState === "stopped" || props.taskState === "crashed";
  const canSendInput = () => isActive() && props.taskState !== "purging";
  const prURL = () => {
    const owner = props.forgeOwner;
    const repo = props.forgeRepo;
    const pr = props.forgePR;
    if (!owner || !repo || !pr) return undefined;
    if (props.forge === "gitlab") return `https://gitlab.com/${owner}/${repo}/-/merge_requests/${pr}`;
    return `https://github.com/${owner}/${repo}/pull/${pr}`;
  };

  const prLabel = () => {
    const pr = props.forgePR;
    if (!pr) return undefined;
    return props.forge === "gitlab" ? `MR #${pr}` : `PR #${pr}`;
  };

  function clearAndExecutePlan() {
    const prompt = props.inputDraft.trim();
    // eslint-disable-next-line solid/reactivity -- only called from onClick
    runAction("restart", async () => {
      await api.restartTask(props.taskId, { prompt: { text: prompt } });
      props.onInputDraft("");
    });
  }

  function doCompact() {
    // eslint-disable-next-line solid/reactivity -- only called from onClick
    runAction("compact", () => api.compactContext(props.taskId, {}));
  }

  async function doSync(force: boolean, target?: SyncTarget) {
    if (pendingAction()) return;
    setPendingAction("sync");
    setActionError(null);
    setSafetyIssues([]);
    try {
      const resp = await api.syncTask(props.taskId, {
        force,
        ...(target ? { target } : {}),
      });
      if (resp.status === "blocked" && resp.safetyIssues?.length) {
        setSafetyIssues(resp.safetyIssues);
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Unknown error";
      setActionError(`sync failed: ${msg}`);
      setTimeout(() => setActionError(null), 5000);
    } finally {
      setPendingAction(null);
    }
  }

  async function handleFixPR() {
    if (fixingPR()) return;
    setFixingPR(true);
    setActionError(null);
    try {
      await api.botFixPR({ taskId: props.taskId });
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Unknown error";
      setActionError(`fix PR failed: ${msg}`);
      setTimeout(() => setActionError(null), 5000);
    } finally {
      setFixingPR(false);
    }
  }

  async function runAction(name: "sync" | "restart" | "compact", fn: () => Promise<unknown>) {
    if (pendingAction()) return;
    setPendingAction(name);
    setActionError(null);
    try {
      await fn();
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Unknown error";
      setActionError(`${name} failed: ${msg}`);
      setTimeout(() => setActionError(null), 5000);
    } finally {
      setPendingAction(null);
    }
  }

  // Payload-free lifecycle summaries remain available across context boundaries.
  const nativeSettled = () => !isActive();
  const renderNativeTranscript = (groups: () => MessageGroup[]) => (
    <Index each={groups()}>
      {(group) => (
        <div data-testid="stream-message" data-message-key={group().messageKey}>
          <Show when={hasGroupTiming(group())}>
            <div class={styles.timedItemContent}>
              <span class={styles.messageTiming}>
                <TimingIcon
                  events={group().events}
                  range={group().content?.range ?? group().tool?.range ?? group().range}
                />
              </span>
              <GroupContent
                group={group}
                taskId={props.taskId}
                isWaiting={() => false}
                lastAskGroup={() => null}
                onAskAnswer={sendAskAnswer}
                currentPlan={null}
                model={props.model ?? null}
                turnTiming={() => undefined}
                nativeByToolUseID={nativeByToolUseID}
                nativeSettled={nativeSettled}
                backgroundByToolUseID={backgroundByToolUseID}
                backgroundSettled={backgroundSettled}
              />
            </div>
          </Show>
        </div>
      )}
    </Index>
  );
  // Correlate canonical native activity with the tool card that spawned it.
  const nativeByToolUseID = createMemo(() => {
    const byTool = new Map<string, HistoryActivity[]>();
    for (const activity of (metadata()?.activities ?? []).filter((value) => value.kind === "nativeSubagent")) {
      if (!activity.toolUseID) continue;
      const key = toolIdentity(activity.source, activity.toolUseID);
      const list = byTool.get(key);
      if (list) list.push(activity);
      else byTool.set(key, [activity]);
    }
    return byTool;
  });
  // Anchor each card to the transcript item it settled at, or spawned at while
  // running, so agents render in the flow instead of a task-wide panel.
  const nativeByAnchor = createMemo(() =>
    assignNativeAnchors(
      transcriptItems(),
      nativeActivities().map((activity) => ({ ...activity, anchorTs: Date.parse(activity.anchorAt) })),
    ),
  );
  // An activity whose turn produced no anchorable transcript item still needs a
  // home, so it renders after the transcript instead of disappearing.
  const nativeUnanchored = createMemo(() => {
    const anchored = new Set<string>();
    for (const list of nativeByAnchor().values()) for (const a of list) anchored.add(a.id);
    return nativeActivities().filter((activity) => !anchored.has(activity.id));
  });
  type TranscriptItem = MsgItem | { kind: "native"; activity: HistoryActivity; key: string };
  // Native cards are keyed siblings, including when no parent content exists
  // yet. Adding root content or folding lifecycle updates retains their DOM.
  const items = createMemo((): TranscriptItem[] => {
    const root = transcriptItems();
    const firstRoot = root.find((item) => item.kind === "group");
    const firstRootTs = firstRoot?.kind === "group" ? (firstRoot.group.events[0]?.ts ?? 0) : 0;
    const early = new Set(
      nativeActivities()
        .filter((activity) => firstRootTs > Date.parse(activity.anchorAt))
        .map((activity) => activity.id),
    );
    const nativeItem = (activity: HistoryActivity): TranscriptItem => ({
      kind: "native",
      activity,
      key: `native:${activity.id}`,
    });
    const result: TranscriptItem[] = [];
    for (const item of root) {
      if (item === firstRoot)
        result.push(
          ...nativeActivities()
            .filter((activity) => early.has(activity.id))
            .map(nativeItem),
        );
      result.push(item);
      for (const activity of nativeByAnchor().get(item.key) ?? []) {
        if (!early.has(activity.id)) result.push(nativeItem(activity));
      }
    }
    for (const activity of nativeUnanchored()) {
      if (!early.has(activity.id)) result.push(nativeItem(activity));
    }
    return result;
  });
  const itemsByKey = createMemo<Record<string, TranscriptItem>>(() =>
    Object.fromEntries(items().map((item) => [item.key, item])),
  );
  const itemKeys = createMemo(() => items().map((item) => item.key));
  // Detached command summaries share the server-owned lifecycle metadata.
  const backgroundCommands = createMemo(() =>
    currentActivities().filter((activity) => activity.kind === "backgroundCommand"),
  );
  const backgroundSettled = () => !isActive();
  const backgroundByToolUseID = createMemo(() => {
    const byTool = new Map<string, HistoryActivity[]>();
    for (const command of (metadata()?.activities ?? []).filter((value) => value.kind === "backgroundCommand")) {
      if (!command.toolUseID) continue;
      const key = toolIdentity(command.source, command.toolUseID);
      const list = byTool.get(key);
      if (list) list.push(command);
      else byTool.set(key, [command]);
    }
    return byTool;
  });
  const backgroundByAnchor = createMemo(() =>
    assignNativeAnchors(
      transcriptItems(),
      backgroundCommands().map((activity) => ({ ...activity, anchorTs: Date.parse(activity.anchorAt) })),
    ),
  );
  const backgroundUnanchored = createMemo(() => {
    const anchored = new Set<string>();
    for (const list of backgroundByAnchor().values()) for (const c of list) anchored.add(c.id);
    return backgroundCommands().filter((command) => !anchored.has(command.id));
  });

  const renderArchivedGroups = (groups: Accessor<MessageGroup[]>, position: (event: EventMessage) => string | null) => {
    const waits = createMemo(() => {
      const values = new Map<EventMessage, number>();
      const facts = numericWaits();
      for (const group of groups())
        for (const event of group.events) {
          const key = position(event);
          const wait = key ? facts.get(key) : undefined;
          if (wait !== undefined) values.set(event, wait);
        }
      return values;
    });
    return (
      <Index each={groups()}>
        {(group) => (
          <div class={styles.timedItemContent}>
            <Show when={hasGroupTiming(group()) && !group().events.some((event) => event.kind === "result")}>
              <span class={styles.messageTiming}>
                <TimingIcon
                  events={group().events}
                  range={group().content?.range ?? group().tool?.range ?? group().range}
                  segments={group().timingSegments}
                  userWaitMs={waits()}
                />
              </span>
            </Show>
            <GroupContent
              group={group}
              taskId={props.taskId}
              isWaiting={() => false}
              lastAskGroup={() => null}
              onAskAnswer={sendAskAnswer}
              currentPlan={null}
              onClearAndExecutePlan={clearAndExecutePlan}
              pendingAction={pendingAction}
              model={props.model ?? null}
              turnTiming={(event) => {
                const key = position(event);
                return key ? numericTurns().get(key) : undefined;
              }}
              nativeByToolUseID={nativeByToolUseID}
              nativeSettled={nativeSettled}
              backgroundByToolUseID={backgroundByToolUseID}
              backgroundSettled={backgroundSettled}
            />
          </div>
        )}
      </Index>
    );
  };
  const renderArchivedActivities = (id: string) => {
    const activities = () =>
      (metadata()?.activities ?? []).filter(
        (activity) =>
          activity.status !== "running" &&
          activity.sectionID === id &&
          !(activity.kind === "nativeSubagent" && openActivityIDs().has(activity.id)),
      );
    return (
      <>
        <NativeAgents
          activities={activities().filter((activity) => activity.kind === "nativeSubagent")}
          settled={nativeSettled()}
          renderTranscript={renderNativeTranscript}
        />
        <BackgroundCommands
          commands={activities().filter((activity) => activity.kind === "backgroundCommand")}
          settled={backgroundSettled()}
        />
      </>
    );
  };

  const markdownTaskId = createMemo(() => props.taskId);
  return (
    <MarkdownTaskContext.Provider value={markdownTaskId}>
      <div class={styles.container}>
        <div
          class={styles.header}
          data-testid="task-detail-header"
          ref={(element) => {
            headerRef = element;
          }}
        >
          <ActionButton
            class={styles.closeBtn}
            onClick={() => props.onClose()}
            label="Close task details"
            title="Close"
          >
            <CloseIcon width={20} height={20} />
          </ActionButton>
          <Show when={props.title}>
            <span class={styles.headerTitle}>{props.title}</span>
          </Show>
          <span
            class={styles.headerMeta}
            ref={(element) => {
              headerMetaRef = element;
            }}
          >
            <Show when={props.remoteURL} fallback={<span class={styles.headerRepo}>{props.repo}</span>}>
              <a class={styles.headerRepo} href={props.remoteURL} target="_blank" rel="noopener">
                {props.repo}
              </a>
            </Show>
            <span class={styles.headerBranch}>{props.branch}</span>
            <Show when={prURL()}>
              <a class={styles.headerPR} href={prURL()} target="_blank" rel="noopener">
                {prLabel()}
              </a>
            </Show>
            <Show when={props.ciStatus && props.ciStatus in CI_STATUS_CLASS}>
              {(() => {
                const s = props.ciStatus as CIStatus;
                const hasChecks = () => (props.ciChecks?.length ?? 0) > 0;
                const actionsURL = () => ciActionsURL(props.remoteURL, props.forge);
                return (
                  <>
                    <Show
                      when={hasChecks()}
                      fallback={
                        <Show
                          when={actionsURL()}
                          keyed
                          fallback={
                            <span class={`${styles.ciStatus} ${CI_STATUS_CLASS[s]}`}>{ciLabel(s, props.ciChecks)}</span>
                          }
                        >
                          {(url) => (
                            <a
                              class={`${styles.ciStatus} ${CI_STATUS_CLASS[s]}`}
                              href={url}
                              target="_blank"
                              rel="noopener"
                            >
                              {ciLabel(s, props.ciChecks)}
                            </a>
                          )}
                        </Show>
                      }
                    >
                      <details class={styles.ciDetails}>
                        <summary class={`${styles.ciStatus} ${CI_STATUS_CLASS[s]}`}>
                          {ciLabel(s, props.ciChecks)}
                        </summary>
                        <div class={styles.ciDropdown}>
                          <For each={props.ciChecks}>
                            {(c) => {
                              const statusCls =
                                c.status === "completed"
                                  ? c.conclusion === "success" ||
                                    c.conclusion === "neutral" ||
                                    c.conclusion === "skipped"
                                    ? styles.ciCheckPassed
                                    : styles.ciCheckFailed
                                  : c.status === "in_progress"
                                    ? styles.ciCheckRunning
                                    : styles.ciCheckQueued;
                              const jobURL = () => checkJobURL(c, props.forge);
                              return (
                                <Show
                                  when={jobURL()}
                                  keyed
                                  fallback={
                                    <div class={`${styles.ciCheckRow} ${statusCls}`}>
                                      <span class={styles.ciCheckName}>{c.name}</span>
                                      <span class={styles.ciCheckStatus}>{checkStatusLabel(c)}</span>
                                      <Show when={c.startedAt || c.queuedAt}>
                                        <span class={styles.ciCheckDuration}>{checkDuration(c, Date.now())}</span>
                                      </Show>
                                    </div>
                                  }
                                >
                                  {(url) => (
                                    <a
                                      class={`${styles.ciCheckRow} ${styles.ciCheckLink} ${statusCls}`}
                                      href={url}
                                      target="_blank"
                                      rel="noopener"
                                    >
                                      <span class={styles.ciCheckName}>{c.name}</span>
                                      <span class={styles.ciCheckStatus}>{checkStatusLabel(c)}</span>
                                      <Show when={c.startedAt || c.queuedAt}>
                                        <span class={styles.ciCheckDuration}>{checkDuration(c, Date.now())}</span>
                                      </Show>
                                    </a>
                                  )}
                                </Show>
                              );
                            }}
                          </For>
                        </div>
                      </details>
                    </Show>
                    <Show
                      when={
                        s === "failure" &&
                        props.ciChecks?.some(
                          (c) => c.conclusion !== "success" && c.conclusion !== "neutral" && c.conclusion !== "skipped",
                        )
                      }
                    >
                      <button
                        class={styles.fixCIBtn}
                        onClick={handleFixPR}
                        disabled={fixingPR()}
                        title="Inject a fix-PR command into this task (auto mode — no new task created)"
                      >
                        {fixingPR() ? "Sending…" : "Fix PR"}
                      </button>
                    </Show>
                  </>
                );
              })()}
            </Show>
          </span>
          <A class={styles.diffLink} href={`${taskPath()}/info`} aria-label="Task info" title="Task info">
            <InfoIcon width="13" height="13" aria-hidden="true" />
          </A>
          <Show
            when={repoStates().some((state) => repoStateLabel(state)) || repoStateLabel(diffStatState(props.diffStat))}
          >
            <span class={styles.repoStateLinks}>
              <For each={repoStates()}>
                {(state) => (
                  <RepoStateIcons
                    state={state}
                    href={`${taskPath()}/diff`}
                    elideDiffStats={elideHeaderGitStats()}
                    onNavigateIntent={() => void prefetchTaskDiff(props.taskId).catch(() => undefined)}
                  />
                )}
              </For>
              <Show when={!repoStates().some((state) => repoStateLabel(state))}>
                <RepoStateIcons
                  state={diffStatState(props.diffStat)}
                  href={`${taskPath()}/diff`}
                  elideDiffStats={elideHeaderGitStats()}
                  onNavigateIntent={() => void prefetchTaskDiff(props.taskId).catch(() => undefined)}
                />
              </Show>
            </span>
          </Show>
          <Show
            when={
              props.taskState !== "pending" &&
              props.taskState !== "branching" &&
              props.taskState !== "provisioning" &&
              props.taskState !== "starting" &&
              props.taskState !== "stopped" &&
              props.taskState !== "crashed" &&
              props.taskState !== "purged" &&
              props.taskState !== "failed"
            }
          >
            <A
              class={styles.diffLink}
              href={`${taskPath()}/processes`}
              aria-label="Task processes"
              title="Task processes"
            >
              <ProcessesIcon width="13" height="13" aria-hidden="true" />
            </A>
          </Show>
          <Show when={(props.vncPort ?? 0) > 0}>
            <A class={styles.diffLink} href={`${taskPath()}/vnc`}>
              VNC
            </A>
          </Show>
          <Show when={props.sudoPassword}>
            <span class={styles.sudoPassword}>
              <span class={styles.sudoPasswordText}>{props.sudoPassword}</span>
              <button
                class={styles.sudoCopyBtn}
                title="Copy password"
                onClick={(e) => {
                  const btn = e.currentTarget;
                  if (props.sudoPassword) {
                    navigator.clipboard.writeText(props.sudoPassword);
                  }
                  const copiedClass = styles.copied;
                  btn.classList.add(copiedClass);
                  setTimeout(() => btn.classList.remove(copiedClass), 1500);
                }}
              >
                <CopyIcon width={14} height={14} class={styles.copyIcon} />
                <CheckIcon width={14} height={14} class={styles.checkIcon} />
              </button>
            </span>
          </Show>
          <Show when={props.inPlanMode}>
            <span class={styles.planIndicator} title="Agent is in plan mode">
              Plan Mode
            </span>
          </Show>
          <Show when={props.taskState === "stopped" && props.stoppedDiskUsedBytes >= 0}>
            <span class={styles.stoppedDiskUsage} title="Writable disk space retained by this stopped task">
              Disk {formatBytes(props.stoppedDiskUsedBytes)}
            </span>
          </Show>
          <StatsIcon
            href={`${taskPath()}/stats`}
            stats={statsHistory()}
            usage={{
              inputTokens: props.cumulativeInputTokens ?? 0,
              cacheWriteInputTokens: props.cumulativeCacheCreationInputTokens ?? 0,
              cacheReadInputTokens: props.cumulativeCacheReadInputTokens ?? 0,
              outputTokens: props.cumulativeOutputTokens ?? 0,
              costUSD: props.costUSD ?? 0,
            }}
          />
        </div>
        <Show when={props.parentTaskID || props.childTasks.length > 0}>
          <nav class={styles.hierarchy} aria-label="Task hierarchy">
            <Show when={props.parentTaskID} keyed>
              {(parentID) => (
                <A class={styles.hierarchyLink} href={`/task/@${parentID}`}>
                  Parent task
                </A>
              )}
            </Show>
            <For each={props.childTasks}>
              {(child) => (
                <A class={styles.hierarchyLink} href={`/task/@${child.id}`}>
                  Child: {child.title || child.id}
                </A>
              )}
            </For>
          </nav>
        </Show>
        <SkillStrip taskId={props.taskId} skills={currentSection()?.skills ?? []} label="Loaded skills" />
        <Show when={props.rateLimit?.blocked && !isQuotaDismissed()}>
          <section
            class={styles.quotaRecovery}
            aria-labelledby="quota-recovery-title"
            data-testid="quota-recovery-detail"
          >
            <div>
              <h4 id="quota-recovery-title" class={styles.quotaRecoveryTitle}>
                Agent quota exhausted
              </h4>
              <p class={styles.quotaRecoveryText}>
                {props.rateLimit?.window || "Current"} quota resets in{" "}
                {formatQuotaCountdown(props.rateLimit?.resetsAt ?? "", props.now)}. You can keep this task unchanged and
                continue its workspace in a new agent.
              </p>
            </div>
            <div class={styles.quotaRecoveryActions}>
              <Show when={props.onQuotaRecovery && props.repo}>
                <Button
                  type="button"
                  onClick={() => props.onQuotaRecovery?.(props.taskId)}
                  data-testid="quota-recovery-detail-action"
                >
                  Continue in new agent
                </Button>
              </Show>
              <button
                type="button"
                class={styles.quotaDismiss}
                onClick={handleDismissQuota}
                aria-label="Dismiss quota warning"
                title="Dismiss quota warning"
                data-testid="quota-recovery-detail-dismiss"
              >
                <CloseIcon width={18} height={18} aria-hidden="true" />
              </button>
            </div>
          </section>
        </Show>
        <Show when={props.error} keyed>
          {(error) => (
            <section class={styles.taskError} aria-labelledby="task-error-title">
              <h4 id="task-error-title" class={styles.taskErrorTitle}>
                Task error
              </h4>
              <pre class={styles.taskErrorText}>{error}</pre>
            </section>
          )}
        </Show>
        <Show when={timeline.loading() && !timeline.error()}>
          <div class={styles.historyLoading} role="status" aria-label="Loading history">
            <LoadingIcon aria-hidden="true" />
          </div>
        </Show>
        <div
          class={styles.messageArea}
          ref={messageAreaRef}
          onScroll={historyScroll.scroll}
          tabIndex={0}
          role="region"
          aria-label="Task conversation"
          data-testid="task-message-area"
          aria-busy={timeline.loading()}
        >
          <Show when={timeline.error()} keyed>
            {(error) => (
              <div class={styles.historyNavigation} role="alert">
                <span>{error}</span>
                <Button variant="gray" onClick={timeline.retry}>
                  Retry history
                </Button>
              </div>
            )}
          </Show>

          <Show when={!hasInitialPromptEvent() && props.initialPrompt} keyed>
            {(prompt) => (
              <section class={styles.taskPrompt} aria-labelledby="task-prompt-title">
                <h4 id="task-prompt-title" class={styles.taskPromptTitle}>
                  Prompt
                </h4>
                <div class={styles.userInputMsg}>
                  <Markdown text={prompt} />
                </div>
              </section>
            )}
          </Show>
          <Show when={metadata()?.setup}>
            <SetupHistory startedAt={props.startedAt} />
          </Show>
          <For each={archivedIDs()}>
            {(id) => (
              <Show when={sections().find((section) => section.id === id)}>
                {(section) => (
                  <ArchivedSession
                    section={section()}
                    taskId={props.taskId}
                    open={expandedSessionKeys().has(id)}
                    expandedTurns={expandedTurnKeys()}
                    onToggleTurn={anchoredToggleTurn}
                    onToggle={(event) => anchoredToggleSession(event, id)}
                    renderGroups={renderArchivedGroups}
                    renderActivities={() => renderArchivedActivities(id)}
                  />
                )}
              </Show>
            )}
          </For>
          <For each={itemKeys()}>
            {(key) => {
              const item = (): TranscriptItem => {
                const current = itemsByKey()[key];
                if (!current) throw new Error("Missing transcript item");
                return current;
              };
              // Type-narrowing accessors for the MsgItem discriminated union.
              const sessBoundary = () =>
                item().kind === "sessionBoundary" ? (item() as Extract<MsgItem, { kind: "sessionBoundary" }>) : null;
              const elided = () => (item().kind === "elided" ? (item() as Extract<MsgItem, { kind: "elided" }>) : null);
              const expHdr = () =>
                item().kind === "expandedHeader" ? (item() as Extract<MsgItem, { kind: "expandedHeader" }>) : null;
              const grpItem = () => (item().kind === "group" ? (item() as Extract<MsgItem, { kind: "group" }>) : null);
              // Canonical native activity anchored to this item: a settled card
              // renders where it settled, a running one where it spawned.
              const anchorIndent = () => {
                const current = item();
                if (current.kind === "group") {
                  return current.indent === "turn" ? styles.indentTurn : undefined;
                }
                return undefined;
              };
              return (
                <>
                  <Switch>
                    <Match when={item().kind === "native"}>
                      <NativeAgents
                        activities={
                          item().kind === "native"
                            ? [(item() as Extract<TranscriptItem, { kind: "native" }>).activity]
                            : []
                        }
                        settled={nativeSettled()}
                        renderTranscript={renderNativeTranscript}
                        onOpenChange={pinActivity}
                      />
                    </Match>
                    {/* Session boundary: init, compaction, or clear rendered as a separator. */}
                    <Match when={sessBoundary()} keyed>
                      {(sb) => (
                        <SessionBoundaryItem
                          event={sb.event}
                          grouped={sb.event === currentSessionBoundaryEvent() && archivedIDs().length > 0}
                        />
                      )}
                    </Match>
                    {/* Collapsed past turn: single clickable row. */}
                    <Match when={elided()} keyed>
                      {(e) => (
                        <div class={styles.elidedTurn} data-anchor-key={`turn:${e.key}`}>
                          <button
                            type="button"
                            class={styles.turnToggle}
                            onClick={(ev) => anchoredToggleTurn(ev, e.key)}
                          >
                            <span class={styles.turnSummaryText}>{turnSummary(e.turn)}</span>
                            <span class={styles.turnDuration}>
                              {e.turn.durationMs > 0 ? formatTimingDuration(e.turn.durationMs) : "0s"}
                            </span>
                          </button>
                          <Show when={turnTiming(e.turn)}>
                            {(turn) => <TurnInvocationIcon turn={turn()} model={props.model ?? null} />}
                          </Show>
                        </div>
                      )}
                    </Match>
                    {/* Expanded past turn header: click to collapse. */}
                    <Match when={expHdr()} keyed>
                      {(h) => (
                        <div
                          class={`${styles.elidedTurn} ${styles.elidedTurnExpanded}`}
                          data-anchor-key={`turn:${h.turnKey}`}
                        >
                          <button
                            type="button"
                            class={styles.turnToggle}
                            onClick={(ev) => anchoredToggleTurn(ev, h.turnKey)}
                          >
                            <span class={styles.turnSummaryText}>{turnSummary(h.turn)}</span>
                            <span class={styles.turnDuration}>
                              {h.turn.durationMs > 0 ? formatTimingDuration(h.turn.durationMs) : "0s"}
                            </span>
                          </button>
                          <Show when={turnTiming(h.turn)}>
                            {(turn) => <TurnInvocationIcon turn={turn()} model={props.model ?? null} />}
                          </Show>
                        </div>
                      )}
                    </Match>
                    {/* Message group: non-keyed to preserve iframe state in WidgetCard. */}
                    <Match when={grpItem()}>
                      {(gi) => (
                        <div
                          class={gi().indent === "turn" ? styles.indentTurn : undefined}
                          data-testid={gi().group.messageKey ? "stream-message" : undefined}
                          data-message-key={gi().group.messageKey}
                        >
                          <div class={styles.timedItemContent}>
                            <Show
                              when={
                                hasGroupTiming(gi().group) &&
                                !gi().group.events.some((event) => event.kind === "result") &&
                                (gi().group.kind !== "action" || gi().group.toolCalls.length !== 1)
                              }
                            >
                              <span class={styles.messageTiming}>
                                <TimingIcon
                                  events={gi().group.events}
                                  range={gi().group.content?.range ?? gi().group.tool?.range ?? gi().group.range}
                                  segments={gi().group.timingSegments}
                                  userWaitMs={userWaits()}
                                  previousEventTs={previousVisualTimes()}
                                />
                              </span>
                            </Show>
                            <GroupContent
                              group={() => gi().group}
                              taskId={props.taskId}
                              isWaiting={isWaiting}
                              lastAskGroup={lastAskGroup}
                              onAskAnswer={sendAskAnswer}
                              currentPlan={props.currentPlan}
                              onClearAndExecutePlan={clearAndExecutePlan}
                              pendingAction={pendingAction}
                              model={props.model ?? null}
                              turnTiming={(event) => turnForEvent(event)}
                              nativeByToolUseID={nativeByToolUseID}
                              nativeSettled={nativeSettled}
                              backgroundByToolUseID={backgroundByToolUseID}
                              backgroundSettled={backgroundSettled}
                            />
                          </div>
                        </div>
                      )}
                    </Match>
                  </Switch>
                  <Show when={backgroundByAnchor().get(item().key)?.length}>
                    <div class={anchorIndent()}>
                      <BackgroundCommands
                        commands={backgroundByAnchor().get(item().key) ?? []}
                        settled={backgroundSettled()}
                      />
                    </div>
                  </Show>
                </>
              );
            }}
          </For>
          <Show when={backgroundUnanchored().length > 0}>
            <BackgroundCommands commands={backgroundUnanchored()} settled={backgroundSettled()} />
          </Show>
          <Show when={messages().length === 0 && !timeline.loading()}>
            <p class={styles.placeholder}>Waiting for agent output...</p>
          </Show>
        </div>

        <ProgressPanel todos={metadata()?.todo?.todos ?? []} />

        <Show when={isActive() || isRecoverable() || !!pendingAction()}>
          <form
            ref={bindPromptSubmitShortcut}
            onSubmit={(e) => {
              e.preventDefault();
              if (canSendInput()) sendInput();
            }}
            class={styles.inputForm}
            data-testid="task-detail-form"
          >
            <PromptInput
              ref={setPromptRef}
              value={props.inputDraft}
              onInput={props.onInputDraft}
              onSubmit={sendInput}
              onKeyDown={handlePromptNavigation}
              placeholder={isRecoverable() ? "Revive or fork to continue..." : "Send message to agent..."}
              disabled={!canSendInput()}
              class={styles.textInput}
              tabIndex={0}
              data-testid="task-detail-prompt"
              supportsImages={props.supportsImages}
              images={props.inputImages}
              imageConstraints={props.imageConstraints}
              imageGeneration={props.imageGeneration}
              onAddImages={props.onAddImages}
              onRemoveImage={props.onRemoveImage}
              sendButton={
                <Button
                  type="submit"
                  disabled={
                    !canSendInput() || sending() || (!props.inputDraft.trim() && props.inputImages.length === 0)
                  }
                  title="Send"
                  data-testid="send-input"
                >
                  <SendIcon width="1.1em" height="1.1em" />
                </Button>
              }
            />
            <Dropdown
              open={contextMenuOpen()}
              onOpenChange={setContextMenuOpen}
              class={styles.syncButtonGroup}
              content={
                <TaskActionsMenu
                  class={styles.syncDropdown}
                  forge={props.forge}
                  forgePR={props.forgePR}
                  baseBranch={props.baseBranch}
                  active={isActive()}
                  recoverable={isRecoverable()}
                  waiting={isWaiting()}
                  purging={props.taskState === "purging"}
                  supportsCompact={props.supportsCompact}
                  canFork={!!props.onFork && !!props.repo}
                  onSync={() => {
                    setContextMenuOpen(false);
                    doSync(false);
                  }}
                  onSyncDefault={() => {
                    setContextMenuOpen(false);
                    doSync(false, SyncTargetDefault);
                  }}
                  onStop={() => {
                    setContextMenuOpen(false);
                    props.onStop(props.taskId);
                  }}
                  onRevive={() => {
                    setContextMenuOpen(false);
                    props.onRevive(props.taskId);
                  }}
                  onPurge={() => {
                    setContextMenuOpen(false);
                    props.onPurge(props.taskId);
                  }}
                  onCompact={() => {
                    setContextMenuOpen(false);
                    doCompact();
                  }}
                  onFork={() => {
                    setContextMenuOpen(false);
                    props.onFork?.(props.taskId);
                  }}
                />
              }
            >
              <Show
                when={!!pendingAction()}
                fallback={
                  <button
                    type="button"
                    class={styles.contextMenuToggle}
                    disabled={!!pendingAction()}
                    onClick={() => setContextMenuOpen((v) => !v)}
                    aria-label="Context actions"
                    title="Context actions"
                  >
                    &#8942;
                  </button>
                }
              >
                <button type="button" class={styles.contextMenuToggle} disabled>
                  &#8987;
                </button>
              </Show>
            </Dropdown>
          </form>
          <Show when={safetyIssues().length > 0}>
            <div class={styles.safetyWarning}>
              <strong>Safety issues detected:</strong>
              <ul>
                <For each={safetyIssues()}>
                  {(issue) => (
                    <li>
                      <strong>{issue.file}</strong>: {issue.detail} ({issue.kind})
                    </li>
                  )}
                </For>
              </ul>
              <Button
                type="button"
                variant="red"
                loading={pendingAction() === "sync"}
                disabled={!!pendingAction()}
                onClick={() => {
                  setSafetyIssues([]);
                  doSync(true);
                }}
              >
                Force Push to {props.branch}
              </Button>
            </div>
          </Show>
          <Show when={actionError()}>
            <div class={styles.actionError}>{actionError()}</div>
          </Show>
        </Show>
      </div>
    </MarkdownTaskContext.Provider>
  );
}

// Renders the content for a single message group.
// group is a reactive accessor so group().kind etc track correctly in JSX.
function ArchivedSession(props: {
  section: HistorySection;
  taskId: string;
  open: boolean;
  onToggle: (event: MouseEvent) => void;
  expandedTurns: ReadonlySet<string>;
  onToggleTurn: (event: MouseEvent, key: string) => void;
  renderGroups: (groups: Accessor<MessageGroup[]>, position: (event: EventMessage) => string | null) => JSX.Element;
  renderActivities: () => JSX.Element;
}) {
  const timeline = useTaskTimeline();
  const turns = () => (timeline.manifest()?.metadata.turns ?? []).filter((turn) => turn.sectionID === props.section.id);
  const summary = () => {
    const section = props.section;
    const parts = [
      section.boundary?.message.system?.subtype === "context_cleared" ? "Cleared session" : "Compacted session",
      `${section.turnCount} ${section.turnCount === 1 ? "turn" : "turns"}`,
    ];
    if (section.textCount) parts.push(`${section.textCount} ${section.textCount === 1 ? "message" : "messages"}`);
    if (section.toolCount) parts.push(`${section.toolCount} ${section.toolCount === 1 ? "tool call" : "tool calls"}`);
    return parts.join(" · ");
  };
  const compaction = () => {
    const boundary = props.section.endBoundary?.message.system;
    return boundary?.subtype === "compact_boundary"
      ? compactTokenSummary(boundary.contextTokensBefore, boundary.contextTokensAfter)
      : "";
  };
  return (
    <section data-testid="history-section" data-section-id={props.section.id}>
      <div
        class={`${styles.sessionElided}${props.open ? ` ${styles.sessionElidedExpanded}` : ""}`}
        data-anchor-key={`session:${props.section.id}`}
      >
        <button
          type="button"
          class={styles.sessionToggle}
          onClick={(event) => props.onToggle(event)}
          aria-expanded={props.open}
        >
          <span class={styles.turnSummaryText}>{summary()}</span>
          <span class={styles.sessionDuration}>
            {props.section.durationMs > 0 ? formatTimingDuration(props.section.durationMs) : "0s"}
          </span>
        </button>
        <SessionInvocationIcon
          turns={turns()}
          model={props.section.reportedModel ?? null}
          compactionSummary={compaction()}
        />
      </div>
      <Show when={props.open}>
        <div class={styles.indentSession}>
          <SkillStrip taskId={props.taskId} skills={props.section.skills} label="Previous context skills" />
          <ArchivedSessionBody
            section={props.section}
            renderGroups={props.renderGroups}
            expandedTurns={props.expandedTurns}
            onToggleTurn={props.onToggleTurn}
          />
          {props.renderActivities()}
        </div>
      </Show>
    </section>
  );
}

function ArchivedSessionBody(props: {
  section: HistorySection;
  expandedTurns: ReadonlySet<string>;
  onToggleTurn: (event: MouseEvent, key: string) => void;
  renderGroups: (groups: Accessor<MessageGroup[]>, position: (event: EventMessage) => string | null) => JSX.Element;
}) {
  const timeline = useTaskTimeline();
  const body = createHistoryDisclosure(
    () =>
      `${timeline.manifest()?.bootstrap.generation}:${props.section.id}:${props.section.body ? historyPositionKey(props.section.body.first) + ":" + historyPositionKey(props.section.body.last) : "empty"}`,
    (signal) => timeline.readSection(props.section, signal),
  );
  const positions = createMemo(() => new Map(body.events().map((event) => [event.message, historyPositionKey(event)])));
  const routingRevision = createMemo(() => timeline.manifest()?.metadata.routingRevision);
  const turns = createMemo(() => {
    routingRevision();
    return groupTurns(
      aggregateToolGroups(
        groupMessages(
          body
            .events()
            .map((event) => event.message)
            .filter(
              (event) =>
                historyRoot(
                  event,
                  untrack(() => timeline.manifest()?.metadata.routing ?? []),
                ) && !isSessionBoundary(event),
            ),
        ),
      ),
    );
  });
  return (
    <>
      <Show when={body.loading()}>
        <span class={styles.historyLoading} role="status" aria-label="Loading session history">
          <LoadingIcon aria-hidden="true" />
        </span>
      </Show>
      <Show when={body.error()}>
        {(error) => (
          <p role="alert">
            {error()} <Button onClick={body.retry}>Retry</Button>
          </p>
        )}
      </Show>
      <For each={turns()}>
        {(turn, index) => {
          const key = () => `session:${props.section.id}:turn:${index()}:${turn.groups[0]?.events[0]?.ts ?? ""}`;
          const expanded = () => props.expandedTurns.has(key());
          const fact = () => {
            const event = turn.groups
              .flatMap((group) => group.events)
              .find((message) => message.result === turn.result);
            const position = event ? positions().get(event) : null;
            return position
              ? timeline.manifest()?.metadata.turns.find((item) => historyPositionKey(item) === position)
              : null;
          };
          return (
            <>
              <div
                class={`${styles.elidedTurn}${expanded() ? ` ${styles.elidedTurnExpanded}` : ""}`}
                data-anchor-key={`turn:${key()}`}
              >
                <button
                  type="button"
                  class={styles.turnToggle}
                  aria-expanded={expanded()}
                  onClick={(event) => props.onToggleTurn(event, key())}
                >
                  <span class={styles.turnSummaryText}>{turnSummary(turn)}</span>
                  <span class={styles.turnDuration}>
                    {turn.durationMs > 0 ? formatTimingDuration(turn.durationMs) : "0s"}
                  </span>
                </button>
                <Show when={fact()}>
                  {(value) => <TurnInvocationIcon turn={value()} model={props.section.reportedModel ?? null} />}
                </Show>
              </div>
              <Show when={expanded()}>
                <div class={styles.indentTurn}>
                  {props.renderGroups(
                    () => turn.groups,
                    (event) => positions().get(event) ?? null,
                  )}
                </div>
              </Show>
            </>
          );
        }}
      </For>
    </>
  );
}

function SetupHistory(props: { startedAt?: string }) {
  const timeline = useTaskTimeline();
  const metadata = () => timeline.manifest()?.metadata;
  const [open, setOpen] = createSignal(!metadata()?.initialInit);
  const started = createMemo(() => metadata()?.initialInit !== undefined);
  createEffect(() => {
    if (started()) setOpen(false);
  });
  const range = () => {
    const start = props.startedAt ? Date.parse(props.startedAt) : 0;
    const at = metadata()?.setupEndedAt;
    const end = at ? Date.parse(at) : 0;
    return start > 0 && end >= start && end - start <= maxCredibleSetupDurationMs ? { start, end } : null;
  };
  return (
    <details
      class={styles.taskSetup}
      open={open()}
      onToggle={(event) => setOpen(event.currentTarget.open)}
      data-testid="task-setup"
    >
      <summary class={styles.taskSetupTitle}>
        <span>Setup logs</span>
        <TimingIcon
          events={
            metadata()?.initialInit?.message
              ? [metadata()?.initialInit?.message].filter((event): event is EventMessage => event !== undefined)
              : []
          }
          range={range()}
          showZero
        />
      </summary>
      <Show when={open()}>
        <SetupHistoryBody />
      </Show>
    </details>
  );
}
function SetupHistoryBody() {
  const timeline = useTaskTimeline();
  const body = createHistoryDisclosure(
    () => {
      const metadata = timeline.manifest()?.metadata;
      return `${metadata?.setup ? historyPositionKey(metadata.setup.last) : ""}:${metadata?.initialInit ? historyPositionKey(metadata.initialInit) : ""}`;
    },
    (signal) => timeline.readSetup(signal),
  );
  return (
    <>
      <Show when={body.loading()}>
        <span class={styles.historyLoading} role="status" aria-label="Loading setup logs">
          <LoadingIcon aria-hidden="true" />
        </span>
      </Show>
      <Show when={body.error()}>
        {(error) => (
          <p role="alert">
            {error()} <Button onClick={body.retry}>Retry</Button>
          </p>
        )}
      </Show>
      <pre class={styles.taskSetupLogs} data-testid="task-setup-logs">
        {body
          .events()
          .flatMap((event) => (event.message.log ? [event.message.log.line] : []))
          .join("\n")}
      </pre>
    </>
  );
}

function GroupContent(props: {
  group: () => MessageGroup;
  taskId: string;
  isWaiting: () => boolean;
  lastAskGroup: () => MessageGroup | null;
  onAskAnswer: (text: string) => void;
  currentPlan: TaskPlan | null;
  onClearAndExecutePlan?: () => void;
  pendingAction?: () => string | null;
  model: string | null;
  turnTiming: (event: EventMessage) => HistoryTurn | undefined;
  nativeByToolUseID: () => ReadonlyMap<string, HistoryActivity[]>;
  nativeSettled: () => boolean;
  backgroundByToolUseID: () => ReadonlyMap<string, HistoryActivity[]>;
  backgroundSettled: () => boolean;
}) {
  // eslint-disable-next-line solid/reactivity -- props.group is a function reference, not a reactive read
  const group = props.group;
  return (
    <Switch>
      <Match when={group().kind === "ask" && group().ask} keyed>
        {(ask) => (
          <AskQuestionCard
            ask={ask}
            interactive={props.isWaiting() && group() === props.lastAskGroup()}
            answerText={group().answerText}
            onSubmit={props.onAskAnswer}
          />
        )}
      </Match>
      <Match when={group().kind === "userInput" && group().events[0]?.userInput} keyed>
        {(ui) => (
          <div class={styles.userInputMsg}>
            <Markdown text={ui.text} />
            <Show when={ui.images?.length}>
              <div class={styles.userInputImages}>
                <For each={ui.images}>
                  {(img) => (
                    <Show
                      when={img.hash}
                      fallback={
                        <img
                          class={styles.userInputImage}
                          src={`data:${img.mediaType};base64,${img.data}`}
                          alt="attached"
                        />
                      }
                    >
                      {(hash) => (
                        <A
                          class={styles.userInputImageLink}
                          href={taskAttachmentViewURL(props.taskId, hash())}
                          aria-label="Open attached image"
                        >
                          <img
                            class={styles.userInputImage}
                            src={taskAttachmentURL(props.taskId, hash(), attachmentThumbnailWidth)}
                            alt="attached"
                            onError={(event) => {
                              // The event still carries the payload, so a digest the
                              // log cannot resolve falls back to the inline image,
                              // once: a failed inline decode must not retry itself.
                              const image = event.currentTarget;
                              if (image.src.startsWith("data:")) return;
                              image.src = `data:${img.mediaType};base64,${img.data}`;
                            }}
                          />
                        </A>
                      )}
                    </Show>
                  )}
                </For>
              </div>
            </Show>
          </div>
        )}
      </Match>
      <Match when={group().kind === "action"}>
        <Show
          when={group().toolCalls.length > 0}
          fallback={
            <Show when={group().tool} fallback={<ThinkingCard events={group().events} text={group().content?.text} />}>
              {(tool) => (
                <details data-testid="orphan-tool">
                  <summary>
                    Tool {tool().result ? "result" : "output"}:{" "}
                    {tool().result?.toolUseID ?? group().events[0]?.toolOutputDelta?.toolUseID}
                  </summary>
                  <pre>{tool().result?.error || tool().output || "Result received; no output reported."}</pre>
                </details>
              )}
            </Show>
          }
        >
          <ToolMessageGroup
            toolCalls={group().toolCalls}
            taskId={props.taskId}
            events={group().events}
            currentPlan={props.currentPlan}
            onClearAndExecutePlan={props.isWaiting() ? props.onClearAndExecutePlan : undefined}
            pendingAction={props.pendingAction}
            nativeByToolUseID={props.nativeByToolUseID}
            nativeSettled={props.nativeSettled}
            backgroundByToolUseID={props.backgroundByToolUseID}
            backgroundSettled={props.backgroundSettled}
          />
        </Show>
      </Match>
      <Match when={group().kind === "text"}>
        <TextMessageGroup events={group().events} text={group().content?.text} />
      </Match>
      <Match when={group().kind === "widget"}>
        <WidgetCard group={group()} />
      </Match>
      <Match when={group().kind === "other"}>
        <For each={group().events}>
          {(ev) => (
            <MessageItem
              ev={ev}
              taskId={props.taskId}
              model={props.model}
              turnTiming={props.turnTiming(ev)}
              hideResultText={group().resultDuplicatesText}
            />
          )}
        </For>
      </Match>
    </Switch>
  );
}

function hasGroupTiming(group: MessageGroup): boolean {
  if (group.tool && !group.tool.use) return true;
  if (group.content) return group.content.text.trim() !== "";
  if (group.kind === "action") {
    return (
      group.toolCalls.length > 0 ||
      group.events.some(
        (event) =>
          (event.kind === "thinking" && (event.thinking?.text ?? "").trim() !== "") ||
          (event.kind === "thinkingDelta" && (event.thinkingDelta?.text ?? "").trim() !== ""),
      )
    );
  }
  if (group.kind === "text") {
    return group.events.some(
      (event) =>
        (event.kind === "text" && (event.text?.text ?? "").trim() !== "") ||
        (event.kind === "textDelta" && (event.textDelta?.text ?? "").trim() !== "") ||
        (event.kind === "thinking" && (event.thinking?.text ?? "").trim() !== "") ||
        (event.kind === "thinkingDelta" && (event.thinkingDelta?.text ?? "").trim() !== ""),
    );
  }
  return (
    group.kind !== "other" ||
    group.events.some((event) => event.kind !== "usage" && event.system?.subtype !== "step_start")
  );
}

// compactTokenSummary describes the harness-reported context transition on a
// compaction boundary, e.g. "128k → 20k tokens". Empty when unreported.
function compactTokenSummary(before: number | undefined, after: number | undefined): string {
  const from = before ?? 0;
  const to = after ?? 0;
  if (from > 0 && to > 0) return `${formatTokens(from)} → ${formatTokens(to)} tokens`;
  if (to > 0) return `${formatTokens(to)} tokens`;
  if (from > 0) return `from ${formatTokens(from)} tokens`;
  return "";
}

function SkillStrip(props: { taskId: string; skills: EventSkillLoad[]; label: string }) {
  const byKey = createMemo(
    () => new Map(props.skills.map((skill) => [JSON.stringify([skill.path ?? null, skill.skill]), skill])),
  );
  const keys = createMemo(() => [...byKey().keys()]);
  return (
    <Show when={keys().length > 0}>
      <nav class={styles.skills} aria-label={props.label}>
        <SkillIcon class={styles.skillsIcon} width="14" height="14" aria-hidden="true" />
        <For each={keys()}>
          {(key) => {
            const skill = () => {
              const value = byKey().get(key);
              if (!value) throw new Error("Missing skill");
              return value;
            };
            return (
              <Show when={skill().path} keyed fallback={<span>{skill().skill}</span>}>
                {(path) => (
                  <A class={styles.skillLink} href={taskViewURL(props.taskId, path)} title={path}>
                    {skill().skill}
                  </A>
                )}
              </Show>
            );
          }}
        </For>
      </nav>
    </Show>
  );
}

// Renders the header for a session boundary (init, compaction, or clear).
// These events are extracted from the message stream and shown as section separators.
function SessionBoundaryItem(props: { event: EventMessage; grouped: boolean }) {
  const ev = () => props.event;
  const compactSummary = () => compactTokenSummary(ev().system?.contextTokensBefore, ev().system?.contextTokensAfter);
  return (
    <>
      <Switch>
        <Match when={ev().init} keyed>
          {(init) => (
            <div class={styles.systemInit}>
              {[
                "Session started",
                init.reportedModel,
                init.reportedEffort ? `${init.reportedEffort} effort` : "",
                init.agentVersion,
                init.sessionID,
              ]
                .filter(Boolean)
                .join(" · ")}
            </div>
          )}
        </Match>
        <Match when={ev().system?.subtype === "compact_boundary" && !props.grouped}>
          <div class={styles.contextCleared}>
            Conversation compacted
            <Show when={compactSummary()}>{(summary) => ` · ${summary()}`}</Show>
          </div>
        </Match>
        <Match when={ev().system?.subtype === "context_cleared"}>
          <div class={styles.contextCleared}>Context cleared</div>
        </Match>
      </Switch>
    </>
  );
}

function rateLimitLabel(rateLimitType: string | undefined): string {
  switch (rateLimitType) {
    case "five_hour":
      return "5-hour quota";
    case "seven_day":
      return "7-day quota";
    case "":
    case undefined:
      return "quota";
    default:
      return `${rateLimitType.replaceAll("_", "-")} quota`;
  }
}

function RateLimitBanner(props: { ev: EventMessage }) {
  const rl = () => props.ev.rateLimit;
  const resetsLabel = () => {
    const r = rl()?.resetsAt;
    if (!r) return "";
    const resetMS = Date.parse(r);
    if (!Number.isFinite(resetMS) || resetMS <= 0) return "";
    const dayMS = 24 * 60 * 60 * 1000;
    const d = new Date(resetMS);
    const now = new Date();
    const localDay = (date: Date) => Date.UTC(date.getFullYear(), date.getMonth(), date.getDate()) / dayMS;
    const days = localDay(d) - localDay(now);
    if (days === 1) {
      return ` · resets tomorrow at ${d.toLocaleTimeString()}`;
    }
    if (days > 1) {
      return ` · resets in ${days} days at ${d.toLocaleTimeString()}`;
    }
    return ` · resets at ${d.toLocaleTimeString()}`;
  };
  const rejectedLabel = () => {
    const r = rl();
    const label = rateLimitLabel(r?.rateLimitType);
    if (r?.isUsingOverage) return `${label} reached; using extra usage`;
    return `Rate limited (${label})`;
  };
  // Utilization is negative when the harness reported only a status change, so
  // the banner must not turn it into a percentage.
  const warningLabel = () => {
    const r = rl();
    const label = rateLimitLabel(r?.rateLimitType);
    const utilization = r?.utilization;
    if (utilization === undefined || utilization < 0) {
      return `⚠️ ${label} nearing its limit`;
    }
    return `⚠️ ${rateLimitPercentage(utilization)}% of ${label} used`;
  };
  return (
    <Switch>
      <Match when={rl()?.status === "rejected"}>
        <div class={rl()?.isUsingOverage ? styles.rateLimitWarning : styles.rateLimitRejected}>
          {rejectedLabel()}
          {resetsLabel()}
        </div>
      </Match>
      <Match when={rl()?.status === "allowed_warning"}>
        <div class={styles.rateLimitWarning}>
          {warningLabel()}
          {resetsLabel()}
        </div>
      </Match>
    </Switch>
  );
}

function MessageItem(props: {
  taskId: string;
  ev: EventMessage;
  model: string | null;
  turnTiming?: HistoryTurn;
  hideResultText?: boolean;
}) {
  return (
    <Switch>
      <Match when={props.ev.rateLimit}>
        <RateLimitBanner ev={props.ev} />
      </Match>
      <Match when={props.ev.system?.subtype === "api_error"}>
        <div class={styles.parseError}>API error</div>
      </Match>
      <Match when={props.ev.system?.subtype === "error"}>
        <div class={styles.parseError}>{props.ev.system?.detail || "Agent error"}</div>
      </Match>
      <Match when={props.ev.toolResult || props.ev.toolOutputDelta}>
        <details data-testid="orphan-tool">
          <summary>
            Tool {props.ev.toolResult ? "result" : "output"}:{" "}
            {props.ev.toolResult?.toolUseID ?? props.ev.toolOutputDelta?.toolUseID}
          </summary>
          <pre>
            {props.ev.toolResult?.error || props.ev.toolOutputDelta?.delta || "Result received; no output reported."}
          </pre>
        </details>
      </Match>
      <Match when={props.ev.todo}>
        <For each={props.ev.todo?.todos}>
          {(todo) => (
            <div>
              {todo.status}: {todo.content}
            </div>
          )}
        </For>
      </Match>
      <Match when={props.ev.skillLoad}>
        <SkillStrip
          taskId={props.taskId}
          skills={props.ev.skillLoad ? [props.ev.skillLoad] : []}
          label="Child loaded skill"
        />
      </Match>
      <Match when={props.ev.system?.subtype === "compact_start"}>
        <div class={styles.systemMsg} data-testid="compact-start">
          Compacting context…{props.ev.system?.detail ? ` (${props.ev.system.detail})` : ""}
        </div>
      </Match>
      <Match when={props.ev.system?.subtype === "compact_error"}>
        <div class={styles.parseError} data-testid="compact-error">
          Context compaction failed
          {props.ev.system?.detail ? `: ${props.ev.system.detail}` : ""}
        </div>
      </Match>
      <Match when={props.ev.system?.subtype === "step_start"}>
        <></>
      </Match>
      <Match when={props.ev.system?.subtype === "model_rerouted"}>
        <div class={styles.systemMsg}>
          Model rerouted
          {props.ev.system?.detail ? `: ${props.ev.system.detail}` : ""}
        </div>
      </Match>
      <Match when={props.ev.system} keyed>
        {(sys) => <div class={styles.systemMsg}>[{sys.subtype}]</div>}
      </Match>
      <Match when={props.ev.text} keyed>
        {(text) => (
          <div class={styles.assistantMsg}>
            <Markdown text={text.text} />
          </div>
        )}
      </Match>
      <Match when={props.ev.result} keyed>
        {(result) => (
          <ResultCard
            result={result}
            model={props.model}
            turnTiming={props.turnTiming}
            hideText={props.hideResultText}
          />
        )}
      </Match>
      <Match when={props.ev.error} keyed>
        {(err) => <div class={styles.parseError}>Parse error: {err.err}</div>}
      </Match>
      <Match when={props.ev.log} keyed>
        {(log) => <div class={styles.logLine}>{log.line}</div>}
      </Match>
    </Switch>
  );
}

function ResultCard(props: {
  result: EventResult;
  model: string | null;
  turnTiming?: HistoryTurn;
  hideText?: boolean;
}) {
  const result = () => props.result;
  const meta = createMemo(() => {
    const current = result();
    const parts: string[] = [];
    if (current.cost && current.cost.usd > 0)
      parts.push(`${current.cost.source === "estimated" ? "~" : ""}$${current.cost.usd.toFixed(4)}`);
    if (current.numTurns > 0) parts.push(`${current.numTurns} ${current.numTurns === 1 ? "turn" : "turns"}`);
    return parts.join(" · ");
  });
  return (
    <div class={`${styles.result} ${result().isError ? styles.resultError : styles.resultSuccess}`}>
      <Show when={props.turnTiming}>
        {(turn) => (
          <div class={styles.resultHeader}>
            <strong>{result().isError ? "Error" : "Done"}</strong>
            <div class={styles.resultTiming}>
              <span class={styles.resultDuration} data-testid="turn-duration">
                {turn().durationMs > 0 ? formatTimingDuration(turn().durationMs) : "0s"}
              </span>
              <TurnInvocationIcon turn={turn()} model={props.model} />
            </div>
          </div>
        )}
      </Show>
      <Show when={!props.turnTiming}>
        <strong>{result().isError ? "Error" : "Done"}</strong>
      </Show>
      <Show when={!props.hideText && result().result}>
        <div class={styles.resultText}>
          <Markdown text={result().result} />
        </div>
      </Show>
      <Show when={meta()} keyed>
        {(text) => <div class={styles.resultMeta}>{text}</div>}
      </Show>
    </div>
  );
}

function ToolMessageGroup(props: {
  toolCalls: ToolCall[];
  taskId: string;
  events?: EventMessage[];
  currentPlan: TaskPlan | null;
  onClearAndExecutePlan?: () => void;
  pendingAction?: () => string | null;
  nativeByToolUseID: () => ReadonlyMap<string, HistoryActivity[]>;
  nativeSettled: () => boolean;
  backgroundByToolUseID: () => ReadonlyMap<string, HistoryActivity[]>;
  backgroundSettled: () => boolean;
}) {
  const calls = () => props.toolCalls;
  const currentPlan = () => {
    const plan = props.currentPlan;
    return plan &&
      calls().some(
        (call) => call.use.name === "ExitPlanMode" && call.use.toolUseID === plan.toolUseID && !isChildEvent(call),
      )
      ? plan
      : null;
  };
  const callKey = (call: ToolCall) => `${props.taskId}:tool:${toolIdentity(call.source, call.use.toolUseID)}`;
  const groupKey = () => {
    const first = calls()[0];
    return `${props.taskId}:group:${first?.source ? toolIdentity(first.source, "") : first?.use.toolUseID}`;
  };
  const isOpen = () => detailsOpenState.get(groupKey()) ?? false;
  const thinkingEvents = () => (props.events ?? []).filter((e) => e.kind === "thinking" || e.kind === "thinkingDelta");
  const durations = createMemo(() => toolCallDurations(props.events ?? []));
  // Compute accumulated tool output deltas per toolUseID from the group events.
  const outputDeltaEvents = (toolUseID: string) =>
    (props.events ?? []).filter((e) => e.kind === "toolOutputDelta" && e.toolOutputDelta?.toolUseID === toolUseID);
  return (
    <Show when={calls().length > 0}>
      <Show
        when={calls().length > 1 || calls()[0]?.source !== undefined}
        fallback={
          <Show when={calls()[0]}>
            {(call) => (
              <ToolCallCard
                call={call()}
                taskId={props.taskId}
                durationMs={call().durationMs ?? toolCallDurationMs(call(), durations())}
                thinkingEvents={thinkingEvents()}
                outputDeltaEvents={outputDeltaEvents(call().use.toolUseID)}
                open={detailsOpenState.get(callKey(call())) ?? false}
                onToggle={(v) => detailsOpenState.set(callKey(call()), v)}
                nativeActivity={props.nativeByToolUseID().get(toolIdentity(call().source, call().use.toolUseID))}
                nativeSettled={props.nativeSettled}
                backgroundCommands={props
                  .backgroundByToolUseID()
                  .get(toolIdentity(call().source, call().use.toolUseID))}
                backgroundSettled={props.backgroundSettled}
              />
            )}
          </Show>
        }
      >
        <>
          <details
            class={styles.toolGroup}
            open={isOpen()}
            onToggle={(e) => detailsOpenState.set(groupKey(), e.currentTarget.open)}
          >
            <summary>
              {calls().filter((c) => c.done).length}/{calls().length} tools: {toolCountSummary(calls())}
            </summary>
            <div class={styles.toolGroupInner}>
              <Show when={thinkingEvents().length > 0}>
                <ThinkingCard events={thinkingEvents()} />
              </Show>
              <Index each={calls()}>
                {(call) => (
                  <ToolCallCard
                    call={call()}
                    taskId={props.taskId}
                    durationMs={call().durationMs ?? toolCallDurationMs(call(), durations())}
                    outputDeltaEvents={outputDeltaEvents(call().use.toolUseID)}
                    open={detailsOpenState.get(callKey(call())) ?? false}
                    onToggle={(v) => detailsOpenState.set(callKey(call()), v)}
                    nativeActivity={props.nativeByToolUseID().get(toolIdentity(call().source, call().use.toolUseID))}
                    nativeSettled={props.nativeSettled}
                    backgroundCommands={props
                      .backgroundByToolUseID()
                      .get(toolIdentity(call().source, call().use.toolUseID))}
                    backgroundSettled={props.backgroundSettled}
                  />
                )}
              </Index>
            </div>
          </details>
        </>
      </Show>
      <Show when={currentPlan()}>
        {(plan) => (
          <div class={styles.planAction}>
            <div class={styles.planContent} data-testid="plan-content">
              <Markdown text={plan().content} />
            </div>
            <Show when={props.onClearAndExecutePlan}>
              <Button
                variant="gray"
                loading={props.pendingAction?.() === "restart"}
                disabled={!!props.pendingAction?.()}
                onClick={props.onClearAndExecutePlan}
                data-testid="clear-and-execute-plan"
              >
                Clear and execute plan
              </Button>
            </Show>
          </div>
        )}
      </Show>
    </Show>
  );
}

// Renders a thinking group, collapsed by default like a ToolCallCard.
function ThinkingCard(props: { events: EventMessage[]; text?: string }) {
  const text = createMemo(() => {
    if (props.text !== undefined) return props.text;
    // Process events sequentially so each thinking block (final or still-streaming
    // deltas) is collected independently. Multiple thinking blocks can land in the
    // same group when consecutive tool-call turns are merged by groupMessages.
    const parts: string[] = [];
    let deltaBuffer: string[] = [];
    for (const ev of props.events) {
      if (ev.kind === "thinkingDelta" && ev.thinkingDelta) {
        deltaBuffer.push(ev.thinkingDelta.text);
      } else if (ev.kind === "thinking" && ev.thinking) {
        deltaBuffer = [];
        parts.push(ev.thinking.text);
      }
    }
    if (deltaBuffer.length > 0) parts.push(deltaBuffer.join(""));
    return parts.join("\n\n");
  });
  const excerpt = createMemo(() => {
    const t = text();
    return t ? (t.split("\n", 1)[0] ?? "").trim() : "";
  });
  const key = () => "thinking:" + (props.events[0] ? (messageIdentity(props.events[0]) ?? props.events[0].ts) : 0);
  const isOpen = () => detailsOpenState.get(key()) ?? false;
  return (
    <Show when={text()}>
      <details
        class={styles.thinkingBlock}
        open={isOpen()}
        onToggle={(e) => detailsOpenState.set(key(), e.currentTarget.open)}
      >
        <summary>
          Thinking
          <Show when={excerpt()}>
            <span class={styles.thinkingExcerpt}>{excerpt()}</span>
          </Show>
        </summary>
        <pre class={styles.thinkingText}>{text()}</pre>
      </details>
    </Show>
  );
}

// Returns true when text is a raw HTML fragment (e.g. a weaker model dumped
// widget HTML as text instead of calling show_widget).
function looksLikeHTML(text: string): boolean {
  const trimmed = text.trimStart();
  return trimmed.startsWith("<style") || trimmed.startsWith("<div") || trimmed.startsWith("<!--");
}

// Renders raw HTML inside a shadow DOM so its styles cannot leak out and
// parent styles cannot bleed in.
function ShadowHTML(props: { html: string }) {
  const [el, setEl] = createSignal<HTMLDivElement>();
  createEffect(() => {
    const div = el();
    if (!div) return;
    const shadow = div.shadowRoot ?? div.attachShadow({ mode: "open" });
    shadow.innerHTML = props.html;
  });
  return <div ref={setEl} />;
}

// Renders a text group, combining textDelta fragments into a single view.
function TextMessageGroup(props: { events: EventMessage[]; text?: string }) {
  const thinkingEvents = createMemo(() =>
    props.events.filter((e) => e.kind === "thinking" || e.kind === "thinkingDelta"),
  );
  const text = createMemo(() => {
    if (props.text !== undefined) return props.text;
    const finalEv = props.events.findLast((e) => e.kind === "text");
    if (finalEv?.text) return finalEv.text.text;
    return props.events
      .filter((e): e is EventMessage & { textDelta: EventTextDelta } => e.kind === "textDelta" && !!e.textDelta)
      .map((e) => e.textDelta.text)
      .join("");
  });
  return (
    <>
      <Show when={thinkingEvents().length > 0}>
        <ThinkingCard events={thinkingEvents()} />
      </Show>
      <Show when={text()}>
        <Show
          when={looksLikeHTML(text())}
          fallback={
            <div class={styles.assistantMsg}>
              <Markdown text={text()} />
            </div>
          }
        >
          <ShadowHTML html={text()} />
        </Show>
      </Show>
    </>
  );
}

// Returns true if every value in the object is a scalar (string, number, boolean, null).
function isFlat(obj: Record<string, unknown>): boolean {
  return Object.values(obj).every(
    (v) => v === null || typeof v === "string" || typeof v === "number" || typeof v === "boolean",
  );
}

// Formats a scalar value for display: strings as-is, others via JSON.
function fmtValue(v: unknown): string {
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

function GenericToolCallInput(props: { input: Record<string, unknown> }) {
  const flat = () => isFlat(props.input);
  return (
    <Show when={flat()} fallback={<pre class={styles.toolBlockPre}>{JSON.stringify(props.input, null, 2)}</pre>}>
      <div class={styles.toolInputList}>
        <For each={Object.entries(props.input)}>
          {([k, v]) => {
            const multiline = typeof v === "string" && v.includes("\n");
            return (
              <div class={styles.toolInputRow}>
                <span class={styles.toolInputKey}>{k}:</span>
                {multiline ? <pre class={styles.toolInputBlock}>{v as string}</pre> : <> {fmtValue(v)}</>}
              </div>
            );
          }}
        </For>
      </div>
    </Show>
  );
}

function FileChangesInputView(props: { files: NonNullable<EventToolInputView["files"]> }) {
  return (
    <div class={styles.fileChangesInput}>
      <For each={props.files}>
        {(file) => (
          <div class={styles.toolPatchFile}>
            <div class={styles.toolPatchPath}>{file.path}</div>
            <UnifiedDiffBlock diff={file.patch} compact />
          </div>
        )}
      </For>
    </div>
  );
}

function ToolInputView(props: { view: EventToolInputView }) {
  return (
    <Switch>
      <Match when={props.view.kind === "fileChanges" && props.view.files} keyed>
        {(files) => <FileChangesInputView files={files} />}
      </Match>
      <Match when={props.view.kind === "subagents" && props.view.subagents} keyed>
        {(spawns) => <SubagentSpawns spawns={spawns} />}
      </Match>
    </Switch>
  );
}

function ToolCallInput(props: { input: Record<string, unknown>; inputView?: EventToolInputView }) {
  return (
    <Show when={props.inputView} keyed fallback={<GenericToolCallInput input={props.input} />}>
      {(view) => <ToolInputView view={view} />}
    </Show>
  );
}

// Renders subagents: one row per agent with its type, optional phase/label,
// and task prompt.
function SubagentSpawns(props: { spawns: EventSubagentSpawn[] }) {
  return (
    <div class={styles.subagentList}>
      <For each={props.spawns}>
        {(s) => (
          <div class={styles.subagentRow}>
            <div class={styles.subagentHead}>
              <span class={styles.subagentAgent}>{s.agent}</span>
              <Show when={s.phase || s.label}>
                <span class={styles.subagentPhase}>{[s.phase, s.label].filter(Boolean).join(" · ")}</span>
              </Show>
            </div>
            <Show when={s.task}>
              <pre class={styles.subagentTask}>{s.task}</pre>
            </Show>
          </div>
        )}
      </For>
    </div>
  );
}

function ToolCallCard(props: {
  call: ToolCall;
  taskId: string;
  durationMs: number | null;
  open: boolean;
  onToggle: (open: boolean) => void;
  thinkingEvents?: EventMessage[];
  outputDeltaEvents?: EventMessage[];
  nativeActivity?: HistoryActivity[];
  nativeSettled?: () => boolean;
  backgroundCommands?: HistoryActivity[];
  backgroundSettled?: () => boolean;
}) {
  const [loadedInput, setLoadedInput] = createSignal<Record<string, unknown> | null>(null);
  const [loading, setLoading] = createSignal(false);
  const [inputError, setInputError] = createSignal("");
  let inputRequest = 0;
  const inputOwner = createMemo(() =>
    JSON.stringify([props.taskId, toolIdentity(props.call.source, props.call.use.toolUseID)]),
  );
  createEffect(() => {
    inputOwner();
    inputRequest++;
    setLoadedInput(null);
    setLoading(false);
    setInputError("");
  });
  onCleanup(() => inputRequest++);

  const timedCommand = () => props.backgroundCommands?.[0];
  const durationMs = createLiveDuration(
    () => {
      const command = timedCommand();
      const start = command?.startedAt ? Date.parse(command.startedAt) : props.call.startedAt;
      return start !== undefined && start > 0 ? start : null;
    },
    () => {
      const command = timedCommand();
      if (!command) return props.durationMs;
      return command.startedAt && command.endedAt
        ? Math.max(0, Date.parse(command.endedAt) - Date.parse(command.startedAt))
        : null;
    },
    () => {
      const command = timedCommand();
      return !(props.backgroundSettled?.() ?? false) && (command ? command.status === "running" : !props.call.done);
    },
  );
  const error = () => props.call.result?.error ?? "";
  const effectiveInput = (): Record<string, unknown> =>
    (loadedInput() ?? props.call.use.input ?? {}) as Record<string, unknown>;
  const detail = () => props.call.use.detail || toolCallDetail(props.call.use.name, effectiveInput());
  const showLoadBtn = () => props.call.use.inputTruncated && !loadedInput();

  async function loadInput() {
    const request = ++inputRequest;
    setLoading(true);
    setInputError("");
    try {
      const source = props.call.source;
      const resp = await api.getTaskToolInput(
        props.taskId,
        props.call.use.toolUseID,
        source?.agentID ?? "",
        source?.delegationToolUseID ?? "",
        source?.threadID ?? "",
        source?.turnID ?? "",
      );
      if (request === inputRequest) setLoadedInput(resp.input as Record<string, unknown>);
    } catch (err) {
      if (request === inputRequest) setInputError(err instanceof Error ? err.message : String(err));
    } finally {
      if (request === inputRequest) setLoading(false);
    }
  }

  return (
    <>
      <details
        class={styles.toolBlock}
        data-testid="tool-call"
        data-tool-key={toolIdentity(props.call.source, props.call.use.toolUseID)}
        open={props.open}
        onToggle={(e) => props.onToggle(e.currentTarget.open)}
      >
        <summary>
          <span class={styles.toolSummaryContent}>
            <span class={styles.toolSummaryMain}>
              <Show
                when={!props.call.done}
                fallback={
                  // A folded background command outranks the static badge: the
                  // chip resolves to the command's outcome instead of leaving a
                  // perpetual "running in background" mark on a finished call.
                  <Show
                    when={props.backgroundCommands?.[0]}
                    keyed
                    fallback={
                      <Show when={props.call.use.background} fallback={<span class={styles.toolDone}>&#10003;</span>}>
                        <span class={styles.toolBackground} title="Running in background">
                          &#8943;
                        </span>
                      </Show>
                    }
                  >
                    {(command) => (
                      <BackgroundCommandStatusChip command={command} settled={props.backgroundSettled?.() ?? false} />
                    )}
                  </Show>
                }
              >
                <span class={styles.toolPending} />
              </Show>
              {props.call.use.name}
              <For each={props.nativeActivity ?? []}>
                {(activity) => <NativeActivityStatus activity={activity} settled={props.nativeSettled?.() ?? false} />}
              </For>
              <Show when={detail()}>
                <span class={styles.toolDetail}>{detail()}</span>
              </Show>
              <Show when={error()}>
                <span class={styles.toolError}> error</span>
              </Show>
            </span>
            <Show when={durationMs() !== null}>
              <span class={styles.toolDuration} data-testid="tool-duration">
                {formatTimingDuration(durationMs() ?? 0)}
              </span>
            </Show>
          </span>
        </summary>
        <Show when={(props.thinkingEvents?.length ?? 0) > 0}>
          <ThinkingCard events={props.thinkingEvents ?? []} />
        </Show>
        <Show
          when={showLoadBtn()}
          fallback={<ToolCallInput input={effectiveInput()} inputView={props.call.use.inputView} />}
        >
          <button class={styles.loadInputBtn} onClick={loadInput} disabled={loading()}>
            {loading() ? "Loading…" : "Load input"}
          </button>
        </Show>
        <Show when={error()}>
          <pre class={styles.toolErrorPre}>{error()}</pre>
        </Show>
        <Show when={inputError()}>
          <pre class={styles.toolErrorPre} role="alert">
            {inputError()}
          </pre>
        </Show>
        <Show when={props.call.output || (props.outputDeltaEvents?.length ?? 0) > 0}>
          <pre class={styles.toolOutputDelta}>
            {props.call.output ?? props.outputDeltaEvents?.map((e) => e.toolOutputDelta?.delta ?? "").join("")}
          </pre>
        </Show>
      </details>
    </>
  );
}

// Answers submitted locally but not yet confirmed via SSE userInput event.
// Keyed by toolUseID so the state survives component remounts caused by
// keyed Match re-creation when group object identities change.
const pendingAskAnswers = new Map<string, string>();

function AskQuestionCard(props: {
  ask: EventAsk;
  interactive: boolean;
  answerText?: string;
  onSubmit: (text: string) => void;
}) {
  const questions = () => props.ask.questions;
  const [selections, setSelections] = createSignal<Map<number, Set<string>>>(new Map());
  const [otherTexts, setOtherTexts] = createSignal<Map<number, string>>(new Map());
  // eslint-disable-next-line solid/reactivity -- toolUseID is immutable per ask instance
  const toolUseID = props.ask.toolUseID;
  const [submitted, setSubmitted] = createSignal(pendingAskAnswers.has(toolUseID));
  const answered = () => props.answerText !== undefined || submitted();

  // Clean up pending entry once the server confirms the answer via SSE.
  createEffect(() => {
    if (props.answerText !== undefined) pendingAskAnswers.delete(toolUseID);
  });

  function toggleOption(qIdx: number, label: string, multiSelect: boolean) {
    setSelections((prev) => {
      const next = new Map(prev);
      const set = new Set(next.get(qIdx) ?? []);
      if (label === "__other__") {
        if (set.has(label)) {
          set.delete(label);
        } else {
          if (!multiSelect) set.clear();
          set.add(label);
        }
      } else if (set.has(label)) {
        set.delete(label);
      } else {
        if (!multiSelect) set.clear();
        set.add(label);
      }
      next.set(qIdx, set);
      return next;
    });
  }

  function setOtherText(qIdx: number, text: string) {
    setOtherTexts((prev) => {
      const next = new Map(prev);
      next.set(qIdx, text);
      return next;
    });
  }

  function formatAnswer(): string {
    const qs = questions();
    const parts: string[] = [];
    for (let i = 0; i < qs.length; i++) {
      const q = qs[i];
      const sel = selections().get(i) ?? new Set();
      const labels: string[] = [];
      for (const s of sel) {
        if (s === "__other__") {
          labels.push(otherTexts().get(i) ?? "");
        } else {
          labels.push(s);
        }
      }
      const answer = labels.filter((l) => l.length > 0).join(", ");
      if (qs.length === 1) {
        parts.push(answer);
      } else {
        parts.push(`${q?.header ?? `Q${i + 1}`}: ${answer}`);
      }
    }
    return parts.join("\n");
  }

  function handleSubmit() {
    const answer = formatAnswer();
    if (!answer.trim()) return;
    pendingAskAnswers.set(toolUseID, answer);
    setSubmitted(true);
    props.onSubmit(answer);
  }

  const canInteract = (): boolean => props.interactive && !answered();

  return (
    <div class={canInteract() ? `${styles.askGroup} ${styles.askGroupActive}` : styles.askGroup}>
      <For each={questions()}>
        {(q: AskQuestion, qIdx: Accessor<number>) => (
          <div class={styles.askQuestion}>
            <Show when={q.header}>
              <div class={styles.askHeader}>{q.header}</div>
            </Show>
            <div class={styles.askText}>{q.question}</div>
            <div class={styles.askOptions}>
              <For each={q.options}>
                {(opt) => {
                  const selected = (): boolean => selections().get(qIdx())?.has(opt.label) ?? false;
                  return (
                    <button
                      class={selected() ? `${styles.askChip} ${styles.askChipSelected}` : styles.askChip}
                      disabled={!canInteract()}
                      onClick={() => toggleOption(qIdx(), opt.label, q.multiSelect ?? false)}
                      data-testid={`ask-option-${opt.label}`}
                    >
                      <span class={styles.askChipLabel}>{opt.label}</span>
                      <Show when={opt.description}>
                        <span class={styles.askChipDesc}>{opt.description}</span>
                      </Show>
                    </button>
                  );
                }}
              </For>
              {/* "Other" option */}
              <button
                class={
                  selections().get(qIdx())?.has("__other__")
                    ? `${styles.askChip} ${styles.askChipSelected}`
                    : styles.askChip
                }
                disabled={!canInteract()}
                onClick={() => toggleOption(qIdx(), "__other__", q.multiSelect ?? false)}
              >
                <span class={styles.askChipLabel}>Other</span>
              </button>
            </div>
            <Show when={selections().get(qIdx())?.has("__other__")}>
              <AutoResizeTextarea
                class={styles.askOtherInput}
                placeholder="Type your answer..."
                value={otherTexts().get(qIdx()) ?? ""}
                onInput={(v) => setOtherText(qIdx(), v)}
                disabled={!canInteract()}
              />
            </Show>
          </div>
        )}
      </For>
      <Show when={canInteract()}>
        <button class={styles.askSubmit} onClick={() => handleSubmit()} data-testid="ask-submit">
          Submit
        </button>
      </Show>
      <Show when={answered()}>
        <div class={styles.askSubmitted} data-testid="ask-submitted-answer">
          {props.answerText ?? pendingAskAnswers.get(toolUseID) ?? formatAnswer()}
        </div>
      </Show>
    </div>
  );
}
