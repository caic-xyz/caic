// TodoPanel renders the agent's current todo list from TodoWrite events.
import { For, Show, createMemo } from "solid-js";
import type { EventMessage } from "@sdk/types.gen";
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

export default function TodoPanel(props: { messages: EventMessage[] }) {
  const todos = createMemo(() => {
    // Find the last "todo" event.
    for (let i = props.messages.length - 1; i >= 0; i--) {
      const todo = props.messages[i].todo;
      if (todo) return todo.todos;
    }
    return [];
  });

  return (
    <Show when={todos().length > 0}>
      <div class={styles.panel}>
        <h4 class={styles.heading}>Todos</h4>
        <For each={todos()}>
          {(item) => (
            <div class={`${styles.item} ${statusClass(item.status)}`}>
              <span class={styles.icon}>{statusIcon(item.status)}</span>
              <span>{item.status === "in_progress" ? item.activeForm : item.content}</span>
            </div>
          )}
        </For>
      </div>
    </Show>
  );
}
