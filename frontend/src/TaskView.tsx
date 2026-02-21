// TaskView renders the real-time agent output stream for a single task.
import { createSignal, createMemo, For, Index, Show, onCleanup, createEffect, Switch, Match, type Accessor, type JSX } from "solid-js";
import { sendInput as apiSendInput, restartTask as apiRestartTask, terminateTask as apiTerminateTask, syncTask as apiSyncTask, taskRawEvents } from "@sdk/api.gen";
import type { ClaudeEventMessage, ClaudeAskQuestion, ClaudeEventTextDelta, SafetyIssue, ImageData as APIImageData, SyncTarget, DiffFileStat, DiffResp } from "@sdk/types.gen";
import { groupMessages, groupTurns, toolCountSummary, turnSummary, turnHasExitPlanMode, turnPlanContent } from "./grouping";
import type { ToolCall, Turn } from "./grouping";
import { SyncTargetDefault } from "@sdk/types.gen";
import { Marked } from "marked";
import AutoResizeTextarea from "./AutoResizeTextarea";
import PromptInput from "./PromptInput";
import Button from "./Button";
import TodoPanel from "./TodoPanel";
import CloseIcon from "@material-symbols/svg-400/outlined/close.svg?solid";
import DeleteIcon from "@material-symbols/svg-400/outlined/delete.svg?solid";
import SendIcon from "@material-symbols/svg-400/outlined/send.svg?solid";
import SyncIcon from "@material-symbols/svg-400/outlined/sync.svg?solid";
import GitHubIcon from "./github.svg?solid";
import styles from "./TaskView.module.css";

async function fetchFileDiff(taskId: string, path: string): Promise<string> {
  const res = await fetch(`/api/v1/tasks/${taskId}/diff?path=${encodeURIComponent(path)}`);
  if (!res.ok) throw new Error(`diff fetch failed: ${res.status}`);
  const data = (await res.json()) as DiffResp;
  return data.diff;
}

// Module-level store for <details> open/closed state so it survives
// component remounts (task switching, memo re-evaluation).
// Keys: toolUseID (tool calls), "group:<firstToolUseID>" (tool groups),
// "turn:<firstEventTs>" (elided turns).
export const detailsOpenState = new Map<string, boolean>();

// A group of consecutive events that should be rendered together.

interface Props {
  taskId: string;
  taskState: string;
  inPlanMode?: boolean;
  repo: string;
  repoURL?: string;
  branch: string;
  baseBranch: string;
  supportsImages?: boolean;
  onClose: () => void;
  inputDraft: string;
  onInputDraft: (value: string) => void;
  children?: JSX.Element;
}

export default function TaskView(props: Props) {
  const [messages, setMessages] = createSignal<ClaudeEventMessage[]>([]);
  const [pendingImages, setPendingImages] = createSignal<APIImageData[]>([]);
  const [sending, setSending] = createSignal(false);
  const [pendingAction, setPendingAction] = createSignal<"sync" | "terminate" | "restart" | null>(null);
  const [actionError, setActionError] = createSignal<string | null>(null);
  const [safetyIssues, setSafetyIssues] = createSignal<SafetyIssue[]>([]);
  const [syncMenuOpen, setSyncMenuOpen] = createSignal(false);

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
    // Use requestAnimationFrame so the DOM has updated before we measure.
    requestAnimationFrame(scrollToBottom);
  });

  createEffect(() => {
    const id = props.taskId;
    userScrolledUp = false;

    let es: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let delay = 500;
    // Buffer accumulates replayed history; swapped into signal on "ready" event.
    let buf: ClaudeEventMessage[] = [];
    let live = false;

    function connect() {
      buf = [];
      live = false;
      es = taskRawEvents(id, (ev) => {
        if (live) {
          setMessages((prev) => [...prev, ev]);
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
        // For terminal tasks the server closes the stream after sending
        // history — no new messages will arrive, so stop reconnecting.
        const st = props.taskState;
        if (live && messages().length > 0 && (st === "terminated" || st === "failed")) {
          return;
        }
        timer = setTimeout(connect, delay);
        delay = Math.min(delay * 1.5, 4000);
      };
    }

    connect();

    onCleanup(() => {
      es?.close();
      if (timer !== null) clearTimeout(timer);
    });
  });

  async function sendInput() {
    const text = props.inputDraft.trim();
    const imgs = pendingImages();
    if (!text && imgs.length === 0) return;
    setSending(true);
    try {
      await apiSendInput(props.taskId, { prompt: { text, ...(imgs.length > 0 ? { images: imgs } : {}) } });
      props.onInputDraft("");
      setPendingImages([]);
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
    return s === "running" || s === "branching" || s === "provisioning" || s === "starting" || s === "waiting" || s === "asking" || s === "terminating";
  };

  const isWaiting = () => props.taskState === "waiting" || props.taskState === "asking";
  const isGitHub = () => !!props.repoURL?.includes("github.com");

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

  async function runAction(name: "sync" | "terminate" | "restart", fn: () => Promise<unknown>) {
    if (pendingAction()) return;
    setPendingAction(name);
    setActionError(null);
    let failed = false;
    try {
      await fn();
    } catch (e) {
      failed = true;
      const msg = e instanceof Error ? e.message : "Unknown error";
      setActionError(`${name} failed: ${msg}`);
      setTimeout(() => setActionError(null), 5000);
    } finally {
      // For terminate, keep pendingAction set until the server state catches
      // up to "terminating" — clearing it immediately causes a spinner flicker
      // in the gap before the SSE state update arrives.
      if (failed || name !== "terminate") {
        setPendingAction(null);
      }
    }
  }

  // Clear stale "terminate" pendingAction once the server state reflects it.
  createEffect(() => {
    if (pendingAction() === "terminate" && (props.taskState === "terminating" || props.taskState === "terminated" || props.taskState === "failed")) {
      setPendingAction(null);
    }
  });

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button class={styles.closeBtn} onClick={() => props.onClose()} title="Close"><CloseIcon width={20} height={20} /></button>
        <span class={styles.headerMeta}>
          <Show when={props.repoURL} fallback={<span class={styles.headerRepo}>{props.repo}</span>}>
            <a class={styles.headerRepo} href={props.repoURL} target="_blank" rel="noopener">{props.repo}</a>
          </Show>
          <Show when={isGitHub()} fallback={<span class={styles.headerBranch}>{props.branch}</span>}>
            <a class={styles.headerBranch} href={`${props.repoURL}/compare/${props.branch}?expand=1`} target="_blank" rel="noopener">{props.branch}</a>
          </Show>
        </span>
        <Show when={props.inPlanMode}>
          <span class={styles.planIndicator} title="Agent is in plan mode">Plan Mode</span>
        </Show>
        {props.children}
      </div>
      <div class={styles.messageArea} ref={messageAreaRef} onScroll={handleScroll}>
        {(() => {
          const grouped = createMemo(() => groupMessages(messages()));
          const turns = createMemo(() => groupTurns(grouped()));

          // Find the index of the last "ask" group across all turns for interactivity.
          const lastAskIdx = createMemo(() => {
            const g = grouped();
            for (let i = g.length - 1; i >= 0; i--) {
              if (g[i].kind === "ask") return i;
            }
            return -1;
          });

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

          function clearAndExecutePlan() {
            const prompt = props.inputDraft.trim();
            // eslint-disable-next-line solid/reactivity -- only called from onClick
            runAction("restart", async () => {
              await apiRestartTask(props.taskId, { prompt: { text: prompt } });
              props.onInputDraft("");
            });
          }

          return (
            <Index each={turns()}>
              {(turn, turnIdx) => {
                const isLastTurn = () => turnIdx === turns().length - 1;

                return (
                  <Show when={isLastTurn()} fallback={
                    <ElidedTurn turn={turn()} taskId={props.taskId} />
                  }>
                    <Index each={turn().groups}>
                      {(group) => (
                        <Switch>
                          <Match when={group().kind === "ask" && group().ask} keyed>
                            {(ask) => (
                              <AskQuestionGroup
                                ask={ask}
                                interactive={isWaiting() && group() === grouped()[lastAskIdx()]}
                                answerText={group().answerText}
                                onSubmit={sendAskAnswer}
                              />
                            )}
                          </Match>
                          <Match when={group().kind === "userInput" && group().events[0]?.userInput} keyed>
                            {(ui) => (
                              <div class={styles.userInputMsg}>
                                {ui.text}
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
                          <Match when={group().kind === "tool"}>
                            <ToolMessageGroup toolCalls={group().toolCalls} />
                          </Match>
                          <Match when={group().kind === "text"}>
                            <TextMessageGroup events={group().events} />
                          </Match>
                          <Match when={group().kind === "other"}>
                            <For each={group().events}>
                              {(ev) => (
                                <>
                                  <MessageItem ev={ev} taskId={props.taskId} />
                                  <Show when={ev.result && turnHasExitPlanMode(turn()) && isWaiting()}>
                                    <div class={styles.planAction}>
                                      <Show when={turnPlanContent(turn())} keyed>
                                        {(plan) => (
                                          <div class={styles.planContent}>
                                            <Markdown text={plan} />
                                          </div>
                                        )}
                                      </Show>
                                      <Button variant="gray" loading={pendingAction() === "restart"} disabled={!!pendingAction()} onClick={() => clearAndExecutePlan()}>
                                        Clear and execute plan
                                      </Button>
                                    </div>
                                  </Show>
                                </>
                              )}
                            </For>
                          </Match>
                        </Switch>
                      )}
                    </Index>
                  </Show>
                );
              }}
            </Index>
          );
        })()}
        <Show when={messages().length === 0}>
          <p class={styles.placeholder}>Waiting for agent output...</p>
        </Show>
      </div>

      <TodoPanel messages={messages()} />

      <Show when={isActive() || !!pendingAction()}>
        <form onSubmit={(e) => { e.preventDefault(); sendInput(); }} class={styles.inputForm}>
          <PromptInput
            value={props.inputDraft}
            onInput={props.onInputDraft}
            onSubmit={sendInput}
            placeholder="Send message to agent..."
            disabled={sending()}
            class={styles.textInput}
            tabIndex={1}
            supportsImages={props.supportsImages}
            images={pendingImages()}
            onImagesChange={setPendingImages}
          >
            <Button type="submit" disabled={sending() || (!props.inputDraft.trim() && pendingImages().length === 0)} title="Send"><SendIcon width="1.1em" height="1.1em" /></Button>
            <div class={styles.syncButtonGroup}>
              <Button type="button" variant="gray" loading={pendingAction() === "sync"} disabled={!!pendingAction() || props.taskState === "terminating"} onClick={() => doSync(false)} title={`Push to ${props.branch}`}>
                <Show when={isGitHub()} fallback={<SyncIcon width="1.1em" height="1.1em" />}>
                  <GitHubIcon width="1.1em" height="1.1em" style={{ color: "black" }} />
                </Show>
              </Button>
              <button type="button" class={styles.syncDropdownToggle} disabled={!!pendingAction() || props.taskState === "terminating"} onClick={() => setSyncMenuOpen((v) => !v)} aria-label="Sync options">&#9660;</button>
              <Show when={syncMenuOpen()}>
                <div class={styles.syncDropdown}>
                  <button type="button" class={styles.syncDropdownItem} onClick={() => doSync(false)}>Push to {props.branch}</button>
                  <button type="button" class={styles.syncDropdownItem} onClick={() => doSync(false, SyncTargetDefault)}>Push to {props.baseBranch}</button>
                </div>
              </Show>
            </div>
            <Button type="button" variant="red" loading={pendingAction() === "terminate" || props.taskState === "terminating"} disabled={!!pendingAction() || props.taskState === "terminating"} onClick={() => { const id = props.taskId; runAction("terminate", () => apiTerminateTask(id)); }} title="Terminate" data-testid="terminate-task"><DeleteIcon width="1.1em" height="1.1em" /></Button>
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

function MessageItem(props: { ev: ClaudeEventMessage; taskId?: string }) {
  return (
    <Switch>
      <Match when={props.ev.init} keyed>
        {(init) => (
          <div class={styles.systemInit}>
            Session started &middot; {init.model} &middot; {init.agentVersion} &middot; {init.sessionID}
          </div>
        )}
      </Match>
      <Match when={props.ev.system?.subtype === "context_cleared"}>
        <div class={styles.contextCleared}>Context cleared</div>
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
        {(result) => (
          <div class={`${styles.result} ${result.isError ? styles.resultError : styles.resultSuccess}`}>
            <strong>{result.isError ? "Error" : "Done"}</strong>
            <Show when={result.result}>
              <div class={styles.resultText}><Markdown text={result.result} /></div>
            </Show>
            <Show when={result.diffStat} keyed>
              {(files) => (
                <DiffStatBlock files={files} taskId={props.taskId} />
              )}
            </Show>
            <div class={styles.resultMeta}>
              ${result.totalCostUSD.toFixed(4)} &middot; {result.duration.toFixed(1)}s &middot; {result.numTurns} turns
            </div>
          </div>
        )}
      </Match>
    </Switch>
  );
}

function DiffStatBlock(props: { files: DiffFileStat[]; taskId?: string }) {
  const [expandedFile, setExpandedFile] = createSignal<string | null>(null);
  return (
    <div class={styles.resultDiffStat}>
      <For each={props.files}>
        {(f) => (
          <>
            <div
              class={f.binary || !props.taskId ? styles.diffFile : `${styles.diffFile} ${styles.diffFileClickable}`}
              onClick={() => {
                if (f.binary || !props.taskId) return;
                setExpandedFile((prev) => (prev === f.path ? null : f.path));
              }}
            >
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
            <Show when={expandedFile() === f.path ? props.taskId : undefined} keyed>
              {(id) => <DiffViewer taskId={id} path={f.path} />}
            </Show>
          </>
        )}
      </For>
    </div>
  );
}


function formatDuration(seconds: number): string {
  if (seconds < 1) return `${Math.round(seconds * 1000)}ms`;
  return `${seconds.toFixed(1)}s`;
}

function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}Mt`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(0)}kt`;
  return `${n}t`;
}


function ToolMessageGroup(props: { toolCalls: ToolCall[] }) {
  const calls = () => props.toolCalls;
  const groupKey = () => "group:" + calls()[0]?.use.toolUseID;
  const isOpen = () => detailsOpenState.get(groupKey()) ?? false;
  return (
    <Show when={calls().length > 0}>
      <Show when={calls().length > 1} fallback={
        <ToolCallBlock call={calls()[0]}
          open={detailsOpenState.get(calls()[0].use.toolUseID) ?? false}
          onToggle={(v) => detailsOpenState.set(calls()[0].use.toolUseID, v)} />
      }>
        <details class={styles.toolGroup} open={isOpen()}
          onToggle={(e) => detailsOpenState.set(groupKey(), e.currentTarget.open)}>
          <summary>
            {calls().filter((c) => c.done).length}/{calls().length} tools: {toolCountSummary(calls())}
          </summary>
          <div class={styles.toolGroupInner}>
            <For each={calls()}>
              {(call) => <ToolCallBlock call={call}
                open={detailsOpenState.get(call.use.toolUseID) ?? false}
                onToggle={(v) => detailsOpenState.set(call.use.toolUseID, v)} />}
            </For>
          </div>
        </details>
      </Show>
    </Show>
  );
}

// Renders a text group, combining textDelta fragments into a single view.
// When a final "text" event arrives, it replaces the accumulated deltas.
function TextMessageGroup(props: { events: ClaudeEventMessage[] }) {
  const text = createMemo(() => {
    // If a final text event exists, use it (it has the complete content).
    const finalEv = props.events.findLast((e) => e.kind === "text");
    if (finalEv?.text) return finalEv.text.text;
    // Otherwise, accumulate textDelta fragments.
    return props.events
      .filter((e): e is ClaudeEventMessage & { textDelta: ClaudeEventTextDelta } => e.kind === "textDelta" && !!e.textDelta)
      .map((e) => e.textDelta.text)
      .join("");
  });
  return (
    <Show when={text()}>
      <div class={styles.assistantMsg}>
        <Markdown text={text()} />
      </div>
    </Show>
  );
}


function ElidedTurn(props: { turn: Turn; taskId?: string }) {
  const turnKey = () => "turn:" + (props.turn.groups[0]?.events[0]?.ts ?? 0);
  const isOpen = () => detailsOpenState.get(turnKey()) ?? false;
  return (
    <details class={styles.elidedTurn} open={isOpen()}
      onToggle={(e) => detailsOpenState.set(turnKey(), e.currentTarget.open)}>
      <summary>{turnSummary(props.turn)}</summary>
      <div class={styles.elidedTurnInner}>
        <For each={props.turn.groups}>
          {(group) => (
            <Switch>
              <Match when={group.kind === "ask" && group.ask} keyed>
                {(ask) => (
                  <div class={styles.askGroup}>
                    <div class={styles.askText}>
                      {ask.questions[0]?.question ?? "Question"}
                    </div>
                    <Show when={group.answerText}>
                      <div class={styles.askSubmitted}>{group.answerText}</div>
                    </Show>
                  </div>
                )}
              </Match>
              <Match when={group.kind === "userInput" && group.events[0]?.userInput} keyed>
                {(ui) => (
                  <div class={styles.userInputMsg}>
                    {ui.text}
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
              <Match when={group.kind === "tool"}>
                <ToolMessageGroup toolCalls={group.toolCalls} />
              </Match>
              <Match when={group.kind === "text"}>
                <TextMessageGroup events={group.events} />
              </Match>
              <Match when={group.kind === "other"}>
                <For each={group.events}>
                  {(ev) => <MessageItem ev={ev} taskId={props.taskId} />}
                </For>
              </Match>
            </Switch>
          )}
        </For>
      </div>
    </details>
  );
}

// Extracts a short, tool-specific detail string from tool input for display
// in collapsed summaries (e.g. file path for Read, command for Bash).
function toolCallDetail(name: string, input: Record<string, unknown>): string {
  switch (name.toLowerCase()) {
    case "read":
    case "write":
      return typeof input.file_path === "string" ? input.file_path.replace(/^.*\//, "") : "";
    case "edit":
      return typeof input.file_path === "string" ? input.file_path.replace(/^.*\//, "") : "";
    case "bash":
      if (typeof input.command === "string") {
        const cmd = input.command.trimStart();
        return cmd.length > 60 ? cmd.slice(0, 57) + "..." : cmd;
      }
      return "";
    case "grep":
      return typeof input.pattern === "string" ? input.pattern : "";
    case "glob":
      return typeof input.pattern === "string" ? input.pattern : "";
    case "task":
      return typeof input.description === "string" ? input.description : "";
    case "webfetch":
      return typeof input.url === "string" ? input.url : "";
    case "websearch":
      return typeof input.query === "string" ? input.query : "";
    case "notebookedit":
      return typeof input.notebook_path === "string" ? input.notebook_path.replace(/^.*\//, "") : "";
    default:
      return "";
  }
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

function ToolCallBlock(props: { call: ToolCall; open: boolean; onToggle: (open: boolean) => void }) {
  const duration = () => props.call.result?.duration ?? 0;
  const error = () => props.call.result?.error ?? "";
  const detail = () => toolCallDetail(props.call.use.name, props.call.use.input ?? {});
  return (
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
      <ToolCallInput input={props.call.use.input ?? {}} />
      <Show when={error()}>
        <pre class={styles.toolErrorPre}>{error()}</pre>
      </Show>
    </details>
  );
}

function DiffViewer(props: { taskId: string; path: string }) {
  const [diff, setDiff] = createSignal<string | null>(null);
  const [error, setError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(true);

  createEffect(() => {
    setLoading(true);
    setError(null);
    fetchFileDiff(props.taskId, props.path)
      .then((d) => setDiff(d))
      .catch((e) => setError(e instanceof Error ? e.message : "Unknown error"))
      .finally(() => setLoading(false));
  });

  return (
    <Show when={!loading()} fallback={<div class={styles.diffLoading}>Loading diff...</div>}>
      <Show when={!error()} fallback={<div class={styles.diffError}>{error()}</div>}>
        <pre class={styles.diffContent}>
          <For each={(diff() ?? "").split("\n")}>
            {(line) => {
              let cls = "";
              if (line.startsWith("+")) cls = styles.diffLineAdded;
              else if (line.startsWith("-")) cls = styles.diffLineDeleted;
              else if (line.startsWith("@@")) cls = styles.diffLineHunk;
              return <div class={cls}>{line}</div>;
            }}
          </For>
        </pre>
      </Show>
    </Show>
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

function AskQuestionGroup(props: { ask: ClaudeEventAsk; interactive: boolean; answerText?: string; onSubmit: (text: string) => void }) {
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
        {(q: ClaudeAskQuestion, qIdx: Accessor<number>) => (
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
        <button class={styles.askSubmit} onClick={() => handleSubmit()}>Submit</button>
      </Show>
      <Show when={answered()}>
        <div class={styles.askSubmitted}>
          {props.answerText ?? formatAnswer()}
        </div>
      </Show>
    </div>
  );
}
