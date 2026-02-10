// TaskView renders the real-time agent output stream for a single task.
import { createSignal, createMemo, For, Show, onCleanup, createEffect, Switch, Match } from "solid-js";
import { taskEvents, sendInput as apiSendInput, finishTask as apiFinishTask, endTask as apiEndTask, pullTask as apiPullTask, pushTask as apiPushTask, reconnectTask as apiReconnectTask, takeoverTask as apiTakeoverTask } from "@sdk/api.gen";
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

// A group of consecutive messages that should be rendered together.
// "text" groups contain assistant text blocks.
// "tool" groups coalesce tool_use blocks and their user (tool result) messages.
// "other" groups contain standalone messages (system, result, etc.).
interface MessageGroup {
  kind: "text" | "tool" | "other";
  messages: AgentMessage[];
  toolBlocks: ContentBlock[];
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
        <For each={groupMessages(messages())}>
          {(group) => (
            <Switch>
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

      // Append tool_use blocks to existing tool group or start a new one.
      if (toolBlocks.length > 0) {
        const last = lastGroup();
        if (last && last.kind === "tool") {
          last.messages.push(msg);
          last.toolBlocks.push(...toolBlocks);
        } else {
          groups.push({ kind: "tool", messages: [msg], toolBlocks: [...toolBlocks] });
        }
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

const marked = new Marked({
  breaks: true,
  gfm: true,
});

function Markdown(props: { text: string }) {
  const html = createMemo(() => marked.parse(props.text) as string);
  // eslint-disable-next-line solid/no-innerhtml -- rendering trusted marked output
  return <div class={styles.markdown} innerHTML={html()} />;
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
