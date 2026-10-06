// Application state store for SSE-owned task membership, selected REST detail, settings, warnings, actions, caches, and Blob image drafts.
// Provided once near the router root and consumed by the shell, layout, and route panes.

import { batch, createContext, createEffect, createSignal, onCleanup, useContext, type JSX } from "solid-js";
import { createStore } from "solid-js/store";
import { useNavigate, useLocation } from "@solidjs/router";

import type {
  Config,
  Harness,
  HarnessInfo,
  ImageRefreshStatus,
  Repo,
  Task,
  TaskState,
  UsageResp,
  ImageConstraints,
  CacheMappingResp,
  CacheSize,
  OAuthGrantResp,
  MountMappingResp,
  RuntimeSettings,
  PreferencesResp,
  RuntimeInfo,
  WellKnownCachesResp,
  VersionResp,
  Warning as ServerWarning,
  WarningCategory,
  WarningDetail,
} from "@sdk/types.gen";

import { useHostMode } from "@maruel/gomode/web/HostMode";

import type { RepoEntry } from "./components/RepoChipStrip";
import { useAuth } from "./AuthContext";
import { notifications } from "@maruel/gomode/web/notifications";
import { QuotaRecoveryTracker, quotaDismissalKey } from "./quota";
import { quotaRecoveryTargets } from "./quotaTargets";
import { taskPath, taskIdFromPath, taskPathForTask } from "./taskPath";
import { evictTaskDiff, invalidateTaskDiff, taskDiffCache } from "./diffCache";
import { api } from "./api";
import { ImageDraftOwner, imagesToAPI, type DraftImage } from "./images";

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

type AliveFocusTarget = { kind: "task"; task: Task } | { kind: "prompt" };

function confirmImmediatePurge(task: Task): boolean {
  return window.confirm(`Purge runtime instance?\n\n${task.title}\nbranch: ${task.repos?.[0]?.branch ?? ""}`);
}

type PendingTaskUpdate = { kind: "patch"; patch: Record<string, unknown> } | { kind: "replace" } | { kind: "delete" };

type TaskRecovery = {
  updates: PendingTaskUpdate[];
};

type ToastWarning = {
  id: string;
  message: string;
  details: WarningDetail[];
  category: WarningCategory | null;
};

function taskDiffChanged(previous: Task, next: Task): boolean {
  return (
    previous.state !== next.state || JSON.stringify(previous.diffStat ?? []) !== JSON.stringify(next.diffStat ?? [])
  );
}

function updateTaskDiffCache(previous: Task | undefined, next: Task): void {
  if (next.state === "purged") {
    evictTaskDiff(next.id);
  } else if (previous && taskDiffChanged(previous, next)) {
    invalidateTaskDiff(next.id);
  }
}

function createAppStore() {
  const navigate = useNavigate();
  const location = useLocation();
  const auth = useAuth();
  const hostMode = useHostMode();
  const initialTaskId = taskIdFromPath(location.pathname);
  let initialTaskFocusClaimed = false;

  const claimInitialTaskFocus = (taskId: string): boolean => {
    if (initialTaskFocusClaimed || taskId !== initialTaskId) return false;
    initialTaskFocusClaimed = true;
    return true;
  };

  const [prompt, setPromptValue] = createSignal("");
  let promptRevision = 0;
  const setPrompt = (value: string) => {
    promptRevision++;
    setPromptValue(value);
  };
  const [tasks, setTasks] = createSignal<Task[]>([]);
  // Only SSE introduces list membership. REST detail is bounded to the selected
  // route and cannot enable task image drafts before the stream introduces it.
  const [detailTask, setDetailTask] = createSignal<Task | null>(null);
  const [taskStreamReady, setTaskStreamReady] = createSignal(false);
  const [taskSnapshotComplete, setTaskSnapshotComplete] = createSignal(false);
  let taskStreamGeneration = 0;
  let taskMembership = new Set<string>();
  const [tasksLoading, setTasksLoading] = createSignal(true);
  // Runtime and history restoration state, driven by the task-list stream.
  const [settledLoading, setSettledLoading] = createSignal(false);
  const [settledError, setSettledError] = createSignal("");
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
  const [caicMCP, setCaicMCP] = createSignal(true);
  const [mcpOAuthAvailable, setMCPOAuthAvailable] = createSignal(false);
  const [voiceGatewayAvailable, setVoiceGatewayAvailable] = createSignal(false);
  const [recentCount, setRecentCount] = createSignal(0);
  const [actionId, setActionId] = createSignal<string | null>(null);

  const [autoFixCI, setAutoFixCI] = createSignal(false);
  const [autoFixPR, setAutoFixPR] = createSignal(false);
  const [runtimeSettings, setRuntimeSettings] = createSignal<Record<string, RuntimeSettings>>({});
  const updateRuntimeSettings = (name: string, config: Partial<RuntimeSettings>) => {
    setRuntimeSettings((current) => ({ ...current, [name]: { ...current[name], ...config } }));
  };
  const [purgeDelay, setPurgeDelay] = createSignal(0);
  const [wellKnownCaches, setWellKnownCaches] = createSignal<Record<string, boolean | undefined>>({});
  const [wellKnownCachesList, setWellKnownCachesList] = createSignal<WellKnownCachesResp["wellKnown"]>([]);
  const [wellKnownCacheSizes, setWellKnownCacheSizes] = createSignal<Record<string, CacheSize | undefined>>({});
  const [cacheMappings, setCacheMappings] = createSignal<CacheMappingResp[]>([]);
  const [customMounts, setCustomMounts] = createSignal<MountMappingResp[]>([]);
  const [settingsError, setSettingsError] = createSignal("");
  const [settingsSavePhase, setSettingsSavePhase] = createSignal<"idle" | "saving" | "saved">("idle");
  const [settingsDraftDirty, setSettingsDraftDirty] = createSignal(false);
  const settingsSaveState = () => (settingsDraftDirty() ? "dirty" : settingsSavePhase());
  const [oauthGrants, setOAuthGrants] = createSignal<OAuthGrantResp[]>([]);
  const [oauthGrantError, setOAuthGrantError] = createSignal("");
  const [revokingOAuthGrantID, setRevokingOAuthGrantID] = createSignal<string | null>(null);
  const [versionInfo, setVersionInfo] = createSignal<VersionResp | null>(null);
  const [versionCheckError, setVersionCheckError] = createSignal("");
  const [updateStatus, setUpdateStatus] = createSignal<string>("");
  const [refreshingHarness, setRefreshingHarness] = createSignal<Harness | null>(null);
  const [modelRefreshStatus, setModelRefreshStatus] = createSignal<{
    state: "error" | "success";
    message: string;
  } | null>(null);
  const [imageRefreshStatuses, setImageRefreshStatuses] = createSignal<Record<string, ImageRefreshStatus>>({});
  const imageRefreshStatus = (runtimeName: string) => imageRefreshStatuses()[runtimeName] ?? { state: "idle" as const };
  const imageRefreshPolls = new Map<string, ReturnType<typeof setTimeout>>();
  const imageRefreshRequests = new Map<string, number>();
  let imageRefreshDisposed = false;
  onCleanup(() => {
    imageRefreshDisposed = true;
    for (const poll of imageRefreshPolls.values()) clearTimeout(poll);
  });
  const [checkingUpdate, setCheckingUpdate] = createSignal(false);
  const [updating, setUpdating] = createSignal(false);
  let latestSettingsSave = 0;
  let settingsEditRevision = 0;
  let settingsSaveQueue = Promise.resolve();

  createEffect(() => {
    const available = runtimes();
    if (available.length > 0 && !available.some((rt) => rt.name === selectedRuntimeName())) {
      setSelectedRuntimeName(available[0].name);
    }
  });

  /** Build the current settings payload for api.updatePreferences, with optional overrides. */
  const currentSettings = (overrides: Partial<Parameters<typeof api.updatePreferences>[0]["settings"]> = {}) => {
    const settings = {
      autoFixOnCIFailure: autoFixCI(),
      autoFixOnPROpen: autoFixPR(),
      baseImage: selectedImage() || "",
      runtimeSettings: runtimeSettings(),
      purgeDelay: purgeDelay(),
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
  const [imageConstraints, setImageConstraints] = createSignal<ImageConstraints | null>(null);
  const imageOwner = new ImageDraftOwner(imageConstraints);
  const accountController = new AbortController();
  const inputConversions = new Map<string, AbortController>();
  const dispatchedInputs = new Set<string>();
  const inputRevisions = new Map<string, number>();
  const [pendingImages, setPendingImages] = createSignal<DraftImage[]>([]);
  const [pendingImageGeneration, setPendingImageGeneration] = createSignal(0);
  const [inputImageGenerations, setInputImageGenerations] = createSignal<Map<string, number>>(new Map());
  onCleanup(() => {
    accountController.abort();
    for (const controller of inputConversions.values()) controller.abort();
    imageOwner.dispose();
  });
  const addPendingImages = (blobs: Blob[]) => setPendingImages(imageOwner.adopt(pendingImages(), blobs));
  const removePendingImages = (images: readonly DraftImage[]) => {
    const removed = new Set(images);
    imageOwner.release(images);
    setPendingImages((current) => current.filter((img) => !removed.has(img)));
  };

  // Per-task input drafts survive task switching.
  const [inputDrafts, setInputDrafts] = createSignal<Map<string, string>>(new Map());

  // Per-task image drafts survive task switching.
  const [inputImageDrafts, setInputImageDrafts] = createSignal<Map<string, DraftImage[]>>(new Map());

  function removeTaskDrafts(id: string) {
    inputConversions.get(id)?.abort();
    inputConversions.delete(id);
    inputRevisions.delete(id);
    imageOwner.release(inputImageDrafts().get(id) ?? []);
    setInputImageGenerations((prev) => {
      const next = new Map(prev);
      next.delete(id);
      return next;
    });
    setInputDrafts((prev) => {
      if (!prev.has(id)) return prev;
      const next = new Map(prev);
      next.delete(id);
      return next;
    });
    setInputImageDrafts((prev) => {
      if (!prev.has(id)) return prev;
      const next = new Map(prev);
      next.delete(id);
      return next;
    });
  }

  // Categorized server alerts retain identity across updates and reconnects.
  // Keep one last-seen ID per category, rather than an unbounded episode history.
  const [warningState, setWarningState] = createStore<{ items: ToastWarning[] }>({ items: [] });
  const warnings = () => warningState.items;
  const latestWarningIDs = new Map<WarningCategory, string>();
  const warningTimers = new Map<string, ReturnType<typeof setTimeout>>();
  let nextWarningId = 0;
  function dismissWarning(id: string) {
    clearTimeout(warningTimers.get(id));
    warningTimers.delete(id);
    setWarningState("items", (items) => items.filter((w) => w.id !== id));
  }
  function addWarning(warning: ToastWarning) {
    setWarningState("items", (items) => [...items, warning]);
    warningTimers.set(
      warning.id,
      setTimeout(() => dismissWarning(warning.id), 8000),
    );
  }
  function showWarning(message: string) {
    addWarning({ id: `local-${nextWarningId++}`, message, details: [], category: null });
  }
  function showServerWarning(warning: ServerWarning) {
    if (latestWarningIDs.get(warning.category) === warning.id) {
      const idx = warnings().findIndex((w) => w.id === warning.id);
      // Update an existing toast in place without remounting its disclosure or
      // restarting its timer. Dismissed episodes stay quiet on SSE replay.
      if (idx >= 0) {
        setWarningState("items", idx, { message: warning.message, details: warning.details });
      }
      return;
    }
    latestWarningIDs.set(warning.category, warning.id);
    for (const item of warnings().filter((w) => w.category === warning.category)) dismissWarning(item.id);
    addWarning(warning);
  }
  onCleanup(() => {
    for (const timer of warningTimers.values()) clearTimeout(timer);
  });

  const harnessSupportsImages = () => harnesses().find((h) => h.name === selectedHarness())?.supportsImages ?? false;

  const selectRuntimeName = (runtimeName: string) => {
    setSelectedRuntimeName(runtimeName);
  };

  const applySettings = (settings: PreferencesResp["settings"]) => {
    setAutoFixCI(settings.autoFixOnCIFailure);
    setAutoFixPR(settings.autoFixOnPROpen);
    setRuntimeSettings(settings.runtimeSettings ?? {});
    setPurgeDelay(settings.purgeDelay);
    setSelectedRuntimeName(settings.runtimeName ?? selectedRuntimeName());
    setWellKnownCaches(settings.wellKnownCaches ?? {});
    setCacheMappings(settings.cacheMappings ?? []);
    setCustomMounts(settings.customMounts ?? []);
  };

  const applyResolvedContainerPaths = (settings: PreferencesResp["settings"]) => {
    setCacheMappings((current) =>
      current.map((mapping, i) => {
        const saved = settings.cacheMappings?.[i];
        if (!saved || saved.hostPath !== mapping.hostPath || saved.containerPath !== mapping.containerPath) {
          return { ...mapping, resolvedContainerPath: undefined };
        }
        return {
          ...mapping,
          resolvedContainerPath: saved.resolvedContainerPath,
        };
      }),
    );
    setCustomMounts((current) =>
      current.map((mount, i) => {
        const saved = settings.customMounts?.[i];
        if (!saved || saved.hostPath !== mount.hostPath || saved.containerPath !== mount.containerPath) {
          return { ...mount, resolvedContainerPath: undefined };
        }
        return { ...mount, resolvedContainerPath: saved.resolvedContainerPath };
      }),
    );
  };

  const applyServerConfig = (config: Config) => {
    setImageConstraints(config.imageConstraints);
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
    setMCPOAuthAvailable(config.mcpOAuthAvailable);
    setVoiceGatewayAvailable(config.voiceGateway.mode !== "disabled");
    const displayName = config.displayName || window.location.hostname.split(".")[0];
    document.title = `${displayName} — caic`;
  };

  async function refreshServerConfig(currentStream: () => boolean) {
    try {
      const config = await api.getConfig();
      if (currentStream()) applyServerConfig(config);
    } catch {
      if (currentStream()) setVoiceGatewayAvailable(false);
    }
  }

  const selectedId = (): string | null => taskIdFromPath(location.pathname);
  const selectedTask = (): Task | null => {
    const id = selectedId();
    return id !== null ? (taskById(id) ?? null) : null;
  };
  const taskById = (id: string): Task | undefined =>
    tasks().find((t) => t.id === id) ??
    (selectedId() === id && detailTask()?.id === id ? (detailTask() ?? undefined) : undefined);
  const taskIntroduced = (id: string) => tasks().some((task) => task.id === id);
  createEffect(() => {
    const id = selectedId();
    if (detailTask()?.id !== id) setDetailTask(null);
  });

  function tasksInSidebarOrder(): Task[] {
    const byId = new Map(tasks().map((t) => [t.id, t]));
    const ordered = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]"))
      .map((el) => el.dataset.taskId ?? "")
      .map((id) => byId.get(id))
      .filter((t): t is Task => t !== undefined);
    return ordered.length > 0 ? ordered : tasks();
  }

  function nextAliveFocusTarget(id: string): AliveFocusTarget {
    const ordered = tasksInSidebarOrder();
    const currentIdx = ordered.findIndex((t) => t.id === id);
    const rotated = currentIdx === -1 ? ordered : ordered.slice(currentIdx + 1).concat(ordered.slice(0, currentIdx));
    const candidates = rotated.filter((t) => t.id !== id);
    const nextTask =
      candidates.find((t) => inputNeededTaskStates.has(t.state)) ??
      candidates.find((t) => t.state === "running") ??
      candidates.find((t) => otherAliveTaskStates.has(t.state));
    return nextTask ? { kind: "task", task: nextTask } : { kind: "prompt" };
  }

  function focusTaskCard(id: string) {
    requestAnimationFrame(() => {
      const card = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]")).find(
        (el) => el.dataset.taskId === id,
      );
      card?.focus();
    });
  }

  function focusPrompt() {
    requestAnimationFrame(() => {
      document.querySelector<HTMLElement>("[data-testid='prompt-input']")?.focus();
    });
  }

  function navigateToAliveFocusTarget(target: AliveFocusTarget) {
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
  const upsertTask = (t: Task) => {
    setTasks((prev) => {
      const idx = prev.findIndex((p) => p.id === t.id);
      if (idx >= 0) {
        const next = [...prev];
        next[idx] = t;
        return next;
      }
      return [...prev, t].sort((a, b) => (a.id < b.id ? -1 : 1));
    });
  };

  type EffortPreferences = Record<string, Record<string, string>>;

  // In-memory per-harness model and per-harness/model effort preferences from the server.
  let prefModels: Record<string, string> = {};
  let prefEfforts: EffortPreferences = {};
  const getPrefModel = (harness: string): string | undefined => prefModels[harness];
  const setPrefModel = (harness: string, model: string) => {
    if (model) prefModels[harness] = model;
    else delete prefModels[harness];
  };
  const getPrefEffort = (harness: string, model: string): string | undefined =>
    prefEfforts[harness]?.[model] ?? prefEfforts[harness]?.[""];
  const setPrefEffort = (harness: string, model: string, effort: string) => {
    if (effort) {
      prefEfforts[harness] = {
        ...(prefEfforts[harness] ?? {}),
        [model]: effort,
      };
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

  // Track previous task states to detect transitions to "waiting".
  let prevStates = new Map<string, string>();
  const notifiedTaskIDs = new Set<string>();
  onCleanup(() => {
    for (const id of notifiedTaskIDs) notifications.dismissNotification(id);
  });
  const quotaRecoveryTracker = new QuotaRecoveryTracker();
  const notifyQuotaRecoveries = (currentTasks: Task[]) => {
    for (const task of quotaRecoveryTracker.update(currentTasks)) {
      if (taskIdFromPath(location.pathname) === task.id) continue;
      notifiedTaskIDs.add(task.id);
      notifications.notify(task.id, `${task.title} quota is available`, `caic-event-${task.id}`, {
        enabled: hostMode.browserNotificationsEnabled(),
      });
    }
  };
  const [dismissedQuotaResets, setDismissedQuotaResets] = createSignal<Map<string, string>>(new Map());
  createEffect(() => {
    const currentTasks = tasks();
    const map = dismissedQuotaResets();
    if (map.size === 0) return;
    let changed = false;
    const next = new Map(map);
    for (const [id] of map) {
      const task = currentTasks.find((t) => t.id === id);
      if (!task || !task.rateLimit?.blocked) {
        next.delete(id);
        changed = true;
      }
    }
    if (changed) {
      setDismissedQuotaResets(next);
    }
  });
  const checkAndNotify = (task: Task) => {
    const needsInput = task.state === "waiting" || task.state === "asking" || task.state === "has_plan";
    const prevState = prevStates.get(task.id);
    const prevNeedsInput = prevState === "waiting" || prevState === "asking" || prevState === "has_plan";
    if (needsInput && prevState === "running" && taskIdFromPath(location.pathname) !== task.id) {
      notifiedTaskIDs.add(task.id);
      notifications.notify(task.id, `${task.title} is ready`, `caic-waiting-${task.id}`, {
        enabled: hostMode.browserNotificationsEnabled(),
      });
    } else if (!needsInput && prevNeedsInput) {
      notifiedTaskIDs.delete(task.id);
      notifications.dismissNotification(task.id);
    }
  };
  const applyAuthoritativeTask = (task: Task) => {
    const previous = tasks().find((candidate) => candidate.id === task.id);
    updateTaskDiffCache(previous, task);
    checkAndNotify(task);
    prevStates.set(task.id, task.state);
    upsertTask(task);
    notifyQuotaRecoveries(tasks());
  };
  const applyTaskPatch = (id: string, patch: Record<string, unknown>) => {
    if (patch["state"] === "purged") {
      evictTaskDiff(id);
    } else if ("diffStat" in patch || "state" in patch) {
      invalidateTaskDiff(id);
    }
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

  async function checkForUpdate() {
    setCheckingUpdate(true);
    setVersionCheckError("");
    try {
      setVersionInfo(await api.getVersion());
    } catch (e: unknown) {
      setVersionCheckError(e instanceof Error ? e.message : "Version check failed");
    } finally {
      setCheckingUpdate(false);
    }
  }

  // Fetch version, MCP grant, and cache size info when the settings page opens.
  createEffect(() => {
    if (location.pathname !== "/settings") return;
    const initialMcpOAuthAvailable = mcpOAuthAvailable();
    void checkForUpdate();
    setOAuthGrantError("");
    void api
      .getCacheSizes()
      .then((sizes) => updateWellKnownCacheSizes(sizes.wellKnown))
      .catch(() => undefined);
    if (initialMcpOAuthAvailable) {
      void api
        .listOAuthGrants()
        .then((grants) => setOAuthGrants(grants.grants))
        .catch((e: unknown) => {
          setOAuthGrantError(e instanceof Error ? e.message : "Could not load MCP clients");
        });
    }
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
    if ((err as { status?: number }).status !== 404 || !taskSnapshotComplete()) return false;
    if (!taskMembership.has(id)) removeTaskDrafts(id);
    if (detailTask()?.id === id) setDetailTask(null);
    if (selectedId() === id) navigate("/", { replace: true });
    return true;
  }

  // Wait for the stream snapshot before REST recovery. A GET can fill metadata
  // for an SSE-introduced unknown patch, or the one selected detail fallback;
  // it never manufactures list membership. Stream replacements invalidate all
  // outstanding recoveries, and later SSE boundaries supersede stale results.
  const taskRecoveries = new Map<string, TaskRecovery>();
  const queueTaskUpdate = (id: string, update: PendingTaskUpdate) => {
    taskRecoveries.get(id)?.updates.push(update);
  };
  const ensureTask = async (id: string) => {
    if (!taskStreamReady() || taskRecoveries.has(id)) return;
    if (!taskSnapshotComplete() && !taskMembership.has(id)) return;
    const streamGeneration = taskStreamGeneration;
    const recovery: TaskRecovery = { updates: [] };
    taskRecoveries.set(id, recovery);
    try {
      let task: Task | null = null;
      let getTaskError: unknown = null;
      try {
        task = await api.getTask(id);
      } catch (e) {
        getTaskError = e;
      }
      // A newer snapshot or upsert supplied the complete task while this GET
      // was in flight. Its response is now stale, and later patches were
      // applied directly after that authoritative event.
      if (
        accountController.signal.aborted ||
        taskStreamGeneration !== streamGeneration ||
        taskRecoveries.get(id) !== recovery
      )
        return;

      const updates = recovery.updates;
      let replayFrom = 0;
      for (const [index, update] of updates.entries()) {
        if (update.kind !== "patch") replayFrom = index + 1;
      }
      if (task && replayFrom === 0) {
        if (taskMembership.has(id)) applyAuthoritativeTask(task);
        else if (selectedId() === id) setDetailTask(task);
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
    if (taskStreamReady() && taskSnapshotComplete() && id !== null && selectedTask() === null) void ensureTask(id);
  });

  // Repos available to add (not already selected).
  const availableRecent = () =>
    repos()
      .slice(0, recentCount())
      .filter((r) => !selectedRepos().some((s) => s.path === r.path));
  const availableRest = () =>
    repos()
      .slice(recentCount())
      .filter((r) => !selectedRepos().some((s) => s.path === r.path));

  const isAuthenticated = () => auth.ready() && (auth.providers().length === 0 || auth.user() !== null);

  // Load initial data once authentication is confirmed.
  let dataLoaded = false;
  createEffect(() => {
    if (!isAuthenticated() || dataLoaded) return;
    dataLoaded = true;
    void (async () => {
      try {
        const [data, prefs, h, config, usageData, cachesData, cacheSizesData] = await Promise.all([
          api.listRepos(),
          api.getPreferences().catch(() => null),
          api.listHarnesses().catch(() => [] as HarnessInfo[]),
          api.getConfig().catch(() => null),
          api.getUsage().catch(() => null),
          api.listCaches().catch(() => null) as Promise<WellKnownCachesResp | null>,
          api.getCacheSizes().catch(() => null),
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
          const harness = prefHarness && h.find((x) => x.name === prefHarness) ? prefHarness : (h[0]?.name ?? "");
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
  // On a confirmed missing session, stop retrying and clear account state.
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
    let active = false;
    let generation = 0;

    /** Probe whether the session is gone. EventSource doesn't expose status codes. */
    async function checkUnauthorized(probeGeneration: number): Promise<boolean> {
      if (!active || generation !== probeGeneration) return true;
      try {
        const res = await fetch("/auth/me", {
          signal: AbortSignal.timeout(5000),
        });
        if (!active || generation !== probeGeneration) return true;
        if (res.status === 401 || res.status === 404) {
          auth.confirmLoggedOut();
          return true;
        }
      } catch {
        // Network errors do not confirm that the session is gone.
      }
      return false;
    }
    const initialScriptSrc = document.querySelector<HTMLScriptElement>("script[src^='/assets/']")?.src ?? "";

    function onOpen() {
      setConnected(true);
    }

    function connectTasks() {
      const streamGeneration = ++taskStreamGeneration;
      setTaskStreamReady(false);
      setTaskSnapshotComplete(false);
      taskRecoveries.clear();
      setDetailTask(null);
      const currentStream = () =>
        active && !accountController.signal.aborted && streamGeneration === taskStreamGeneration;
      taskES = api.globalTaskEvents({
        onMessage: (event) => {
          if (!currentStream()) return;
          if (event.kind === "snapshot" && event.snapshot) {
            const snapshot = event.snapshot;
            const snapshotByID = new Map(snapshot.map((task) => [task.id, task]));
            const complete = event.complete === true;
            const retained = complete ? [] : tasks().filter((task) => !snapshotByID.has(task.id));
            for (const task of snapshot) {
              const previous = tasks().find((candidate) => candidate.id === task.id);
              updateTaskDiffCache(previous, task);
            }
            for (const id of taskRecoveries.keys()) {
              if (snapshotByID.has(id)) taskRecoveries.delete(id);
              else if (complete && taskMembership.has(id)) queueTaskUpdate(id, { kind: "delete" });
            }
            batch(() => {
              if (complete) {
                for (const id of taskMembership) {
                  if (!snapshotByID.has(id)) {
                    removeTaskDrafts(id);
                    evictTaskDiff(id);
                    if (detailTask()?.id === id) setDetailTask(null);
                    if (selectedId() === id) navigate("/", { replace: true });
                  }
                }
                taskMembership = new Set(snapshotByID.keys());
              } else {
                for (const id of snapshotByID.keys()) taskMembership.add(id);
              }
              const reconciled = [...snapshot, ...retained];
              prevStates = new Map(reconciled.map((task) => [task.id, task.state]));
              setTasks(reconciled);
              if (detailTask() && snapshotByID.has(detailTask()?.id ?? "")) setDetailTask(null);
              setTasksLoading(false);
              setTaskSnapshotComplete(complete);
              setTaskStreamReady(true);
              notifyQuotaRecoveries(reconciled);
            });
          } else if (event.kind === "upsert" && event.upsert) {
            const task = event.upsert;
            taskMembership.add(task.id);
            if (detailTask()?.id === task.id) setDetailTask(null);
            // A complete upsert supersedes a recovery GET. Removing its entry
            // also makes later patches update this task in place.
            taskRecoveries.delete(task.id);
            applyAuthoritativeTask(task);
          } else if (event.kind === "patch" && event.patch) {
            const patch = event.patch as Record<string, unknown>;
            const id = patch["id"] as string;
            if (!id) return;
            taskMembership.add(id);
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
            taskMembership.delete(event.delete);
            if (detailTask()?.id === event.delete) setDetailTask(null);
            // Authoritative removal: if the deleted task is the one being viewed,
            // leave its now-dead detail route.
            if (event.delete === selectedId()) navigate("/", { replace: true });
            if (taskRecoveries.has(event.delete)) queueTaskUpdate(event.delete, { kind: "delete" });
            prevStates.delete(event.delete);
            removeTaskDrafts(event.delete);
            evictTaskDiff(event.delete);
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
            showServerWarning(event.warning);
          } else if (event.kind === "status") {
            // Runtime/history restoration state: emitted on connect and on every
            // transition (in-progress -> completed | failed).
            setSettledLoading(!!event.status?.loading);
            setSettledError(event.status?.error ?? "");
          }
        },
        onError: (err) => {
          if (!currentStream()) return;
          const msg = err instanceof Error ? err.message : String(err);
          showWarning(`Task list event error: ${msg}`);
        },
      });
      taskES.addEventListener("open", () => {
        if (!currentStream()) return;
        onOpen();
        void refreshServerConfig(currentStream);
        taskDelay = 500;
        // Check if frontend was rebuilt while disconnected.
        fetch("/index.html")
          .then((r) => r.text())
          .then((html) => {
            if (!currentStream()) return;
            const m = html.match(/<script[^>]+src="([^"]*\/assets\/[^"]+)"/);
            if (m && initialScriptSrc && !initialScriptSrc.endsWith(m[1])) {
              window.location.reload();
            }
          })
          .catch(() => {});
      });
      taskES.onerror = () => {
        if (!currentStream()) return;
        taskStreamGeneration++;
        setTaskStreamReady(false);
        setTaskSnapshotComplete(false);
        taskRecoveries.clear();
        setDetailTask(null);
        taskES?.close();
        taskES = null;
        setConnected(false);
        if (taskTimer !== null) clearTimeout(taskTimer);
        const probeGeneration = generation;
        checkUnauthorized(probeGeneration).then((loggedOut) => {
          if (loggedOut || !active || generation !== probeGeneration) return;
          taskTimer = setTimeout(connectTasks, jitteredDelay(taskDelay));
          taskDelay = Math.min(taskDelay * 1.5, 30_000);
        });
      };
    }

    function connectUsage() {
      usageES = api.globalUsageEvents({
        onMessage: (event) => setUsage(event),
        onError: (err) => {
          const msg = err instanceof Error ? err.message : String(err);
          showWarning(`Usage event error: ${msg}`);
        },
      });
      usageES.addEventListener("open", () => {
        onOpen();
        usageDelay = 500;
      });
      usageES.onerror = () => {
        usageES?.close();
        usageES = null;
        if (usageTimer !== null) clearTimeout(usageTimer);
        const probeGeneration = generation;
        checkUnauthorized(probeGeneration).then((loggedOut) => {
          if (loggedOut || !active || generation !== probeGeneration) return;
          usageTimer = setTimeout(connectUsage, jitteredDelay(usageDelay));
          usageDelay = Math.min(usageDelay * 1.5, 30_000);
        });
      };
    }

    function closeAll() {
      generation++;
      taskStreamGeneration++;
      setTaskStreamReady(false);
      setTaskSnapshotComplete(false);
      taskRecoveries.clear();
      setDetailTask(null);
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
      active = true;
      generation++;
      connectAll();
      document.addEventListener("visibilitychange", onVisibilityChange);
      window.addEventListener("online", onOnline);
      window.addEventListener("offline", onOffline);
      onCleanup(() => {
        active = false;
        generation++;
        closeAll();
        document.removeEventListener("visibilitychange", onVisibilityChange);
        window.removeEventListener("online", onOnline);
        window.removeEventListener("offline", onOffline);
      });
    });
  }

  // Clear stale actionId once the server state reflects the transition.
  createEffect(() => {
    const initialActionId = actionId();
    if (!initialActionId) return;
    const t = tasks().find((task) => task.id === initialActionId);
    if (
      t &&
      (t.state === "purging" ||
        t.state === "purged" ||
        t.state === "failed" ||
        t.state === "crashed" ||
        t.state === "stopping" ||
        t.state === "stopped" ||
        t.state === "provisioning")
    ) {
      setActionId(null);
    }
  });

  async function handleStop(id: string) {
    if (actionId()) return;
    const target = selectedId() === id ? nextAliveFocusTarget(id) : null;
    setActionId(id);
    try {
      await api.stopTask(id);
      if (target && selectedId() === id) navigateToAliveFocusTarget(target);
    } catch {
      setActionId(null);
    }
  }

  async function handlePurge(id: string) {
    if (actionId()) return;
    if (purgeDelay() === 0) {
      const task = taskById(id);
      if (!task || !confirmImmediatePurge(task)) return;
    }
    const target = selectedId() === id ? nextAliveFocusTarget(id) : null;
    setActionId(id);
    try {
      await api.purgeTask(id);
      if (target && selectedId() === id) navigateToAliveFocusTarget(target);
    } catch {
      setActionId(null);
    }
  }

  async function handleRevive(id: string) {
    if (actionId()) return;
    setActionId(id);
    try {
      await api.reviveTask(id);
    } catch {
      setActionId(null);
    }
  }

  // Fork dialog state.
  const [forkTaskId, setForkTaskId] = createSignal<string | null>(null);
  const [forkQuotaRecovery, setForkQuotaRecovery] = createSignal(false);
  const [forkPrompt, setForkPrompt] = createSignal("");
  const [forkHandoffLoading, setForkHandoffLoading] = createSignal(false);
  const [forkHandoffError, setForkHandoffError] = createSignal("");
  const [forkHarness, setForkHarness] = createSignal("");
  const [forkModel, setForkModel] = createSignal("");
  const [forkEffort, setForkEffort] = createSignal("");
  const [forkExtraRepos, setForkExtraRepos] = createSignal<RepoEntry[]>([]);
  const [forkTailscale, setForkTailscale] = createSignal(false);
  const [forkUSB, setForkUSB] = createSignal(false);
  const [forkDisplay, setForkDisplay] = createSignal(false);
  const [forkSudo, setForkSudo] = createSignal(false);
  const [forkGitHubToken, setForkGitHubToken] = createSignal(false);
  let forkDialogGeneration = 0;
  let forkTargetTouched = false;
  // Update the selection atomically so quota recommendations cannot interrupt it after the harness changes.
  const applyForkHarness = (harness: string) => {
    const model = selectedModelForHarness(harness);
    const effort = selectedEffortForModel(harness, model);
    batch(() => {
      setForkHarness(harness);
      setForkModel(model);
      setForkEffort(effort);
    });
  };
  const selectForkHarness = (harness: string) => {
    forkTargetTouched = true;
    applyForkHarness(harness);
  };
  const selectForkModel = (model: string) => {
    forkTargetTouched = true;
    setForkModel(model);
    setPrefModel(forkHarness(), model);
    setForkEffort(selectedEffortForModel(forkHarness(), model));
  };
  const selectForkEffort = (effort: string) => {
    forkTargetTouched = true;
    setForkEffort(effort);
    setPrefEffort(forkHarness(), forkModel(), effort);
  };

  // Repos available to add in the fork dialog (exclude already-selected extras and source task repos).
  const forkSourceRepoPaths = () => {
    const initialForkTaskId = forkTaskId();
    if (!initialForkTaskId) return new Set<string>();
    const task = tasks().find((t) => t.id === initialForkTaskId);
    return new Set((task?.repos ?? []).map((r) => r.name));
  };
  const forkAvailableRecent = () =>
    repos()
      .slice(0, recentCount())
      .filter((r) => !forkSourceRepoPaths().has(r.path) && !forkExtraRepos().some((s) => s.path === r.path));
  const forkAvailableRest = () =>
    repos()
      .slice(recentCount())
      .filter((r) => !forkSourceRepoPaths().has(r.path) && !forkExtraRepos().some((s) => s.path === r.path));
  const forkTargets = () => {
    const source = tasks().find((task) => task.id === forkTaskId());
    return quotaRecoveryTargets(harnesses(), usage(), source?.rateLimit?.quotaGroup, selectedHarness(), now());
  };
  const forkHarnesses = () => (forkQuotaRecovery() ? forkTargets().map((target) => target.harness) : harnesses());
  const forkHarnessLabel = (harness: HarnessInfo) => {
    if (!forkQuotaRecovery()) return harness.name;
    const target = forkTargets().find((candidate) => candidate.harness.name === harness.name);
    return target ? `${harness.name} — ${target.label}` : harness.name;
  };
  const forkSelectedTargetLabel = () =>
    forkTargets().find((target) => target.harness.name === forkHarness())?.label ?? "Quota status unknown";

  createEffect(() => {
    if (!forkQuotaRecovery() || forkTargetTouched) return;
    const recommended = forkTargets().find((target) => target.recommended)?.harness.name;
    if (recommended && recommended !== forkHarness()) applyForkHarness(recommended);
  });

  function openFork(id: string, quotaRecovery: boolean): number {
    forkDialogGeneration++;
    const task = tasks().find((t) => t.id === id);
    const harness = task?.harness ?? selectedHarness();
    forkTargetTouched = false;
    setForkTaskId(id);
    setForkQuotaRecovery(quotaRecovery);
    setForkPrompt("");
    setForkHandoffLoading(false);
    setForkHandoffError("");
    applyForkHarness(harness);
    setForkExtraRepos([]);
    setForkTailscale(task?.runtime?.tailscale === "true" || task?.runtime?.tailscale?.startsWith("https://") || false);
    setForkUSB(task?.runtime?.usb ?? false);
    setForkDisplay(task?.runtime?.display ?? false);
    setForkSudo(task?.runtime?.sudo ?? false);
    setForkGitHubToken(task?.gitHubToken ?? false);
    return forkDialogGeneration;
  }

  function handleFork(id: string) {
    openFork(id, false);
  }

  function handleQuotaRecovery(id: string) {
    const task = tasks().find((candidate) => candidate.id === id);
    if (task?.rateLimit?.blocked !== true) return;
    const generation = openFork(id, true);
    void generateForkHandoffFor(id, generation);
  }

  function dismissQuotaWarning(taskId: string) {
    const task = taskById(taskId);
    const key = quotaDismissalKey(task?.rateLimit);
    setDismissedQuotaResets((prev) => {
      const next = new Map(prev);
      next.set(taskId, key);
      return next;
    });
  }

  function isQuotaWarningDismissed(taskId: string): boolean {
    const task = taskById(taskId);
    if (!task || !task.rateLimit?.blocked) return false;
    const dismissedKey = dismissedQuotaResets().get(taskId);
    return dismissedKey !== undefined && dismissedKey === quotaDismissalKey(task.rateLimit);
  }

  function closeFork() {
    forkDialogGeneration++;
    setForkTaskId(null);
    setForkQuotaRecovery(false);
  }

  async function generateForkHandoff() {
    const id = forkTaskId();
    if (!id) return;
    await generateForkHandoffFor(id, forkDialogGeneration);
  }

  async function generateForkHandoffFor(id: string, generation: number) {
    if (forkHandoffLoading()) return;
    setForkHandoffLoading(true);
    setForkHandoffError("");
    try {
      const resp = await api.getTaskHandoff(id);
      if (forkTaskId() === id && forkDialogGeneration === generation) {
        setForkPrompt(resp.prompt);
      }
    } catch (e) {
      if (forkTaskId() === id && forkDialogGeneration === generation) {
        setForkHandoffError(e instanceof Error ? e.message : "Could not generate handoff");
      }
    } finally {
      if (forkTaskId() === id && forkDialogGeneration === generation) setForkHandoffLoading(false);
    }
  }

  async function submitFork() {
    const id = forkTaskId();
    const text = forkPrompt().trim();
    if (!id || !text) return;
    closeFork();
    try {
      const h = forkHarness();
      const m = forkModel();
      const e = forkEffort();
      const extras = forkExtraRepos();
      const sourceTask = tasks().find((t) => t.id === id);
      const resp = await api.forkTask(id, {
        prompt: { text },
        harness: h !== (sourceTask?.harness ?? "") ? (h as Harness) : undefined,
        model: m !== (sourceTask?.requestedModel ?? "") ? m : undefined,
        effort: e !== (sourceTask?.requestedEffort ?? "") ? e : undefined,
        extraRepos:
          extras.length > 0
            ? extras.map((r) => ({
                name: r.path,
                ...(r.branch ? { baseBranch: r.branch } : {}),
              }))
            : undefined,
        tailscale: forkTailscale(),
        usb: forkUSB(),
        display: forkDisplay(),
        sudo: forkSudo(),
        gitHubToken: forkGitHubToken(),
      });
      accountController.signal.throwIfAborted();
      navigate(
        taskPath(
          resp.id,
          resp.repos?.[0]?.name ?? sourceTask?.repos?.[0]?.name ?? "",
          resp.repos?.[0]?.branch ?? "",
          text,
        ),
      );
    } catch (err) {
      if (!accountController.signal.aborted)
        showWarning(`Task fork failed: ${err instanceof Error ? err.message : "Unknown error"}`);
    }
  }

  async function submitTask() {
    const p = prompt().trim();
    const imgs = pendingImages();
    const revision = promptRevision;
    const selRepos = selectedRepos();
    if (submitting() || (!p && imgs.length === 0)) return;
    notifications.requestNotificationPermission({
      enabled: hostMode.browserNotificationsEnabled(),
    });
    setSubmitting(true);
    // Pending captures are outside this snapshot; captures started afterwards belong to the next draft.
    setPendingImageGeneration((value) => value + 1);
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
      const mcp = caicMCP();
      const harness = selectedHarness();
      const runtimeName = selectedRuntimeName();
      const repoSpecs =
        selRepos.length > 0
          ? selRepos.map((r) => ({
              name: r.path,
              ...(r.branch ? { baseBranch: r.branch } : {}),
            }))
          : undefined;
      const images = await imagesToAPI(imgs, accountController.signal);
      accountController.signal.throwIfAborted();
      const data = await api.createTask({
        initialPrompt: {
          text: p,
          ...(images.length > 0 ? { images } : {}),
        },
        repos: repoSpecs,
        harness: harness as Harness,
        ...(runtimeName ? { runtimeName } : {}),
        ...(model ? { model } : {}),
        ...(effort ? { effort } : {}),
        ...(ts ? { tailscale: true } : {}),
        ...(usb ? { usb: true } : {}),
        ...(disp ? { display: true } : {}),
        ...(sudo ? { sudo: true } : {}),
        ...(ght ? { gitHubToken: true } : {}),
        ...(mcp ? { caicMCP: true } : {}),
      });
      accountController.signal.throwIfAborted();
      setPrefModel(harness, model);
      setPrefEffort(harness, model, effort);
      if (promptRevision === revision) setPrompt("");
      removePendingImages(imgs);
      navigate(taskPath(data.id, selRepos[0]?.path ?? "", "", p));
    } catch (err) {
      if (!accountController.signal.aborted)
        showWarning(`Task creation failed: ${err instanceof Error ? err.message : "Unknown error"}`);
    } finally {
      setSubmitting(false);
    }
  }

  async function submitClone(url: string, path?: string) {
    setCloning(true);
    setCloneError("");
    try {
      const repo = await api.cloneRepo({ url, ...(path ? { path } : {}) });
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

  function markSettingsDraft(dirty: boolean) {
    if (dirty) settingsEditRevision++;
    setSettingsDraftDirty(dirty);
  }

  function saveSettings(
    overrides: Partial<Parameters<typeof api.updatePreferences>[0]["settings"]> = {},
  ): Promise<void> {
    const saveID = ++latestSettingsSave;
    const editRevision = settingsEditRevision;
    const settings = currentSettings(overrides);
    setSettingsError("");
    setSettingsSavePhase("saving");
    settingsSaveQueue = settingsSaveQueue.then(async () => {
      try {
        const preferences = await api.updatePreferences(settings);
        if (saveID === latestSettingsSave) {
          if (editRevision === settingsEditRevision) applyResolvedContainerPaths(preferences.settings);
          setSettingsSavePhase("saved");
        }
      } catch (e: unknown) {
        if (saveID === latestSettingsSave) {
          setSettingsError(e instanceof Error ? e.message : "Could not save settings");
          setSettingsSavePhase("idle");
        }
      }
    });
    return settingsSaveQueue;
  }

  async function revokeOAuthClientGrant(grantID: string) {
    setRevokingOAuthGrantID(grantID);
    setOAuthGrantError("");
    try {
      await api.revokeOAuthGrant(grantID, {});
      const grants = await api.listOAuthGrants();
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
      const resp = await api.triggerUpdate();
      setUpdateStatus(
        resp.status === "started"
          ? "Update started in background. The server will restart shortly."
          : "Already up to date.",
      );
    } catch (e: unknown) {
      setUpdateStatus(e instanceof Error ? e.message : "Update failed");
    } finally {
      setUpdating(false);
    }
  }

  async function refreshAvailableModels(harness: Harness) {
    setRefreshingHarness(harness);
    setModelRefreshStatus(null);
    try {
      const refreshed = await api.refreshHarness(harness, {});
      setHarnesses((prev) => prev.map((info) => (info.name === harness ? refreshed : info)));
      if (selectedHarness() === harness) selectHarness(harness);
      setModelRefreshStatus({ state: "success", message: `${harness} models refreshed.` });
    } catch (e: unknown) {
      setModelRefreshStatus({ state: "error", message: e instanceof Error ? e.message : "Could not refresh models" });
    } finally {
      setRefreshingHarness(null);
    }
  }

  async function loadImageRefreshStatus(runtimeName: string) {
    const poll = imageRefreshPolls.get(runtimeName);
    if (poll !== undefined) clearTimeout(poll);
    imageRefreshPolls.delete(runtimeName);
    const request = (imageRefreshRequests.get(runtimeName) ?? 0) + 1;
    imageRefreshRequests.set(runtimeName, request);
    try {
      const status = await api.getImageRefresh(runtimeName);
      if (imageRefreshDisposed || request !== imageRefreshRequests.get(runtimeName)) return;
      setImageRefreshStatuses((current) => ({ ...current, [runtimeName]: status }));
      if (status.state === "running") {
        imageRefreshPolls.set(
          runtimeName,
          setTimeout(() => void loadImageRefreshStatus(runtimeName), 2000),
        );
      }
    } catch (e: unknown) {
      if (imageRefreshDisposed || request !== imageRefreshRequests.get(runtimeName)) return;
      setImageRefreshStatuses((current) => ({
        ...current,
        [runtimeName]: { state: "failed", error: e instanceof Error ? e.message : "Could not check image refresh" },
      }));
    }
  }

  async function startImageRefresh(runtimeName: string) {
    const poll = imageRefreshPolls.get(runtimeName);
    if (poll !== undefined) clearTimeout(poll);
    imageRefreshPolls.delete(runtimeName);
    const request = (imageRefreshRequests.get(runtimeName) ?? 0) + 1;
    imageRefreshRequests.set(runtimeName, request);
    setImageRefreshStatuses((current) => ({ ...current, [runtimeName]: { state: "running" } }));
    try {
      await settingsSaveQueue;
      if (settingsError()) throw new Error(`Could not save image settings: ${settingsError()}`);
      const status = await api.startImageRefresh(runtimeName, {});
      if (imageRefreshDisposed || request !== imageRefreshRequests.get(runtimeName)) return;
      setImageRefreshStatuses((current) => ({ ...current, [runtimeName]: status }));
      if (status.state === "running") void loadImageRefreshStatus(runtimeName);
    } catch (e: unknown) {
      if (imageRefreshDisposed || request !== imageRefreshRequests.get(runtimeName)) return;
      setImageRefreshStatuses((current) => ({
        ...current,
        [runtimeName]: { state: "failed", error: e instanceof Error ? e.message : "Could not start image refresh" },
      }));
    }
  }

  // Navigate to a task's detail route, building the slugged path from its repo/branch/title.
  const navigateToTask = (id: string) => {
    const found = taskById(id);
    navigate(found ? taskPathForTask(found) : `/task/@${id}`);
  };
  const fixCI = (repoPath: string) => {
    void api
      .botFixCI({ repo: repoPath })
      .then((data) => {
        if (!accountController.signal.aborted) navigate(taskPath(data.id, repoPath, "", `Fix CI: ${repoPath}`));
      })
      .catch((err: unknown) => {
        if (!accountController.signal.aborted)
          showWarning(`CI task creation failed: ${err instanceof Error ? err.message : "Unknown error"}`);
      });
  };

  // Per-task input/image drafts, keyed by task ID.
  const inputDraft = (id: string) => inputDrafts().get(id) ?? "";
  const setInputDraft = (id: string, v: string) => {
    inputRevisions.set(id, (inputRevisions.get(id) ?? 0) + 1);
    setInputDrafts((prev) => {
      const next = new Map(prev);
      if (v) next.set(id, v);
      else next.delete(id);
      return next;
    });
  };
  const inputImages = (id: string) => inputImageDrafts().get(id) ?? [];
  const inputImageGeneration = (id: string) => inputImageGenerations().get(id) ?? 0;
  const addInputImages = (id: string, blobs: Blob[]) => {
    if (!taskMembership.has(id)) throw new Error("Waiting for task updates before attaching images.");
    const images = imageOwner.adopt(inputImages(id), blobs);
    setInputImageDrafts((prev) => new Map(prev).set(id, images));
  };
  const removeInputImages = (id: string, images: readonly DraftImage[]) => {
    const removed = new Set(images);
    imageOwner.release(images);
    setInputImageDrafts((prev) => {
      const next = new Map(prev);
      const remaining = (prev.get(id) ?? []).filter((img) => !removed.has(img));
      if (remaining.length) next.set(id, remaining);
      else next.delete(id);
      return next;
    });
  };
  const cancelInputConversion = (id: string) => {
    if (!dispatchedInputs.has(id)) inputConversions.get(id)?.abort();
  };
  async function sendTaskInput(id: string) {
    if (inputConversions.has(id)) {
      if (!accountController.signal.aborted) showWarning("A message is already being sent to this task.");
      return;
    }
    const text = inputDraft(id).trim();
    const drafts = inputImages(id);
    const revision = inputRevisions.get(id) ?? 0;
    const controller = new AbortController();
    inputConversions.set(id, controller);
    setInputImageGenerations((prev) => new Map(prev).set(id, inputImageGeneration(id) + 1));
    try {
      const signal = AbortSignal.any([controller.signal, accountController.signal]);
      const images = await imagesToAPI(drafts, signal);
      signal.throwIfAborted();
      // Route changes cancel conversion; a dispatched request still owns its original task.
      dispatchedInputs.add(id);
      await api.sendInput(id, { prompt: { text, ...(images.length ? { images } : {}) } });
      signal.throwIfAborted();
      if ((inputRevisions.get(id) ?? 0) === revision) setInputDraft(id, "");
      removeInputImages(id, drafts);
    } catch (err) {
      // The account owns dispatched requests and their failures across route changes.
      // Deliberate cancellation or account disposal does not report a send failure.
      if (!accountController.signal.aborted && !controller.signal.aborted) {
        showWarning(`Message send failed: ${err instanceof Error ? err.message : "Unknown error"}`);
      }
    } finally {
      dispatchedInputs.delete(id);
      if (inputConversions.get(id) === controller) inputConversions.delete(id);
    }
  }

  return {
    navigate,
    auth,
    // task data + selection
    tasks,
    tasksLoading,
    settledLoading,
    settledError,
    repos,
    selectedId,
    selectedTask,
    taskById,
    taskIntroduced,
    dismissSelectedTaskOnNotFound,
    claimInitialTaskFocus,
    // new-task form
    prompt,
    setPrompt,
    selectedRepos,
    setSelectedRepos,
    selectedModel,
    setSelectedModel: selectModel,
    selectedEffort,
    setSelectedEffort: selectEffort,
    selectedHarness,
    setSelectedHarness: selectHarness,
    harnesses,
    runtimes,
    selectedRuntimeName,
    setSelectedRuntimeName: selectRuntimeName,
    harnessSupportsImages,
    pendingImages,
    addPendingImages,
    removePendingImages,
    imageConstraints,
    pendingImageGeneration,
    availableRecent,
    availableRest,
    getPrefModel,
    setPrefModel,
    getPrefEffort,
    setPrefEffort,
    initializing,
    submitting,
    submitTask,
    // capability toggles
    tailscaleAvailable,
    tailscaleEnabled,
    setTailscaleEnabled,
    usbAvailable,
    usbEnabled,
    setUSBEnabled,
    displayAvailable,
    displayEnabled,
    setDisplayEnabled,
    sudoAvailable,
    sudoEnabled,
    setSudoEnabled,
    gitHubTokenAvailable,
    gitHubTokenEnabled,
    setGitHubTokenEnabled,
    caicMCP,
    setCaicMCP,
    voiceGatewayAvailable,
    // sidebar + actions
    sidebarOpen,
    setSidebarOpen,
    now,
    actionId,
    handleStop,
    handlePurge,
    handleRevive,
    handleFork,
    handleQuotaRecovery,
    dismissQuotaWarning,
    isQuotaWarningDismissed,
    navigateToTask,
    fixCI,
    // input drafts
    inputDraft,
    setInputDraft,
    inputImages,
    addInputImages,
    removeInputImages,
    inputImageGeneration,
    sendTaskInput,
    cancelInputConversion,
    // warnings
    warnings,
    showWarning,
    dismissWarning,
    // clone dialog
    cloneOpen,
    setCloneOpen,
    cloning,
    cloneError,
    setCloneError,
    submitClone,
    // fork dialog
    forkTaskId,
    forkQuotaRecovery,
    closeFork,
    forkPrompt,
    setForkPrompt,
    forkHandoffLoading,
    forkHandoffError,
    generateForkHandoff,
    forkHarnesses,
    forkHarnessLabel,
    forkSelectedTargetLabel,
    forkHarness,
    setForkHarness: selectForkHarness,
    forkModel,
    setForkModel: selectForkModel,
    forkEffort,
    setForkEffort: selectForkEffort,
    forkExtraRepos,
    setForkExtraRepos,
    forkTailscale,
    setForkTailscale,
    forkUSB,
    setForkUSB,
    forkDisplay,
    setForkDisplay,
    forkSudo,
    setForkSudo,
    forkGitHubToken,
    setForkGitHubToken,
    forkAvailableRecent,
    forkAvailableRest,
    submitFork,
    // settings
    selectedImage,
    setSelectedImage,
    runtimeSettings,
    updateRuntimeSettings,
    purgeDelay,
    setPurgeDelay,
    wellKnownCaches,
    setWellKnownCaches,
    wellKnownCachesList,
    wellKnownCacheSizes,
    cacheMappings,
    setCacheMappings,
    customMounts,
    setCustomMounts,
    settingsError,
    settingsSaveState,
    markSettingsDraft,
    autoFixCI,
    setAutoFixCI,
    autoFixPR,
    setAutoFixPR,
    mcpOAuthAvailable,
    oauthGrants,
    oauthGrantError,
    revokingOAuthGrantID,
    revokeOAuthClientGrant,
    versionInfo,
    versionCheckError,
    checkingUpdate,
    updating,
    updateStatus,
    refreshingHarness,
    modelRefreshStatus,
    imageRefreshStatus,
    loadImageRefreshStatus,
    startImageRefresh,
    saveSettings,
    checkForUpdate,
    triggerServerUpdate,
    refreshAvailableModels,
    // usage + connection
    usage,
    connected,
  };
}

export type AppStore = ReturnType<typeof createAppStore>;

const AppStateContext = createContext<AppStore>();

export function AppStateProvider(props: { children: JSX.Element }) {
  const store = createAppStore();
  onCleanup(() => taskDiffCache.clear());
  return <AppStateContext.Provider value={store}>{props.children}</AppStateContext.Provider>;
}

export function useAppState(): AppStore {
  const ctx = useContext(AppStateContext);
  if (!ctx) throw new Error("useAppState must be used within an AppStateProvider");
  return ctx;
}
