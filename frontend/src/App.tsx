// Main application component for wmao web UI.
import { createSignal, For, Show, onMount } from "solid-js";
import type { RepoJSON, TaskJSON } from "@sdk/types.gen";
import { listRepos, listTasks, createTask } from "@sdk/api.gen";
import TaskView from "./TaskView";
import { requestNotificationPermission, notifyWaiting } from "./notifications";
import styles from "./App.module.css";

export default function App() {
  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<TaskJSON[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [selectedId, setSelectedId] = createSignal<number | null>(null);
  const [repos, setRepos] = createSignal<RepoJSON[]>([]);
  const [selectedRepo, setSelectedRepo] = createSignal("");

  // Track previous task states to detect transitions to "waiting".
  let prevStates = new Map<number, string>();

  onMount(async () => {
    requestNotificationPermission();
    const data = await listRepos();
    setRepos(data);
    if (data.length > 0) {
      setSelectedRepo(data[0].path);
    }
  });

  async function submitTask() {
    const p = prompt().trim();
    const repo = selectedRepo();
    if (!p || !repo) return;
    setSubmitting(true);
    try {
      const data = await createTask({ prompt: p, repo });
      setPrompt("");
      await refreshTasks();
      setSelectedId(data.id);
    } finally {
      setSubmitting(false);
    }
  }

  async function refreshTasks() {
    const updated = await listTasks();
    for (const t of updated) {
      if (t.state === "waiting" && prevStates.get(t.id) !== "waiting") {
        notifyWaiting(t.id, t.task);
      }
    }
    prevStates = new Map(updated.map((t) => [t.id, t.state]));
    setTasks(updated);
  }

  // Poll for updates.
  setInterval(refreshTasks, 5000);
  refreshTasks();

  // Most recent first; adopted tasks last.
  const sortedTasks = () =>
    [...tasks()].sort((a, b) => {
      const aAdopted = a.task.startsWith("(adopted)") ? 1 : 0;
      const bAdopted = b.task.startsWith("(adopted)") ? 1 : 0;
      if (aAdopted !== bAdopted) return aAdopted - bAdopted;
      return b.id - a.id;
    });

  const selectedTask = () => {
    const id = selectedId();
    if (id === null) return null;
    return tasks().find((t) => t.id === id) ?? null;
  };

  return (
    <div class={styles.app}>
      <h1>wmao</h1>
      <p>Work my ass off. Manage coding agents.</p>

      <form onSubmit={(e) => { e.preventDefault(); submitTask(); }} class={styles.submitForm}>
        <select
          value={selectedRepo()}
          onChange={(e) => setSelectedRepo(e.currentTarget.value)}
          disabled={submitting()}
          class={styles.repoSelect}
        >
          <For each={repos()}>
            {(r) => <option value={r.path}>{r.path}</option>}
          </For>
        </select>
        <input
          type="text"
          value={prompt()}
          onInput={(e) => setPrompt(e.currentTarget.value)}
          placeholder="Describe a task..."
          disabled={submitting()}
          class={styles.promptInput}
        />
        <button type="submit" disabled={submitting() || !prompt().trim() || !selectedRepo()}>
          {submitting() ? "Submitting..." : "Run"}
        </button>
      </form>

      <div class={styles.layout}>
        <div class={`${styles.taskList} ${selectedId() !== null ? styles.taskListCollapsed : ""}`}>
          <h2>Tasks</h2>
          <Show when={tasks().length === 0}>
            <p class={styles.placeholder}>No tasks yet.</p>
          </Show>
          <For each={sortedTasks()}>
            {(t) => (
              <div
                onClick={() => setSelectedId(t.id)}
                class={`${styles.taskCard} ${selectedId() === t.id ? styles.taskCardSelected : ""}`}>
                <div class={styles.taskHeader}>
                  <strong class={styles.taskTitle}>{t.task}</strong>
                  <span class={styles.stateBadge} style={{ background: stateColor(t.state) }}>
                    {t.state}
                  </span>
                </div>
                <Show when={t.repo}>
                  <div class={styles.repoLabel}>{t.repo}</div>
                </Show>
                <Show when={t.branch}>
                  <div class={styles.branchLabel}>{t.branch}</div>
                </Show>
                <Show when={t.costUSD > 0}>
                  <span class={styles.costLabel}>
                    ${t.costUSD.toFixed(4)} &middot; {(t.durationMs / 1000).toFixed(1)}s
                  </span>
                </Show>
                <Show when={t.error}>
                  <div class={styles.errorLabel}>{t.error}</div>
                </Show>
              </div>
            )}
          </For>
        </div>

        <Show when={selectedId() !== null}>
          <div class={styles.detailPane}>
            <button class={styles.backBtn} onClick={() => setSelectedId(null)}>&larr; Back</button>
            <TaskView
              taskId={selectedId() ?? 0}
              taskState={selectedTask()?.state ?? "pending"}
              onClose={() => setSelectedId(null)}
            />
          </div>
        </Show>
      </div>
    </div>
  );
}

function stateColor(state: string): string {
  switch (state) {
    case "done": return "#d4edda";
    case "failed": return "#f8d7da";
    case "ended": return "#e2e3e5";
    default: return "#fff3cd";
  }
}
