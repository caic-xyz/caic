// Sidebar task list with collapsible panel, grouped by repo for active tasks.
import { For, Index, Show } from "solid-js";
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
  onTerminate: (id: string) => void;
  terminatingId: Accessor<string | null>;
  onDiffClick?: (id: string) => void;
}

const naturalCompare = (a: string, b: string) =>
  a.localeCompare(b, undefined, { numeric: true, sensitivity: "base" });

/** Sort active tasks by repo then branch; terminal tasks by ID descending. */
export function sortTasks(tasks: Task[]): Task[] {
  const isTerminal = (s: string) => s === "failed" || s === "terminated";
  const active = tasks.filter((t) => !isTerminal(t.state));
  const terminal = tasks.filter((t) => isTerminal(t.state));
  active.sort((a, b) => {
    const rc = naturalCompare(a.repo, b.repo);
    if (rc !== 0) return rc;
    return naturalCompare(a.branch, b.branch);
  });
  terminal.sort((a, b) => (b.id > a.id ? -1 : b.id < a.id ? 1 : 0));
  return [...active, ...terminal];
}

interface RepoGroup {
  repo: string;
  tasks: Task[];
}

export default function TaskList(props: TaskListProps) {
  const isTerminal = (s: string) => s === "failed" || s === "terminated";

  const grouped = () => {
    const all = [...props.tasks()];
    const active = all.filter((t) => !isTerminal(t.state));
    const terminal = all.filter((t) => isTerminal(t.state));
    active.sort((a, b) => {
      const rc = naturalCompare(a.repo, b.repo);
      if (rc !== 0) return rc;
      return naturalCompare(a.branch, b.branch);
    });
    const groups: RepoGroup[] = [];
    for (const t of active) {
      const last = groups[groups.length - 1];
      if (last && last.repo === t.repo) {
        last.tasks.push(t);
      } else {
        groups.push({ repo: t.repo, tasks: [t] });
      }
    }
    terminal.sort((a, b) => (b.id > a.id ? -1 : b.id < a.id ? 1 : 0));
    return { groups, terminal };
  };

  const renderTask = (t: () => Task) => (
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
      startedAt={t().startedAt}
      diffStat={t().diffStat}
      error={t().error}
      inPlanMode={t().inPlanMode}
      tailscale={t().tailscale}
      usb={t().usb}
      display={t().display}
      selected={props.selectedId === t().id}
      now={props.now}
      onClick={() => props.onSelect(t().id)}
      onTerminate={() => props.onTerminate(t().id)}
      terminateLoading={props.terminatingId() === t().id}
      onDiffClick={props.onDiffClick ? () => { const fn = props.onDiffClick; if (fn) fn(t().id); } : undefined}
    />
  );

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
        <For each={grouped().groups}>
          {(group) => (
            <div class={styles.repoGroup}>
              <div class={styles.repoGroupHeader}>{group.repo}</div>
              <Index each={group.tasks}>{renderTask}</Index>
            </div>
          )}
        </For>
        <Index each={grouped().terminal}>{renderTask}</Index>
      </div>
      <Show when={!props.sidebarOpen() && props.selectedId !== null}>
        <button class={styles.expandBtn} onClick={() => props.setSidebarOpen(true)} title="Expand sidebar"><LeftPanelOpen width={20} height={20} /></button>
      </Show>
    </>
  );
}
