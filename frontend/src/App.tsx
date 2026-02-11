// Main application component for wmao web UI.
import { createSignal, For, Show, Switch, Match, onMount, onCleanup } from "solid-js";
import { useNavigate, useLocation } from "@solidjs/router";
import type { RepoJSON, TaskJSON } from "@sdk/types.gen";
import { listRepos, listTasks, createTask } from "@sdk/api.gen";
import TaskView from "./TaskView";
import TaskList from "./TaskList";
import AutoResizeTextarea from "./AutoResizeTextarea";
import Button from "./Button";
import { requestNotificationPermission, notifyWaiting } from "./notifications";
import styles from "./App.module.css";

export default function App() {
  const navigate = useNavigate();
  const location = useLocation();

  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<TaskJSON[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [repos, setRepos] = createSignal<RepoJSON[]>([]);
  const [selectedRepo, setSelectedRepo] = createSignal("");
  const [sidebarOpen, setSidebarOpen] = createSignal(true);

  // Track previous task states to detect transitions to "waiting".
  let prevStates = new Map<string, string>();

  // Tick every second for live elapsed-time display.
  const [now, setNow] = createSignal(Date.now());
  {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    onCleanup(() => clearInterval(timer));
  }

  const selectedId = (): string | null => {
    const prefix = "/task/";
    return location.pathname.startsWith(prefix) ? location.pathname.slice(prefix.length) : null;
  };
  const selectedTask = (): TaskJSON | null => {
    const id = selectedId();
    return id !== null ? (tasks().find((t) => t.id === id) ?? null) : null;
  };

  onMount(async () => {
    requestNotificationPermission();
    const data = await listRepos();
    setRepos(data);
    if (data.length > 0) {
      const last = localStorage.getItem("wmao:lastRepo");
      const match = last && data.find((r) => r.path === last);
      setSelectedRepo(match ? match.path : data[0].path);
    }
  });

  // Subscribe to task list updates via SSE with automatic reconnection.
  // Backoff: 500ms × 1.5 each failure, capped at 4s, reset on success.
  // On reconnect, check if the frontend was rebuilt and reload if so.
  const [connected, setConnected] = createSignal(true);
  {
    let es: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let delay = 500;
    const initialScriptSrc = document.querySelector<HTMLScriptElement>("script[src^='/assets/']")?.src ?? "";

    function connect() {
      es = new EventSource("/api/v1/events");
      es.addEventListener("open", () => {
        setConnected(true);
        delay = 500;
        // Check if frontend was rebuilt while disconnected.
        fetch("/index.html")
          .then((r) => r.text())
          .then((html) => {
            const m = html.match(/<script[^>]+src="([^"]*\/assets\/[^"]+)"/);
            if (m && initialScriptSrc && !initialScriptSrc.endsWith(m[1])) {
              window.location.reload();
            }
          })
          .catch(() => {});
        listTasks().then(setTasks).catch(() => {});
      });
      es.addEventListener("tasks", (e) => {
        try {
          const updated = JSON.parse(e.data) as TaskJSON[];
          for (const t of updated) {
            const needsInput = t.state === "waiting" || t.state === "asking";
            const prevNeedsInput = prevStates.get(t.id) === "waiting" || prevStates.get(t.id) === "asking";
            if (needsInput && !prevNeedsInput) {
              notifyWaiting(t.id, t.task);
            }
          }
          prevStates = new Map(updated.map((t) => [t.id, t.state]));
          setTasks(updated);
        } catch {
          // Ignore unparseable messages.
        }
      });
      es.onerror = () => {
        setConnected(false);
        es?.close();
        es = null;
        timer = setTimeout(connect, delay);
        delay = Math.min(delay * 1.5, 4000);
      };
    }

    connect();

    onCleanup(() => {
      es?.close();
      if (timer !== null) clearTimeout(timer);
    });
  }

  async function submitTask() {
    const p = prompt().trim();
    const repo = selectedRepo();
    if (!p || !repo) return;
    setSubmitting(true);
    localStorage.setItem("wmao:lastRepo", repo);
    try {
      const data = await createTask({ prompt: p, repo });
      setPrompt("");
      navigate(`/task/${data.id}`);
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div class={styles.app}>
      <div class={styles.titleRow}>
        <h1 class={styles.title}>wmao</h1>
        <span class={styles.subtitle}>Work my ass off. Manage coding agents.</span>
      </div>

      <Show when={!connected()}>
        <div class={styles.reconnecting}>Reconnecting to server...</div>
      </Show>

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
        <AutoResizeTextarea
          value={prompt()}
          onInput={setPrompt}
          onSubmit={submitTask}
          placeholder="Describe a task..."
          disabled={submitting()}
          class={styles.promptInput}
        />
        <Button type="submit" disabled={submitting() || !prompt().trim() || !selectedRepo()}>
          {submitting() ? "Submitting..." : "Run"}
        </Button>
      </form>

      <div class={styles.layout}>
        <TaskList
          tasks={tasks}
          selectedId={selectedId()}
          sidebarOpen={sidebarOpen}
          setSidebarOpen={setSidebarOpen}
          now={now}
          onSelect={(id) => navigate(`/task/${id}`)}
        />

        <Switch>
          <Match when={location.pathname !== "/" && selectedTask() === null && tasks().length > 0}>
            <div class={styles.detailPane}>
              <button class={styles.backBtn} onClick={() => navigate("/")}>&larr; Back</button>
              <p class={styles.placeholder}>Task not found.</p>
            </div>
          </Match>
          <Match when={selectedId()} keyed>
            {(id) => (
              <div class={styles.detailPane}>
                <button class={styles.backBtn} onClick={() => navigate("/")}>&larr; Back</button>
                <TaskView
                  taskId={id}
                  taskState={selectedTask()?.state ?? "pending"}
                  taskQuery={selectedTask()?.task ?? ""}
                  onClose={() => navigate("/")}
                />
              </div>
            )}
          </Match>
        </Switch>
      </div>
    </div>
  );
}
