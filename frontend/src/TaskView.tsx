// TaskView renders the real-time agent output stream for a single task.
import { createSignal, createMemo, For, Index, Show, onCleanup, createEffect, Switch, Match, type Accessor } from "solid-js";
import { sendInput as apiSendInput, finishTask as apiFinishTask, endTask as apiEndTask, pullTask as apiPullTask, pushTask as apiPushTask } from "@sdk/api.gen";
import { Marked } from "marked";
import styles from "./TaskView.module.css";

interface ContentBlock {
  type: string;
  text?: string;
  id?: string;
  name?: string;
  input?: unknown;
}

interface AgentMessage {
  type: string;
  subtype?: string;
  message?: {
    model?: string;
    content?: ContentBlock[];
  };
  result?: string;
  total_cost_usd?: number;
  duration_ms?: number;
  num_turns?: number;
  is_error?: boolean;
  cwd?: string;
  model?: string;
  claude_code_version?: string;
}

interface AskOption { label: string; description?: string }
interface AskQuestion { question: string; header?: string; options: AskOption[]; multiSelect?: boolean }
interface AskUserQuestionInput { questions: AskQuestion[] }

// A group of consecutive messages that should be rendered together.
// "text" groups contain assistant text blocks.
// "tool" groups coalesce tool_use blocks and their user (tool result) messages.
// "ask" groups contain a single AskUserQuestion tool_use block.
// "other" groups contain standalone messages (system, result, etc.).
interface MessageGroup {
  kind: "text" | "tool" | "ask" | "other";
  messages: AgentMessage[];
  toolBlocks: ContentBlock[];
}

// A turn is a sequence of message groups between user interactions.
// Turns are separated by "result" messages (end of a Claude Code query).
interface Turn {
  groups: MessageGroup[];
  toolCount: number;
  textCount: number;
}

interface Props {
  taskId: number;
  taskState: string;
  taskQuery: string;
  onClose: () => void;
}

export default function TaskView(props: Props) {
  const [messages, setMessages] = createSignal<AgentMessage[]>([]);
  const [input, setInput] = createSignal("");
  const [sending, setSending] = createSignal(false);

  createEffect(() => {
    const id = props.taskId;
    setMessages([]);

    let es: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let delay = 500;

    function connect() {
      setMessages([]);
      es = new EventSource(`/api/v1/tasks/${id}/events`);
      es.addEventListener("open", () => { delay = 500; });
      es.addEventListener("message", (e) => {
        try {
          const msg = JSON.parse(e.data) as AgentMessage;
          setMessages((prev) => [...prev, msg]);
        } catch {
          // Ignore unparseable messages.
        }
      });
      es.onerror = () => {
        es?.close();
        es = null;
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
    const text = input().trim();
    if (!text) return;
    setSending(true);
    try {
      await apiSendInput(props.taskId, { prompt: text });
      setInput("");
    } finally {
      setSending(false);
    }
  }

  const isActive = () => {
    const s = props.taskState;
    return s === "running" || s === "branching" || s === "provisioning" || s === "starting" || s === "waiting" || s === "asking";
  };

  const isWaiting = () => props.taskState === "waiting" || props.taskState === "asking";

  async function finishTask() {
    await apiFinishTask(props.taskId);
  }

  async function pullTask() {
    await apiPullTask(props.taskId);
  }

  async function pushTask() {
    await apiPushTask(props.taskId);
  }

  async function endTask() {
    await apiEndTask(props.taskId);
  }

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <h3>Task #{props.taskId}</h3>
        <button class={styles.closeBtn} onClick={() => props.onClose()}>Close</button>
      </div>
      <div class={styles.query}>{props.taskQuery}</div>

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
                          <Match when={group.kind === "ask"}>
                            <AskQuestionGroup
                              block={group.toolBlocks[0]}
                              interactive={isWaiting() && group === grouped()[lastAskIdx()]}
                              onSubmit={sendAskAnswer}
                            />
                          </Match>
                          <Match when={group.kind === "tool"}>
                            <ToolMessageGroup toolBlocks={group.toolBlocks} />
                          </Match>
                          <Match when={group.kind === "text" || group.kind === "other"}>
                            <For each={group.messages}>
                              {(msg) => <MessageItem msg={msg} />}
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

      <Show when={isActive()}>
        <form onSubmit={(e) => { e.preventDefault(); sendInput(); }} class={styles.inputForm}>
          <input
            type="text"
            value={input()}
            onInput={(e) => setInput(e.currentTarget.value)}
            placeholder="Send message to agent..."
            disabled={sending()}
            class={styles.textInput}
          />
          <button type="submit" disabled={sending() || !input().trim()}>Send</button>
          <button type="button" class={`${styles.btn} ${styles.btnGray}`} onClick={() => pullTask()}>
            Pull
          </button>
          <button type="button" class={`${styles.btn} ${styles.btnGray}`} onClick={() => pushTask()}>
            Push
          </button>
          <Show when={isWaiting()}>
            <button type="button" class={`${styles.btn} ${styles.btnGreen}`} onClick={() => finishTask()}>
              Finish
            </button>
          </Show>
          <button type="button" class={`${styles.btn} ${styles.btnRed}`} onClick={() => endTask()}>
            End
          </button>
        </form>
      </Show>
    </div>
  );
}

function MessageItem(props: { msg: AgentMessage }) {
  return (
    <Switch>
      <Match when={props.msg.type === "system" && props.msg.subtype === "init"}>
        <div class={styles.systemInit}>
          Session started &middot; {props.msg.model} &middot; {props.msg.claude_code_version}
        </div>
      </Match>
      <Match when={props.msg.type === "system"}>
        <div class={styles.systemMsg}>
          [{props.msg.subtype}]
        </div>
      </Match>
      <Match when={props.msg.type === "assistant"}>
        <div class={styles.assistantMsg}>
          <For each={props.msg.message?.content?.filter((b) => b.type === "text") ?? []}>
            {(block) => <Markdown text={block.text ?? ""} />}
          </For>
        </div>
      </Match>
      <Match when={props.msg.type === "result"}>
        <div class={`${styles.result} ${props.msg.is_error ? styles.resultError : styles.resultSuccess}`}>
          <strong>{props.msg.is_error ? "Error" : "Done"}</strong>
          <Show when={props.msg.result}>
            <div class={styles.resultText}>{props.msg.result}</div>
          </Show>
          <Show when={props.msg.total_cost_usd}>
            <div class={styles.resultMeta}>
              ${props.msg.total_cost_usd?.toFixed(4)} &middot; {((props.msg.duration_ms ?? 0) / 1000).toFixed(1)}s &middot; {props.msg.num_turns} turns
            </div>
          </Show>
        </div>
      </Match>
    </Switch>
  );
}

// Groups consecutive messages so that tool_use blocks and their user (tool result)
// messages are coalesced into a single "tool" group. Text-only assistant messages
// become "text" groups. Everything else (system, result) becomes "other".
function groupMessages(msgs: AgentMessage[]): MessageGroup[] {
  const groups: MessageGroup[] = [];

  function lastGroup(): MessageGroup | undefined {
    return groups[groups.length - 1];
  }

  for (const msg of msgs) {
    if (msg.type === "assistant") {
      const content = msg.message?.content ?? [];
      const textBlocks = content.filter((b) => b.type === "text");
      const toolBlocks = content.filter((b) => b.type === "tool_use");

      // Emit text group for any text blocks.
      if (textBlocks.length > 0) {
        groups.push({ kind: "text", messages: [msg], toolBlocks: [] });
      }

      // Split tool_use blocks: AskUserQuestion gets its own "ask" group.
      const askBlocks = toolBlocks.filter((b) => b.name === "AskUserQuestion");
      const otherToolBlocks = toolBlocks.filter((b) => b.name !== "AskUserQuestion");

      if (otherToolBlocks.length > 0) {
        const last = lastGroup();
        if (last && last.kind === "tool") {
          last.messages.push(msg);
          last.toolBlocks.push(...otherToolBlocks);
        } else {
          groups.push({ kind: "tool", messages: [msg], toolBlocks: [...otherToolBlocks] });
        }
      }

      for (const askBlock of askBlocks) {
        groups.push({ kind: "ask", messages: [msg], toolBlocks: [askBlock] });
      }
    } else if (msg.type === "user") {
      // Tool results — coalesce into the preceding tool group.
      const last = lastGroup();
      if (last && last.kind === "tool") {
        last.messages.push(msg);
      } else {
        // Orphaned user message; start a tool group anyway.
        groups.push({ kind: "tool", messages: [msg], toolBlocks: [] });
      }
    } else {
      groups.push({ kind: "other", messages: [msg], toolBlocks: [] });
    }
  }
  return groups;
}

// Splits message groups into turns separated by "result" messages.
// Each turn represents a segment of agent work between user interactions.
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
      toolCount += g.toolBlocks.length;
    } else if (g.kind === "text") {
      textCount++;
    }
    // A result message ends the current turn.
    if (g.kind === "other" && g.messages.some((m) => m.type === "result")) {
      flush();
    }
  }
  flush();
  return turns;
}

function toolCountSummary(tools: ContentBlock[]): string {
  const counts = new Map<string, number>();
  for (const t of tools) {
    const n = t.name ?? "tool";
    counts.set(n, (counts.get(n) ?? 0) + 1);
  }
  return Array.from(counts.entries())
    .map(([name, c]) => (c > 1 ? `${name} \u00d7${c}` : name))
    .join(", ");
}

function ToolMessageGroup(props: { toolBlocks: ContentBlock[] }) {
  const blocks = () => props.toolBlocks;
  return (
    <Show when={blocks().length > 0} fallback={
      <details class={styles.toolResult}><summary>tool result</summary></details>
    }>
      <Show when={blocks().length > 1} fallback={
        <ToolUseBlock name={blocks()[0].name ?? "tool"} input={blocks()[0].input} />
      }>
        <details class={styles.toolGroup}>
          <summary>
            {blocks().length} tools: {toolCountSummary(blocks())}
          </summary>
          <div class={styles.toolGroupInner}>
            <For each={blocks()}>
              {(block) => (
                <ToolUseBlock name={block.name ?? "tool"} input={block.input} />
              )}
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
              <Match when={group.kind === "ask"}>
                <div class={styles.askGroup}>
                  <div class={styles.askText}>
                    {(parseAskInput(group.toolBlocks[0]?.input))?.questions[0]?.question ?? "Question"}
                  </div>
                </div>
              </Match>
              <Match when={group.kind === "tool"}>
                <ToolMessageGroup toolBlocks={group.toolBlocks} />
              </Match>
              <Match when={group.kind === "text" || group.kind === "other"}>
                <For each={group.messages}>
                  {(msg) => <MessageItem msg={msg} />}
                </For>
              </Match>
            </Switch>
          )}
        </For>
      </div>
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

function parseAskInput(input: unknown): AskUserQuestionInput | undefined {
  if (typeof input !== "object" || input === null) return undefined;
  const obj = input as Record<string, unknown>;
  if (!Array.isArray(obj.questions)) return undefined;
  return obj as unknown as AskUserQuestionInput;
}

function AskQuestionGroup(props: { block: ContentBlock; interactive: boolean; onSubmit: (text: string) => void }) {
  const parsed = createMemo(() => parseAskInput(props.block.input));
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
    const questions = parsed()?.questions ?? [];
    const parts: string[] = [];
    for (let i = 0; i < questions.length; i++) {
      const q = questions[i];
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
      if (questions.length === 1) {
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
    <Switch fallback={<ToolUseBlock name={props.block.name ?? "AskUserQuestion"} input={props.block.input} />}>
      <Match when={parsed()}>
        <div class={canInteract() ? `${styles.askGroup} ${styles.askGroupActive}` : styles.askGroup}>
          <For each={parsed()?.questions ?? []}>
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
                  <input
                    type="text"
                    class={styles.askOtherInput}
                    placeholder="Type your answer..."
                    value={otherTexts().get(qIdx()) ?? ""}
                    onInput={(e) => setOtherText(qIdx(), e.currentTarget.value)}
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
      </Match>
    </Switch>
  );
}

function ToolUseBlock(props: { name: string; input: unknown }) {
  return (
    <details class={styles.toolBlock}>
      <summary>{props.name}</summary>
      <pre class={styles.toolBlockPre}>
        {JSON.stringify(props.input, null, 2)}
      </pre>
    </details>
  );
}
