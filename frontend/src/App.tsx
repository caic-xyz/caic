// Main application component for caic web UI.
import { createEffect, createSignal, For, Show, Switch, Match, onMount, onCleanup } from "solid-js";
import { useNavigate, useLocation } from "@solidjs/router";
import type { RepoJSON, TaskJSON, UsageResp } from "@sdk/types.gen";
import { listRepos, listTasks, createTask, getUsage } from "@sdk/api.gen";
import TaskView from "./TaskView";
import TaskList from "./TaskList";
import AutoResizeTextarea from "./AutoResizeTextarea";
import Button from "./Button";
import { requestNotificationPermission, notifyWaiting } from "./notifications";
import styles from "./App.module.css";

/** Max slug length in the URL (characters after the "+"). */
const MAX_SLUG = 80;

/** Build a URL-safe slug from arbitrary text: lowercase, non-alnum replaced with "-", collapsed. */
function slugify(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
}

/** Build the path portion for a task URL: /task/@{id}+{slug}. */
function taskPath(id: string, repo: string, branch: string, query: string): string {
  const repoName = repo.split("/").pop() ?? repo;
  const parts = [repoName, branch, query].filter(Boolean).map(slugify).join("-");
  const slug = parts.slice(0, MAX_SLUG).replace(/-$/, "");
  return `/task/@${id}+${slug}`;
}

/** Extract the task ID from a /task/@{id}+{slug} pathname, or null. */
function taskIdFromPath(pathname: string): string | null {
  const prefix = "/task/@";
  if (!pathname.startsWith(prefix)) return null;
  const rest = pathname.slice(prefix.length);
  const plus = rest.indexOf("+");
  return plus === -1 ? rest : rest.slice(0, plus);
}

function formatReset(iso: string): string {
  const d = new Date(iso);
  const now = Date.now();
  const diffMs = d.getTime() - now;
  if (diffMs <= 0) return "now";
  const hours = Math.floor(diffMs / 3_600_000);
  const mins = Math.floor((diffMs % 3_600_000) / 60_000);
  if (hours >= 24) {
    const days = Math.floor(hours / 24);
    return `in ${days}d ${hours % 24}h`;
  }
  if (hours > 0) return `in ${hours}h ${mins}m`;
  return `in ${mins}m`;
}

function UsageBadge(props: { label: string; utilization: number; resetsAt: string }) {
  const pct = () => Math.round(props.utilization);
  const cls = () => (pct() >= 80 ? styles.usageRed : pct() >= 50 ? styles.usageYellow : styles.usageGreen);
  return (
    <span class={`${styles.usageBadge} ${cls()}`} title={`Resets ${formatReset(props.resetsAt)}`}>
      {props.label}: {pct()}%
    </span>
  );
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
  const [usage, setUsage] = createSignal<UsageResp | null>(null);

  // Per-task input drafts survive task switching.
  const [inputDrafts, setInputDrafts] = createSignal<Map<string, string>>(new Map());

  // Track previous task states to detect transitions to "waiting".
  let prevStates = new Map<string, string>();

  // Tick every second for live elapsed-time display.
  const [now, setNow] = createSignal(Date.now());
  {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    onCleanup(() => clearInterval(timer));
  }

  const selectedId = (): string | null => taskIdFromPath(location.pathname);
  const selectedTask = (): TaskJSON | null => {
    const id = selectedId();
    return id !== null ? (tasks().find((t) => t.id === id) ?? null) : null;
  };

  // Re-open sidebar when task view is closed while sidebar is collapsed.
  createEffect(() => {
    if (selectedId() === null) setSidebarOpen(true);
  });

  onMount(async () => {
    requestNotificationPermission();
    const data = await listRepos();
    setRepos(data);
    if (data.length > 0) {
      const last = localStorage.getItem("caic:lastRepo");
      const match = last && data.find((r) => r.path === last);
      setSelectedRepo(match ? match.path : data[0].path);
    }
    getUsage().then(setUsage).catch(() => {});
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
      es.addEventListener("usage", (e) => {
        try {
          setUsage(JSON.parse(e.data) as UsageResp);
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
    localStorage.setItem("caic:lastRepo", repo);
    try {
      const data = await createTask({ prompt: p, repo });
      setPrompt("");
      navigate(taskPath(data.id, repo, "", p));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div class={styles.app}>
      <div class={`${styles.titleRow} ${selectedId() ? styles.hideOnMobile : ""}`}>
        <h1 class={styles.title}>caic</h1>
        <span class={styles.subtitle}>Coding Agents in Containers</span>
        <Show when={usage()} keyed>
          {(u) => (
            <span class={styles.usageRow}>
              <Show when={u.fiveHour} keyed>
                {(w) => <UsageBadge label="5h" utilization={w.utilization} resetsAt={w.resetsAt} />}
              </Show>
              <Show when={u.sevenDay} keyed>
                {(w) => <UsageBadge label="Weekly" utilization={w.utilization} resetsAt={w.resetsAt} />}
              </Show>
            </span>
          )}
        </Show>
      </div>

      <Show when={!connected()}>
        <div class={styles.reconnecting}>Reconnecting to server...</div>
      </Show>

      <form onSubmit={(e) => { e.preventDefault(); submitTask(); }} class={`${styles.submitForm} ${selectedId() ? styles.hideOnMobile : ""}`}>
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
          onSelect={(id) => {
            const found = tasks().find((t) => t.id === id);
            navigate(found ? taskPath(found.id, found.repo, found.branch, found.task) : `/task/@${id}`);
          }}
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
                  onClose={() => navigate("/")}
                  inputDraft={inputDrafts().get(id) ?? ""}
                  onInputDraft={(v) => setInputDrafts((prev) => { const next = new Map(prev); next.set(id, v); return next; })}
                />
              </div>
            )}
          </Match>
        </Switch>
      </div>
    </div>
  );
}
