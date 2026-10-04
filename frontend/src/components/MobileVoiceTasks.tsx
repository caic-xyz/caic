// Full-screen numbered voice task links with the conversationally focused task highlighted.

import { For, Show, createEffect, createMemo, type Accessor } from "solid-js";
import { A } from "@solidjs/router";

import type { Task } from "@sdk/types.gen";
import { isInactiveTask } from "../TaskNumberMap";

import { focusedVoiceTask } from "../voiceTaskState";

import styles from "./MobileVoiceTasks.module.css";

export interface MobileVoiceTasksProps {
  tasks: Accessor<Task[]>;
  tasksLoading: Accessor<boolean>;
  settledLoading: Accessor<boolean>;
  getTaskNumber: (id: string) => number | undefined;
}

export default function MobileVoiceTasks(props: MobileVoiceTasksProps) {
  const tasksById = createMemo(() => new Map(props.tasks().map((task) => [task.id, task])));
  const visibleIds = createMemo(() => {
    const active: Array<{ id: string; number: number }> = [];
    for (const task of props.tasks()) {
      if (isInactiveTask(task) && task.id !== focusedVoiceTask()) continue;
      active.push({ id: task.id, number: props.getTaskNumber(task.id) ?? Infinity });
    }
    active.sort((a, b) => a.number - b.number);
    return active.map((task) => task.id);
  });

  return (
    <main class={styles.view} aria-label="Voice tasks" data-testid="mobile-voice-tasks">
      <Show when={visibleIds().length === 0}>
        <p class={styles.empty}>
          {props.tasksLoading() || props.settledLoading()
            ? "Loading…"
            : props.tasks().length === 0
              ? "No tasks yet."
              : "No active tasks."}
        </p>
      </Show>
      <ul class={styles.list}>
        <For each={visibleIds()}>
          {(id) => {
            let link: HTMLAnchorElement | undefined;
            createEffect(() => {
              if (focusedVoiceTask() === id) link?.scrollIntoView({ block: "nearest" });
            });
            const task = () => tasksById().get(id);
            const number = () => props.getTaskNumber(id);
            const stateName = () => (task()?.state === "has_plan" ? "plan ready" : task()?.state);
            return (
              <li class={styles.item}>
                <A
                  ref={link}
                  class={styles.taskLink}
                  aria-current={focusedVoiceTask() === id ? "true" : undefined}
                  href={`/task/@${id}`}
                  data-task-id={id}
                  data-state={task()?.state}
                  aria-label={`Task ${number() ?? "loading"}, ${stateName()}: ${task()?.title}`}
                >
                  <span class={styles.number}>#{number() ?? "…"}</span>
                  <span class={styles.name}>{task()?.title}</span>
                </A>
              </li>
            );
          }}
        </For>
      </ul>
    </main>
  );
}
