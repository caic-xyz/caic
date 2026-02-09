// TaskView renders the real-time agent output stream for a single task.
import { createSignal, For, Show, onCleanup, createEffect, Switch, Match } from "solid-js";
import { taskEvents, sendInput as apiSendInput, finishTask as apiFinishTask, endTask as apiEndTask } from "@sdk/api.gen";

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

  async function endTask() {
    await apiEndTask(props.taskId);
  }

  return (
    <div style={{ display: "flex", "flex-direction": "column", height: "100%" }}>
      <div style={{ display: "flex", "justify-content": "space-between", "align-items": "center", "margin-bottom": "0.5rem" }}>
        <h3 style={{ margin: 0 }}>Task #{props.taskId}</h3>
        <button onClick={() => props.onClose()} style={{ cursor: "pointer" }}>Close</button>
      </div>

      <div style={{
        flex: 1, overflow: "auto", border: "1px solid #ddd", "border-radius": "6px",
        padding: "0.75rem", background: "#fafafa", "font-size": "0.85rem",
        "min-height": "200px",
      }}>
        <For each={messages()}>
          {(msg) => <MessageItem msg={msg} />}
        </For>
        <Show when={messages().length === 0}>
          <p style={{ color: "#888" }}>Waiting for agent output...</p>
        </Show>
      </div>

      <Show when={isActive()}>
        <form onSubmit={(e) => { e.preventDefault(); sendInput(); }}
          style={{ display: "flex", gap: "0.5rem", "margin-top": "0.5rem" }}>
          <input
            type="text"
            value={input()}
            onInput={(e) => setInput(e.currentTarget.value)}
            placeholder="Send message to agent..."
            disabled={sending()}
            style={{ flex: 1, padding: "0.4rem" }}
          />
          <button type="submit" disabled={sending() || !input().trim()}>Send</button>
          <Show when={isWaiting()}>
            <button type="button" onClick={() => finishTask()}
              style={{ background: "#28a745", color: "white", border: "none", "border-radius": "4px", padding: "0.4rem 0.75rem", cursor: "pointer" }}>
              Finish
            </button>
          </Show>
          <button type="button" onClick={() => endTask()}
            style={{ background: "#dc3545", color: "white", border: "none", "border-radius": "4px", padding: "0.4rem 0.75rem", cursor: "pointer" }}>
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
        <div style={{ color: "#888", "font-size": "0.8rem", "margin-bottom": "0.5rem" }}>
          Session started &middot; {props.msg.model} &middot; {props.msg.claude_code_version}
        </div>
      </Match>
      <Match when={props.msg.type === "system"}>
        <div style={{ color: "#888", "font-size": "0.8rem", "margin-bottom": "0.25rem" }}>
          [{props.msg.subtype}]
        </div>
      </Match>
      <Match when={props.msg.type === "assistant"}>
        <div style={{ "margin-bottom": "0.5rem" }}>
          <For each={props.msg.message?.content ?? []}>
            {(block) => (
              <Show when={block.type === "text"} fallback={
                <ToolUseBlock name={block.name ?? "tool"} input={block.input} />
              }>
                <div style={{ "white-space": "pre-wrap", "margin-bottom": "0.25rem" }}>{block.text}</div>
              </Show>
            )}
          </For>
        </div>
      </Match>
      <Match when={props.msg.type === "result"}>
        <div style={{
          "margin-top": "0.5rem", padding: "0.5rem", "border-radius": "4px",
          background: props.msg.is_error ? "#f8d7da" : "#d4edda", "font-size": "0.85rem",
        }}>
          <strong>{props.msg.is_error ? "Error" : "Done"}</strong>
          <Show when={props.msg.result}>
            <div style={{ "white-space": "pre-wrap", "margin-top": "0.25rem" }}>{props.msg.result}</div>
          </Show>
          <Show when={props.msg.total_cost_usd}>
            <div style={{ "font-size": "0.8rem", color: "#555", "margin-top": "0.25rem" }}>
              ${props.msg.total_cost_usd?.toFixed(4)} &middot; {((props.msg.duration_ms ?? 0) / 1000).toFixed(1)}s &middot; {props.msg.num_turns} turns
            </div>
          </Show>
        </div>
      </Match>
      <Match when={props.msg.type === "user"}>
        <details style={{ "margin-bottom": "0.25rem", "font-size": "0.8rem", color: "#666" }}>
          <summary style={{ cursor: "pointer" }}>tool result</summary>
        </details>
      </Match>
    </Switch>
  );
}

function ToolUseBlock(props: { name: string; input: unknown }) {
  return (
    <details style={{ "margin-bottom": "0.25rem", background: "#f0f0f0", "border-radius": "4px", padding: "0.25rem 0.5rem" }}>
      <summary style={{ cursor: "pointer", "font-size": "0.85rem" }}>{props.name}</summary>
      <pre style={{ "font-size": "0.75rem", "white-space": "pre-wrap", "max-height": "200px", overflow: "auto" }}>
        {JSON.stringify(props.input, null, 2)}
      </pre>
    </details>
  );
}
