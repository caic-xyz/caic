// TaskView renders the real-time agent output stream for a single task.
import { createSignal, createMemo, For, Index, Show, onCleanup, createEffect, Switch, Match, type Accessor, type JSX } from "solid-js";
import { sendInput as apiSendInput, terminateTask as apiTerminateTask, pullTask as apiPullTask, pushTask as apiPushTask, taskEvents } from "@sdk/api.gen";
import { Marked } from "marked";
import AutoResizeTextarea from "./AutoResizeTextarea";
import Button from "./Button";
import CloseIcon from "@material-symbols/svg-400/outlined/close.svg?solid";
import styles from "./TaskView.module.css";

// A group of consecutive events that should be rendered together.
interface MessageGroup {
  kind: "text" | "tool" | "ask" | "userInput" | "other";
  events: EventMessage[];
  // For "tool" groups: paired tool_use and tool_result events.
  toolCalls: ToolCall[];
  // For "ask" groups: the ask payload.
  ask?: EventAsk;
}

// A tool_use event paired with its optional tool_result.
interface ToolCall {
  use: EventToolUse;
  result?: EventToolResult;
}

// A turn is a sequence of message groups between user interactions.
// Turns are separated by "result" messages (end of a Claude Code query).
interface Turn {
  groups: MessageGroup[];
  toolCount: number;
  textCount: number;
}

interface Props {
  taskId: string;
  taskState: string;
  repo: string;
  repoURL?: string;
  branch: string;
  onClose: () => void;
  inputDraft: string;
  onInputDraft: (value: string) => void;
  children?: JSX.Element;
}

export default function TaskView(props: Props) {
  const [messages, setMessages] = createSignal<EventMessage[]>([]);
  const [sending, setSending] = createSignal(false);
  const [pendingAction, setPendingAction] = createSignal<"pull" | "push" | "terminate" | null>(null);
  const [actionError, setActionError] = createSignal<string | null>(null);

  createEffect(() => {
    const id = props.taskId;

    let es: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let delay = 500;
    // Buffer accumulates replayed history; swapped into signal on "ready" event.
    let buf: EventMessage[] = [];
    let live = false;

    function connect() {
      buf = [];
      live = false;
      es = taskEvents(id, (ev) => {
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
    if (!text) return;
    setSending(true);
    try {
      await apiSendInput(props.taskId, { prompt: text });
      props.onInputDraft("");
    } finally {
      setSending(false);
    }
  }

  const isActive = () => {
    const s = props.taskState;
    return s === "running" || s === "branching" || s === "provisioning" || s === "starting" || s === "waiting" || s === "asking" || s === "terminating";
  };

  const isWaiting = () => props.taskState === "waiting" || props.taskState === "asking";

  async function runAction(name: "pull" | "push" | "terminate", fn: () => Promise<unknown>) {
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
          <Show when={props.repoURL} fallback={<span class={styles.headerRepo}>{props.repo}</span>}>
            <a class={styles.headerRepo} href={props.repoURL} target="_blank" rel="noopener">{props.repo}</a>
          </Show>
          <span class={styles.headerBranch}>{props.branch}</span>
        </span>
        {props.children}
      </div>
      <div class={styles.messageArea}>
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
              await apiSendInput(props.taskId, { prompt: text });
            } finally {
              setSending(false);
            }
          }

          return (
            <Index each={turns()}>
              {(turn, turnIdx) => {
                const isLastTurn = () => turnIdx === turns().length - 1;

                return (
                  <Show when={isLastTurn()} fallback={
                    <ElidedTurn turn={turn()} />
                  }>
                    <For each={turn().groups}>
                      {(group) => (
                        <Switch>
                          <Match when={group.kind === "ask" && group.ask} keyed>
                            {(ask) => (
                              <AskQuestionGroup
                                ask={ask}
                                interactive={isWaiting() && group === grouped()[lastAskIdx()]}
                                onSubmit={sendAskAnswer}
                              />
                            )}
                          </Match>
                          <Match when={group.kind === "userInput" && group.events[0]?.userInput} keyed>
                            {(ui) => (
                              <div class={styles.userInputMsg}>{ui.text}</div>
                            )}
                          </Match>
                          <Match when={group.kind === "tool"}>
                            <ToolMessageGroup toolCalls={group.toolCalls} />
                          </Match>
                          <Match when={group.kind === "text" || group.kind === "other"}>
                            <For each={group.events}>
                              {(ev) => <MessageItem ev={ev} />}
                            </For>
                          </Match>
                        </Switch>
                      )}
                    </For>
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

      <Show when={isActive() || !!pendingAction()}>
        <form onSubmit={(e) => { e.preventDefault(); sendInput(); }} class={styles.inputForm}>
          <AutoResizeTextarea
            value={props.inputDraft}
            onInput={props.onInputDraft}
            onSubmit={sendInput}
            placeholder="Send message to agent..."
            disabled={sending()}
            class={styles.textInput}
          />
          <Button type="submit" disabled={sending() || !props.inputDraft.trim()}>Send</Button>
          <Button type="button" variant="gray" loading={pendingAction() === "pull"} disabled={!!pendingAction()} onClick={() => { const id = props.taskId; runAction("pull", () => apiPullTask(id)); }}>Pull</Button>
          <Button type="button" variant="gray" loading={pendingAction() === "push"} disabled={!!pendingAction()} onClick={() => { const id = props.taskId; runAction("push", () => apiPushTask(id)); }}>Push</Button>
          <Button type="button" variant="red" loading={pendingAction() === "terminate"} disabled={!!pendingAction()} onClick={() => { const id = props.taskId; runAction("terminate", () => apiTerminateTask(id)); }}>Terminate</Button>
        </form>
        <Show when={actionError()}>
          <div class={styles.actionError}>{actionError()}</div>
        </Show>
      </Show>
    </div>
  );
}

function MessageItem(props: { ev: EventMessage }) {
  return (
    <Switch>
      <Match when={props.ev.init} keyed>
        {(init) => (
          <div class={styles.systemInit}>
            Session started &middot; {init.model} &middot; {init.claudeCodeVersion}
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
            {usage.model} &middot; {usage.inputTokens}in + {usage.outputTokens}out
            <Show when={usage.cacheReadInputTokens > 0}>
              {" "}&middot; {usage.cacheReadInputTokens} cache
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
            <div class={styles.resultMeta}>
              ${result.totalCostUSD.toFixed(4)} &middot; {(result.durationMs / 1000).toFixed(1)}s &middot; {result.numTurns} turns
              &middot; {result.usage.inputTokens + result.usage.outputTokens} tokens
            </div>
          </div>
        )}
      </Match>
    </Switch>
  );
}

// Groups consecutive events for cohesive rendering.
function groupMessages(msgs: EventMessage[]): MessageGroup[] {
  const groups: MessageGroup[] = [];

  function lastGroup(): MessageGroup | undefined {
    return groups[groups.length - 1];
  }

  for (const ev of msgs) {
    switch (ev.kind) {
      case "text":
        groups.push({ kind: "text", events: [ev], toolCalls: [] });
        break;
      case "toolUse": {
        if (ev.toolUse) {
          const last = lastGroup();
          const call: ToolCall = { use: ev.toolUse };
          if (last && last.kind === "tool") {
            last.events.push(ev);
            last.toolCalls.push(call);
          } else {
            groups.push({ kind: "tool", events: [ev], toolCalls: [call] });
          }
        }
        break;
      }
      case "toolResult": {
        if (ev.toolResult) {
          const last = lastGroup();
          if (last && last.kind === "tool") {
            last.events.push(ev);
            const tr = ev.toolResult;
            const match = last.toolCalls.find((tc) => tc.use.toolUseID === tr.toolUseID && !tc.result);
            if (match) {
              match.result = ev.toolResult;
            }
          } else {
            groups.push({ kind: "tool", events: [ev], toolCalls: [] });
          }
        }
        break;
      }
      case "ask":
        if (ev.ask) {
          groups.push({ kind: "ask", events: [ev], toolCalls: [], ask: ev.ask });
        }
        break;
      case "userInput":
        groups.push({ kind: "userInput", events: [ev], toolCalls: [] });
        break;
      case "usage":
        {
          const last = lastGroup();
          if (last && (last.kind === "text" || last.kind === "tool")) {
            last.events.push(ev);
          } else {
            groups.push({ kind: "other", events: [ev], toolCalls: [] });
          }
        }
        break;
      default:
        groups.push({ kind: "other", events: [ev], toolCalls: [] });
        break;
    }
  }
  return groups;
}

// Splits message groups into turns separated by "result" events.
function groupTurns(groups: MessageGroup[]): Turn[] {
  const turns: Turn[] = [];
  let current: MessageGroup[] = [];
  let toolCount = 0;
  let textCount = 0;

  function flush() {
    if (current.length > 0) {
      turns.push({ groups: current, toolCount, textCount });
      current = [];
      toolCount = 0;
      textCount = 0;
    }
  }

  for (const g of groups) {
    current.push(g);
    if (g.kind === "tool") {
      toolCount += g.toolCalls.length;
    } else if (g.kind === "text") {
      textCount++;
    }
    if (g.kind === "other" && g.events.some((ev) => ev.kind === "result")) {
      flush();
    }
  }
  flush();
  return turns;
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function toolCountSummary(calls: ToolCall[]): string {
  const counts = new Map<string, number>();
  for (const tc of calls) {
    const n = tc.use.name;
    counts.set(n, (counts.get(n) ?? 0) + 1);
  }
  return Array.from(counts.entries())
    .map(([name, c]) => (c > 1 ? `${name} \u00d7${c}` : name))
    .join(", ");
}

function ToolMessageGroup(props: { toolCalls: ToolCall[] }) {
  const calls = () => props.toolCalls;
  return (
    <Show when={calls().length > 0}>
      <Show when={calls().length > 1} fallback={
        <ToolCallBlock call={calls()[0]} />
      }>
        <details class={styles.toolGroup}>
          <summary>
            {calls().length} tools: {toolCountSummary(calls())}
          </summary>
          <div class={styles.toolGroupInner}>
            <For each={calls()}>
              {(call) => <ToolCallBlock call={call} />}
            </For>
          </div>
        </details>
      </Show>
    </Show>
  );
}

function turnSummary(turn: Turn): string {
  const parts: string[] = [];
  if (turn.textCount > 0) {
    parts.push(turn.textCount === 1 ? "1 message" : `${turn.textCount} messages`);
  }
  if (turn.toolCount > 0) {
    parts.push(turn.toolCount === 1 ? "1 tool call" : `${turn.toolCount} tool calls`);
  }
  return parts.length > 0 ? parts.join(", ") : "empty turn";
}

function ElidedTurn(props: { turn: Turn }) {
  return (
    <details class={styles.elidedTurn}>
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
                  </div>
                )}
              </Match>
              <Match when={group.kind === "userInput" && group.events[0]?.userInput} keyed>
                {(ui) => (
                  <div class={styles.userInputMsg}>{ui.text}</div>
                )}
              </Match>
              <Match when={group.kind === "tool"}>
                <ToolMessageGroup toolCalls={group.toolCalls} />
              </Match>
              <Match when={group.kind === "text" || group.kind === "other"}>
                <For each={group.events}>
                  {(ev) => <MessageItem ev={ev} />}
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

function ToolCallBlock(props: { call: ToolCall }) {
  const duration = () => props.call.result?.durationMs ?? 0;
  const error = () => props.call.result?.error ?? "";
  const detail = () => toolCallDetail(props.call.use.name, props.call.use.input ?? {});
  return (
    <details class={styles.toolBlock}>
      <summary>
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
      <pre class={styles.toolBlockPre}>
        {JSON.stringify(props.call.use.input, null, 2)}
      </pre>
      <Show when={error()}>
        <pre class={styles.toolErrorPre}>{error()}</pre>
      </Show>
    </details>
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

function AskQuestionGroup(props: { ask: EventAsk; interactive: boolean; onSubmit: (text: string) => void }) {
  const questions = () => props.ask.questions;
  const [selections, setSelections] = createSignal<Map<number, Set<string>>>(new Map());
  const [otherTexts, setOtherTexts] = createSignal<Map<number, string>>(new Map());
  const [submitted, setSubmitted] = createSignal(false);

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

  const canInteract = (): boolean => props.interactive && !submitted();

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
      <Show when={submitted()}>
        <div class={styles.askSubmitted}>Answer submitted</div>
      </Show>
    </div>
  );
}
