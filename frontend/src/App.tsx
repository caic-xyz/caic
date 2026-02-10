// Main application component for wmao web UI.
import { createSignal, createEffect, For, Index, Show, Switch, Match, onMount, onCleanup } from "solid-js";
import type { Accessor } from "solid-js";
import { useNavigate, useLocation } from "@solidjs/router";
import type { RepoJSON, TaskJSON } from "@sdk/types.gen";
import { listRepos, listTasks, createTask } from "@sdk/api.gen";
import TaskView from "./TaskView";
import AutoResizeTextarea from "./AutoResizeTextarea";
import { requestNotificationPermission, notifyWaiting } from "./notifications";
import styles from "./App.module.css";

function taskUrl(t: TaskJSON): string {
  return `/${t.repo}/${t.branch}`;
}

function findTaskByPath(tasks: TaskJSON[], pathname: string): TaskJSON | null {
  if (pathname === "/") return null;
  for (const t of tasks) {
    if (t.repo && t.branch && taskUrl(t) === pathname) return t;
  }
  return null;
}

export default function App() {
  const navigate = useNavigate();
  const location = useLocation();

  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<TaskJSON[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [repos, setRepos] = createSignal<RepoJSON[]>([]);
  const [selectedRepo, setSelectedRepo] = createSignal("");
  const [sidebarOpen, setSidebarOpen] = createSignal(true);

  // After creating a task, it may not have a branch yet. Store the task ID to
  // navigate once the branch appears.
  const [pendingNavId, setPendingNavId] = createSignal<string | null>(null);

  // Track previous task states to detect transitions to "waiting".
  let prevStates = new Map<string, string>();

  // Tick every second for live elapsed-time display.
  const [now, setNow] = createSignal(Date.now());
  {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    onCleanup(() => clearInterval(timer));
  }

  const selectedTask = (): TaskJSON | null => findTaskByPath(tasks(), location.pathname);
  const selectedId = (): string | null => selectedTask()?.id ?? null;

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
  const [connected, setConnected] = createSignal(true);
  {
    let es: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let delay = 500;

    function connect() {
      es = new EventSource("/api/v1/events");
      es.addEventListener("open", () => {
        setConnected(true);
        delay = 500;
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

  // Navigate to a newly created task once its branch becomes available.
  createEffect(() => {
    const id = pendingNavId();
    if (id === null) return;
    const found = tasks().find((item) => item.id === id);
    if (found && found.branch) {
      setPendingNavId(null);
      navigate(taskUrl(found));
    }
  });

  async function submitTask() {
    const p = prompt().trim();
    const repo = selectedRepo();
    if (!p || !repo) return;
    setSubmitting(true);
    localStorage.setItem("wmao:lastRepo", repo);
    try {
      const data = await createTask({ prompt: p, repo });
      setPrompt("");
      setPendingNavId(data.id);
    } finally {
      setSubmitting(false);
    }
  }

  // Most recent first; terminal tasks last.
  const isTerminal = (s: string) => s === "failed" || s === "terminated";
  const sortedTasks = () =>
    [...tasks()].sort((a, b) => {
      const aT = isTerminal(a.state) ? 1 : 0;
      const bT = isTerminal(b.state) ? 1 : 0;
      if (aT !== bT) return aT - bT;
      return b.id > a.id ? -1 : b.id < a.id ? 1 : 0;
    });

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
        <button type="submit" disabled={submitting() || !prompt().trim() || !selectedRepo()}>
          {submitting() ? "Submitting..." : "Run"}
        </button>
      </form>

      <div class={styles.layout}>
        <div class={`${styles.taskList} ${selectedId() !== null ? styles.taskListNarrow : ""} ${sidebarOpen() ? "" : styles.taskListHidden}`}>
          <div class={styles.taskListHeader}>
            <h2>Tasks</h2>
            <button class={styles.collapseBtn} onClick={() => setSidebarOpen(false)} title="Collapse sidebar">&lsaquo;</button>
          </div>
          <Show when={tasks().length === 0}>
            <p class={styles.placeholder}>No tasks yet.</p>
          </Show>
          <Index each={sortedTasks()}>
            {(t) => (
              <div
                onClick={() => {
                  if (t().branch) {
                    navigate(taskUrl(t()));
                  }
                }}
                class={`${styles.taskCard} ${selectedId() === t().id ? styles.taskCardSelected : ""}`}>
                <div class={styles.taskHeader}>
                  <strong class={styles.taskTitle}>{t().task}</strong>
                  <span class={styles.stateGroup}>
                    <span class={styles.stateBadge} style={{ background: stateColor(t().state) }}>
                      {t().state}
                    </span>
                    <Show when={t().stateUpdatedAtMs > 0}>
                      <StateDuration stateUpdatedAtMs={t().stateUpdatedAtMs} now={now} />
                    </Show>
                  </span>
                </div>
                <Show when={t().repo}>
                  <div class={styles.repoLabel}>{t().repo}</div>
                </Show>
                <Show when={t().branch}>
                  <div class={styles.branchLabel}>{t().branch}</div>
                </Show>
                <Show when={t().costUSD > 0}>
                  <span class={styles.costLabel}>
                    ${t().costUSD.toFixed(4)} &middot; {(t().durationMs / 1000).toFixed(1)}s
                  </span>
                </Show>
                <Show when={t().error}>
                  <div class={styles.errorLabel}>{t().error}</div>
                </Show>
              </div>
            )}
          </Index>
        </div>
        <Show when={!sidebarOpen()}>
          <button class={styles.expandBtn} onClick={() => setSidebarOpen(true)} title="Expand sidebar">&rsaquo;</button>
        </Show>

        <Switch>
          <Match when={location.pathname !== "/" && selectedTask() === null && tasks().length > 0}>
            <div class={styles.detailPane}>
              <button class={styles.backBtn} onClick={() => navigate("/")}>&larr; Back</button>
              <p class={styles.placeholder}>Task not found.</p>
            </div>
          </Match>
          <Match when={selectedId() !== null}>
            <div class={styles.detailPane}>
              <button class={styles.backBtn} onClick={() => navigate("/")}>&larr; Back</button>
              <TaskView
                taskId={selectedId() ?? ""}
                taskState={selectedTask()?.state ?? "pending"}
                taskQuery={selectedTask()?.task ?? ""}
                onClose={() => navigate("/")}
              />
            </div>
          </Match>
        </Switch>
      </div>
    </div>
  );
}

function formatElapsed(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

function StateDuration(props: { stateUpdatedAtMs: number; now: Accessor<number> }) {
  const elapsed = () => Math.max(0, props.now() - props.stateUpdatedAtMs);
  return <span class={styles.durationLabel}>{formatElapsed(elapsed())}</span>;
}

function stateColor(state: string): string {
  switch (state) {
    case "running":
      return "#d4edda";
    case "asking":
      return "#cce5ff";
    case "failed":
      return "#f8d7da";
    case "terminated":
      return "#e2e3e5";
    default:
      return "#fff3cd";
  }
}
