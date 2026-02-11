// Sidebar task list with collapsible panel and sorted task cards.
import { Index, Show } from "solid-js";
import type { Accessor } from "solid-js";
import type { TaskJSON } from "@sdk/types.gen";
import TaskItemSummary from "./TaskItemSummary";
import styles from "./TaskList.module.css";

export interface TaskListProps {
  tasks: Accessor<TaskJSON[]>;
  selectedId: string | null;
  sidebarOpen: Accessor<boolean>;
  setSidebarOpen: (open: boolean) => void;
  now: Accessor<number>;
  onSelect: (id: string) => void;
}

export default function TaskList(props: TaskListProps) {
  const isTerminal = (s: string) => s === "failed" || s === "terminated";
  const sorted = () =>
    [...props.tasks()].sort((a, b) => {
      const aT = isTerminal(a.state) ? 1 : 0;
      const bT = isTerminal(b.state) ? 1 : 0;
      if (aT !== bT) return aT - bT;
      return b.id > a.id ? -1 : b.id < a.id ? 1 : 0;
    });

  return (
    <>
      <div class={`${styles.list} ${props.selectedId !== null ? styles.narrow : ""} ${props.sidebarOpen() ? "" : styles.hidden}`}>
        <div class={styles.header}>
          <h2>Tasks</h2>
          <button class={styles.collapseBtn} onClick={() => props.setSidebarOpen(false)} title="Collapse sidebar">&lsaquo;</button>
        </div>
        <Show when={props.tasks().length === 0}>
          <p class={styles.placeholder}>No tasks yet.</p>
        </Show>
        <Index each={sorted()}>
          {(t) => (
            <TaskItemSummary
              task={t().task}
              state={t().state}
              stateUpdatedAt={t().stateUpdatedAt}
              repo={t().repo}
              branch={t().branch}
              model={t().model}
              claudeCodeVersion={t().claudeCodeVersion}
              costUSD={t().costUSD}
              durationMs={t().durationMs}
              numTurns={t().numTurns}
              containerUptimeMs={t().containerUptimeMs}
              error={t().error}
              selected={props.selectedId === t().id}
              now={props.now}
              onClick={() => props.onSelect(t().id)}
            />
          )}
        </Index>
      </div>
      <Show when={!props.sidebarOpen()}>
        <button class={styles.expandBtn} onClick={() => props.setSidebarOpen(true)} title="Expand sidebar">&rsaquo;</button>
      </Show>
    </>
  );
}
