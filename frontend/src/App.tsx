// Main application component for wmao web UI.
import { createSignal, For, Show, onMount } from "solid-js";
import type { RepoJSON, TaskJSON } from "@sdk/types.gen";
import { listRepos, listTasks, createTask } from "@sdk/api.gen";
import TaskView from "./TaskView";

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
    <div style={{ "max-width": "1200px", margin: "0 auto", padding: "2rem", "font-family": "system-ui" }}>
      <h1>wmao</h1>
      <p>Work my ass off. Manage coding agents.</p>

      <form onSubmit={(e) => { e.preventDefault(); submitTask(); }}
        style={{ display: "flex", gap: "0.5rem", "margin-bottom": "2rem", "align-items": "center" }}>
        <select
          value={selectedRepo()}
          onChange={(e) => setSelectedRepo(e.currentTarget.value)}
          disabled={submitting()}
          style={{ padding: "0.5rem" }}
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
          style={{ flex: 1, padding: "0.5rem" }}
        />
        <button type="submit" disabled={submitting() || !prompt().trim() || !selectedRepo()}>
          {submitting() ? "Submitting..." : "Run"}
        </button>
      </form>

      <div style={{ display: "flex", gap: "1.5rem" }}>
        <div style={{ flex: selectedId() !== null ? "0 0 340px" : "1" }}>
          <h2>Tasks</h2>
          <Show when={tasks().length === 0}>
            <p style={{ color: "#888" }}>No tasks yet.</p>
          </Show>
          <For each={tasks()}>
            {(t) => (
              <div
                onClick={() => setSelectedId(t.id)}
                style={{
                  border: selectedId() === t.id ? "2px solid #007bff" : "1px solid #ddd",
                  "border-radius": "6px",
                  padding: "0.75rem", "margin-bottom": "0.5rem", cursor: "pointer",
                }}>
                <div style={{ display: "flex", "justify-content": "space-between" }}>
                  <strong style={{ "font-size": "0.9rem" }}>{t.task}</strong>
                  <span style={{
                    padding: "0.1rem 0.4rem", "border-radius": "4px", "font-size": "0.8rem",
                    background: stateColor(t.state),
                  }}>
                    {t.state}
                  </span>
                </div>
                <Show when={t.repo}>
                  <div style={{ "font-size": "0.75rem", color: "#777" }}>{t.repo}</div>
                </Show>
                <Show when={t.branch}>
                  <div style={{ "font-size": "0.8rem", color: "#555" }}>{t.branch}</div>
                </Show>
                <Show when={t.costUSD > 0}>
                  <span style={{ "font-size": "0.75rem", color: "#888" }}>
                    ${t.costUSD.toFixed(4)} &middot; {(t.durationMs / 1000).toFixed(1)}s
                  </span>
                </Show>
                <Show when={t.error}>
                  <div style={{ color: "red", "font-size": "0.8rem" }}>{t.error}</div>
                </Show>
              </div>
            )}
          </For>
        </div>

        <Show when={selectedId() !== null}>
          <div style={{ flex: 1, "min-width": 0 }}>
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
