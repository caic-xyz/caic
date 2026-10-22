// ProgressPanel incrementally folds root-owned todos above the prompt.

import { For, Show, createEffect, createMemo } from "solid-js";

import type { EventMessage, EventTodo } from "@sdk/types.gen";
import { isChildEvent } from "../grouping";

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
  let processed = 0;
  let lastEvent: EventMessage | null = null;
  const todos = createMemo<EventTodo["todos"]>((previous) => {
    const messages = props.messages;
    const reset = messages.length < processed || (processed > 0 && messages[processed - 1] !== lastEvent);
    let current = reset ? [] : (previous ?? []);
    if (reset) processed = 0;
    for (let i = processed; i < messages.length; i++) {
      const event = messages[i];
      if (!event) throw new Error("Missing timeline event");
      if (event.todo && !isChildEvent(event)) current = event.todo.todos;
    }
    processed = messages.length;
    lastEvent = messages.at(-1) ?? null;
    return current;
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
