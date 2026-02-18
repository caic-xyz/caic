// Main application component for caic web UI.
import { createEffect, createSignal, For, Show, Switch, Match, onMount, onCleanup } from "solid-js";
import { useNavigate, useLocation } from "@solidjs/router";
import type { HarnessJSON, RepoJSON, TaskJSON, UsageResp } from "@sdk/types.gen";
import { getConfig, listHarnesses, listRepos, listTasks, createTask, getUsage } from "@sdk/api.gen";
import TaskView from "./TaskView";
import TaskList from "./TaskList";
import AutoResizeTextarea from "./AutoResizeTextarea";
import Button from "./Button";
import { requestNotificationPermission, notifyWaiting } from "./notifications";
import UsageBadges from "./UsageBadges";
import SendIcon from "@material-symbols/svg-400/outlined/send.svg?solid";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import TailscaleIcon from "./tailscale.svg?solid";
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

export default function App() {
  const navigate = useNavigate();
  const location = useLocation();

  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<TaskJSON[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [repos, setRepos] = createSignal<RepoJSON[]>([]);
  const [selectedRepo, setSelectedRepo] = createSignal("");
  const [selectedModel, setSelectedModel] = createSignal("");
  const [selectedImage, setSelectedImage] = createSignal("");
  const [harnesses, setHarnesses] = createSignal<HarnessJSON[]>([]);
  const [selectedHarness, setSelectedHarness] = createSignal("claude");
  const [sidebarOpen, setSidebarOpen] = createSignal(true);
  const [usage, setUsage] = createSignal<UsageResp | null>(null);
  const [tailscaleAvailable, setTailscaleAvailable] = createSignal(false);
  const [tailscaleEnabled, setTailscaleEnabled] = createSignal(false);
  const [usbAvailable, setUSBAvailable] = createSignal(false);
  const [usbEnabled, setUSBEnabled] = createSignal(false);
  const [displayAvailable, setDisplayAvailable] = createSignal(false);
  const [displayEnabled, setDisplayEnabled] = createSignal(false);

  // Per-task input drafts survive task switching.
  const [inputDrafts, setInputDrafts] = createSignal<Map<string, string>>(new Map());

  // Ref to the main prompt textarea for focusing after Escape.
  let promptRef: HTMLTextAreaElement | undefined;

  // Sort tasks the same way TaskList does: active first, then by ID descending.
  const isTerminal = (s: string) => s === "failed" || s === "terminated";
  const sortedTasks = () =>
    [...tasks()].sort((a, b) => {
      const aT = isTerminal(a.state) ? 1 : 0;
      const bT = isTerminal(b.state) ? 1 : 0;
      if (aT !== bT) return aT - bT;
      return b.id > a.id ? -1 : b.id < a.id ? 1 : 0;
    });

  // Global keyboard shortcuts:
  // - Escape: dismiss task view, focus prompt
  // - ArrowUp/ArrowDown: switch to previous/next task in sidebar order
  {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && selectedId() !== null) {
        navigate("/");
        promptRef?.focus();
        return;
      }
      if (e.key !== "ArrowUp" && e.key !== "ArrowDown") return;
      // Don't intercept when typing in an input/textarea.
      const tag = (e.target as HTMLElement)?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
      const list = sortedTasks();
      if (list.length === 0) return;
      const curIdx = list.findIndex((task) => task.id === selectedId());
      let nextIdx: number;
      if (e.key === "ArrowUp") {
        nextIdx = curIdx <= 0 ? list.length - 1 : curIdx - 1;
      } else {
        nextIdx = curIdx === -1 || curIdx >= list.length - 1 ? 0 : curIdx + 1;
      }
      const next = list[nextIdx];
      navigate(taskPath(next.id, next.repo, next.branch, next.task));
      const card = document.querySelector<HTMLElement>(`[data-task-id="${next.id}"]`);
      card?.focus();
      e.preventDefault();
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  }

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

  // Redirect to home when a task URL points to a non-existent task.
  // Guard on connected() to avoid spurious redirects during reconnection.
  createEffect(() => {
    if (connected() && selectedId() !== null && tasks().length > 0 && selectedTask() === null) {
      navigate("/", { replace: true });
    }
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
    {
      const current = selectedHarness();
      listHarnesses().then((h) => {
        setHarnesses(h);
        // If the default harness isn't in the list (e.g. fake mode), select the first.
        if (h.length > 0 && !h.find((x) => x.name === current)) {
          setSelectedHarness(h[0].name);
        }
      }).catch(() => {});
    }
    getConfig().then((c) => {
      setTailscaleAvailable(c.tailscaleAvailable);
      setUSBAvailable(c.usbAvailable);
      setDisplayAvailable(c.displayAvailable);
    }).catch(() => {});
    getUsage().then(setUsage).catch(() => {});
  });

  // Subscribe to task list updates via SSE with automatic reconnection.
  // Backoff: 500ms × 1.5 each failure, capped at 4s, reset on success.
  // On reconnect, check if the frontend was rebuilt and reload if so.
  const [connected, setConnected] = createSignal(true);
  {
    let es: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let bannerTimer: ReturnType<typeof setTimeout> | null = null;
    let delay = 500;
    const initialScriptSrc = document.querySelector<HTMLScriptElement>("script[src^='/assets/']")?.src ?? "";

    function connect() {
      es = new EventSource("/api/v1/events");
      es.addEventListener("open", () => {
        if (bannerTimer !== null) { clearTimeout(bannerTimer); bannerTimer = null; }
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
        es?.close();
        es = null;
        // Delay showing the banner so brief disconnects don't flash it.
        if (bannerTimer === null) {
          bannerTimer = setTimeout(() => { bannerTimer = null; setConnected(false); }, 2000);
        }
        timer = setTimeout(connect, delay);
        delay = Math.min(delay * 1.5, 4000);
      };
    }

    connect();

    onCleanup(() => {
      es?.close();
      if (timer !== null) clearTimeout(timer);
      if (bannerTimer !== null) clearTimeout(bannerTimer);
    });
  }

  async function submitTask() {
    const p = prompt().trim();
    const repo = selectedRepo();
    if (!p || !repo) return;
    setSubmitting(true);
    localStorage.setItem("caic:lastRepo", repo);
    try {
      const model = selectedModel();
      const image = selectedImage().trim();
      const ts = tailscaleEnabled();
      const usb = usbEnabled();
      const disp = displayEnabled();
      const data = await createTask({ prompt: p, repo, harness: selectedHarness(), ...(model ? { model } : {}), ...(image ? { image } : {}), ...(ts ? { tailscale: true } : {}), ...(usb ? { usb: true } : {}), ...(disp ? { display: true } : {}) });
      setPrompt("");
      navigate(taskPath(data.id, repo, "", p));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div class={styles.app}>
      <div class={`${styles.titleRow} ${selectedId() ? styles.hidden : ""}`}>
        <h1 class={styles.title}>caic</h1>
        <span class={styles.subtitle}>Coding Agents in Containers</span>
        <UsageBadges usage={usage} now={now} />
      </div>

      <Show when={!connected()}>
        <div class={styles.reconnecting} data-testid="reconnect-banner">Reconnecting to server...</div>
      </Show>

      <form onSubmit={(e) => { e.preventDefault(); submitTask(); }} class={`${styles.submitForm} ${selectedId() ? styles.hidden : ""}`}>
        <select
          value={selectedRepo()}
          onChange={(e) => setSelectedRepo(e.currentTarget.value)}
          disabled={submitting()}
          class={styles.repoSelect}
          data-testid="repo-select"
        >
          <For each={repos()}>
            {(r) => <option value={r.path}>{r.path}</option>}
          </For>
        </select>
        <Show when={harnesses().length > 1}>
          <select
            value={selectedHarness()}
            onChange={(e) => {
              const h = e.currentTarget.value;
              setSelectedHarness(h);
              const models = harnesses().find((x) => x.name === h)?.models ?? [];
              if (!models.includes(selectedModel())) setSelectedModel("");
            }}
            disabled={submitting()}
            class={styles.modelSelect}
          >
            <For each={harnesses()}>
              {(h) => <option value={h.name}>{h.name}</option>}
            </For>
          </select>
        </Show>
        <Show when={(harnesses().find((h) => h.name === selectedHarness())?.models ?? []).length > 0}>
          <select
            value={selectedModel()}
            onChange={(e) => setSelectedModel(e.currentTarget.value)}
            disabled={submitting()}
            class={styles.modelSelect}
          >
            <option value="">Default model</option>
            <For each={harnesses().find((h) => h.name === selectedHarness())?.models ?? []}>
              {(m) => <option value={m}>{m}</option>}
            </For>
          </select>
        </Show>
        <input
          type="text"
          value={selectedImage()}
          onInput={(e) => setSelectedImage(e.currentTarget.value)}
          placeholder="ghcr.io/maruel/md:latest (Docker image)"
          disabled={submitting()}
          class={styles.imageInput}
        />
        <Show when={tailscaleAvailable()}>
          <label class={styles.checkboxLabel} title="Enable Tailscale networking">
            <input
              type="checkbox"
              checked={tailscaleEnabled()}
              onChange={(e) => setTailscaleEnabled(e.currentTarget.checked)}
              disabled={submitting()}
            />
            <TailscaleIcon width="1.2em" height="1.2em" />
          </label>
        </Show>
        <Show when={usbAvailable()}>
          <label class={styles.checkboxLabel} title="Enable USB passthrough">
            <input
              type="checkbox"
              checked={usbEnabled()}
              onChange={(e) => setUSBEnabled(e.currentTarget.checked)}
              disabled={submitting()}
            />
            <USBIcon width="1.2em" height="1.2em" />
          </label>
        </Show>
        <Show when={displayAvailable()}>
          <label class={styles.checkboxLabel} title="Enable virtual display">
            <input
              type="checkbox"
              checked={displayEnabled()}
              onChange={(e) => setDisplayEnabled(e.currentTarget.checked)}
              disabled={submitting()}
            />
            <DisplayIcon width="1.2em" height="1.2em" />
          </label>
        </Show>
        <div class={styles.promptRow}>
          <AutoResizeTextarea
            ref={(el) => { promptRef = el; }}
            value={prompt()}
            onInput={setPrompt}
            onSubmit={submitTask}
            placeholder="Describe a task..."
            disabled={submitting()}
            class={styles.promptInput}
            data-testid="prompt-input"
          />
          <Button type="submit" disabled={submitting() || !prompt().trim() || !selectedRepo()} loading={submitting()} title="Start a new container with this prompt" data-testid="submit-task">
            <SendIcon width="1.2em" height="1.2em" />
          </Button>
        </div>
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
          <Match when={selectedId()} keyed>
            {(id) => (
              <div class={styles.detailPane}>
                <TaskView
                  taskId={id}
                  taskState={selectedTask()?.state ?? "pending"}
                  inPlanMode={selectedTask()?.inPlanMode}
                  repo={selectedTask()?.repo ?? ""}
                  repoURL={selectedTask()?.repoURL}
                  branch={selectedTask()?.branch ?? ""}
                  onClose={() => navigate("/")}
                  inputDraft={inputDrafts().get(id) ?? ""}
                  onInputDraft={(v) => setInputDrafts((prev) => { const next = new Map(prev); next.set(id, v); return next; })}
                >
                  <UsageBadges usage={usage} now={now} />
                </TaskView>
              </div>
            )}
          </Match>
        </Switch>
      </div>
    </div>
  );
}
