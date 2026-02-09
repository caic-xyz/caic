// Main application component for wmao web UI.
import { createSignal, For, Show, onMount } from "solid-js";
import type { RepoJSON, TaskJSON } from "@sdk/types.gen";
import { listRepos, listTasks, createTask } from "@sdk/api.gen";
import TaskView from "./TaskView";
import styles from "./App.module.css";

export default function App() {
  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<TaskJSON[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [selectedId, setSelectedId] = createSignal<number | null>(null);
  const [repos, setRepos] = createSignal<RepoJSON[]>([]);
  const [selectedRepo, setSelectedRepo] = createSignal("");

  onMount(async () => {
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
    setTasks(await listTasks());
  }

  // Poll for updates.
  setInterval(refreshTasks, 5000);
  refreshTasks();

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
        <div style={{ flex: selectedId() !== null ? "0 0 340px" : "1" }}>
          <h2>Tasks</h2>
          <Show when={tasks().length === 0}>
            <p class={styles.placeholder}>No tasks yet.</p>
          </Show>
          <For each={tasks()}>
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
