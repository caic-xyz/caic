// Application state store: owns task data, settings, SSE wiring, and task actions.
// Provided once near the router root and consumed by the shell, layout, and route panes.
import { createContext, createEffect, createSignal, onCleanup, useContext, type JSX } from "solid-js";
import { useNavigate, useLocation } from "@solidjs/router";
import type { Harness, HarnessInfo, Repo, Task, UsageResp, ImageData as APIImageData, CacheMappingResp, CacheSize, MountMappingResp, Platform, WellKnownCachesResp, VersionResp } from "@sdk/types.gen";
import { getConfig, getPreferences, updatePreferences, listHarnesses, listCaches, getCacheSizes, listRepos, createTask, cloneRepo, getUsage, forkTask, stopTask, purgeTask, reviveTask, botFixCI, globalTaskEvents, globalUsageEvents, getVersion, triggerUpdate } from "./api";
import type { RepoEntry } from "./components/RepoChipStrip";
import { useAuth } from "./AuthContext";
import { confirmTaskAction } from "./components/TaskCard";
import { requestNotificationPermission, notifyWaiting, dismissNotification } from "./notifications";
import { taskPath, taskIdFromPath } from "./taskPath";
import { useHostMode } from "./hostMode";

/** Add ±25% jitter to a delay to avoid thundering herd on server restart. */
function jitteredDelay(base: number): number {
  return base * (0.75 + Math.random() * 0.5);
}

function createAppStore() {
  const navigate = useNavigate();
  const location = useLocation();
  const auth = useAuth();
  const hostMode = useHostMode();

  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<Task[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [initializing, setInitializing] = createSignal(true);
  const [repos, setRepos] = createSignal<Repo[]>([]);
  const [selectedRepos, setSelectedRepos] = createSignal<RepoEntry[]>([]);
  const [selectedModel, setSelectedModel] = createSignal("");
  const [selectedEffort, setSelectedEffort] = createSignal("");
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
  const [sudoAvailable, setSudoAvailable] = createSignal(false);
  const [sudoEnabled, setSudoEnabled] = createSignal(false);
  const [gitHubTokenAvailable, setGitHubTokenAvailable] = createSignal(false);
  const [gitHubTokenEnabled, setGitHubTokenEnabled] = createSignal(false);
  const [recentCount, setRecentCount] = createSignal(0);
  const [actionId, setActionId] = createSignal<string | null>(null);

  const [autoFixCI, setAutoFixCI] = createSignal(false);
  const [autoFixPR, setAutoFixPR] = createSignal(false);
  const [maxCPUs, setMaxCPUs] = createSignal(0);
  const [containerPlatform, setContainerPlatform] = createSignal("");
  const [wellKnownCaches, setWellKnownCaches] = createSignal<Record<string, boolean | undefined>>({});
  const [wellKnownCachesList, setWellKnownCachesList] = createSignal<WellKnownCachesResp["wellKnown"]>([]);
  const [wellKnownCacheSizes, setWellKnownCacheSizes] = createSignal<Record<string, CacheSize | undefined>>({});
  const [cacheMappings, setCacheMappings] = createSignal<CacheMappingResp[]>([]);
  const [customMounts, setCustomMounts] = createSignal<MountMappingResp[]>([]);
  const [versionInfo, setVersionInfo] = createSignal<VersionResp | null>(null);
  const [versionCheckError, setVersionCheckError] = createSignal("");
  const [updateStatus, setUpdateStatus] = createSignal<string>("");
  const [checkingUpdate, setCheckingUpdate] = createSignal(false);
  const [updating, setUpdating] = createSignal(false);

  /** Build the current settings payload for updatePreferences, with optional overrides. */
  const currentSettings = (overrides: Partial<Parameters<typeof updatePreferences>[0]["settings"]> = {}) => ({
    settings: {
      autoFixOnCIFailure: autoFixCI(),
      autoFixOnPROpen: autoFixPR(),
      baseImage: selectedImage() || "",
      containerPlatform: (containerPlatform() || "") as Platform,
      maxCPUs: maxCPUs(),
      wellKnownCaches: wellKnownCaches() as Record<string, boolean>,
      cacheMappings: cacheMappings(),
      customMounts: customMounts(),
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

  // Transient server warnings shown as auto-dismissing toasts.
  const [warnings, setWarnings] = createSignal<{ id: number; message: string }[]>([]);
  let nextWarningId = 0;
  function showWarning(message: string) {
    const id = nextWarningId++;
    setWarnings((prev) => [...prev, { id, message }]);
    setTimeout(() => setWarnings((prev) => prev.filter((w) => w.id !== id)), 8000);
  }
  const dismissWarning = (id: number) => setWarnings((prev) => prev.filter((w) => w.id !== id));

  const harnessSupportsImages = () => harnesses().find((h) => h.name === selectedHarness())?.supportsImages ?? false;

  const selectedId = (): string | null => taskIdFromPath(location.pathname);
  const selectedTask = (): Task | null => {
    const id = selectedId();
    return id !== null ? (tasks().find((t) => t.id === id) ?? null) : null;
  };
  const taskById = (id: string): Task | undefined => tasks().find((t) => t.id === id);

  // In-memory per-harness model preferences from the server.
  let prefModels: Record<string, string> = {};
  const getPrefModel = (harness: string): string | undefined => prefModels[harness];
  const setPrefModel = (harness: string, model: string) => {
    if (model) prefModels[harness] = model;
    else delete prefModels[harness];
  };

  // Global keyboard shortcuts:
  // - ArrowUp/ArrowDown: switch to previous/next task in sidebar order
  // - Shift+Delete: purge the currently selected task
  {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Delete" && e.shiftKey && selectedId() !== null) {
        const t = selectedTask();
        if (t) {
          e.preventDefault();
          const terminalPurge = new Set(["stopping", "purging", "purged", "failed"]);
          if (!terminalPurge.has(t.state) && confirmTaskAction("Purge", t.title, t.repos?.[0]?.branch ?? "")) {
            handlePurge(t.id);
          }
        }
        return;
      }
      if (e.key !== "ArrowUp" && e.key !== "ArrowDown") return;
      // Don't intercept when typing in an input/textarea.
      const tag = (e.target as HTMLElement)?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
      // Query visible task cards in DOM (visual) order to match the grouped sidebar layout.
      const cards = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]"));
      if (cards.length === 0) return;
      const curIdx = cards.findIndex((el) => el.dataset.taskId === selectedId());
      let nextIdx: number;
      if (e.key === "ArrowUp") {
        nextIdx = curIdx <= 0 ? cards.length - 1 : curIdx - 1;
      } else {
        nextIdx = curIdx === -1 || curIdx >= cards.length - 1 ? 0 : curIdx + 1;
      }
      const card = cards[nextIdx];
      const id = card.dataset.taskId;
      if (!id) return;
      const task = tasks().find((t) => t.id === id);
      if (!task) return;
      navigate(taskPath(task.id, task.repos?.[0]?.name ?? "", task.repos?.[0]?.branch ?? "", task.title));
      card.focus();
      e.preventDefault();
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  }

  // Track previous task states to detect transitions to "waiting".
  let prevStates = new Map<string, string>();

  const updateWellKnownCacheSizes = (sizes: CacheSize[]) => {
    setWellKnownCacheSizes(Object.fromEntries(sizes.map((size) => [size.name, size])));
  };

  // Fetch version and cache size info when the settings page opens.
  createEffect(() => {
    if (location.pathname !== "/settings") return;
    void (async () => {
      setCheckingUpdate(true);
      setVersionCheckError("");
      try {
        const [v, sizes] = await Promise.all([
          getVersion(),
          getCacheSizes().catch(() => null),
        ]);
        setVersionInfo(v);
        if (sizes) updateWellKnownCacheSizes(sizes.wellKnown);
      } catch (e: unknown) {
        setVersionCheckError(e instanceof Error ? e.message : "Version check failed");
      } finally {
        setCheckingUpdate(false);
      }
    })();
  });

  // Tick every second for live elapsed-time display.
  const [now, setNow] = createSignal(Date.now());
  {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    onCleanup(() => clearInterval(timer));
  }

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

  const isAuthenticated = () => auth.ready() && (auth.providers().length === 0 || auth.user() !== null);

  // Load initial data once authentication is confirmed.
  let dataLoaded = false;
  createEffect(() => {
    if (!isAuthenticated() || dataLoaded) return;
    dataLoaded = true;
    void (async () => {
      try {
        const [data, prefs, h, config, usageData, cachesData, cacheSizesData] = await Promise.all([
          listRepos(),
          getPreferences().catch(() => null),
          listHarnesses().catch(() => [] as HarnessInfo[]),
          getConfig().catch(() => null),
          getUsage().catch(() => null),
          listCaches().catch(() => null) as Promise<WellKnownCachesResp | null>,
          getCacheSizes().catch(() => null),
        ]);
        if (cachesData) setWellKnownCachesList(cachesData.wellKnown);
        if (cacheSizesData) updateWellKnownCacheSizes(cacheSizesData.wellKnown);
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
          setSudoAvailable(config.sudoAvailable);
          setGitHubTokenAvailable(config.gitHubTokenAvailable);
          const displayName = config.displayName || window.location.hostname.split('.')[0];
          document.title = `${displayName} — caic`;
        }
        if (prefs?.settings) {
          setAutoFixCI(prefs.settings.autoFixOnCIFailure);
          setAutoFixPR(prefs.settings.autoFixOnPROpen);
          setMaxCPUs(prefs.settings.maxCPUs ?? 0);
          setContainerPlatform(prefs.settings.containerPlatform ?? "");
          setWellKnownCaches(prefs.settings.wellKnownCaches ?? {});
          setCacheMappings(prefs.settings.cacheMappings ?? []);
          setCustomMounts(prefs.settings.customMounts ?? []);
        }
        if (usageData) setUsage(usageData);
      } finally {
        setInitializing(false);
      }
    })();
  });

  // Subscribe to task list updates via SSE with automatic reconnection.
  // Backoff: 500ms × 1.5 each failure, capped at 30s with ±25% jitter, reset on success.
  // On 401, stop retrying and clear auth state so the login page shows.
  // On reconnect, check if the frontend was rebuilt and reload if so.
  // Pauses reconnection when tab is hidden or browser goes offline.
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
        const res = await fetch("/api/caic/v1/auth/me", { signal: AbortSignal.timeout(5000) });
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
      // eslint-disable-next-line solid/reactivity -- globalTaskEvents is an SSE event handler
      taskES = globalTaskEvents((event) => {
        const checkAndNotify = (t: Task) => {
          const needsInput = t.state === "waiting" || t.state === "asking" || t.state === "has_plan";
          const prevState = prevStates.get(t.id);
          const prevNeedsInput = prevState === "waiting" || prevState === "asking" || prevState === "has_plan";
          if (needsInput && prevState === "running") {
            notifyWaiting(t.id, t.title, { enabled: hostMode.browserNotificationsEnabled() });
          } else if (!needsInput && prevNeedsInput) {
            dismissNotification(t.id);
          }
        };
        if (event.kind === "snapshot" && event.snapshot) {
          prevStates = new Map(event.snapshot.map((t) => [t.id, t.state]));
          setTasks(event.snapshot);
        } else if (event.kind === "upsert" && event.upsert) {
          const t = event.upsert;
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
        } else if (event.kind === "delete" && event.delete) {
          prevStates.delete(event.delete);
          setTasks((prev) => prev.filter((t) => t.id !== event.delete));
        } else if (event.kind === "repos" && event.repos) {
          const updatedRepos = event.repos;
          setRepos((prev) => {
            // Merge updated CI status into existing repo order.
            const byPath = new Map(updatedRepos.map((r) => [r.path, r]));
            return prev.map((r) => byPath.get(r.path) ?? r);
          });
        } else if (event.kind === "warning" && event.warning) {
          showWarning(event.warning);
        }
      }, (err) => {
        const msg = err instanceof Error ? err.message : String(err);
        showWarning(`Task list event error: ${msg}`);
      });
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
      taskES.onerror = () => {
        taskES?.close();
        taskES = null;
        setConnected(false);
        if (taskTimer !== null) clearTimeout(taskTimer);
        checkUnauthorized().then((is401) => {
          if (is401) return; // Stop retrying; effect restarts after re-login.
          taskTimer = setTimeout(connectTasks, jitteredDelay(taskDelay));
          taskDelay = Math.min(taskDelay * 1.5, 30_000);
        });
      };
    }

    function connectUsage() {
      usageES = globalUsageEvents((event) => {
        setUsage(event);
      }, (err) => {
        const msg = err instanceof Error ? err.message : String(err);
        showWarning(`Usage event error: ${msg}`);
      });
      usageES.addEventListener("open", () => {
        onOpen();
        usageDelay = 500;
      });
      usageES.onerror = () => {
        usageES?.close();
        usageES = null;
        if (usageTimer !== null) clearTimeout(usageTimer);
        checkUnauthorized().then((is401) => {
          if (is401) return;
          usageTimer = setTimeout(connectUsage, jitteredDelay(usageDelay));
          usageDelay = Math.min(usageDelay * 1.5, 30_000);
        });
      };
    }

    function closeAll() {
      taskES?.close();
      taskES = null;
      usageES?.close();
      usageES = null;
      if (taskTimer !== null) clearTimeout(taskTimer);
      taskTimer = null;
      if (usageTimer !== null) clearTimeout(usageTimer);
      usageTimer = null;
    }

    function connectAll() {
      closeAll();
      taskDelay = 500;
      usageDelay = 500;
      connectTasks();
      connectUsage();
    }

    /** Pause reconnection when tab is hidden or offline; reconnect immediately when back. */
    function onVisibilityChange() {
      if (document.hidden) {
        closeAll();
        setConnected(false);
      } else if (navigator.onLine) {
        connectAll();
      }
    }

    function onOnline() {
      if (!document.hidden) connectAll();
    }

    function onOffline() {
      closeAll();
      setConnected(false);
    }

    createEffect(() => {
      if (!isAuthenticated()) return;
      connectAll();
      document.addEventListener("visibilitychange", onVisibilityChange);
      window.addEventListener("online", onOnline);
      window.addEventListener("offline", onOffline);
      onCleanup(() => {
        closeAll();
        document.removeEventListener("visibilitychange", onVisibilityChange);
        window.removeEventListener("online", onOnline);
        window.removeEventListener("offline", onOffline);
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

  // Fork dialog state.
  const [forkTaskId, setForkTaskId] = createSignal<string | null>(null);
  const [forkPrompt, setForkPrompt] = createSignal("");
  const [forkHarness, setForkHarness] = createSignal("");
  const [forkModel, setForkModel] = createSignal("");
  const [forkEffort, setForkEffort] = createSignal("");
  const [forkExtraRepos, setForkExtraRepos] = createSignal<RepoEntry[]>([]);
  const [forkTailscale, setForkTailscale] = createSignal(false);
  const [forkUSB, setForkUSB] = createSignal(false);
  const [forkDisplay, setForkDisplay] = createSignal(false);
  const [forkSudo, setForkSudo] = createSignal(false);
  const [forkGitHubToken, setForkGitHubToken] = createSignal(false);

  // Repos available to add in the fork dialog (exclude already-selected extras and source task repos).
  const forkSourceRepoPaths = () => {
    const id = forkTaskId();
    if (!id) return new Set<string>();
    const task = tasks().find((t) => t.id === id);
    return new Set((task?.repos ?? []).map((r) => r.name));
  };
  const forkAvailableRecent = () => repos().slice(0, recentCount()).filter((r) => !forkSourceRepoPaths().has(r.path) && !forkExtraRepos().some((s) => s.path === r.path));
  const forkAvailableRest = () => repos().slice(recentCount()).filter((r) => !forkSourceRepoPaths().has(r.path) && !forkExtraRepos().some((s) => s.path === r.path));

  function handleFork(id: string) {
    const task = tasks().find((t) => t.id === id);
    setForkTaskId(id);
    setForkPrompt("");
    setForkHarness(task?.harness ?? selectedHarness());
    setForkModel(task?.model ?? "");
    setForkEffort(task?.effort ?? "");
    setForkExtraRepos([]);
    setForkTailscale(task?.runtime?.tailscale === "true" || task?.runtime?.tailscale?.startsWith("https://") || false);
    setForkUSB(task?.runtime?.usb ?? false);
    setForkDisplay(task?.runtime?.display ?? false);
    setForkSudo(task?.runtime?.sudo ?? false);
    setForkGitHubToken(task?.gitHubToken ?? false);
  }

  async function submitFork() {
    const id = forkTaskId();
    const text = forkPrompt().trim();
    if (!id || !text) return;
    setForkTaskId(null);
    try {
      const h = forkHarness();
      const m = forkModel();
      const e = forkEffort();
      const extras = forkExtraRepos();
      const sourceTask = tasks().find((t) => t.id === id);
      const resp = await forkTask(id, {
        prompt: { text },
        harness: h !== (sourceTask?.harness ?? "") ? h as Harness : undefined,
        model: m !== (sourceTask?.model ?? "") ? m : undefined,
        effort: e !== (sourceTask?.effort ?? "") ? e : undefined,
        extraRepos: extras.length > 0 ? extras.map((r) => ({ name: r.path, ...(r.branch ? { baseBranch: r.branch } : {}) })) : undefined,
        tailscale: forkTailscale(),
        usb: forkUSB(),
        display: forkDisplay(),
        sudo: forkSudo(),
        gitHubToken: forkGitHubToken(),
      });
      navigate(`/task/${resp.id}`);
    } catch {
      // Fork failed — no state to clean up.
    }
  }

  async function submitTask() {
    const p = prompt().trim();
    const imgs = pendingImages();
    const selRepos = selectedRepos();
    if (!p && imgs.length === 0) return;
    requestNotificationPermission({ enabled: hostMode.browserNotificationsEnabled() });
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
      const effort = selectedEffort();
      const ts = tailscaleEnabled();
      const usb = usbEnabled();
      const disp = displayEnabled();
      const sudo = sudoEnabled();
      const ght = gitHubTokenEnabled();
      const harness = selectedHarness();
      const repoSpecs = selRepos.length > 0 ? selRepos.map((r) => ({ name: r.path, ...(r.branch ? { baseBranch: r.branch } : {}) })) : undefined;
      const data = await createTask({ initialPrompt: { text: p, ...(imgs.length > 0 ? { images: imgs } : {}) }, repos: repoSpecs, harness: harness as Harness, ...(model ? { model } : {}), ...(effort ? { effort } : {}), ...(ts ? { tailscale: true } : {}), ...(usb ? { usb: true } : {}), ...(disp ? { display: true } : {}), ...(sudo ? { sudo: true } : {}), ...(ght ? { gitHubToken: true } : {}) });
      setPrefModel(harness, model);
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

  async function saveSettings(overrides: Partial<Parameters<typeof updatePreferences>[0]["settings"]> = {}) {
    await updatePreferences(currentSettings(overrides));
  }

  async function triggerServerUpdate() {
    setUpdating(true);
    setUpdateStatus("");
    try {
      const resp = await triggerUpdate();
      setUpdateStatus(resp.status === "started" ? "Update started in background. The server will restart shortly." : "Already up to date.");
    } catch (e: unknown) {
      setUpdateStatus(e instanceof Error ? e.message : "Update failed");
    } finally {
      setUpdating(false);
    }
  }

  // Navigate to a task's detail route, building the slugged path from its repo/branch/title.
  const navigateToTask = (id: string) => {
    const found = taskById(id);
    navigate(found ? taskPath(found.id, found.repos?.[0]?.name ?? "", found.repos?.[0]?.branch ?? "", found.title) : `/task/@${id}`);
  };
  const navigateToDiff = (id: string) => {
    const found = taskById(id);
    if (found?.diffStat?.length) {
      navigate(taskPath(found.id, found.repos?.[0]?.name ?? "", found.repos?.[0]?.branch ?? "", found.title) + "/diff");
    }
  };
  const fixCI = (repoPath: string) => {
    void botFixCI({ repo: repoPath }).then((data) => {
      navigate(taskPath(data.id, repoPath, "", `Fix CI: ${repoPath}`));
    });
  };

  // Per-task input/image drafts, keyed by task ID.
  const inputDraft = (id: string) => inputDrafts().get(id) ?? "";
  const setInputDraft = (id: string, v: string) => setInputDrafts((prev) => { const next = new Map(prev); next.set(id, v); return next; });
  const inputImages = (id: string) => inputImageDrafts().get(id) ?? [];
  const setInputImages = (id: string, imgs: APIImageData[]) => setInputImageDrafts((prev) => { const next = new Map(prev); next.set(id, imgs); return next; });

  const serverCaps = () => ({
    tailscaleAvailable: tailscaleAvailable(),
    usbAvailable: usbAvailable(),
    displayAvailable: displayAvailable(),
    sudoAvailable: sudoAvailable(),
    gitHubTokenAvailable: gitHubTokenAvailable(),
  });

  return {
    navigate,
    auth,
    // task data + selection
    tasks, repos, selectedId, selectedTask, taskById,
    // new-task form
    prompt, setPrompt, selectedRepos, setSelectedRepos, selectedModel, setSelectedModel,
    selectedEffort, setSelectedEffort, selectedHarness, setSelectedHarness, harnesses,
    harnessSupportsImages, pendingImages, setPendingImages, availableRecent, availableRest,
    getPrefModel, setPrefModel, initializing, submitting, submitTask,
    // capability toggles
    tailscaleAvailable, tailscaleEnabled, setTailscaleEnabled,
    usbAvailable, usbEnabled, setUSBEnabled,
    displayAvailable, displayEnabled, setDisplayEnabled,
    sudoAvailable, sudoEnabled, setSudoEnabled,
    gitHubTokenAvailable, gitHubTokenEnabled, setGitHubTokenEnabled,
    serverCaps,
    // sidebar + actions
    sidebarOpen, setSidebarOpen, now, actionId, handleStop, handlePurge, handleRevive, handleFork,
    navigateToTask, navigateToDiff, fixCI,
    // input drafts
    inputDraft, setInputDraft, inputImages, setInputImages,
    // warnings
    warnings, showWarning, dismissWarning,
    // clone dialog
    cloneOpen, setCloneOpen, cloning, cloneError, setCloneError, submitClone,
    // fork dialog
    forkTaskId, setForkTaskId, forkPrompt, setForkPrompt, forkHarness, setForkHarness,
    forkModel, setForkModel, forkEffort, setForkEffort, forkExtraRepos, setForkExtraRepos,
    forkTailscale, setForkTailscale, forkUSB, setForkUSB, forkDisplay, setForkDisplay,
    forkSudo, setForkSudo, forkGitHubToken, setForkGitHubToken,
    forkAvailableRecent, forkAvailableRest, submitFork,
    // settings
    selectedImage, setSelectedImage, containerPlatform, setContainerPlatform,
    maxCPUs, setMaxCPUs, wellKnownCaches, setWellKnownCaches,
    wellKnownCachesList, wellKnownCacheSizes, cacheMappings, setCacheMappings, customMounts, setCustomMounts,
    autoFixCI, setAutoFixCI, autoFixPR, setAutoFixPR,
    versionInfo, versionCheckError, checkingUpdate, updating, updateStatus, saveSettings, triggerServerUpdate,
    // usage + connection
    usage, connected,
  };
}

export type AppStore = ReturnType<typeof createAppStore>;

const AppStateContext = createContext<AppStore>();

export function AppStateProvider(props: { children: JSX.Element }) {
  const store = createAppStore();
  return <AppStateContext.Provider value={store}>{props.children}</AppStateContext.Provider>;
}

export function useAppState(): AppStore {
  const ctx = useContext(AppStateContext);
  if (!ctx) throw new Error("useAppState must be used within an AppStateProvider");
  return ctx;
}
