// TaskDetail renders the real-time agent output stream for a single task.
import { createSignal, createMemo, createEffect, For, Index, Show, onCleanup, onMount, untrack, Switch, Match, type Accessor } from "solid-js";
import { A, useNavigate, useLocation } from "@solidjs/router";
import { sendInput as apiSendInput, restartTask as apiRestartTask, syncTask as apiSyncTask, taskEvents, getTaskToolInput, getTaskCILog, createTask, botFixPR } from "./api";
import type { EventMessage, EventResult, AskQuestion, EventAsk, EventTextDelta, SafetyIssue, ImageData as APIImageData, SyncTarget, DiffFileStat, ForgeCheck } from "@sdk/types.gen";
import { groupMessages, groupSessions, isSessionBoundary, buildPastSessionItems, buildTurnItems, toolCountSummary, turnSummary, sessionSummary, type MsgItem, type MessageGroup, type Session } from "./grouping";
import { formatDuration, formatElapsed, formatTokens, toolCallDetail } from "./formatting";
import type { ToolCall } from "./grouping";
import { SyncTargetDefault } from "@sdk/types.gen";
import { Marked } from "marked";
import AutoResizeTextarea from "./AutoResizeTextarea";
import PromptInput from "./PromptInput";
import Button from "./Button";
import { requestNotificationPermission } from "./notifications";
import ProgressPanel from "./ProgressPanel";
import CloseIcon from "@material-symbols/svg-400/outlined/close.svg?solid";
import SendIcon from "@material-symbols/svg-400/outlined/send.svg?solid";
import SyncIcon from "@material-symbols/svg-400/outlined/sync.svg?solid";
import GitHubIcon from "./github.svg?solid";
import GitLabIcon from "./gitlab.svg?solid";
import styles from "./TaskDetail.module.css";

// Module-level store for <details> open/closed state (tool calls, thinking blocks).
// Keys: toolUseID, "group:<firstToolUseID>", "thinking:<firstEventTs>".
// Survives component remounts on task switching.
export const detailsOpenState = new Map<string, boolean>();

// Per-task expansion state for collapsed past turns and sessions.
// Turn keys: "<sessionKey>:turn:<firstEventTs>".
// Session keys: "session:<firstEventTs>".
const expandedTurnsByTask = new Map<string, Set<string>>();
const expandedSessionsByTask = new Map<string, Set<string>>();

interface Props {
  taskId: string;
  taskState: string;
  initialPrompt?: string;
  inPlanMode?: boolean;
  planContent?: string;
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
  diffStat?: DiffFileStat[];
  supportsImages?: boolean;
  onClose: () => void;
  inputDraft: string;
  onInputDraft: (value: string) => void;
  inputImages: APIImageData[];
  onInputImages: (imgs: APIImageData[]) => void;
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
  const navigate = useNavigate();
  const location = useLocation();
  const [messages, setMessages] = createSignal<EventMessage[]>([]);
  const [sending, setSending] = createSignal(false);
  const [pendingAction, setPendingAction] = createSignal<"sync" | "restart" | null>(null);
  const [actionError, setActionError] = createSignal<string | null>(null);
  const [safetyIssues, setSafetyIssues] = createSignal<SafetyIssue[]>([]);
  const [syncMenuOpen, setSyncMenuOpen] = createSignal(false);
  const [fixingCI, setFixingCI] = createSignal(false);

  let promptRef: HTMLTextAreaElement | undefined;

  onMount(() => {
    // Skip autofocus on touch-primary devices (mobile) to avoid the soft keyboard
    // taking up half the screen. No reliable way to detect a physical keyboard.
    if (!window.matchMedia('(hover: none) and (pointer: coarse)').matches) {
      promptRef?.focus();
    }
  });

  // Auto-scroll: keep scrolled to bottom unless the user scrolled up.
  let messageAreaRef: HTMLDivElement | undefined; // eslint-disable-line no-unassigned-vars -- assigned by SolidJS ref
  let userScrolledUp = false;

  function isNearBottom(el: HTMLElement): boolean {
    return el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  }

  function handleScroll() {
    if (!messageAreaRef) return;
    userScrolledUp = !isNearBottom(messageAreaRef);
  }

  function scrollToBottom() {
    if (messageAreaRef && !userScrolledUp) {
      messageAreaRef.scrollTop = messageAreaRef.scrollHeight;
    }
  }

  // Close sync dropdown on outside click.
  {
    const onOutsideClick = (e: MouseEvent) => {
      if (!syncMenuOpen()) return;
      const target = e.target as HTMLElement;
      if (target.closest(`.${styles.syncButtonGroup}`)) return;
      setSyncMenuOpen(false);
    };
    document.addEventListener("mousedown", onOutsideClick);
    onCleanup(() => document.removeEventListener("mousedown", onOutsideClick));
  }

  // Scroll to bottom whenever messages change, if the user hasn't scrolled up.
  createEffect(() => {
    messages(); // track dependency
    requestAnimationFrame(scrollToBottom);
  });

  // Expansion state for collapsed past turns and sessions. Persisted per task across remounts.
  const [expandedTurnKeys, setExpandedTurnKeys] = createSignal<ReadonlySet<string>>(
    expandedTurnsByTask.get(props.taskId) ?? new Set(), // eslint-disable-line solid/reactivity -- initial value; createEffect below syncs on taskId changes
  );
  const [expandedSessionKeys, setExpandedSessionKeys] = createSignal<ReadonlySet<string>>(
    expandedSessionsByTask.get(props.taskId) ?? new Set(), // eslint-disable-line solid/reactivity -- initial value
  );
  // Sync expansion state when taskId changes.
  createEffect(() => {
    setExpandedTurnKeys(expandedTurnsByTask.get(props.taskId) ?? new Set());
    setExpandedSessionKeys(expandedSessionsByTask.get(props.taskId) ?? new Set());
  });
  function toggleTurn(key: string) {
    setExpandedTurnKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      expandedTurnsByTask.set(props.taskId, next);
      return next;
    });
  }
  function toggleSession(key: string) {
    setExpandedSessionKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      expandedSessionsByTask.set(props.taskId, next);
      return next;
    });
  }

  // Incremental grouping: cache completed-turn groups so streaming only reprocesses the current turn.
  // splitIdx is the index into messages() where the current (incomplete) turn starts.
  // It advances only when a "result" event arrives, keeping completedMsgs stable during streaming.
  const [splitIdx, setSplitIdx] = createSignal(0);
  const [completedMsgs, setCompletedMsgs] = createSignal<EventMessage[]>([]);
  createEffect(() => {
    const msgs = messages();
    let idx = untrack(splitIdx);
    for (let i = idx; i < msgs.length; i++) {
      if (msgs[i].kind === "result") idx = i + 1;
    }
    if (idx !== untrack(splitIdx)) {
      setSplitIdx(idx);
      setCompletedMsgs(msgs.slice(0, idx));
    }
  });

  // Sessions from completed messages: stable during streaming (only updates on turn completion).
  // groupSessions extracts init/compact_boundary events as session headers, not message groups.
  const allCompletedSessions = createMemo(() => groupSessions(completedMsgs()));
  // Past sessions: all sessions before the current (last) one.
  const pastSessions = createMemo(() => allCompletedSessions().slice(0, -1));
  // Current session: the last session in completedMsgs (contains its completed turns + boundary event).
  const currentSessionEntry = createMemo((): Session | null => {
    const sessions = allCompletedSessions();
    return sessions.length > 0 ? sessions[sessions.length - 1] : null;
  });
  const currentSessionCompletedTurns = createMemo(() => currentSessionEntry()?.turns ?? []);
  // Boundary event for the current session from completed messages.
  const currentSessionBoundaryFromCompleted = createMemo(() => currentSessionEntry()?.boundaryEvent);

  // Scan live messages for a session boundary event (e.g., init before the first result).
  // This handles the common case where the first session starts before any turn completes.
  const liveSessionBoundary = createMemo(() => {
    const msgs = messages();
    const start = splitIdx();
    for (let i = start; i < msgs.length; i++) {
      if (isSessionBoundary(msgs[i])) return msgs[i];
    }
    return undefined;
  });
  const currentSessionBoundaryEvent = createMemo(() =>
    currentSessionBoundaryFromCompleted() ?? liveSessionBoundary(),
  );
  const currentSessionKey = createMemo(() => {
    const idx = allCompletedSessions().length - 1;
    const ev = currentSessionBoundaryEvent();
    return `session:${Math.max(0, idx)}:${ev?.ts ?? ""}`;
  });

  // Live groups: recomputes on every message. Session boundary events are excluded —
  // they become session headers, not message groups.
  const currentGroups = createMemo(() => {
    const msgs = messages().slice(splitIdx()).filter((ev) => !isSessionBoundary(ev));
    return groupMessages(msgs);
  });

  // Flat item model: past sessions (elided) + current session boundary + current session
  // completed turns (elided) + last completed turn (always expanded) + live turn groups.
  // The last completed turn is always expanded to provide context for the current state.
  const items = createMemo((): MsgItem[] => {
    const pastSessionItems = buildPastSessionItems(pastSessions(), expandedSessionKeys(), expandedTurnKeys());
    const boundaryEv = currentSessionBoundaryEvent();
    const sessionBoundaryItems: MsgItem[] = boundaryEv
      ? [{ kind: "sessionBoundary", event: boundaryEv, key: "cur-sess-boundary" }]
      : [];
    const liveGroups = currentGroups();
    const completedTurns = currentSessionCompletedTurns();
    // The last completed turn is always expanded — it provides context whether
    // the agent is idle (no live groups) or actively streaming (live groups present).
    const elidableTurns = completedTurns.slice(0, -1);
    const lastCompletedTurn = completedTurns[completedTurns.length - 1] as typeof completedTurns[number] | undefined;
    const completedTurnItems = buildTurnItems(elidableTurns, expandedTurnKeys(), currentSessionKey());
    const lastTurnItems: MsgItem[] = lastCompletedTurn
      ? lastCompletedTurn.groups.map((g, j) => ({ kind: "group" as const, group: g, isLive: false, key: `last-g${j}` }))
      : [];
    const liveItems: MsgItem[] = liveGroups.map((g, j) => ({
      kind: "group" as const,
      group: g,
      isLive: true,
      key: `live-g${j}`,
    }));
    return [...pastSessionItems, ...sessionBoundaryItems, ...completedTurnItems, ...lastTurnItems, ...liveItems];
  });

  // Last ask group: only the most recent ask is interactive.
  // Flatten all groups from all sessions + current live turn for ask detection.
  const allGroups = createMemo(() => {
    const sessionGroups = allCompletedSessions().flatMap((s) => s.turns.flatMap((t) => t.groups));
    return [...sessionGroups, ...currentGroups()];
  });
  const lastAskGroup = createMemo((): MessageGroup | null => {
    const groups = allGroups();
    for (let i = groups.length - 1; i >= 0; i--) {
      if (groups[i].kind === "ask") return groups[i];
    }
    return null;
  });

  createEffect(() => {
    const id = props.taskId;
    userScrolledUp = false;

    // Reset incremental grouping state for the new task.
    setSplitIdx(0);
    setCompletedMsgs([]);

    let es: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let delay = 500;
    // Buffer accumulates replayed history; swapped into signal on "ready" event.
    let buf: EventMessage[] = [];
    let live = false;
    // rAF batching for live events: reduces setMessages calls from one-per-event
    // to one-per-frame, cutting O(n²) streaming overhead to O(n/fps).
    let pendingLive: EventMessage[] = [];
    let rafId: number | null = null;

    function flushLive() {
      rafId = null;
      const evs = pendingLive;
      pendingLive = [];
      // Each ExitPlanMode event keeps its own planContent snapshot so the evolution
      // of the plan is visible at each point it was written.
      setMessages((prev) => [...prev, ...evs]);
    }

    function connect() {
      // Close any stale connection that may exist if connect() is called while
      // a previous EventSource is still open (e.g. from a duplicate timer fire).
      es?.close();
      buf = [];
      live = false;
      es = taskEvents(id, (ev) => {
        if (live) {
          pendingLive.push(ev);
          if (rafId === null) rafId = requestAnimationFrame(flushLive);
        } else {
          buf.push(ev);
        }
      });
      es.addEventListener("open", () => {
        delay = 500;
      });
      // The server sends a "ready" event after replaying full history.
      // Swap the buffer in atomically to avoid a flash of empty content.
      es.addEventListener("ready", () => {
        live = true;
        setMessages(buf);
      });
      es.onerror = () => {
        es?.close();
        es = null;
        const st = props.taskState;
        if (live && messages().length > 0 && (st === "purged" || st === "failed")) {
          return;
        }
        // Cancel any pending timer before scheduling a new one. Without this,
        // a second onerror fire (possible with some EventSource implementations)
        // would leave the first timer running, causing connect() to be called
        // twice and creating a leaked/duplicate SSE connection.
        if (timer !== null) clearTimeout(timer);
        timer = setTimeout(connect, delay);
        delay = Math.min(delay * 1.5, 4000);
      };
    }

    connect();

    onCleanup(() => {
      es?.close();
      if (timer !== null) clearTimeout(timer);
      if (rafId !== null) {
        cancelAnimationFrame(rafId);
        rafId = null;
        pendingLive = [];
      }
    });
  });

  async function sendInput() {
    const text = props.inputDraft.trim();
    const imgs = props.inputImages;
    if (!text && imgs.length === 0) return;
    requestNotificationPermission();
    setSending(true);
    try {
      await apiSendInput(props.taskId, { prompt: { text, ...(imgs.length > 0 ? { images: imgs } : {}) } });
      props.onInputDraft("");
      props.onInputImages([]);
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Unknown error";
      setActionError(`send failed: ${msg}`);
      setTimeout(() => setActionError(null), 5000);
    } finally {
      setSending(false);
    }
  }

  async function sendAskAnswer(text: string) {
    setSending(true);
    try {
      await apiSendInput(props.taskId, { prompt: { text } });
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Unknown error";
      setActionError(`send failed: ${msg}`);
      setTimeout(() => setActionError(null), 5000);
    } finally {
      setSending(false);
    }
  }

  const isActive = () => {
    const s = props.taskState;
    return s === "running" || s === "branching" || s === "provisioning" || s === "starting" || s === "waiting" || s === "asking" || s === "has_plan" || s === "purging";
  };

  const isWaiting = () => props.taskState === "waiting" || props.taskState === "asking" || props.taskState === "has_plan";
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
      await apiRestartTask(props.taskId, { prompt: { text: prompt } });
      props.onInputDraft("");
    });
  }

  async function doSync(force: boolean, target?: SyncTarget) {
    if (pendingAction()) return;
    setPendingAction("sync");
    setActionError(null);
    setSafetyIssues([]);
    setSyncMenuOpen(false);
    try {
      const resp = await apiSyncTask(props.taskId, { force, ...(target ? { target } : {}) });
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

  async function handleFixCI() {
    if (fixingCI()) return;
    setFixingCI(true);
    setActionError(null);
    try {
      let resp: { id: string };
      if (props.forgePR) {
        resp = await botFixPR({ taskId: props.taskId });
      } else {
        const failedCheck = props.ciChecks?.find((c) => c.conclusion !== "success" && c.conclusion !== "neutral" && c.conclusion !== "skipped");
        if (!failedCheck) { setFixingCI(false); return; }
        const ciLog = await getTaskCILog(props.taskId, String(failedCheck.jobID));
        const prompt = `CI failed on GitHub Actions for step ${JSON.stringify(ciLog.stepName)}, with log:\n\`\`\`\n${ciLog.log}\n\`\`\``;
        resp = await createTask({
          initialPrompt: { text: prompt },
          repos: props.repo ? [{ name: props.repo, ...(props.baseBranch ? { baseBranch: props.baseBranch } : {}) }] : undefined,
          harness: props.harness,
          ...(props.model ? { model: props.model } : {}),
        });
      }
      navigate(`/tasks/${resp.id}`);
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Unknown error";
      setActionError(`fix CI failed: ${msg}`);
      setTimeout(() => setActionError(null), 5000);
    } finally {
      setFixingCI(false);
    }
  }

  async function runAction(name: "sync" | "restart", fn: () => Promise<unknown>) {
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

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button class={styles.closeBtn} onClick={() => props.onClose()} title="Close"><CloseIcon width={20} height={20} /></button>
        <span class={styles.headerMeta}>
          <Show when={props.remoteURL} fallback={<span class={styles.headerRepo}>{props.repo}</span>}>
            <a class={styles.headerRepo} href={props.remoteURL} target="_blank" rel="noopener">{props.repo}</a>
          </Show>
          <span class={styles.headerBranch}>{props.branch}</span>
          <Show when={prURL()}>
            <a class={styles.headerPR} href={prURL()} target="_blank" rel="noopener">{prLabel()}</a>
          </Show>
          <Show when={props.ciStatus && props.ciStatus in CI_STATUS_CLASS}>
            {(() => {
              const s = props.ciStatus as CIStatus;
              const hasChecks = () => (props.ciChecks?.length ?? 0) > 0;
              const actionsURL = () => ciActionsURL(props.remoteURL, props.forge);
              return (
                <>
                  <Show when={hasChecks()} fallback={
                    <Show when={actionsURL()} keyed fallback={<span class={`${styles.ciStatus} ${CI_STATUS_CLASS[s]}`}>{ciLabel(s, props.ciChecks)}</span>}>
                      {(url) => <a class={`${styles.ciStatus} ${CI_STATUS_CLASS[s]}`} href={url} target="_blank" rel="noopener">{ciLabel(s, props.ciChecks)}</a>}
                    </Show>
                  }>
                    <details class={styles.ciDetails}>
                      <summary class={`${styles.ciStatus} ${CI_STATUS_CLASS[s]}`}>{ciLabel(s, props.ciChecks)}</summary>
                      <div class={styles.ciDropdown}>
                        <For each={props.ciChecks}>
                          {(c) => {
                            const statusCls = c.status === "completed"
                              ? (c.conclusion === "success" || c.conclusion === "neutral" || c.conclusion === "skipped" ? styles.ciCheckPassed : styles.ciCheckFailed)
                              : c.status === "in_progress" ? styles.ciCheckRunning : styles.ciCheckQueued;
                            const jobURL = () => checkJobURL(c, props.forge);
                            return (
                              <Show when={jobURL()} keyed fallback={
                                <div class={`${styles.ciCheckRow} ${statusCls}`}>
                                  <span class={styles.ciCheckName}>{c.name}</span>
                                  <span class={styles.ciCheckStatus}>{checkStatusLabel(c)}</span>
                                  <Show when={c.startedAt || c.queuedAt}>
                                    <span class={styles.ciCheckDuration}>{checkDuration(c, Date.now())}</span>
                                  </Show>
                                </div>
                              }>
                                {(url) => (
                                  <a class={`${styles.ciCheckRow} ${styles.ciCheckLink} ${statusCls}`} href={url} target="_blank" rel="noopener">
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
                  <Show when={s === "failure" && props.ciChecks?.some((c) => c.conclusion !== "success" && c.conclusion !== "neutral" && c.conclusion !== "skipped")}>
                    <button class={styles.fixCIBtn} onClick={handleFixCI} disabled={fixingCI()} title="Create a new task to investigate this CI failure">
                      {fixingCI() ? "Creating…" : "Fix CI"}
                    </button>
                  </Show>
                </>
              );
            })()}
          </Show>
        </span>
        <Show when={(props.diffStat?.length ?? 0) > 0}>
          <A class={styles.diffLink} href={`${location.pathname}/diff`}>Diff</A>
        </Show>
        <Show when={props.inPlanMode}>
          <span class={styles.planIndicator} title="Agent is in plan mode">Plan Mode</span>
        </Show>
      </div>
      <div class={styles.messageArea} ref={messageAreaRef} onScroll={handleScroll}>
        <Index each={items()}>
          {(item) => {
            // Type-narrowing accessors for the MsgItem discriminated union.
            const sessElided = () => item().kind === "sessionElided" ? item() as Extract<MsgItem, { kind: "sessionElided" }> : null;
            const sessHdr = () => item().kind === "sessionHeader" ? item() as Extract<MsgItem, { kind: "sessionHeader" }> : null;
            const sessBoundary = () => item().kind === "sessionBoundary" ? item() as Extract<MsgItem, { kind: "sessionBoundary" }> : null;
            const elided = () => item().kind === "elided" ? item() as Extract<MsgItem, { kind: "elided" }> : null;
            const expHdr = () => item().kind === "expandedHeader" ? item() as Extract<MsgItem, { kind: "expandedHeader" }> : null;
            const grpItem = () => item().kind === "group" ? item() as Extract<MsgItem, { kind: "group" }> : null;
            return (
              <Switch>
                {/* Collapsed past session: single clickable row. */}
                <Match when={sessElided()} keyed>
                  {(se) => (
                    <button class={styles.sessionElided} onClick={() => toggleSession(se.sessionKey)}>
                      {sessionSummary(se.session)}
                    </button>
                  )}
                </Match>
                {/* Expanded past session header: click to collapse. */}
                <Match when={sessHdr()} keyed>
                  {(sh) => (
                    <button class={`${styles.sessionElided} ${styles.sessionElidedExpanded}`} onClick={() => toggleSession(sh.sessionKey)}>
                      {sessionSummary(sh.session)}
                    </button>
                  )}
                </Match>
                {/* Session boundary: init or compact_boundary rendered as a separator. */}
                <Match when={sessBoundary()} keyed>
                  {(sb) => <SessionBoundaryItem event={sb.event} />}
                </Match>
                {/* Collapsed past turn: single clickable row. */}
                <Match when={elided()} keyed>
                  {(e) => (
                    <button class={`${styles.elidedTurn}${e.indent === "session" ? ` ${styles.indentSession}` : ""}`} onClick={() => toggleTurn(e.key)}>
                      {turnSummary(e.turn)}
                    </button>
                  )}
                </Match>
                {/* Expanded past turn header: click to collapse. */}
                <Match when={expHdr()} keyed>
                  {(h) => (
                    <button class={`${styles.elidedTurn} ${styles.elidedTurnExpanded}${h.indent === "session" ? ` ${styles.indentSession}` : ""}`} onClick={() => toggleTurn(h.turnKey)}>
                      {turnSummary(h.turn)}
                    </button>
                  )}
                </Match>
                {/* Message group: dispatch to group-specific renderer. */}
                <Match when={grpItem()} keyed>
                  {(gi) => (
                    <div class={gi.indent === "turn" ? styles.indentTurn : undefined}>
                      <GroupContent
                        group={() => gi.group}
                        taskId={props.taskId}
                        isWaiting={isWaiting}
                        lastAskGroup={lastAskGroup}
                        onAskAnswer={sendAskAnswer}
                        onClearAndExecutePlan={clearAndExecutePlan}
                        pendingAction={pendingAction}
                      />
                    </div>
                  )}
                </Match>
              </Switch>
            );
          }}
        </Index>
        <Show when={messages().length === 0}>
          <Show when={props.initialPrompt} keyed fallback={<p class={styles.placeholder}>Waiting for agent output...</p>}>
            {(prompt) => (
              <div class={styles.userInputMsg}>
                <Markdown text={prompt} />
              </div>
            )}
          </Show>
        </Show>
      </div>

      <ProgressPanel messages={messages()} />

      <Show when={isActive() || !!pendingAction()}>
        <form onSubmit={(e) => { e.preventDefault(); sendInput(); }} class={styles.inputForm} data-testid="task-detail-form">
          <PromptInput
            ref={(el) => { promptRef = el; }}
            value={props.inputDraft}
            onInput={props.onInputDraft}
            onSubmit={sendInput}
            placeholder="Send message to agent..."
            class={styles.textInput}
            tabIndex={0}
            supportsImages={props.supportsImages}
            images={props.inputImages}
            onImagesChange={props.onInputImages}
          >
            <Button type="submit" disabled={sending() || (!props.inputDraft.trim() && props.inputImages.length === 0)} title="Send" data-testid="send-input"><SendIcon width="1.1em" height="1.1em" /></Button>
            <div class={styles.syncButtonGroup}>
              <Button type="button" variant="gray" loading={pendingAction() === "sync"} disabled={!!pendingAction() || props.taskState === "purging"} onClick={() => doSync(false)} title={`Push to ${props.branch}`}>
                <Switch fallback={<SyncIcon width="1.1em" height="1.1em" />}>
                  <Match when={props.forge === "github"}>
                    <GitHubIcon width="1.1em" height="1.1em" style={{ color: "black" }} />
                  </Match>
                  <Match when={props.forge === "gitlab"}>
                    <GitLabIcon width="1.1em" height="1.1em" style={{ color: "#e24329" }} />
                  </Match>
                </Switch>
                {props.forge ? (props.forgePR ? " Push" : " Create PR") : " Push"}
              </Button>
              <button type="button" class={styles.syncDropdownToggle} disabled={!!pendingAction() || props.taskState === "purging"} onClick={() => setSyncMenuOpen((v) => !v)} aria-label="Sync options">&#9660;</button>
              <Show when={syncMenuOpen()}>
                <div class={styles.syncDropdown}>
                  <button type="button" class={styles.syncDropdownItem} onClick={() => doSync(false, SyncTargetDefault)}>Push to {props.baseBranch}</button>
                </div>
              </Show>
            </div>
          </PromptInput>
        </form>
        <Show when={safetyIssues().length > 0}>
          <div class={styles.safetyWarning}>
            <strong>Safety issues detected:</strong>
            <ul>
              <For each={safetyIssues()}>
                {(issue) => <li><strong>{issue.file}</strong>: {issue.detail} ({issue.kind})</li>}
              </For>
            </ul>
            <Button type="button" variant="red" loading={pendingAction() === "sync"} disabled={!!pendingAction()} onClick={() => { setSafetyIssues([]); doSync(true); }}>Force Push to {props.branch}</Button>
          </div>
        </Show>
        <Show when={actionError()}>
          <div class={styles.actionError}>{actionError()}</div>
        </Show>
      </Show>
    </div>
  );
}

// Renders the content for a single message group.
// group is a reactive accessor so group().kind etc track correctly in JSX.
function GroupContent(props: {
  group: () => MessageGroup;
  taskId: string;
  isWaiting: () => boolean;
  lastAskGroup: () => MessageGroup | null;
  onAskAnswer: (text: string) => void;
  onClearAndExecutePlan?: () => void;
  pendingAction?: () => string | null;
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
                  {(img) => <img class={styles.userInputImage} src={`data:${img.mediaType};base64,${img.data}`} alt="attached" />}
                </For>
              </div>
            </Show>
          </div>
        )}
      </Match>
      <Match when={group().kind === "action"}>
        <Show when={group().toolCalls.length > 0}
          fallback={<ThinkingCard events={group().events} />}>
          <ToolMessageGroup toolCalls={group().toolCalls} taskId={props.taskId} events={group().events} onClearAndExecutePlan={props.isWaiting() ? props.onClearAndExecutePlan : undefined} pendingAction={props.pendingAction} />
        </Show>
      </Match>
      <Match when={group().kind === "text"}>
        <TextMessageGroup events={group().events} />
      </Match>
      <Match when={group().kind === "other"}>
        <For each={group().events}>
          {(ev) => <MessageItem ev={ev} />}
        </For>
      </Match>
    </Switch>
  );
}

// Renders the header for a session boundary (init or compact_boundary).
// These events are extracted from the message stream and shown as section separators.
function SessionBoundaryItem(props: { event: EventMessage }) {
  const ev = () => props.event;
  return (
    <Switch>
      <Match when={ev().init} keyed>
        {(init) => (
          <div class={styles.systemInit}>
            Session started &middot; {init.model} &middot; {init.agentVersion} &middot; {init.sessionID}
          </div>
        )}
      </Match>
      <Match when={ev().system?.subtype === "compact_boundary"}>
        <div class={styles.contextCleared}>Conversation compacted</div>
      </Match>
    </Switch>
  );
}

function MessageItem(props: { ev: EventMessage }) {
  return (
    <Switch>
      <Match when={props.ev.system?.subtype === "context_cleared"}>
        <div class={styles.contextCleared}>Context cleared</div>
      </Match>
      <Match when={props.ev.system?.subtype === "api_error"}>
        <div class={styles.parseError}>API error</div>
      </Match>
      <Match when={props.ev.system?.subtype === "step_start"}>
        {/* suppress: no useful content */}
      </Match>
      <Match when={props.ev.system?.subtype === "model_rerouted"} keyed>
        {() => (
          <div class={styles.systemMsg}>
            Model rerouted{props.ev.system?.detail ? `: ${props.ev.system.detail}` : ""}
          </div>
        )}
      </Match>
      <Match when={props.ev.system} keyed>
        {(sys) => (
          <div class={styles.systemMsg}>
            [{sys.subtype}]
          </div>
        )}
      </Match>
      <Match when={props.ev.text} keyed>
        {(text) => (
          <div class={styles.assistantMsg}>
            <Markdown text={text.text} />
          </div>
        )}
      </Match>
      <Match when={props.ev.usage} keyed>
        {(usage) => (
          <div class={styles.usageMeta}>
            {usage.model} &middot; {formatTokens(usage.inputTokens + usage.cacheCreationInputTokens + usage.cacheReadInputTokens)} in + {formatTokens(usage.outputTokens)} out
            <Show when={usage.cacheReadInputTokens > 0}>
              {" "}&middot; {formatTokens(usage.cacheReadInputTokens)} cached
            </Show>
          </div>
        )}
      </Match>
      <Match when={props.ev.result} keyed>
        {(result) => <ResultCard result={result} />}
      </Match>
      <Match when={props.ev.error} keyed>
        {(err) => (
          <div class={styles.parseError}>
            Parse error: {err.err}
          </div>
        )}
      </Match>
      <Match when={props.ev.log} keyed>
        {(log) => (
          <div class={styles.logLine}>{log.line}</div>
        )}
      </Match>
    </Switch>
  );
}

function ResultCard(props: { result: EventResult }) {
  const result = () => props.result;
  return (
    <div class={`${styles.result} ${result().isError ? styles.resultError : styles.resultSuccess}`}>
      <strong>{result().isError ? "Error" : "Done"}</strong>
      <Show when={result().result}>
        <div class={styles.resultText}><Markdown text={result().result} /></div>
      </Show>
      <Show when={result().diffStat} keyed>
        {(files) => <DiffStatBlock files={files} />}
      </Show>
      <div class={styles.resultMeta}>
        <Show when={result().totalCostUSD !== 0}>
          ${result().totalCostUSD.toFixed(4)} &middot;{" "}
        </Show>
        {result().duration.toFixed(1)}s &middot; {result().numTurns} turns
      </div>
    </div>
  );
}

function DiffStatBlock(props: { files: DiffFileStat[] }) {
  const navigate = useNavigate();
  const location = useLocation();
  return (
    <div class={`${styles.resultDiffStat} ${styles.diffFileClickable}`} role="button" tabIndex={0} onClick={() => navigate(`${location.pathname}/diff`)} onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); navigate(`${location.pathname}/diff`); } }}>
      <For each={props.files}>
        {(f) => (
          <div class={styles.diffFile}>
            <span class={styles.diffPath}>{f.path}</span>
            <Show when={f.binary} fallback={
              <span class={styles.diffCounts}>
                <Show when={f.added > 0}><span class={styles.diffAdded}>+{f.added}</span></Show>
                <Show when={f.deleted > 0}><span class={styles.diffDeleted}>&minus;{f.deleted}</span></Show>
              </span>
            }>
              <span class={styles.diffBinary}>binary</span>
            </Show>
          </div>
        )}
      </For>
    </div>
  );
}

function ToolMessageGroup(props: { toolCalls: ToolCall[]; taskId: string; events?: EventMessage[]; onClearAndExecutePlan?: () => void; pendingAction?: () => string | null }) {
  const calls = () => props.toolCalls;
  const groupKey = () => "group:" + calls()[0]?.use.toolUseID;
  const isOpen = () => detailsOpenState.get(groupKey()) ?? false;
  const thinkingEvents = () => (props.events ?? []).filter(
    (e) => e.kind === "thinking" || e.kind === "thinkingDelta",
  );
  // Compute accumulated tool output deltas per toolUseID from the group events.
  const outputDeltaEvents = (toolUseID: string) => (props.events ?? []).filter(
    (e) => e.kind === "toolOutputDelta" && e.toolOutputDelta?.toolUseID === toolUseID,
  );
  return (
    <Show when={calls().length > 0}>
      <Show when={calls().length > 1} fallback={
        <ToolCallCard call={calls()[0]} taskId={props.taskId}
          thinkingEvents={thinkingEvents()}
          outputDeltaEvents={outputDeltaEvents(calls()[0].use.toolUseID)}
          open={detailsOpenState.get(calls()[0].use.toolUseID) ?? false}
          onToggle={(v) => detailsOpenState.set(calls()[0].use.toolUseID, v)}
          onClearAndExecutePlan={props.onClearAndExecutePlan}
          pendingAction={props.pendingAction} />
      }>
        <>
          <details class={styles.toolGroup} open={isOpen()}
            onToggle={(e) => detailsOpenState.set(groupKey(), e.currentTarget.open)}>
            <summary>
              {calls().filter((c) => c.done).length}/{calls().length} tools: {toolCountSummary(calls())}
            </summary>
            <div class={styles.toolGroupInner}>
              <Show when={thinkingEvents().length > 0}>
                <ThinkingCard events={thinkingEvents()} />
              </Show>
              <Index each={calls()}>
                {(call) => <ToolCallCard call={call()} taskId={props.taskId}
                  outputDeltaEvents={outputDeltaEvents(call().use.toolUseID)}
                  open={detailsOpenState.get(call().use.toolUseID) ?? false}
                  onToggle={(v) => detailsOpenState.set(call().use.toolUseID, v)}
                  suppressPlanContent={true}
                  pendingAction={props.pendingAction} />}
              </Index>
            </div>
          </details>
          <Show when={calls().find((c) => c.use.planContent)?.use.planContent} keyed>
            {(plan) => (
              <div class={styles.planAction}>
                <div class={styles.planContent}>
                  <Markdown text={plan} />
                </div>
                <Show when={props.onClearAndExecutePlan}>
                  <Button variant="gray" loading={props.pendingAction?.() === "restart"} disabled={!!props.pendingAction?.()} onClick={props.onClearAndExecutePlan} data-testid="clear-and-execute-plan">
                    Clear and execute plan
                  </Button>
                </Show>
              </div>
            )}
          </Show>
        </>
      </Show>
    </Show>
  );
}

// Renders a thinking group, collapsed by default like a ToolCallCard.
function ThinkingCard(props: { events: EventMessage[] }) {
  const text = createMemo(() => {
    const finalEv = props.events.findLast((e) => e.kind === "thinking");
    if (finalEv?.thinking) return finalEv.thinking.text;
    return props.events
      .filter((e): e is EventMessage & { thinkingDelta: NonNullable<EventMessage["thinkingDelta"]> } => e.kind === "thinkingDelta" && !!e.thinkingDelta)
      .map((e) => e.thinkingDelta.text)
      .join("");
  });
  const key = () => "thinking:" + (props.events[0]?.ts ?? 0);
  const isOpen = () => detailsOpenState.get(key()) ?? false;
  return (
    <Show when={text()}>
      <details class={styles.thinkingBlock} open={isOpen()}
        onToggle={(e) => detailsOpenState.set(key(), e.currentTarget.open)}>
        <summary>Thinking</summary>
        <pre class={styles.thinkingText}>{text()}</pre>
      </details>
    </Show>
  );
}

// Renders a text group, combining textDelta fragments into a single view.
function TextMessageGroup(props: { events: EventMessage[] }) {
  const thinkingEvents = createMemo(() =>
    props.events.filter((e) => e.kind === "thinking" || e.kind === "thinkingDelta"),
  );
  const text = createMemo(() => {
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
        <div class={styles.assistantMsg}>
          <Markdown text={text()} />
        </div>
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

function ToolCallInput(props: { input: Record<string, unknown> }) {
  const flat = () => isFlat(props.input);
  return (
    <Show
      when={flat()}
      fallback={
        <pre class={styles.toolBlockPre}>{JSON.stringify(props.input, null, 2)}</pre>
      }
    >
      <div class={styles.toolInputList}>
        <For each={Object.entries(props.input)}>
          {([k, v]) => {
            const multiline = typeof v === "string" && v.includes("\n");
            return (
              <div class={styles.toolInputRow}>
                <span class={styles.toolInputKey}>{k}:</span>
                {multiline
                  ? <pre class={styles.toolInputBlock}>{v as string}</pre>
                  : <>{" "}{fmtValue(v)}</>}
              </div>
            );
          }}
        </For>
      </div>
    </Show>
  );
}

function ToolCallCard(props: { call: ToolCall; taskId: string; open: boolean; onToggle: (open: boolean) => void; thinkingEvents?: EventMessage[]; outputDeltaEvents?: EventMessage[]; onClearAndExecutePlan?: () => void; pendingAction?: () => string | null; suppressPlanContent?: boolean }) {
  const [loadedInput, setLoadedInput] = createSignal<Record<string, unknown> | null>(null);
  const [loading, setLoading] = createSignal(false);

  const duration = () => props.call.result?.duration ?? 0;
  const error = () => props.call.result?.error ?? "";
  const effectiveInput = (): Record<string, unknown> =>
    (loadedInput() ?? props.call.use.input ?? {}) as Record<string, unknown>;
  const detail = () => toolCallDetail(props.call.use.name, effectiveInput());
  const showLoadBtn = () => props.call.use.inputTruncated && !loadedInput();

  async function loadInput() {
    setLoading(true);
    try {
      const resp = await getTaskToolInput(props.taskId, props.call.use.toolUseID);
      setLoadedInput(resp.input as Record<string, unknown>);
    } finally {
      setLoading(false);
    }
  }

  return (
    <>
      <details class={styles.toolBlock} open={props.open}
        onToggle={(e) => props.onToggle(e.currentTarget.open)}>
        <summary>
          <Show when={!props.call.done} fallback={<span class={styles.toolDone}>&#10003;</span>}>
            <span class={styles.toolPending} />
          </Show>
          {props.call.use.name}
          <Show when={detail()}>
            <span class={styles.toolDetail}>{detail()}</span>
          </Show>
          <Show when={duration() > 0}>
            <span class={styles.toolDuration}>{formatDuration(duration())}</span>
          </Show>
          <Show when={error()}>
            <span class={styles.toolError}> error</span>
          </Show>
        </summary>
        <Show when={(props.thinkingEvents?.length ?? 0) > 0}>
          <ThinkingCard events={props.thinkingEvents ?? []} />
        </Show>
        <Show when={showLoadBtn()} fallback={<ToolCallInput input={effectiveInput()} />}>
          <button class={styles.loadInputBtn} onClick={loadInput} disabled={loading()}>
            {loading() ? "Loading…" : "Load input"}
          </button>
        </Show>
        <Show when={error()}>
          <pre class={styles.toolErrorPre}>{error()}</pre>
        </Show>
        <Show when={(props.outputDeltaEvents?.length ?? 0) > 0}>
          <pre class={styles.toolOutputDelta}>{props.outputDeltaEvents?.map((e) => e.toolOutputDelta?.delta ?? "").join("")}</pre>
        </Show>
      </details>
      <Show when={!props.suppressPlanContent && props.call.use.planContent} keyed>
        {(plan) => (
          <div class={styles.planAction}>
            <div class={styles.planContent}>
              <Markdown text={plan} />
            </div>
            <Show when={props.onClearAndExecutePlan}>
              <Button variant="gray" loading={props.pendingAction?.() === "restart"} disabled={!!props.pendingAction?.()} onClick={props.onClearAndExecutePlan}>
                Clear and execute plan
              </Button>
            </Show>
          </div>
        )}
      </Show>
    </>
  );
}

const marked = new Marked({
  breaks: true,
  gfm: true,
});

function Markdown(props: { text: string }) {
  const html = createMemo(() => marked.parse(props.text) as string);
  // eslint-disable-next-line solid/no-innerhtml -- rendering trusted marked output
  return <div class={styles.markdown} innerHTML={html()} />;
}

function AskQuestionCard(props: { ask: EventAsk; interactive: boolean; answerText?: string; onSubmit: (text: string) => void }) {
  const questions = () => props.ask.questions;
  const [selections, setSelections] = createSignal<Map<number, Set<string>>>(new Map());
  const [otherTexts, setOtherTexts] = createSignal<Map<number, string>>(new Map());
  const [submitted, setSubmitted] = createSignal(false);
  const answered = () => props.answerText !== undefined || submitted();

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
        parts.push(`${q.header ?? `Q${i + 1}`}: ${answer}`);
      }
    }
    return parts.join("\n");
  }

  function handleSubmit() {
    const answer = formatAnswer();
    if (!answer.trim()) return;
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
                class={selections().get(qIdx())?.has("__other__") ? `${styles.askChip} ${styles.askChipSelected}` : styles.askChip}
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
        <button class={styles.askSubmit} onClick={() => handleSubmit()} data-testid="ask-submit">Submit</button>
      </Show>
      <Show when={answered()}>
        <div class={styles.askSubmitted} data-testid="ask-submitted-answer">
          {props.answerText ?? formatAnswer()}
        </div>
      </Show>
    </div>
  );
}
