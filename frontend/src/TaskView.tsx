// TaskView renders the real-time agent output stream for a single task.
import { createSignal, For, Show, onCleanup, createEffect, Switch, Match } from "solid-js";
import { taskEvents, sendInput as apiSendInput, finishTask as apiFinishTask, endTask as apiEndTask, pullTask as apiPullTask, pushTask as apiPushTask, reconnectTask as apiReconnectTask, takeoverTask as apiTakeoverTask } from "@sdk/api.gen";
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

    const es = taskEvents(id);

    es.addEventListener("message", (e) => {
      try {
        const msg = JSON.parse(e.data) as AgentMessage;
        setMessages((prev) => [...prev, msg]);
      } catch {
        // Ignore unparseable messages.
      }
    });

    onCleanup(() => es.close());
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
    return s === "running" || s === "starting" || s === "waiting";
  };

  const isWaiting = () => props.taskState === "waiting";

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

  async function reconnectTask() {
    await apiReconnectTask(props.taskId);
  }

  async function takeoverTask() {
    await apiTakeoverTask(props.taskId);
  }

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <h3>Task #{props.taskId}</h3>
        <button class={styles.closeBtn} onClick={() => props.onClose()}>Close</button>
      </div>
      <div class={styles.query}>{props.taskQuery}</div>

      <div class={styles.messageArea}>
        <For each={messages()}>
          {(msg) => <MessageItem msg={msg} />}
        </For>
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
            <button type="button" class={`${styles.btn} ${styles.btnGray}`} onClick={() => reconnectTask()}>
              Reconnect
            </button>
            <button type="button" class={`${styles.btn} ${styles.btnGray}`} onClick={() => takeoverTask()}>
              Takeover
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
          <For each={groupContentBlocks(props.msg.message?.content ?? [])}>
            {(group) => (
              <Show when={group.type === "text"} fallback={
                <ToolUseGroup tools={group.blocks} />
              }>
                <For each={group.blocks}>
                  {(block) => <div class={styles.textBlock}>{block.text}</div>}
                </For>
              </Show>
            )}
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
      <Match when={props.msg.type === "user"}>
        <details class={styles.toolResult}>
          <summary>tool result</summary>
        </details>
      </Match>
    </Switch>
  );
}

interface BlockGroup {
  type: "text" | "tool_use";
  blocks: ContentBlock[];
}

function groupContentBlocks(blocks: ContentBlock[]): BlockGroup[] {
  const groups: BlockGroup[] = [];
  for (const block of blocks) {
    const t = block.type === "text" ? "text" as const : "tool_use" as const;
    const last = groups[groups.length - 1];
    if (last && last.type === t) {
      last.blocks.push(block);
    } else {
      groups.push({ type: t, blocks: [block] });
    }
  }
  return groups;
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

function ToolUseGroup(props: { tools: ContentBlock[] }) {
  return (
    <Show when={props.tools.length > 1} fallback={
      <ToolUseBlock name={props.tools[0].name ?? "tool"} input={props.tools[0].input} />
    }>
      <details class={styles.toolGroup}>
        <summary>
          {props.tools.length} tools: {toolCountSummary(props.tools)}
        </summary>
        <div class={styles.toolGroupInner}>
          <For each={props.tools}>
            {(block) => (
              <ToolUseBlock name={block.name ?? "tool"} input={block.input} />
            )}
          </For>
        </div>
      </details>
    </Show>
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
