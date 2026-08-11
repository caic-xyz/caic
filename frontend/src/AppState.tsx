// Application state store: owns task data, settings, SSE wiring, and task actions.
// Provided once near the router root and consumed by the shell, layout, and route panes.

import { createContext, createEffect, createSignal, onCleanup, useContext, type JSX } from "solid-js";
import { useNavigate, useLocation } from "@solidjs/router";

import type { Config, Harness, HarnessInfo, Repo, Task, TaskState, UsageResp, ImageData as APIImageData, CacheMappingResp, CacheSize, OAuthGrantResp, MountMappingResp, Platform, PreferencesResp, RuntimeInfo, WellKnownCachesResp, VersionResp } from "@sdk/types.gen";

import { useHostMode } from "./gomode/HostMode";

import { getConfig, getPreferences, updatePreferences, listOAuthGrants, revokeOAuthGrant, listHarnesses, listCaches, getCacheSizes, listRepos, createTask, cloneRepo, getUsage, forkTask, stopTask, purgeTask, reviveTask, botFixCI, getTask, globalTaskEvents, globalUsageEvents, getVersion, triggerUpdate } from "./api";
import type { RepoEntry } from "./components/RepoChipStrip";
import { useAuth } from "./AuthContext";
import { confirmTaskAction } from "./components/TaskCard";
import { requestNotificationPermission, notifyServiceEvent, notifyWaiting, dismissNotification } from "./gomode/notifications";
import { QuotaRecoveryTracker } from "./quota";
import { taskPath, taskIdFromPath, taskPathForTask } from "./taskPath";

/** Add ±25% jitter to a delay to avoid thundering herd on server restart. */
function jitteredDelay(base: number): number {
  return base * (0.75 + Math.random() * 0.5);
}

const inputNeededTaskStates = new Set<TaskState>(["waiting", "asking", "has_plan"]);
const otherAliveTaskStates = new Set<TaskState>([
  "pending",
  "branching",
  "provisioning",
  "starting",
  "pulling",
  "pushing",
]);

type PurgeFocusTarget =
  | { kind: "task"; task: Task }
  | { kind: "prompt" };

type PendingTaskUpdate =
  | { kind: "patch"; patch: Record<string, unknown> }
  | { kind: "replace" }
  | { kind: "delete" };

type TaskRecovery = {
  updates: PendingTaskUpdate[];
};

function createAppStore() {
  const navigate = useNavigate();
  const location = useLocation();
  const auth = useAuth();
  const hostMode = useHostMode();

  const [prompt, setPrompt] = createSignal("");
  const [tasks, setTasks] = createSignal<Task[]>([]);
  const [tasksLoading, setTasksLoading] = createSignal(true);
  const [submitting, setSubmitting] = createSignal(false);
  const [initializing, setInitializing] = createSignal(true);
  const [repos, setRepos] = createSignal<Repo[]>([]);
  const [selectedRepos, setSelectedRepos] = createSignal<RepoEntry[]>([]);
  const [selectedModel, setSelectedModel] = createSignal("");
  const [selectedEffort, setSelectedEffort] = createSignal("");
  const [selectedImage, setSelectedImage] = createSignal("");
  const [harnesses, setHarnesses] = createSignal<HarnessInfo[]>([]);
  const [selectedHarness, setSelectedHarness] = createSignal("");
  const [runtimes, setRuntimes] = createSignal<RuntimeInfo[]>([]);
  const [selectedRuntimeName, setSelectedRuntimeName] = createSignal("");
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
  const [voiceGatewayAvailable, setVoiceGatewayAvailable] = createSignal(false);
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
  const [settingsError, setSettingsError] = createSignal("");
  const [oauthGrants, setOAuthGrants] = createSignal<OAuthGrantResp[]>([]);
  const [oauthGrantError, setOAuthGrantError] = createSignal("");
  const [revokingOAuthGrantID, setRevokingOAuthGrantID] = createSignal<string | null>(null);
  const [versionInfo, setVersionInfo] = createSignal<VersionResp | null>(null);
  const [versionCheckError, setVersionCheckError] = createSignal("");
  const [updateStatus, setUpdateStatus] = createSignal<string>("");
  const [checkingUpdate, setCheckingUpdate] = createSignal(false);
  const [updating, setUpdating] = createSignal(false);
  let latestSettingsSave = 0;
  let settingsSaveQueue = Promise.resolve();

  createEffect(() => {
    const available = runtimes();
    if (available.length > 0 && !available.some((rt) => rt.name === selectedRuntimeName())) {
      setSelectedRuntimeName(available[0].name);
    }
  });

  /** Build the current settings payload for updatePreferences, with optional overrides. */
  const currentSettings = (overrides: Partial<Parameters<typeof updatePreferences>[0]["settings"]> = {}) => {
    const settings = {
      autoFixOnCIFailure: autoFixCI(),
      autoFixOnPROpen: autoFixPR(),
      baseImage: selectedImage() || "",
      containerPlatform: (containerPlatform() || "") as Platform,
      maxCPUs: maxCPUs(),
      runtimeName: selectedRuntimeName(),
      wellKnownCaches: wellKnownCaches() as Record<string, boolean>,
      cacheMappings: cacheMappings(),
      customMounts: customMounts(),
      ...overrides,
    };
    return {
      settings: {
        ...settings,
        cacheMappings: settings.cacheMappings.map(({ resolvedContainerPath: _, ...mapping }) => mapping),
        customMounts: settings.customMounts.map(({ resolvedContainerPath: _, ...mount }) => mount),
      },
    };
  };

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

  const selectRuntimeName = (runtimeName: string) => {
    setSelectedRuntimeName(runtimeName);
  };

  const applySettings = (settings: PreferencesResp["settings"]) => {
    setAutoFixCI(settings.autoFixOnCIFailure);
    setAutoFixPR(settings.autoFixOnPROpen);
    setMaxCPUs(settings.maxCPUs ?? 0);
    setSelectedRuntimeName(settings.runtimeName ?? selectedRuntimeName());
    setContainerPlatform(settings.containerPlatform ?? "");
    setWellKnownCaches(settings.wellKnownCaches ?? {});
    setCacheMappings(settings.cacheMappings ?? []);
    setCustomMounts(settings.customMounts ?? []);
  };

  const applyResolvedContainerPaths = (settings: PreferencesResp["settings"]) => {
    setCacheMappings((current) => current.map((mapping, i) => {
      const saved = settings.cacheMappings?.[i];
      if (!saved || saved.hostPath !== mapping.hostPath || saved.containerPath !== mapping.containerPath) {
        return { ...mapping, resolvedContainerPath: undefined };
      }
      return { ...mapping, resolvedContainerPath: saved.resolvedContainerPath };
    }));
    setCustomMounts((current) => current.map((mount, i) => {
      const saved = settings.customMounts?.[i];
      if (!saved || saved.hostPath !== mount.hostPath || saved.containerPath !== mount.containerPath) {
        return { ...mount, resolvedContainerPath: undefined };
      }
      return { ...mount, resolvedContainerPath: saved.resolvedContainerPath };
    }));
  };

  const applyServerConfig = (config: Config) => {
    const availableRuntimes = config.runtimes ?? [];
    setRuntimes(availableRuntimes);
    if (availableRuntimes.length > 0 && !availableRuntimes.some((rt) => rt.name === selectedRuntimeName())) {
      setSelectedRuntimeName(availableRuntimes[0].name);
    }
    setTailscaleAvailable(config.tailscaleAvailable);
    setUSBAvailable(config.usbAvailable);
    setDisplayAvailable(config.displayAvailable);
    setSudoAvailable(config.sudoAvailable);
    setGitHubTokenAvailable(config.gitHubTokenAvailable);
    setVoiceGatewayAvailable(config.voiceGateway.mode !== "disabled");
    const displayName = config.displayName || window.location.hostname.split('.')[0];
    document.title = `${displayName} — caic`;
  };

  async function refreshServerConfig() {
    try {
      applyServerConfig(await getConfig());
    } catch {
      setVoiceGatewayAvailable(false);
    }
  }

  const selectedId = (): string | null => taskIdFromPath(location.pathname);
  const selectedTask = (): Task | null => {
    const id = selectedId();
    return id !== null ? (tasks().find((t) => t.id === id) ?? null) : null;
  };
  const taskById = (id: string): Task | undefined => tasks().find((t) => t.id === id);

  function tasksInSidebarOrder(): Task[] {
    const byId = new Map(tasks().map((t) => [t.id, t]));
    const ordered = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]"))
      .map((el) => el.dataset.taskId ?? "")
      .map((id) => byId.get(id))
      .filter((t): t is Task => t !== undefined);
    return ordered.length > 0 ? ordered : tasks();
  }

  function purgeFocusTarget(id: string): PurgeFocusTarget {
    const ordered = tasksInSidebarOrder();
    const currentIdx = ordered.findIndex((t) => t.id === id);
    const rotated = currentIdx === -1
      ? ordered
      : ordered.slice(currentIdx + 1).concat(ordered.slice(0, currentIdx));
    const candidates = rotated.filter((t) => t.id !== id);
    const nextTask = candidates.find((t) => inputNeededTaskStates.has(t.state))
      ?? candidates.find((t) => t.state === "running")
      ?? candidates.find((t) => otherAliveTaskStates.has(t.state));
    return nextTask ? { kind: "task", task: nextTask } : { kind: "prompt" };
  }

  function focusTaskCard(id: string) {
    requestAnimationFrame(() => {
      const card = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]"))
        .find((el) => el.dataset.taskId === id);
      card?.focus();
    });
  }

  function focusPrompt() {
    requestAnimationFrame(() => {
      document.querySelector<HTMLElement>("[data-testid='prompt-input']")?.focus();
    });
  }

  function navigateToPurgeFocusTarget(target: PurgeFocusTarget) {
    if (target.kind === "task") {
      navigate(taskPathForTask(target.task), { replace: true });
      focusTaskCard(target.task.id);
      return;
    }
    navigate("/", { replace: true });
    focusPrompt();
  }

  // Insert or replace an authoritative task-list SSE update by ID, keeping
  // the id-sorted order.
  const upsertTask = (t: Task) => setTasks((prev) => {
    const idx = prev.findIndex((p) => p.id === t.id);
    if (idx >= 0) {
      const next = [...prev];
      next[idx] = t;
      return next;
    }
    return [...prev, t].sort((a, b) => (a.id < b.id ? -1 : 1));
  });

  // Seed a newly-created task only when task-list SSE has not arrived first.
  // The request response may otherwise overwrite a newer state transition.
  const seedTask = (t: Task) => setTasks((prev) => {
    if (prev.some((existing) => existing.id === t.id)) return prev;
    return [...prev, t].sort((a, b) => (a.id < b.id ? -1 : 1));
  });

  type EffortPreferences = Record<string, Record<string, string>>;

  // In-memory per-harness model and per-harness/model effort preferences from the server.
  let prefModels: Record<string, string> = {};
  let prefEfforts: EffortPreferences = {};
  const getPrefModel = (harness: string): string | undefined => prefModels[harness];
  const setPrefModel = (harness: string, model: string) => {
    if (model) prefModels[harness] = model;
    else delete prefModels[harness];
  };
  const getPrefEffort = (harness: string, model: string): string | undefined => prefEfforts[harness]?.[model] ?? prefEfforts[harness]?.[""];
  const setPrefEffort = (harness: string, model: string, effort: string) => {
    if (effort) {
      prefEfforts[harness] = { ...(prefEfforts[harness] ?? {}), [model]: effort };
      return;
    }
    if (!prefEfforts[harness]) return;
    const next = { ...prefEfforts[harness] };
    delete next[model];
    if (Object.keys(next).length > 0) prefEfforts[harness] = next;
    else delete prefEfforts[harness];
  };
  const selectedModelForHarness = (harness: string) => {
    const models = harnesses().find((x) => x.name === harness)?.models ?? [];
    const model = getPrefModel(harness);
    return model && models.some((candidate) => candidate.id === model) ? model : "";
  };
  const selectedEffortForModel = (harness: string, model: string) => {
    const harnessInfo = harnesses().find((x) => x.name === harness);
    const options = harnessInfo?.models.find((candidate) => candidate.id === model)?.effortOptions ?? [];
    const effort = getPrefEffort(harness, model);
    return effort && options.includes(effort) ? effort : "";
  };
  const selectHarness = (harness: string) => {
    const model = selectedModelForHarness(harness);
    setSelectedHarness(harness);
    setSelectedModel(model);
    setSelectedEffort(selectedEffortForModel(harness, model));
  };
  const selectModel = (model: string) => {
    setSelectedModel(model);
    setPrefModel(selectedHarness(), model);
    setSelectedEffort(selectedEffortForModel(selectedHarness(), model));
  };
  const selectEffort = (effort: string) => {
    setSelectedEffort(effort);
    setPrefEffort(selectedHarness(), selectedModel(), effort);
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
      // Don't intercept when typing in an input/textarea, or when a combobox
      // (e.g. the model/branch picker) owns the key — its own handler navigates.
      const target = e.target as HTMLElement | null;
      const tag = target?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
      if (target?.closest('[aria-haspopup="listbox"], [role="listbox"], [role="option"]')) return;
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
      navigate(taskPathForTask(task));
      card.focus();
      e.preventDefault();
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  }

  // Track previous task states to detect transitions to "waiting".
  let prevStates = new Map<string, string>();
  const quotaRecoveryTracker = new QuotaRecoveryTracker();
  const notifyQuotaRecoveries = (currentTasks: Task[]) => {
    for (const task of quotaRecoveryTracker.update(currentTasks)) {
      notifyServiceEvent(task.id, `${task.title} quota is available`, { enabled: hostMode.browserNotificationsEnabled() });
    }
  };
  const checkAndNotify = (task: Task) => {
    const needsInput = task.state === "waiting" || task.state === "asking" || task.state === "has_plan";
    const prevState = prevStates.get(task.id);
    const prevNeedsInput = prevState === "waiting" || prevState === "asking" || prevState === "has_plan";
    if (needsInput && prevState === "running") {
      notifyWaiting(task.id, task.title, { enabled: hostMode.browserNotificationsEnabled() });
    } else if (!needsInput && prevNeedsInput) {
      dismissNotification(task.id);
    }
  };
  const applyAuthoritativeTask = (task: Task) => {
    checkAndNotify(task);
    prevStates.set(task.id, task.state);
    upsertTask(task);
    notifyQuotaRecoveries(tasks());
  };
  const applyTaskPatch = (id: string, patch: Record<string, unknown>) => {
    if (typeof patch["state"] === "string") {
      const newState = patch["state"] as string;
      const existing = tasks().find((task) => task.id === id);
      if (existing) checkAndNotify({ ...existing, state: newState } as Task);
      prevStates.set(id, newState);
    }
    setTasks((prev) => {
      const idx = prev.findIndex((task) => task.id === id);
      if (idx < 0) return prev;
      const next = [...prev];
      next[idx] = { ...next[idx], ...patch } as Task;
      return next;
    });
    notifyQuotaRecoveries(tasks());
  };

  const updateWellKnownCacheSizes = (sizes: CacheSize[]) => {
    setWellKnownCacheSizes(Object.fromEntries(sizes.map((size) => [size.name, size])));
  };

  // Fetch version, MCP grant, and cache size info when the settings page opens.
  createEffect(() => {
    if (location.pathname !== "/settings") return;
    void (async () => {
      setCheckingUpdate(true);
      setVersionCheckError("");
      setOAuthGrantError("");
      try {
        const [v, sizes, grants] = await Promise.all([
          getVersion(),
          getCacheSizes().catch(() => null),
          auth.providers().length > 0 ? listOAuthGrants().catch((e: unknown) => {
            setOAuthGrantError(e instanceof Error ? e.message : "Could not load MCP clients");
            return null;
          }) : Promise.resolve(null),
        ]);
        setVersionInfo(v);
        if (sizes) updateWellKnownCacheSizes(sizes.wellKnown);
        if (grants) setOAuthGrants(grants.grants);
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

  function dismissSelectedTaskOnNotFound(id: string, err: unknown): boolean {
    if ((err as { status?: number }).status !== 404) return false;
    if (selectedId() === id) navigate("/", { replace: true });
    return true;
  }

  // Ensure the task named by the URL exists and is in the store. When it is not
  // (deep link, back button, another client), fetch it as a REST resource: a 404
  // is an authoritative "gone" → home; a 200 seeds the store so the detail view
  // renders with real state. Tasks this client created are already seeded via
  // upsertTask before navigation, so this is a no-op for fresh create/fork.
  // Deletion of the viewed task is handled authoritatively by the SSE "delete"
  // event above.
  // Task-list events received after a recovery GET begins are newer than its
  // response. Queue patches to replay their transitions in order; a snapshot
  // that includes the task or a complete upsert supersedes the GET and updates
  // the task directly.
  const taskRecoveries = new Map<string, TaskRecovery>();
  const queueTaskUpdate = (id: string, update: PendingTaskUpdate) => {
    taskRecoveries.get(id)?.updates.push(update);
  };
  const ensureTask = async (id: string) => {
    if (taskRecoveries.has(id)) return;
    const recovery: TaskRecovery = { updates: [] };
    taskRecoveries.set(id, recovery);
    try {
      let task: Task | null = null;
      let getTaskError: unknown = null;
      try {
        task = await getTask(id);
      } catch (e) {
        getTaskError = e;
      }
      // A newer snapshot or upsert supplied the complete task while this GET
      // was in flight. Its response is now stale, and later patches were
      // applied directly after that authoritative event.
      if (taskRecoveries.get(id) !== recovery) return;

      const updates = recovery.updates;
      let replayFrom = 0;
      for (const [index, update] of updates.entries()) {
        if (update.kind !== "patch") replayFrom = index + 1;
      }
      if (task && replayFrom === 0) {
        applyAuthoritativeTask(task);
      }
      for (const update of updates.slice(replayFrom)) {
        if (update.kind === "patch") applyTaskPatch(id, update.patch);
      }
      const latestBoundary = replayFrom === 0 ? null : updates[replayFrom - 1];
      if (task === null && getTaskError !== null && latestBoundary?.kind !== "replace") {
        // Only a definitive not-found is authoritative. Transient errors, 403s,
        // and 5xx responses keep the route so auth and server state can recover.
        dismissSelectedTaskOnNotFound(id, getTaskError);
      }
    } finally {
      if (taskRecoveries.get(id) === recovery) taskRecoveries.delete(id);
    }
  };
  createEffect(() => {
    const id = selectedId();
    if (id !== null && selectedTask() === null) void ensureTask(id);
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
          prefEfforts = prefs?.efforts ?? {};
          const prefHarness = prefs?.harness ?? "";
          const harness = prefHarness && h.find((x) => x.name === prefHarness)
            ? prefHarness
            : h[0]?.name ?? "";
          selectHarness(harness);
        }
        if (prefs?.settings?.baseImage) setSelectedImage(prefs.settings.baseImage);
        if (config) applyServerConfig(config);
        if (prefs?.settings) applySettings(prefs.settings);
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
        const res = await fetch("/auth/me", { signal: AbortSignal.timeout(5000) });
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
        if (event.kind === "snapshot" && event.snapshot) {
          const snapshotByID = new Map(event.snapshot.map((task) => [task.id, task]));
          for (const id of taskRecoveries.keys()) {
            if (snapshotByID.has(id)) {
              // The snapshot is newer than the recovery GET and contains a
              // complete task. Let later patches update it in place.
              taskRecoveries.delete(id);
            } else {
              queueTaskUpdate(id, { kind: "delete" });
            }
          }
          prevStates = new Map(event.snapshot.map((t) => [t.id, t.state]));
          setTasks(event.snapshot);
          setTasksLoading(false);
          notifyQuotaRecoveries(event.snapshot);
        } else if (event.kind === "upsert" && event.upsert) {
          const task = event.upsert;
          // A complete upsert supersedes a recovery GET. Removing its entry
          // also makes later patches update this task in place.
          taskRecoveries.delete(task.id);
          applyAuthoritativeTask(task);
        } else if (event.kind === "patch" && event.patch) {
          const patch = event.patch as Record<string, unknown>;
          const id = patch["id"] as string;
          if (!id) return;
          if (taskRecoveries.has(id)) {
            queueTaskUpdate(id, { kind: "patch", patch });
            return;
          }
          if (!tasks().some((task) => task.id === id)) {
            void ensureTask(id);
            return;
          }
          applyTaskPatch(id, patch);
        } else if (event.kind === "delete" && event.delete) {
          // Authoritative removal: if the deleted task is the one being viewed,
          // leave its now-dead detail route.
          if (event.delete === selectedId()) navigate("/", { replace: true });
          if (taskRecoveries.has(event.delete)) queueTaskUpdate(event.delete, { kind: "delete" });
          prevStates.delete(event.delete);
          setTasks((prev) => prev.filter((t) => t.id !== event.delete));
          notifyQuotaRecoveries(tasks());
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
        void refreshServerConfig();
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
    if (t && (t.state === "purging" || t.state === "purged" || t.state === "failed" || t.state === "crashed" || t.state === "stopping" || t.state === "stopped" || t.state === "provisioning")) {
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
    const target = selectedId() === id ? purgeFocusTarget(id) : null;
    setActionId(id);
    try {
      await purgeTask(id);
      if (target && selectedId() === id) navigateToPurgeFocusTarget(target);
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
  // Fork dialog harness/model/effort selection, mirroring selectHarness/selectModel/selectEffort.
  const selectForkHarness = (harness: string) => {
    const model = selectedModelForHarness(harness);
    setForkHarness(harness);
    setForkModel(model);
    setForkEffort(selectedEffortForModel(harness, model));
  };
  const selectForkModel = (model: string) => {
    setForkModel(model);
    setPrefModel(forkHarness(), model);
    setForkEffort(selectedEffortForModel(forkHarness(), model));
  };
  const selectForkEffort = (effort: string) => {
    setForkEffort(effort);
    setPrefEffort(forkHarness(), forkModel(), effort);
  };

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
    const harness = task?.harness ?? selectedHarness();
    const model = selectedModelForHarness(harness);
    setForkTaskId(id);
    setForkPrompt("");
    setForkHarness(harness);
    setForkModel(model);
    setForkEffort(selectedEffortForModel(harness, model));
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
      seedTask(resp);
      navigate(taskPath(
        resp.id,
        resp.repos?.[0]?.name ?? sourceTask?.repos?.[0]?.name ?? "",
        resp.repos?.[0]?.branch ?? "",
        text,
      ));
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
      const runtimeName = selectedRuntimeName();
      const repoSpecs = selRepos.length > 0 ? selRepos.map((r) => ({ name: r.path, ...(r.branch ? { baseBranch: r.branch } : {}) })) : undefined;
      const data = await createTask({ initialPrompt: { text: p, ...(imgs.length > 0 ? { images: imgs } : {}) }, repos: repoSpecs, harness: harness as Harness, ...(runtimeName ? { runtimeName } : {}), ...(model ? { model } : {}), ...(effort ? { effort } : {}), ...(ts ? { tailscale: true } : {}), ...(usb ? { usb: true } : {}), ...(disp ? { display: true } : {}), ...(sudo ? { sudo: true } : {}), ...(ght ? { gitHubToken: true } : {}) });
      setPrefModel(harness, model);
      setPrefEffort(harness, model, effort);
      setPrompt("");
      setPendingImages([]);
      seedTask(data);
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

  function saveSettings(overrides: Partial<Parameters<typeof updatePreferences>[0]["settings"]> = {}): Promise<void> {
    const saveID = ++latestSettingsSave;
    const settings = currentSettings(overrides);
    setSettingsError("");
    settingsSaveQueue = settingsSaveQueue.then(async () => {
      try {
        const preferences = await updatePreferences(settings);
        if (saveID === latestSettingsSave) applyResolvedContainerPaths(preferences.settings);
      } catch (e: unknown) {
        if (saveID === latestSettingsSave) setSettingsError(e instanceof Error ? e.message : "Could not save settings");
      }
    });
    return settingsSaveQueue;
  }

  async function revokeOAuthClientGrant(grantID: string) {
    setRevokingOAuthGrantID(grantID);
    setOAuthGrantError("");
    try {
      await revokeOAuthGrant(grantID, {});
      const grants = await listOAuthGrants();
      setOAuthGrants(grants.grants);
    } catch (e: unknown) {
      setOAuthGrantError(e instanceof Error ? e.message : "Could not revoke MCP client");
    } finally {
      setRevokingOAuthGrantID(null);
    }
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
    navigate(found ? taskPathForTask(found) : `/task/@${id}`);
  };
  const navigateToDiff = (id: string) => {
    const found = taskById(id);
    if (found?.diffStat?.length) {
      navigate(taskPathForTask(found) + "/diff");
    }
  };
  const fixCI = (repoPath: string) => {
    void botFixCI({ repo: repoPath }).then((data) => {
      seedTask(data);
      navigate(taskPath(data.id, repoPath, "", `Fix CI: ${repoPath}`));
    });
  };

  // Per-task input/image drafts, keyed by task ID.
  const inputDraft = (id: string) => inputDrafts().get(id) ?? "";
  const setInputDraft = (id: string, v: string) => setInputDrafts((prev) => { const next = new Map(prev); next.set(id, v); return next; });
  const inputImages = (id: string) => inputImageDrafts().get(id) ?? [];
  const setInputImages = (id: string, imgs: APIImageData[]) => setInputImageDrafts((prev) => { const next = new Map(prev); next.set(id, imgs); return next; });

  return {
    navigate,
    auth,
    // task data + selection
    tasks, tasksLoading, repos, selectedId, selectedTask, taskById, dismissSelectedTaskOnNotFound,
    // new-task form
    prompt, setPrompt, selectedRepos, setSelectedRepos, selectedModel, setSelectedModel: selectModel,
    selectedEffort, setSelectedEffort: selectEffort, selectedHarness, setSelectedHarness: selectHarness, harnesses,
    runtimes, selectedRuntimeName, setSelectedRuntimeName: selectRuntimeName,
    harnessSupportsImages, pendingImages, setPendingImages, availableRecent, availableRest,
    getPrefModel, setPrefModel, getPrefEffort, setPrefEffort, initializing, submitting, submitTask,
    // capability toggles
    tailscaleAvailable, tailscaleEnabled, setTailscaleEnabled,
    usbAvailable, usbEnabled, setUSBEnabled,
    displayAvailable, displayEnabled, setDisplayEnabled,
    sudoAvailable, sudoEnabled, setSudoEnabled,
    gitHubTokenAvailable, gitHubTokenEnabled, setGitHubTokenEnabled,
    voiceGatewayAvailable,
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
    forkTaskId, setForkTaskId, forkPrompt, setForkPrompt, forkHarness, setForkHarness: selectForkHarness,
    forkModel, setForkModel: selectForkModel, forkEffort, setForkEffort: selectForkEffort, forkExtraRepos, setForkExtraRepos,
    forkTailscale, setForkTailscale, forkUSB, setForkUSB, forkDisplay, setForkDisplay,
    forkSudo, setForkSudo, forkGitHubToken, setForkGitHubToken,
    forkAvailableRecent, forkAvailableRest, submitFork,
    // settings
    selectedImage, setSelectedImage, containerPlatform, setContainerPlatform,
    maxCPUs, setMaxCPUs, wellKnownCaches, setWellKnownCaches,
    wellKnownCachesList, wellKnownCacheSizes, cacheMappings, setCacheMappings, customMounts, setCustomMounts, settingsError,
    autoFixCI, setAutoFixCI, autoFixPR, setAutoFixPR,
    oauthGrants, oauthGrantError, revokingOAuthGrantID, revokeOAuthClientGrant,
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
