// Main application component for caic web UI.
import { createEffect, createSignal, For, Show, Switch, Match, onCleanup } from "solid-js";
import { Portal } from "solid-js/web";
import { useNavigate, useLocation } from "@solidjs/router";
import type { HarnessInfo, Repo, Task, TaskListEvent, UsageResp, ImageData as APIImageData, CacheMappingResp, WellKnownCachesResp } from "@sdk/types.gen";
import { getConfig, getPreferences, updatePreferences, listHarnesses, listCaches, listRepos, listRepoBranches, createTask, cloneRepo, getUsage, stopTask, purgeTask, reviveTask } from "./api";
import { useAuth } from "./AuthContext";
import Login from "./Login";
import TaskDetail from "./TaskDetail";
import DiffDetail from "./DiffDetail";
import TaskList, { sortTasks } from "./TaskList";
import PromptInput from "./PromptInput";
import Button from "./Button";
import { requestNotificationPermission, notifyWaiting, dismissNotification } from "./notifications";
import UsageBadges from "./UsageBadges";
import SendIcon from "@material-symbols/svg-400/outlined/send.svg?solid";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import SettingsIcon from "@material-symbols/svg-400/outlined/settings.svg?solid";
import TailscaleIcon from "./tailscale.svg?solid";
import CloneRepoDialog from "./CloneRepoDialog";
import VoiceOverlay from "./VoiceOverlay";
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

function ConnectionDot(props: { connected: boolean }) {
  return (
    <span
      class={props.connected ? styles.dotConnected : styles.dotDisconnected}
      title={props.connected ? "Connected" : "Disconnected"}
      data-testid="connection-dot"
    />
  );
}

export default function App() {
  const navigate = useNavigate();
  const location = useLocation();
  const auth = useAuth();

  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<Task[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [initializing, setInitializing] = createSignal(true);
  const [repos, setRepos] = createSignal<Repo[]>([]);
  type RepoEntry = { path: string; branch: string };
  const [selectedRepos, setSelectedRepos] = createSignal<RepoEntry[]>([]);
  // Branch dropdown state: which chip is open, its fetched branch list, and the trigger's rect.
  const [editingPath, setEditingPath] = createSignal<string | null>(null);
  const [editingBranches, setEditingBranches] = createSignal<string[]>([]);
  const [branchTriggerRect, setBranchTriggerRect] = createSignal<DOMRect | null>(null);
  const [branchFilter, setBranchFilter] = createSignal("");
  // Add-repo dropdown open state.
  const [addOpen, setAddOpen] = createSignal(false);
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
  const [actionId, setActionId] = createSignal<string | null>(null);

  const [autoFixCI, setAutoFixCI] = createSignal(false);
  const [autoFixPR, setAutoFixPR] = createSignal(false);
  const [useDefaultCaches, setUseDefaultCaches] = createSignal(true);
  const [wellKnownCaches, setWellKnownCaches] = createSignal<Record<string, boolean | undefined>>({});
  const [wellKnownCachesList, setWellKnownCachesList] = createSignal<WellKnownCachesResp["wellKnown"]>([]);
  const [cacheMappings, setCacheMappings] = createSignal<CacheMappingResp[]>([]);
  const [settingsOpen, setSettingsOpen] = createSignal(false);

  /** Build the current settings payload for updatePreferences, with optional overrides. */
  const currentSettings = (overrides: Partial<Parameters<typeof updatePreferences>[0]["settings"]> = {}) => ({
    settings: {
      autoFixOnCIFailure: autoFixCI(),
      autoFixOnPROpen: autoFixPR(),
      baseImage: selectedImage() || "",
      useDefaultCaches: useDefaultCaches(),
      wellKnownCaches: wellKnownCaches() as Record<string, boolean>,
      cacheMappings: cacheMappings(),
      ...overrides,
    },
  });

  // Clone repo dialog state.
  const [cloneOpen, setCloneOpen] = createSignal(false);
  const [cloning, setCloning] = createSignal(false);
  const [cloneError, setCloneError] = createSignal("");

  // Images attached to the new-task prompt.
  const [pendingImages, setPendingImages] = createSignal<APIImageData[]>([]);

  // Per-task input drafts survive task switching.
  const [inputDrafts, setInputDrafts] = createSignal<Map<string, string>>(new Map());

  // Per-task image drafts survive task switching.
  const [inputImageDrafts, setInputImageDrafts] = createSignal<Map<string, APIImageData[]>>(new Map());

  const harnessSupportsImages = () => harnesses().find((h) => h.name === selectedHarness())?.supportsImages ?? false;

  // Ref to the main prompt textarea for focusing after Escape.
  let promptRef: HTMLTextAreaElement | undefined;

  // Sort tasks the same way TaskList does: active by repo/branch, then terminal by ID.
  const sortedTasks = () => sortTasks(tasks());

  // Global keyboard shortcuts:
  // - Escape: from diff view, return to task detail; from task detail, dismiss and focus prompt
  // - ArrowUp/ArrowDown: switch to previous/next task in sidebar order
  {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && selectedId() !== null) {
        if (isDiffPath(location.pathname)) {
          navigate(location.pathname.replace(/\/diff$/, ""));
        } else {
          navigate("/");
          promptRef?.focus();
        }
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
      navigate(taskPath(next.id, next.repos?.[0]?.name ?? "", next.repos?.[0]?.branch ?? "", next.title));
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

  // Repos available to add (not already selected).
  const availableRecent = () => repos().slice(0, recentCount()).filter((r) => !selectedRepos().some((s) => s.path === r.path));
  const availableRest = () => repos().slice(recentCount()).filter((r) => !selectedRepos().some((s) => s.path === r.path));

  function addRepo(path: string) {
    if (selectedRepos().some((r) => r.path === path)) return;
    setSelectedRepos((prev) => [...prev, { path, branch: "" }]);
    setAddOpen(false);
  }

  function removeRepo(path: string) {
    setSelectedRepos((prev) => prev.filter((r) => r.path !== path));
    if (editingPath() === path) setEditingPath(null);
  }

  function startEditBranch(path: string, triggerRect: DOMRect) {
    if (editingPath() === path) { setEditingPath(null); return; }
    setEditingPath(path);
    setBranchTriggerRect(triggerRect);
    setBranchFilter(selectedRepos().find((r) => r.path === path)?.branch ?? "");
    setEditingBranches([]);
    listRepoBranches(path).then((r) => setEditingBranches(r.branches)).catch(() => {});
  }

  function commitBranch(branch: string) {
    const path = editingPath();
    if (!path) return;
    setSelectedRepos((prev) => prev.map((r) => r.path === path ? { ...r, branch } : r));
    setEditingPath(null);
  }

  // In-memory per-harness model preferences from the server.
  let prefModels: Record<string, string> = {};

  const isAuthenticated = () => auth.ready() && (auth.providers().length === 0 || auth.user() !== null);

  // Load initial data once authentication is confirmed.
  let dataLoaded = false;
  createEffect(() => {
    if (!isAuthenticated() || dataLoaded) return;
    dataLoaded = true;
    void (async () => {
      try {
        const [data, prefs, h, config, usageData, cachesData] = await Promise.all([
          listRepos(),
          getPreferences().catch(() => null),
          listHarnesses().catch(() => [] as HarnessInfo[]),
          getConfig().catch(() => null),
          getUsage().catch(() => null),
          listCaches().catch(() => null) as Promise<WellKnownCachesResp | null>,
        ]);
        if (cachesData) setWellKnownCachesList(cachesData.wellKnown);
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
          setSelectedRepos([{ path: first, branch: "" }]);
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
        if (prefs?.settings?.baseImage) setSelectedImage(prefs.settings.baseImage);
        if (config) {
          setTailscaleAvailable(config.tailscaleAvailable);
          setUSBAvailable(config.usbAvailable);
          setDisplayAvailable(config.displayAvailable);
        }
        if (prefs?.settings) {
          setAutoFixCI(prefs.settings.autoFixOnCIFailure);
          setAutoFixPR(prefs.settings.autoFixOnPROpen);
          setUseDefaultCaches(prefs.settings.useDefaultCaches ?? true);
          setWellKnownCaches(prefs.settings.wellKnownCaches ?? {});
          setCacheMappings(prefs.settings.cacheMappings ?? []);
        }
        if (usageData) setUsage(usageData);
      } finally {
        setInitializing(false);
      }
    })();
  });

  // Subscribe to task list updates via SSE with automatic reconnection.
  // Backoff: 500ms × 1.5 each failure, capped at 4s, reset on success.
  // On 401, stop retrying and clear auth state so the login page shows.
  // On reconnect, check if the frontend was rebuilt and reload if so.
  const [connected, setConnected] = createSignal(true);
  {
    let taskES: EventSource | null = null;
    let usageES: EventSource | null = null;
    let taskTimer: ReturnType<typeof setTimeout> | null = null;
    let usageTimer: ReturnType<typeof setTimeout> | null = null;
    let taskDelay = 500;
    let usageDelay = 500;

    /** Probe whether the server is returning 401. EventSource doesn't expose status codes. */
    async function checkUnauthorized(): Promise<boolean> {
      try {
        const res = await fetch("/api/v1/auth/me", { signal: AbortSignal.timeout(5000) });
        if (res.status === 401) {
          auth.clearUser();
          return true;
        }
      } catch {
        // Network error — not a 401.
      }
      return false;
    }
    const initialScriptSrc = document.querySelector<HTMLScriptElement>("script[src^='/assets/']")?.src ?? "";

    function onOpen() {
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
      });
      taskES.addEventListener("message", (e) => {
        try {
          const event = JSON.parse(e.data) as TaskListEvent;
          const checkAndNotify = (t: Task) => {
            const needsInput = t.state === "waiting" || t.state === "asking" || t.state === "has_plan";
            const prevState = prevStates.get(t.id);
            const prevNeedsInput = prevState === "waiting" || prevState === "asking" || prevState === "has_plan";
            if (needsInput && prevState === "running") {
              notifyWaiting(t.id, t.title);
            } else if (!needsInput && prevNeedsInput) {
              dismissNotification(t.id);
            }
          };
          if (event.kind === "snapshot" && event.tasks) {
            prevStates = new Map(event.tasks.map((t) => [t.id, t.state]));
            setTasks(event.tasks);
          } else if (event.kind === "upsert" && event.task) {
            const t = event.task;
            checkAndNotify(t);
            prevStates.set(t.id, t.state);
            setTasks((prev) => {
              const idx = prev.findIndex((p) => p.id === t.id);
              if (idx >= 0) {
                const next = [...prev];
                next[idx] = t;
                return next;
              }
              return [...prev, t].sort((a, b) => (a.id < b.id ? -1 : 1));
            });
          } else if (event.kind === "patch" && event.patch) {
            const patch = event.patch as Record<string, unknown>;
            const id = patch["id"] as string;
            if (!id) return;
            if (typeof patch["state"] === "string") {
              const newState = patch["state"] as string;
              const existing = tasks().find((t) => t.id === id);
              if (existing) {
                checkAndNotify({ ...existing, state: newState } as Task);
              }
              prevStates.set(id, newState);
            }
            setTasks((prev) => {
              const idx = prev.findIndex((p) => p.id === id);
              if (idx < 0) return prev;
              const next = [...prev];
              next[idx] = { ...next[idx], ...patch } as Task;
              return next;
            });
          } else if (event.kind === "delete" && event.id) {
            prevStates.delete(event.id);
            setTasks((prev) => prev.filter((t) => t.id !== event.id));
          } else if (event.kind === "repos" && event.repos) {
            const updatedRepos = event.repos;
            setRepos((prev) => {
              // Merge updated CI status into existing repo order.
              const byPath = new Map(updatedRepos.map((r) => [r.path, r]));
              return prev.map((r) => byPath.get(r.path) ?? r);
            });
          }
        } catch {
          // Ignore unparseable messages.
        }
      });
      taskES.onerror = () => {
        taskES?.close();
        taskES = null;
        setConnected(false);
        checkUnauthorized().then((is401) => {
          if (is401) return; // Stop retrying; effect restarts after re-login.
          taskTimer = setTimeout(connectTasks, taskDelay);
          taskDelay = Math.min(taskDelay * 1.5, 4000);
        });
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
        checkUnauthorized().then((is401) => {
          if (is401) return;
          usageTimer = setTimeout(connectUsage, usageDelay);
          usageDelay = Math.min(usageDelay * 1.5, 4000);
        });
      };
    }

    createEffect(() => {
      if (!isAuthenticated()) return;
      connectTasks();
      connectUsage();
      onCleanup(() => {
        taskES?.close();
        usageES?.close();
        if (taskTimer !== null) clearTimeout(taskTimer);
        if (usageTimer !== null) clearTimeout(usageTimer);
      });
    });
  }

  // Clear stale actionId once the server state reflects the transition.
  createEffect(() => {
    const tid = actionId();
    if (!tid) return;
    const t = tasks().find((task) => task.id === tid);
    if (t && (t.state === "purging" || t.state === "purged" || t.state === "failed" || t.state === "stopping" || t.state === "stopped" || t.state === "provisioning")) {
      setActionId(null);
    }
  });

  async function handleStop(id: string) {
    if (actionId()) return;
    setActionId(id);
    try {
      await stopTask(id);
    } catch {
      setActionId(null);
    }
  }

  async function handlePurge(id: string) {
    if (actionId()) return;
    setActionId(id);
    try {
      await purgeTask(id);
    } catch {
      setActionId(null);
    }
  }

  async function handleRevive(id: string) {
    if (actionId()) return;
    setActionId(id);
    try {
      await reviveTask(id);
    } catch {
      setActionId(null);
    }
  }

  async function submitTask() {
    const p = prompt().trim();
    const imgs = pendingImages();
    const selRepos = selectedRepos();
    if (!p && imgs.length === 0) return;
    requestNotificationPermission();
    setSubmitting(true);
    {
      // Optimistic reorder: move the primary repo to the front of the recent list.
      const primary = selRepos[0]?.path;
      if (primary) {
        const current = repos();
        const idx = current.findIndex((r) => r.path === primary);
        if (idx > 0) {
          setRepos([current[idx], ...current.slice(0, idx), ...current.slice(idx + 1)]);
        }
        setRecentCount(Math.min(recentCount() + (idx > recentCount() - 1 ? 1 : 0), current.length));
      }
    }
    try {
      const model = selectedModel();
      const image = selectedImage().trim();
      const ts = tailscaleEnabled();
      const usb = usbEnabled();
      const disp = displayEnabled();
      const harness = selectedHarness();
      const repoSpecs = selRepos.length > 0 ? selRepos.map((r) => ({ name: r.path, ...(r.branch ? { baseBranch: r.branch } : {}) })) : undefined;
      const data = await createTask({ initialPrompt: { text: p, ...(imgs.length > 0 ? { images: imgs } : {}) }, repos: repoSpecs, harness, ...(model ? { model } : {}), ...(image ? { image } : {}), ...(ts ? { tailscale: true } : {}), ...(usb ? { usb: true } : {}), ...(disp ? { display: true } : {}) });
      if (model) prefModels[harness] = model;
      setPrompt("");
      setPendingImages([]);
      navigate(taskPath(data.id, selRepos[0]?.path ?? "", "", p));
    } finally {
      setSubmitting(false);
    }
  }

  async function submitClone(url: string, path?: string) {
    setCloning(true);
    setCloneError("");
    try {
      const repo = await cloneRepo({ url, ...(path ? { path } : {}) });
      // Insert at the start of "All repositories" (after recent repos) without
      // incrementing recentCount. The repo becomes "recent" when the first task
      // is created for it via submitTask's optimistic reorder.
      const rc = recentCount();
      setRepos((prev) => [...prev.slice(0, rc), repo, ...prev.slice(rc)]);
      setSelectedRepos([{ path: repo.path, branch: "" }]);
      setCloneOpen(false);
    } catch (e: unknown) {
      setCloneError(e instanceof Error ? e.message : "Clone failed");
    } finally {
      setCloning(false);
    }
  }

  return (
    <Show when={auth.providers().length === 0 || auth.user()} fallback={<Login />}>
    <div class={styles.app}>
      <div class={styles.navbar}>
        <h1 class={styles.title}>caic</h1>
        <span class={styles.subtitle}>Coding Agents in Containers</span>
        <UsageBadges usage={usage} now={now} />
        <ConnectionDot connected={connected()} />
        <Show when={auth.providers().length > 0 && auth.user()}>
          {(() => {
            const [menuOpen, setMenuOpen] = createSignal(false);
            let menuRef: HTMLDivElement | undefined;
            const onClickOutside = (e: MouseEvent) => {
              if (menuRef && !menuRef.contains(e.target as Node)) setMenuOpen(false);
            };
            createEffect(() => {
              if (menuOpen()) document.addEventListener("click", onClickOutside, true);
              else document.removeEventListener("click", onClickOutside, true);
              onCleanup(() => document.removeEventListener("click", onClickOutside, true));
            });
            const user = () => auth.user() ?? { username: "", avatarURL: undefined };
            const initials = () => user().username.slice(0, 2).toUpperCase();
            return (
              <div class={styles.userMenu} ref={(el) => { menuRef = el; }}>
                <button
                  class={styles.avatarButton}
                  onClick={() => setMenuOpen((v) => !v)}
                  title={user().username}
                >
                  <Show when={user().avatarURL} keyed fallback={
                    <span class={styles.avatarInitials}>{initials()}</span>
                  }>
                    {(url) => <img src={url} alt={user().username} class={styles.avatarImg} />}
                  </Show>
                </button>
                <Show when={menuOpen()}>
                  <div class={styles.userDropdown}>
                    <span class={styles.dropdownUser}>{user().username}</span>
                    <button class={styles.dropdownItem} onClick={() => { setMenuOpen(false); setSettingsOpen(true); }}>
                      <SettingsIcon width="1em" height="1em" style={{ "vertical-align": "middle", "margin-right": "0.4em" }} />
                      Settings
                    </button>
                    <button class={styles.dropdownItem} onClick={() => { setMenuOpen(false); void auth.logout(); }}>Sign out</button>
                  </div>
                </Show>
              </div>
            );
          })()}
        </Show>
      </div>

      <form onSubmit={(e) => { e.preventDefault(); submitTask(); }} class={`${styles.submitForm} ${selectedId() ? styles.hidden : ""}`}>
        {/* Repo chip strip */}
        {(() => {
          let addRef: HTMLButtonElement | undefined;
          let dropdownRef: HTMLDivElement | undefined;
          let branchDropdownRef: HTMLDivElement | undefined;
          const onAddClickOutside = (e: MouseEvent) => {
            const inTrigger = addRef?.contains(e.target as Node) ?? false;
            const inDropdown = dropdownRef?.contains(e.target as Node) ?? false;
            if (!inTrigger && !inDropdown) setAddOpen(false);
          };
          createEffect(() => {
            if (addOpen()) document.addEventListener("click", onAddClickOutside, true);
            else document.removeEventListener("click", onAddClickOutside, true);
            onCleanup(() => document.removeEventListener("click", onAddClickOutside, true));
          });
          // Position the add-repo portal dropdown below its trigger button.
          createEffect(() => {
            if (!addOpen() || !dropdownRef || !addRef) return;
            const r = addRef.getBoundingClientRect();
            const gap = 4;
            const margin = 8;
            const available = window.innerHeight - r.bottom - gap - margin;
            dropdownRef.style.top = `${r.bottom + gap}px`;
            dropdownRef.style.left = `${r.left}px`;
            dropdownRef.style.maxHeight = `${Math.min(available, 480)}px`;
          });
          // Close branch dropdown on outside click.
          const onBranchClickOutside = (e: MouseEvent) => {
            if (branchDropdownRef?.contains(e.target as Node)) return;
            setEditingPath(null);
          };
          createEffect(() => {
            if (editingPath()) document.addEventListener("click", onBranchClickOutside, true);
            else document.removeEventListener("click", onBranchClickOutside, true);
            onCleanup(() => document.removeEventListener("click", onBranchClickOutside, true));
          });
          // Position the branch portal dropdown below the clicked chip.
          createEffect(() => {
            if (!editingPath() || !branchDropdownRef) return;
            const r = branchTriggerRect();
            if (!r) return;
            const gap = 4;
            const margin = 8;
            const available = window.innerHeight - r.bottom - gap - margin;
            branchDropdownRef.style.top = `${r.bottom + gap}px`;
            branchDropdownRef.style.left = `${r.left}px`;
            branchDropdownRef.style.maxHeight = `${Math.min(available, 360)}px`;
          });
          return (
            <div class={styles.repoChips} data-testid="repo-chips">
              <Show when={editingPath()}>
                <Portal>
                  <div ref={(el) => { branchDropdownRef = el; }} class={styles.branchDropdown}>
                    <input
                      ref={(el) => setTimeout(() => el.focus(), 0)}
                      type="text"
                      class={styles.branchInput}
                      placeholder="Branch name…"
                      value={branchFilter()}
                      onInput={(e) => setBranchFilter(e.currentTarget.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") { commitBranch(branchFilter()); e.preventDefault(); }
                        if (e.key === "Escape") { setEditingPath(null); e.preventDefault(); }
                      }}
                    />
                    <Show when={!branchFilter()}>
                      <button type="button" class={styles.dropdownOption}
                        onClick={() => commitBranch("")}
                      >
                        <span class={styles.dropdownOptionMuted}>Default</span>
                        {" "}({repos().find((r) => r.path === editingPath())?.baseBranch ?? "base"})
                      </button>
                    </Show>
                    <For each={editingBranches().filter((b) => {
                      const f = branchFilter().toLowerCase();
                      return !f || b.toLowerCase().includes(f);
                    })}>
                      {(b) => (
                        <button type="button" class={`${styles.dropdownOption}${selectedRepos().find((r) => r.path === editingPath())?.branch === b ? ` ${styles.dropdownOptionActive}` : ""}`}
                          onClick={() => commitBranch(b)}
                        >{b}</button>
                      )}
                    </For>
                  </div>
                </Portal>
              </Show>
              <For each={selectedRepos()}>
                {(entry) => (
                  <span class={styles.repoChip}>
                    <button
                      type="button"
                      class={`${styles.chipLabel} ${editingPath() === entry.path ? styles.chipLabelActive : ""}`}
                      onClick={(e) => startEditBranch(entry.path, (e.currentTarget as HTMLButtonElement).getBoundingClientRect())}
                      title="Click to set branch"
                      data-testid={`chip-label-${entry.path}`}
                    >
                      {entry.path.split("/").pop()}
                      <Show when={entry.branch}>
                        <span class={styles.chipBranch}> · {entry.branch}</span>
                      </Show>
                    </button>
                    <button
                      type="button"
                      class={styles.chipRemove}
                      onClick={() => removeRepo(entry.path)}
                      aria-label={`Remove ${entry.path}`}
                      data-testid={`chip-remove-${entry.path}`}
                    >×</button>
                  </span>
                )}
              </For>
              <Show when={availableRecent().length > 0 || availableRest().length > 0}>
                <div class={styles.addRepoWrap}>
                  <button
                    ref={(el) => { addRef = el; }}
                    type="button"
                    class={styles.addRepoBtn}
                    onClick={() => setAddOpen((v) => !v)}
                    data-testid="add-repo-button"
                    title="Add a repository"
                  >+</button>
                  <Show when={addOpen()}>
                    <Portal>
                    <div ref={(el) => { dropdownRef = el; }} class={styles.addRepoDropdown} data-testid="add-repo-dropdown">
                      <Show when={availableRecent().length > 0}>
                        <div class={styles.dropdownGroupLabel}>Recent</div>
                        <For each={[...availableRecent()].sort((a, b) => a.path < b.path ? -1 : 1)}>
                          {(r) => (
                            <button type="button" class={styles.dropdownOption} onClick={() => addRepo(r.path)}>
                              {r.path}
                            </button>
                          )}
                        </For>
                      </Show>
                      <Show when={availableRest().length > 0}>
                        <Show when={availableRecent().length > 0}>
                          <div class={styles.dropdownGroupLabel}>All repositories</div>
                        </Show>
                        <For each={availableRest()}>
                          {(r) => (
                            <button type="button" class={styles.dropdownOption} onClick={() => addRepo(r.path)}>
                              {r.path}
                            </button>
                          )}
                        </For>
                      </Show>
                    </div>
                    </Portal>
                  </Show>
                </div>
              </Show>
              <button
                type="button"
                class={styles.cloneButton}
                onClick={() => { setCloneOpen(true); setCloneError(""); }}
                title="Clone a repository"
                data-testid="clone-toggle"
              >⎘</button>
            </div>
          );
        })()}
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
          class={styles.imageInput}
        />
        <Show when={tailscaleAvailable()}>
          <label class={styles.checkboxLabel} title="Enable Tailscale networking">
            <input
              type="checkbox"
              checked={tailscaleEnabled()}
              onChange={(e) => setTailscaleEnabled(e.currentTarget.checked)}
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
            />
            <DisplayIcon width="1.2em" height="1.2em" />
          </label>
        </Show>
        <PromptInput
          value={prompt()}
          onInput={setPrompt}
          onSubmit={submitTask}
          placeholder="Describe a task..."
          class={styles.promptInput}
          ref={(el) => { promptRef = el; }}
          data-testid="prompt-input"
          supportsImages={harnessSupportsImages()}
          images={pendingImages()}
          onImagesChange={setPendingImages}
        >
          <Button type="submit" disabled={initializing() || submitting() || (!prompt().trim() && pendingImages().length === 0)} loading={initializing() || submitting()} title="Start a new container with this prompt" data-testid="submit-task">
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
          repos={repos}
          selectedId={selectedId()}
          sidebarOpen={sidebarOpen}
          setSidebarOpen={setSidebarOpen}
          now={now}
          onSelect={(id) => {
            const found = tasks().find((t) => t.id === id);
            navigate(found ? taskPath(found.id, found.repos?.[0]?.name ?? "", found.repos?.[0]?.branch ?? "", found.title) : `/task/@${id}`);
          }}
          onStop={handleStop}
          onPurge={handlePurge}
          onRevive={handleRevive}
          actionId={actionId}
          onDiffClick={(id) => {
            const found = tasks().find((t) => t.id === id);
            if (found?.diffStat?.length) {
              navigate(taskPath(found.id, found.repos?.[0]?.name ?? "", found.repos?.[0]?.branch ?? "", found.title) + "/diff");
            }
          }}
          autoFixCI={autoFixCI}
          onFixCI={(repoPath) => {
            const repo = repos().find((r) => r.path === repoPath);
            if (!repo) return;
            const nonPassing = new Set(["failure", "cancelled", "timed_out", "action_required", "stale"]);
            const failing = repo.defaultBranchChecks?.filter((c) => nonPassing.has(c.conclusion)) ?? [];
            const names = failing.map((c) => c.name).join(", ");
            const fixPrompt = `CI is failing on the default branch of ${repoPath}. Please fix the failing CI checks and push to the default branch:\n\nFailing checks: ${names || "(unknown)"}`;
            const harness = selectedHarness();
            createTask({ initialPrompt: { text: fixPrompt }, repos: [{ name: repoPath }], harness }).then((data) => {
              navigate(taskPath(data.id, repoPath, "", `Fix CI: ${repoPath}`));
            });
          }}
        />

        <Switch>
          <Match when={isDiffPath(location.pathname) && selectedId()} keyed>
            {(id) => {
              const t = selectedTask();
              const tp = t ? taskPath(t.id, t.repos?.[0]?.name ?? "", t.repos?.[0]?.branch ?? "", t.title) : `/task/@${id}`;
              return (
                <div class={styles.detailPane}>
                  <DiffDetail
                    taskId={id}
                    diffStat={t?.diffStat ?? []}
                    repo={t?.repos?.[0]?.name ?? ""}
                    branch={t?.repos?.[0]?.branch ?? ""}
                    taskPath={tp}
                  />
                </div>
              );
            }}
          </Match>
          <Match when={selectedId()} keyed>
            {(id) => (
              <div class={styles.detailPane}>
                <TaskDetail
                  taskId={id}
                  taskState={selectedTask()?.state ?? "pending"}
                  initialPrompt={selectedTask()?.initialPrompt}
                  inPlanMode={selectedTask()?.inPlanMode}
                  planContent={selectedTask()?.planContent}
                  repo={selectedTask()?.repos?.[0]?.name ?? ""}
                  remoteURL={selectedTask()?.repos?.[0]?.remoteURL}
                  forge={selectedTask()?.repos?.[0]?.forge}
                  branch={selectedTask()?.repos?.[0]?.branch ?? ""}
                  baseBranch={selectedTask()?.repos?.[0]?.baseBranch ?? "main"}
                  forgeOwner={selectedTask()?.forgeOwner}
                  forgeRepo={selectedTask()?.forgeRepo}
                  forgePR={selectedTask()?.forgePR}
                  ciStatus={selectedTask()?.ciStatus}
                  ciChecks={selectedTask()?.ciChecks}
                  harness={selectedTask()?.harness ?? ""}
                  model={selectedTask()?.model}
                  diffStat={selectedTask()?.diffStat}
                  supportsImages={harnesses().find((h) => h.name === (selectedTask()?.harness ?? ""))?.supportsImages}
                  onClose={() => navigate("/")}
                  inputDraft={inputDrafts().get(id) ?? ""}
                  onInputDraft={(v) => setInputDrafts((prev) => { const next = new Map(prev); next.set(id, v); return next; })}
                  inputImages={inputImageDrafts().get(id) ?? []}
                  onInputImages={(imgs) => setInputImageDrafts((prev) => { const next = new Map(prev); next.set(id, imgs); return next; })}
                />
              </div>
            )}
          </Match>
        </Switch>
      </div>
      <Show when={settingsOpen()}>
        <div
          class={styles.settingsOverlay}
          role="button"
          tabIndex={-1}
          onClick={() => setSettingsOpen(false)}
          onKeyDown={(e) => { if (e.key === "Escape") setSettingsOpen(false); }}
        >
          {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions -- click/key propagation stop is supplementary to the overlay backdrop dismiss */}
          <div
            class={styles.settingsPanel}
            role="dialog"
            aria-modal="true"
            aria-label="Settings"
            onClick={(e) => e.stopPropagation()}
            onKeyDown={(e) => e.stopPropagation()}
          >
            <h2 class={styles.settingsPanelTitle}>Settings</h2>
            <div class={styles.settingsSection}>
              <h3 class={styles.settingsSectionTitle}>Container</h3>
              <label class={styles.settingsLabel}>
                Docker image
                <input
                  type="text"
                  class={styles.settingsInput}
                  placeholder="ghcr.io/caic-xyz/md:latest"
                  value={selectedImage() || ""}
                  onChange={(e) => setSelectedImage(e.currentTarget.value)}
                  onBlur={async () => {
                    await updatePreferences(currentSettings());
                  }}
                />
              </label>
            </div>
            <div class={styles.settingsSection}>
              <h3 class={styles.settingsSectionTitle}>Well-known caches</h3>
              <div class={styles.cacheGrid}>
                <For each={wellKnownCachesList()}>
                  {(cache) => {
                    const state = () => wellKnownCaches()[cache.name];
                    const isEnabled = () => state() !== false;
                    return (
                      <label
                        class={styles.cacheCheckbox}
                        data-state={state() === undefined ? "default" : isEnabled() ? "enabled" : "disabled"}
                        title={cache.description}
                      >
                        <input
                          type="checkbox"
                          checked={isEnabled()}
                          onChange={async (e) => {
                            const newCaches = { ...wellKnownCaches() };
                            if (e.currentTarget.checked) {
                              newCaches[cache.name] = true;
                            } else {
                              newCaches[cache.name] = false;
                            }
                            setWellKnownCaches(newCaches);
                            await updatePreferences(currentSettings({ wellKnownCaches: newCaches }));
                          }}
                        />
                        {cache.name}
                      </label>
                    );
                  }}
                </For>
              </div>
            </div>
            <div class={styles.settingsSection}>
              <h3 class={styles.settingsSectionTitle}>Custom cache mappings</h3>
              <For each={cacheMappings()}>
                {(mapping, index) => (
                  <div class={styles.cacheMappingRow}>
                    <input
                      type="text"
                      class={styles.settingsInput}
                      placeholder="Host path"
                      value={mapping.hostPath}
                      onChange={(e) => {
                        const newMappings = [...cacheMappings()];
                        newMappings[index()].hostPath = e.currentTarget.value;
                        setCacheMappings(newMappings);
                      }}
                      onBlur={async () => {
                        await updatePreferences(currentSettings());
                      }}
                    />
                    <span class={styles.cacheMappingArrow}>→</span>
                    <input
                      type="text"
                      class={styles.settingsInput}
                      placeholder="Container path"
                      value={mapping.containerPath}
                      onChange={(e) => {
                        const newMappings = [...cacheMappings()];
                        newMappings[index()].containerPath = e.currentTarget.value;
                        setCacheMappings(newMappings);
                      }}
                      onBlur={async () => {
                        await updatePreferences(currentSettings());
                      }}
                    />
                    <button
                      class={styles.cacheMappingRemove}
                      onClick={async () => {
                        const newMappings = cacheMappings().filter((_, i) => i !== index());
                        setCacheMappings(newMappings);
                        await updatePreferences(currentSettings({ cacheMappings: newMappings }));
                      }}
                    >
                      ×
                    </button>
                  </div>
                )}
              </For>
              <button
                class={styles.settingsButton}
                onClick={() => {
                  setCacheMappings([...cacheMappings(), { hostPath: "", containerPath: "" }]);
                }}
              >
                + Add mapping
              </button>
            </div>
            <div class={styles.settingsSection}>
              <h3 class={styles.settingsSectionTitle}>Automation</h3>
              <label class={styles.settingsLabel}>
                <input
                  type="checkbox"
                  checked={autoFixCI()}
                  onChange={async (e) => {
                    const val = e.currentTarget.checked;
                    setAutoFixCI(val);
                    await updatePreferences(currentSettings({ autoFixOnCIFailure: val }));
                  }}
                />
                Auto-fix CI failures
              </label>
              <p class={styles.settingsDescription}>When CI fails on a PR and the agent has finished, automatically start a new task to fix it.</p>
              <label class={styles.settingsLabel}>
                <input
                  type="checkbox"
                  checked={autoFixPR()}
                  onChange={async (e) => {
                    const val = e.currentTarget.checked;
                    setAutoFixPR(val);
                    await updatePreferences(currentSettings({ autoFixOnPROpen: val }));
                  }}
                />
                Auto-fix PRs
              </label>
              <p class={styles.settingsDescription}>When a pull request is opened or reopened, automatically start a task to review and fix it.</p>
            </div>
          </div>
        </div>
      </Show>
      <VoiceOverlay tasks={tasks} recentRepo={() => repos()[0]?.path ?? ""} selectedHarness={selectedHarness} selectedModel={selectedModel} />
    </div>
    </Show>
  );
}
