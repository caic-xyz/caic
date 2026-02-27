// Sidebar task list with collapsible panel and sorted task cards.
import { Index, Show } from "solid-js";
import type { Accessor } from "solid-js";
import type { Task } from "@sdk/types.gen";
import TaskItemSummary from "./TaskItemSummary";
import styles from "./TaskList.module.css";
import LeftPanelClose from "@material-symbols/svg-400/outlined/left_panel_close.svg?solid";
import LeftPanelOpen from "@material-symbols/svg-400/outlined/left_panel_open.svg?solid";

export interface TaskListProps {
  tasks: Accessor<Task[]>;
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
          <Show when={props.selectedId !== null}>
            <button class={styles.collapseBtn} onClick={() => props.setSidebarOpen(false)} title="Collapse sidebar"><LeftPanelClose width={20} height={20} /></button>
          </Show>
        </div>
        <Show when={props.tasks().length === 0}>
          <p class={styles.placeholder}>No tasks yet.</p>
        </Show>
        <Index each={sorted()}>
          {(t) => (
            <TaskItemSummary
              id={t().id}
              title={t().title}
              state={t().state}
              stateUpdatedAt={t().stateUpdatedAt}
              repo={t().repo}
              repoURL={t().repoURL}
              baseBranch={t().baseBranch}
              branch={t().branch}
              harness={t().harness}
              model={t().model}
              costUSD={t().costUSD}
              duration={t().duration}
              numTurns={t().numTurns}
              activeInputTokens={t().activeInputTokens}
              activeCacheReadTokens={t().activeCacheReadTokens}
              cumulativeInputTokens={t().cumulativeInputTokens}
              cumulativeCacheCreationInputTokens={t().cumulativeCacheCreationInputTokens}
              cumulativeCacheReadInputTokens={t().cumulativeCacheReadInputTokens}
              cumulativeOutputTokens={t().cumulativeOutputTokens}
              contextWindowLimit={t().contextWindowLimit}
              containerUptimeMs={t().containerUptimeMs}
              diffStat={t().diffStat}
              error={t().error}
              inPlanMode={t().inPlanMode}
              tailscale={t().tailscale}
              usb={t().usb}
              display={t().display}
              selected={props.selectedId === t().id}
              now={props.now}
              onClick={() => props.onSelect(t().id)}
            />
          )}
        </Index>
      </div>
      <Show when={!props.sidebarOpen() && props.selectedId !== null}>
        <button class={styles.expandBtn} onClick={() => props.setSidebarOpen(true)} title="Expand sidebar"><LeftPanelOpen width={20} height={20} /></button>
      </Show>
    </>
  );
}
