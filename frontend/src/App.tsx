// Main application component for caic web UI.
import { createEffect, createSignal, For, Show, Switch, Match, onMount, onCleanup } from "solid-js";
import { useNavigate, useLocation } from "@solidjs/router";
import type { HarnessInfo, Repo, Task, UsageResp, ImageData as APIImageData } from "@sdk/types.gen";
import { getConfig, getPreferences, listHarnesses, listRepos, listTasks, createTask, cloneRepo, getUsage, terminateTask } from "@sdk/api.gen";
import TaskView from "./TaskView";
import DiffView from "./DiffView";
import TaskList from "./TaskList";
import PromptInput from "./PromptInput";
import Button from "./Button";
import { requestNotificationPermission, notifyWaiting } from "./notifications";
import UsageBadges from "./UsageBadges";
import SendIcon from "@material-symbols/svg-400/outlined/send.svg?solid";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import TailscaleIcon from "./tailscale.svg?solid";
import CloneRepoDialog from "./CloneRepoDialog";
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

/** Extract the task ID from a /task/@{id}+{slug} or /task/@{id}+{slug}/diff pathname, or null. */
function taskIdFromPath(pathname: string): string | null {
  const prefix = "/task/@";
  if (!pathname.startsWith(prefix)) return null;
  const rest = pathname.slice(prefix.length).replace(/\/diff$/, "");
  const plus = rest.indexOf("+");
  return plus === -1 ? rest : rest.slice(0, plus);
}

/** True when the pathname ends with /diff (diff view route). */
function isDiffPath(pathname: string): boolean {
  return pathname.startsWith("/task/@") && pathname.endsWith("/diff");
}

export default function App() {
  const navigate = useNavigate();
  const location = useLocation();

  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<Task[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [repos, setRepos] = createSignal<Repo[]>([]);
  const [selectedRepo, setSelectedRepo] = createSignal("");
  const [selectedModel, setSelectedModel] = createSignal("");
  const [selectedImage, setSelectedImage] = createSignal("");
  const [harnesses, setHarnesses] = createSignal<HarnessInfo[]>([]);
  const [selectedHarness, setSelectedHarness] = createSignal("");
  const [sidebarOpen, setSidebarOpen] = createSignal(true);
  const [usage, setUsage] = createSignal<UsageResp | null>(null);
  const [tailscaleAvailable, setTailscaleAvailable] = createSignal(false);
  const [tailscaleEnabled, setTailscaleEnabled] = createSignal(false);
  const [usbAvailable, setUSBAvailable] = createSignal(false);
  const [usbEnabled, setUSBEnabled] = createSignal(false);
  const [displayAvailable, setDisplayAvailable] = createSignal(false);
  const [displayEnabled, setDisplayEnabled] = createSignal(false);
  const [recentCount, setRecentCount] = createSignal(0);
  const [selectedBranch, setSelectedBranch] = createSignal("");
  const [terminatingId, setTerminatingId] = createSignal<string | null>(null);

  // Clone repo dialog state.
  const [cloneOpen, setCloneOpen] = createSignal(false);
  const [cloning, setCloning] = createSignal(false);
  const [cloneError, setCloneError] = createSignal("");

  // Images attached to the new-task prompt.
  const [pendingImages, setPendingImages] = createSignal<APIImageData[]>([]);

  // Per-task input drafts survive task switching.
  const [inputDrafts, setInputDrafts] = createSignal<Map<string, string>>(new Map());

  const harnessSupportsImages = () => harnesses().find((h) => h.name === selectedHarness())?.supportsImages ?? false;

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
      navigate(taskPath(next.id, next.repo, next.branch, next.title));
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
  const selectedTask = (): Task | null => {
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

  // In-memory per-harness model preferences from the server.
  let prefModels: Record<string, string> = {};

  onMount(async () => {
    requestNotificationPermission();
    const [data, prefs, h] = await Promise.all([
      listRepos(),
      getPreferences().catch(() => null),
      listHarnesses().catch(() => [] as HarnessInfo[]),
    ]);
    const recentPaths = prefs?.repositories.map((r) => r.path) ?? [];
    const recentSet = new Set(recentPaths);
    const recentRepos = recentPaths.reduce<Repo[]>((acc, r) => {
      const repo = data.find((d) => d.path === r);
      if (repo) acc.push(repo);
      return acc;
    }, []);
    const rest = data.filter((d) => !recentSet.has(d.path));
    const ordered = [...recentRepos, ...rest];
    setRepos(ordered);
    setRecentCount(recentRepos.length);
    if (ordered.length > 0) {
      const first = recentRepos[0]?.path ?? ordered[0].path;
      setSelectedRepo(first);
    }
    {
      setHarnesses(h);
      prefModels = prefs?.models ?? {};
      const prefHarness = prefs?.harness ?? "";
      const harness = prefHarness && h.find((x) => x.name === prefHarness)
        ? prefHarness
        : h[0]?.name ?? "";
      setSelectedHarness(harness);
      const models = h.find((x) => x.name === harness)?.models ?? [];
      const lastModel = prefModels[harness];
      if (lastModel && models.includes(lastModel)) setSelectedModel(lastModel);
    }
    if (prefs?.baseImage) setSelectedImage(prefs.baseImage);
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
    let taskES: EventSource | null = null;
    let usageES: EventSource | null = null;
    let taskTimer: ReturnType<typeof setTimeout> | null = null;
    let usageTimer: ReturnType<typeof setTimeout> | null = null;
    let bannerTimer: ReturnType<typeof setTimeout> | null = null;
    let taskDelay = 500;
    let usageDelay = 500;
    const initialScriptSrc = document.querySelector<HTMLScriptElement>("script[src^='/assets/']")?.src ?? "";

    function onOpen() {
      if (bannerTimer !== null) { clearTimeout(bannerTimer); bannerTimer = null; }
      setConnected(true);
    }

    function connectTasks() {
      taskES = new EventSource("/api/v1/server/tasks/events");
      taskES.addEventListener("open", () => {
        onOpen();
        taskDelay = 500;
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
      taskES.addEventListener("message", (e) => {
        try {
          const updated = JSON.parse(e.data) as Task[];
          for (const t of updated) {
            const needsInput = t.state === "waiting" || t.state === "asking";
            const prevNeedsInput = prevStates.get(t.id) === "waiting" || prevStates.get(t.id) === "asking";
            if (needsInput && !prevNeedsInput) {
              notifyWaiting(t.id, t.title);
            }
          }
          prevStates = new Map(updated.map((t) => [t.id, t.state]));
          setTasks(updated);
        } catch {
          // Ignore unparseable messages.
        }
      });
      taskES.onerror = () => {
        taskES?.close();
        taskES = null;
        if (bannerTimer === null) {
          bannerTimer = setTimeout(() => { bannerTimer = null; setConnected(false); }, 2000);
        }
        taskTimer = setTimeout(connectTasks, taskDelay);
        taskDelay = Math.min(taskDelay * 1.5, 4000);
      };
    }

    function connectUsage() {
      usageES = new EventSource("/api/v1/server/usage/events");
      usageES.addEventListener("open", () => {
        onOpen();
        usageDelay = 500;
      });
      usageES.addEventListener("message", (e) => {
        try {
          setUsage(JSON.parse(e.data) as UsageResp);
        } catch {
          // Ignore unparseable messages.
        }
      });
      usageES.onerror = () => {
        usageES?.close();
        usageES = null;
        usageTimer = setTimeout(connectUsage, usageDelay);
        usageDelay = Math.min(usageDelay * 1.5, 4000);
      };
    }

    connectTasks();
    connectUsage();

    onCleanup(() => {
      taskES?.close();
      usageES?.close();
      if (taskTimer !== null) clearTimeout(taskTimer);
      if (usageTimer !== null) clearTimeout(usageTimer);
      if (bannerTimer !== null) clearTimeout(bannerTimer);
    });
  }

  // Clear stale terminatingId once the server state reflects it.
  createEffect(() => {
    const tid = terminatingId();
    if (!tid) return;
    const t = tasks().find((task) => task.id === tid);
    if (t && (t.state === "terminating" || t.state === "terminated" || t.state === "failed")) {
      setTerminatingId(null);
    }
  });

  async function handleTerminate(id: string) {
    if (terminatingId()) return;
    setTerminatingId(id);
    try {
      await terminateTask(id);
    } catch {
      setTerminatingId(null);
    }
  }

  async function submitTask() {
    const p = prompt().trim();
    const imgs = pendingImages();
    const repo = selectedRepo();
    if ((!p && imgs.length === 0) || !repo) return;
    setSubmitting(true);
    {
      // Optimistic reorder: move the selected repo to the front.
      const current = repos();
      const idx = current.findIndex((r) => r.path === repo);
      if (idx > 0) {
        const updated = [current[idx], ...current.slice(0, idx), ...current.slice(idx + 1)];
        setRepos(updated);
      }
      setRecentCount(Math.min(recentCount() + (idx > recentCount() - 1 ? 1 : 0), current.length));
    }
    try {
      const model = selectedModel();
      const image = selectedImage().trim();
      const branch = selectedBranch().trim();
      const ts = tailscaleEnabled();
      const usb = usbEnabled();
      const disp = displayEnabled();
      const harness = selectedHarness();
      const data = await createTask({ initialPrompt: { text: p, ...(imgs.length > 0 ? { images: imgs } : {}) }, repo, harness, ...(branch ? { baseBranch: branch } : {}), ...(model ? { model } : {}), ...(image ? { image } : {}), ...(ts ? { tailscale: true } : {}), ...(usb ? { usb: true } : {}), ...(disp ? { display: true } : {}) });
      if (model) prefModels[harness] = model;
      setPrompt("");
      setPendingImages([]);
      setSelectedBranch("");
      navigate(taskPath(data.id, repo, "", p));
    } finally {
      setSubmitting(false);
    }
  }

  async function submitClone(url: string, path?: string) {
    setCloning(true);
    setCloneError("");
    try {
      const repo = await cloneRepo({ url, ...(path ? { path } : {}) });
      setRepos((prev) => [repo, ...prev]);
      setRecentCount((c) => c + 1);
      setSelectedRepo(repo.path);
      setCloneOpen(false);
    } catch (e: unknown) {
      setCloneError(e instanceof Error ? e.message : "Clone failed");
    } finally {
      setCloning(false);
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
          <Show when={recentCount() > 0} fallback={
            <For each={repos()}>
              {(r) => <option value={r.path}>{r.path}</option>}
            </For>
          }>
            <optgroup label="Recent">
              <For each={repos().slice(0, recentCount())}>
                {(r) => <option value={r.path}>{r.path}</option>}
              </For>
            </optgroup>
            <optgroup label="All repositories">
              <For each={repos().slice(recentCount())}>
                {(r) => <option value={r.path}>{r.path}</option>}
              </For>
            </optgroup>
          </Show>
        </select>
        <button
          type="button"
          class={styles.cloneButton}
          onClick={() => { setCloneOpen(true); setCloneError(""); }}
          disabled={submitting()}
          title="Clone a repository"
          data-testid="clone-toggle"
        >+</button>
        <input
          type="text"
          value={selectedBranch()}
          onInput={(e) => setSelectedBranch(e.currentTarget.value)}
          placeholder={repos().find((r) => r.path === selectedRepo())?.baseBranch ?? "branch"}
          disabled={submitting()}
          class={styles.branchInput}
          data-testid="branch-input"
          title="Base branch to fork from (leave empty for default)"
        />
        <Show when={harnesses().length > 1}>
          <select
            value={selectedHarness()}
            onChange={(e) => {
              const h = e.currentTarget.value;
              setSelectedHarness(h);
              const models = harnesses().find((x) => x.name === h)?.models ?? [];
              const lastModel = prefModels[h];
              setSelectedModel(lastModel && models.includes(lastModel) ? lastModel : "");
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
            onChange={(e) => {
              const m = e.currentTarget.value;
              setSelectedModel(m);
              if (m) prefModels[selectedHarness()] = m;
              else delete prefModels[selectedHarness()];
            }}
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
        <PromptInput
          value={prompt()}
          onInput={setPrompt}
          onSubmit={submitTask}
          placeholder="Describe a task..."
          disabled={submitting()}
          class={styles.promptInput}
          ref={(el) => { promptRef = el; }}
          data-testid="prompt-input"
          supportsImages={harnessSupportsImages()}
          images={pendingImages()}
          onImagesChange={setPendingImages}
        >
          <Button type="submit" disabled={submitting() || (!prompt().trim() && pendingImages().length === 0) || !selectedRepo()} loading={submitting()} title="Start a new container with this prompt" data-testid="submit-task">
            <SendIcon width="1.2em" height="1.2em" />
          </Button>
        </PromptInput>
      </form>

      <Show when={cloneOpen()}>
        <CloneRepoDialog
          loading={cloning()}
          error={cloneError()}
          onClone={submitClone}
          onClose={() => setCloneOpen(false)}
        />
      </Show>

      <div class={styles.layout}>
        <TaskList
          tasks={tasks}
          selectedId={selectedId()}
          sidebarOpen={sidebarOpen}
          setSidebarOpen={setSidebarOpen}
          now={now}
          onSelect={(id) => {
            const found = tasks().find((t) => t.id === id);
            navigate(found ? taskPath(found.id, found.repo, found.branch, found.title) : `/task/@${id}`);
          }}
          onTerminate={handleTerminate}
          terminatingId={terminatingId}
          onDiffClick={(id) => {
            const found = tasks().find((t) => t.id === id);
            if (found?.diffStat?.length) {
              navigate(taskPath(found.id, found.repo, found.branch, found.title) + "/diff");
            }
          }}
        />

        <Switch>
          <Match when={isDiffPath(location.pathname) && selectedId()} keyed>
            {(id) => {
              const t = selectedTask();
              const tp = t ? taskPath(t.id, t.repo, t.branch, t.title) : `/task/@${id}`;
              return (
                <div class={styles.detailPane}>
                  <DiffView
                    taskId={id}
                    diffStat={t?.diffStat ?? []}
                    repo={t?.repo ?? ""}
                    branch={t?.branch ?? ""}
                    taskPath={tp}
                  />
                </div>
              );
            }}
          </Match>
          <Match when={selectedId()} keyed>
            {(id) => (
              <div class={styles.detailPane}>
                <TaskView
                  taskId={id}
                  taskState={selectedTask()?.state ?? "pending"}
                  inPlanMode={selectedTask()?.inPlanMode}
                  planContent={selectedTask()?.planContent}
                  repo={selectedTask()?.repo ?? ""}
                  repoURL={selectedTask()?.repoURL}
                  branch={selectedTask()?.branch ?? ""}
                  baseBranch={repos().find((r) => r.path === selectedTask()?.repo)?.baseBranch ?? "main"}
                  diffStat={selectedTask()?.diffStat}
                  supportsImages={harnesses().find((h) => h.name === (selectedTask()?.harness ?? ""))?.supportsImages}
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
