// Main application component for wmao web UI.
import { createSignal, For, Show, onMount } from "solid-js";
import TaskView from "./TaskView";

interface RepoInfo {
  path: string;
  baseBranch: string;
}

interface TaskResult {
  id: number;
  task: string;
  repo: string;
  branch: string;
  state: string;
  diffStat: string;
  costUSD: number;
  durationMs: number;
  numTurns: number;
  error?: string;
}

export default function App() {
  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<TaskResult[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [selectedId, setSelectedId] = createSignal<number | null>(null);
  const [repos, setRepos] = createSignal<RepoInfo[]>([]);
  const [selectedRepo, setSelectedRepo] = createSignal("");

  onMount(async () => {
    const res = await fetch("/api/v1/repos");
    if (res.ok) {
      const data = (await res.json()) as RepoInfo[];
      setRepos(data);
      if (data.length > 0) {
        setSelectedRepo(data[0].path);
      }
    }
  });

  async function submitTask() {
    const p = prompt().trim();
    const repo = selectedRepo();
    if (!p || !repo) return;
    setSubmitting(true);
    try {
      const res = await fetch("/api/v1/tasks", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ prompt: p, repo }),
      });
      if (!res.ok) {
        const body = await res.json() as { error?: { message?: string } };
        throw new Error(body.error?.message ?? "request failed");
      }
      const data = await res.json() as { id: number };
      setPrompt("");
      await refreshTasks();
      setSelectedId(data.id);
    } finally {
      setSubmitting(false);
    }
  }

  async function refreshTasks() {
    const res = await fetch("/api/v1/tasks");
    if (res.ok) {
      setTasks(await res.json() as TaskResult[]);
    }
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
