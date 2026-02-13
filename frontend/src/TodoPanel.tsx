// TodoPanel renders the agent's current todo list from TodoWrite events.
import { For, Show, createEffect, createMemo } from "solid-js";
import type { EventMessage } from "@sdk/types.gen";
import { detailsOpenState } from "./TaskView";
import styles from "./TodoPanel.module.css";

function statusIcon(status: string): string {
  switch (status) {
    case "completed":
      return "\u2713"; // checkmark
    case "in_progress":
      return "\u25B6"; // play triangle
    default:
      return "\u25CB"; // circle
  }
}

function statusClass(status: string): string {
  switch (status) {
    case "completed":
      return styles.completed;
    case "in_progress":
      return styles.inProgress;
    default:
      return "";
  }
}

const DETAILS_KEY = "todos";

export default function TodoPanel(props: { messages: EventMessage[] }) {
  const todos = createMemo(() => {
    // Find the last "todo" event.
    for (let i = props.messages.length - 1; i >= 0; i--) {
      const todo = props.messages[i].todo;
      if (todo) return todo.todos;
    }
    return [];
  });

  const allDone = createMemo(() => {
    const t = todos();
    return t.length > 0 && t.every((item) => item.status === "completed");
  });

  // Auto-collapse when all todos are completed.
  createEffect(() => {
    if (allDone()) detailsOpenState.set(DETAILS_KEY, false);
  });

  const isOpen = () => detailsOpenState.get(DETAILS_KEY) ?? true;

  return (
    <Show when={todos().length > 0}>
      <details
        class={styles.panel}
        open={isOpen()}
        onToggle={(e) => detailsOpenState.set(DETAILS_KEY, e.currentTarget.open)}
      >
        <summary class={styles.heading}>Todos</summary>
        <For each={todos()}>
          {(item) => (
            <div class={`${styles.item} ${statusClass(item.status)}`}>
              <span class={styles.icon}>{statusIcon(item.status)}</span>
              <span>{item.status === "in_progress" ? item.activeForm : item.content}</span>
            </div>
          )}
        </For>
      </details>
    </Show>
  );
}
