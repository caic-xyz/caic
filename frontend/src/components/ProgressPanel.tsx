// ProgressPanel renders todos above the prompt; subagents render inline in the transcript.

import { For, Show, createEffect, createMemo } from "solid-js";

import type { EventMessage } from "@sdk/types.gen";

import { detailsOpenState } from "./TaskDetail";
import styles from "./ProgressPanel.module.css";

function todoStatusIcon(status: string): string {
  switch (status) {
    case "completed":
      return "\u2713"; // checkmark
    case "in_progress":
      return "\u25B6"; // play triangle
    default:
      return "\u25CB"; // circle
  }
}

function todoStatusClass(status: string): string {
  switch (status) {
    case "completed":
      return styles.completed;
    case "in_progress":
      return styles.inProgress;
    default:
      return "";
  }
}

const DETAILS_KEY = "progress";

export default function ProgressPanel(props: { messages: EventMessage[] }) {
  const todos = createMemo(() => {
    for (let i = props.messages.length - 1; i >= 0; i--) {
      const todo = props.messages[i].todo;
      if (todo) return todo.todos;
    }
    return [];
  });

  const completedCount = createMemo(() => todos().filter((item) => item.status === "completed").length);

  // Auto-collapse when all todos done and no active agents.
  createEffect(() => {
    const t = todos();
    if (t.length > 0 && t.every((item) => item.status === "completed")) {
      detailsOpenState.set(DETAILS_KEY, false);
    }
  });

  const isOpen = () => detailsOpenState.get(DETAILS_KEY) ?? false;

  const title = createMemo(() => {
    const t = todos();
    return `Todos ${completedCount()}/${t.length}`;
  });

  return (
    <Show when={todos().length > 0}>
      <details
        class={styles.panel}
        open={isOpen()}
        onToggle={(e) => detailsOpenState.set(DETAILS_KEY, e.currentTarget.open)}
      >
        <summary class={styles.heading}>{title()}</summary>
        <For each={todos()}>
          {(item) => (
            <div class={`${styles.item} ${todoStatusClass(item.status)}`}>
              <span class={styles.icon}>{todoStatusIcon(item.status)}</span>
              <span>{item.status === "in_progress" ? item.activeForm : item.content}</span>
            </div>
          )}
        </For>
      </details>
    </Show>
  );
}
