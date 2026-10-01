// Tests app-shell task creation, account isolation, image draft ownership, warning replay, and preferences.

import { afterEach, beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { fireEvent, render, screen, waitFor, within } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import type {
  Config,
  Repo,
  PreferencesResp,
  HarnessInfo,
  Task,
  ISOTimestamp,
  UserResp,
  Warning,
  TaskListEvent,
} from "@sdk/types.gen";

// Structured HTTP errors preserve the transport status used by route recovery.
function apiError(status: number): Error & { status: number } {
  return Object.assign(new Error(`HTTP ${status}`), { status });
}

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: "task1",
    initialPrompt: "do something",
    title: "do something",
    state: "branching",
    stateUpdatedAt: "2026-01-01T00:00:00Z" as ISOTimestamp,
    costUSD: 0,
    duration: 0,
    numTurns: 0,
    cumulativeInputTokens: 0,
    cumulativeOutputTokens: 0,
    cumulativeCacheCreationInputTokens: 0,
    cumulativeCacheReadInputTokens: 0,
    activeInputTokens: 0,
    activeCacheReadTokens: 0,
    stoppedDiskUsedBytes: -1,
    contextWindowLimit: 0,
    harness: "claude",
    runtime: { id: "rt1" },
    ...overrides,
  };
}

function deferred<T>() {
  let resolve: (value: T | PromiseLike<T>) => void = (_value) => {
    throw new Error("Deferred promise is not initialized");
  };
  let reject: (reason?: unknown) => void = (_reason) => {
    throw new Error("Deferred promise is not initialized");
  };
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

// Stub EventSource to prevent real SSE connections.
// FakeEventSource captures message listeners so tests can push SSE events, and
// unregisters them on close so a superseded subscription cannot keep receiving
// events: the application closes its stream on cleanup and on every reconnect.
type MessageListener = (e: { data: string }) => void;
type OpenListener = () => void;
const fakeESListeners: MessageListener[] = [];
const fakeUsageESListeners: MessageListener[] = [];
const fakeESOpenListeners: OpenListener[] = [];

class FakeEventSource {
  /** Undo callbacks for this instance's live listeners. */
  private readonly registered: Array<() => void> = [];

  constructor(
    _url = "",
    private readonly messages: MessageListener[] = fakeESListeners,
    private readonly opens: OpenListener[] = fakeESOpenListeners,
  ) {}

  addEventListener = vi.fn((type: string, handler: MessageListener | OpenListener) => {
    if (type === "message") this.register(this.messages, handler as MessageListener);
    if (type === "open") this.register(this.opens, handler as OpenListener);
  });

  close = vi.fn(() => {
    for (const unregister of this.registered.splice(0)) unregister();
  });

  onerror: ((e: Event) => void) | null = null;

  /** Add one handler and remember how to remove it again. */
  private register<T>(list: T[], handler: T): void {
    list.push(handler);
    this.registered.push(() => {
      const index = list.indexOf(handler);
      if (index >= 0) list.splice(index, 1);
    });
  }
}

vi.stubGlobal("EventSource", FakeEventSource);

/** Task-event subscriptions the application opened since the last render. */
let taskEventSubscriptions = 0;

/** Fail loudly when an event is pushed to a stream no live subscription receives. */
function requireLiveSubscription(listeners: unknown[], caller: string): void {
  if (listeners.length === 0) {
    throw new Error(`${caller} has no live subscription; await waitForTaskEventsSubscription() first.`);
  }
}

let initialTaskSnapshot: Task[] | null = [];

function dispatchSSE(data: TaskListEvent) {
  requireLiveSubscription(fakeESListeners, "dispatchSSE");
  const payload = {
    data: JSON.stringify(data.kind === "snapshot" ? { ...data, complete: data.complete ?? true } : data),
  };
  fakeESListeners.forEach((fn) => fn(payload));
}

function dispatchWarning(warning: Warning) {
  dispatchSSE({ kind: "warning", warning });
}

function dispatchUsageSSE(data: unknown) {
  requireLiveSubscription(fakeUsageESListeners, "dispatchUsageSSE");
  const payload = { data: JSON.stringify(data) };
  fakeUsageESListeners.forEach((fn) => fn(payload));
}

function dispatchOpen() {
  requireLiveSubscription(fakeESOpenListeners, "dispatchOpen");
  fakeESOpenListeners.forEach((fn) => fn());
}

async function waitForTaskEventsSubscription() {
  await waitFor(() => expect(taskEventSubscriptions).toBeGreaterThan(0));
}

import { MemoryRouter, Route, createMemoryHistory } from "@solidjs/router";
import { appRoutes } from "./routes";
import { notifications } from "@maruel/gomode/web/notifications";
import { executeFrontendVoiceTool } from "@maruel/gomode/web/FrontendVoiceTools";
import { voiceSession } from "@maruel/gomode/web/VoiceSession";
import { api } from "./api";
import { taskDiffCache } from "./diffCache";
import { AuthProvider } from "./AuthContext";
import { HostModeProvider } from "@maruel/gomode/web/HostMode";
import { AppStateProvider, useAppState, type AppStore } from "./AppState";
import { getVoiceTaskNumber, focusedVoiceTask } from "./voiceTaskState";
import { installFetchRouter } from "@tests/fetch-router";

// Spies on the real api singleton and the notifications object replace the former module
// mocks; SSE streams arrive through the stubbed global EventSource. Re-applied in beforeEach
// because afterEach's restoreAllMocks returns the originals.
const apiSpyNames = [
  "listRepos",
  "getPreferences",
  "updatePreferences",
  "listHarnesses",
  "refreshHarness",
  "getImageRefresh",
  "startImageRefresh",
  "listCaches",
  "getCacheSizes",
  "getMetrics",
  "getConfig",
  "getVersion",
  "triggerUpdate",
  "listOAuthGrants",
  "revokeOAuthGrant",
  "getUsage",
  "getUsageDashboard",
  "listRepoBranches",
  "cloneRepo",
  "createTask",
  "getTaskHandoff",
  "forkTask",
  "getTask",
  "botFixCI",
  "stopTask",
  "purgeTask",
  "reviveTask",
  "globalTaskEvents",
  "globalUsageEvents",
  "taskEvents",
  "taskEventBackfill",
  "sendInput",
  "restartTask",
  "clearContext",
  "compactContext",
  "syncTask",
  "getTaskDiff",
  "getTaskDiffIndex",
  "getTaskFileDiff",
  "getTaskRepoStatus",
  "getTaskProcesses",
  "signalProcess",
  "getTaskInfo",
] as const;
function spySeams(): void {
  for (const name of apiSpyNames) {
    vi.spyOn(api, name);
  }
  vi.spyOn(notifications, "requestNotificationPermission");
  vi.spyOn(notifications, "notify");
  vi.spyOn(notifications, "dismissNotification");
}

// Real AuthProvider fetches server info on mount; serve it through the network seam.
const fetchRouter = installFetchRouter();
fetchRouter.apiGet("/server-info/config", () => ({ authProviders: [] }));
fetchRouter.apiGet("/.well-known/gomode.json", () => ({ service: "caic", webShell: { voiceGateway: { url: "/" } } }));

/** Render the full app at an initial route, returning the memory history for assertions. */
function renderApp(initial = "/") {
  taskEventSubscriptions = 0;
  const history = createMemoryHistory();
  history.set({ value: initial });
  const utils = render(() => (
    <AuthProvider>
      <MemoryRouter history={history}>{appRoutes()}</MemoryRouter>
    </AuthProvider>
  ));
  return { history, ...utils };
}

const repoA: Repo = {
  path: "repos/a",
  branch: "main",
  baseBranch: { name: "main" },
  remoteURL: "",
};
const repoB: Repo = {
  path: "repos/b",
  branch: "main",
  baseBranch: { name: "main" },
  remoteURL: "",
};
const newRepo: Repo = {
  path: "repos/new",
  branch: "main",
  baseBranch: { name: "main" },
  remoteURL: "",
};

function chipPathValues(): string[] {
  const btns = screen
    .queryAllByTestId("repo-chips")[0]
    ?.querySelectorAll<HTMLButtonElement>("button[data-testid^='chip-label-']");
  return Array.from(btns ?? []).map((b) => b.dataset["testid"]?.replace("chip-label-", "") ?? "");
}

beforeEach(() => {
  spySeams();
  vi.clearAllMocks();
  fetchRouter.reset();
  fetchRouter.apiGet("/server-info/config", () => ({ authProviders: [] }));
  fetchRouter.apiGet("/.well-known/gomode.json", () => ({ service: "caic", webShell: { voiceGateway: { url: "/" } } }));
  fakeESListeners.length = 0;
  fakeUsageESListeners.length = 0;
  fakeESOpenListeners.length = 0;
  initialTaskSnapshot = [];
  window.history.replaceState(null, "", "/");
  delete window.goModeHost;
  vi.mocked(api.globalTaskEvents).mockImplementation(((handlers: { onMessage: (event: unknown) => void }) => {
    const es = new FakeEventSource();
    // Mirror the real client: parse the SSE payload before invoking the handler.
    es.addEventListener("message", (e: { data: string }) => handlers.onMessage(JSON.parse(e.data)));
    taskEventSubscriptions += 1;
    const initial = initialTaskSnapshot;
    if (initial !== null)
      queueMicrotask(() => handlers.onMessage({ kind: "snapshot", snapshot: initial, complete: true }));
    return es;
  }) as unknown as typeof api.globalTaskEvents);
  vi.mocked(api.globalUsageEvents).mockImplementation(((handlers: { onMessage: (event: unknown) => void }) => {
    const es = new FakeEventSource("", fakeUsageESListeners);
    es.addEventListener("message", (e: { data: string }) => handlers.onMessage(JSON.parse(e.data)));
    return es;
  }) as unknown as typeof api.globalUsageEvents);
  vi.mocked(api.taskEvents).mockImplementation((() => new FakeEventSource()) as unknown as typeof api.taskEvents);
  vi.mocked(api.taskEventBackfill).mockImplementation(
    (() => new FakeEventSource()) as unknown as typeof api.taskEventBackfill,
  );
  vi.mocked(api.listCaches).mockResolvedValue(null as never);
  vi.mocked(api.getCacheSizes).mockResolvedValue(null as never);
  vi.mocked(api.getMetrics).mockResolvedValue({ series: [] } as never);
  vi.mocked(api.clearContext).mockResolvedValue({ status: "cleared" } as never);
  vi.mocked(api.compactContext).mockResolvedValue({ status: "compacting" } as never);
  vi.mocked(api.getTaskRepoStatus).mockResolvedValue({ repositories: [] });
  vi.mocked(api.listRepos).mockResolvedValue([repoA, repoB]);
  vi.mocked(api.getPreferences).mockResolvedValue({
    repositories: [{ path: "repos/a" }],
    models: {},
    harness: "",
    settings: { baseImage: "", purgeDelay: 15_000_000_000 },
  } as unknown as PreferencesResp);
  vi.mocked(api.updatePreferences).mockImplementation(
    async (request) =>
      ({
        repositories: [{ path: "repos/a" }],
        models: {},
        harness: "",
        settings: request.settings,
      }) as PreferencesResp,
  );
  vi.mocked(api.listHarnesses).mockResolvedValue([
    {
      name: "claude",
      models: [],
      supportsImages: false,
      supportsCompact: false,
    },
  ] as unknown as HarnessInfo[]);
  vi.mocked(api.refreshHarness).mockResolvedValue({
    name: "claude",
    models: [],
    supportsImages: false,
    supportsCompact: false,
    supportsModelRefresh: false,
  } as unknown as HarnessInfo);
  vi.mocked(api.getImageRefresh).mockResolvedValue({ state: "idle" });
  vi.mocked(api.startImageRefresh).mockResolvedValue({ state: "running" });
  vi.mocked(api.getConfig).mockRejectedValue(new Error("no config"));
  vi.mocked(api.getVersion).mockResolvedValue({
    current: "0.0.1",
    latest: "0.0.1",
    updateAvailable: false,
    autoUpdateEnabled: false,
  });
  vi.mocked(api.listOAuthGrants).mockResolvedValue({ grants: [] });
  vi.mocked(api.getUsage).mockRejectedValue(new Error("no usage"));
  vi.mocked(api.getUsageDashboard).mockRejectedValue(new Error("no usage dashboard"));
  vi.mocked(api.listRepoBranches).mockResolvedValue({
    branches: [{ name: "main" }, { name: "dev", remote: "origin" }],
  });
  vi.mocked(api.cloneRepo).mockResolvedValue(newRepo);
  vi.mocked(api.createTask).mockResolvedValue(makeTask());
  vi.mocked(api.getTaskHandoff).mockResolvedValue({
    prompt: "Generated handoff prompt",
  });
  vi.mocked(api.getTask).mockImplementation(async (id) => makeTask({ id }));
  vi.mocked(api.getTaskDiffIndex).mockResolvedValue({ repositories: [] });
  vi.mocked(api.getTaskProcesses).mockResolvedValue({ processes: [] });
  vi.mocked(api.getTaskInfo).mockResolvedValue({
    id: "task1",
    recorded: { state: "running" },
    observed: {},
  } as Awaited<ReturnType<typeof api.getTaskInfo>>);
});

afterEach(() => {
  delete window.__CAIC_BOOTSTRAP__;
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("structured warnings", () => {
  for (const dismissal of ["automatic", "manual"] as const) {
    it(`updates one episode and stays quiet after ${dismissal} dismissal and replay`, async () => {
      renderApp();
      await screen.findByTestId("chip-label-repos/a");
      await waitForTaskEventsSubscription();
      vi.useFakeTimers();

      const warning: Warning = {
        id: "episode-1",
        category: "ci_poll_failed",
        message: "Échec de la récupération CI.",
        details: [{ repo: "repos/a", error: "rate limit exceeded" }],
      };
      dispatchWarning(warning);
      const disclosure = screen.getByText("Details").closest("details");
      if (!disclosure) throw new Error("Warning details disclosure is missing");
      fireEvent.click(screen.getByText("Details"));
      expect(disclosure.open).toBe(true);
      vi.advanceTimersByTime(7000);
      const updated: Warning = {
        ...warning,
        message: "CI-Abfrage fehlgeschlagen.",
        details: [...warning.details, { repo: "repos/b", error: "connection refused" }],
      };
      dispatchWarning(updated);
      expect(screen.getAllByText(updated.message)).toHaveLength(1);
      expect(screen.queryByText(warning.message)).not.toBeInTheDocument();
      expect(screen.getByText("repos/a")).toBeInTheDocument();
      expect(screen.getByText("repos/b")).toBeInTheDocument();
      expect(screen.getByText("Details").closest("details")).toBe(disclosure);
      expect(disclosure.open).toBe(true);

      if (dismissal === "automatic") {
        vi.advanceTimersByTime(1000);
      } else {
        fireEvent.click(screen.getByRole("button", { name: "Dismiss warning" }));
      }
      expect(screen.queryByText(updated.message)).not.toBeInTheDocument();

      dispatchOpen();
      dispatchWarning({ ...updated, details: [{ repo: "repos/a", error: "request timed out" }] });
      expect(screen.queryByText(updated.message)).not.toBeInTheDocument();

      // The server assigns a new ID to a failure after recovery.
      dispatchWarning({ ...warning, id: "episode-2" });
      expect(screen.getByText(warning.message)).toBeInTheDocument();
      dispatchWarning({ ...updated, id: "episode-3" });
      expect(screen.queryByText(warning.message)).not.toBeInTheDocument();
      expect(screen.getAllByRole("button", { name: "Dismiss warning" })).toHaveLength(1);

      const handlers = vi.mocked(api.globalTaskEvents).mock.calls.at(-1)?.[0];
      if (!handlers?.onError) throw new Error("Task-list error handler is missing");
      handlers.onError(new Error("malformed event"));
      expect(screen.getByText("Task list event error: malformed event")).toBeInTheDocument();
    });
  }
});

it("destroys account A task and repo state before rendering confirmed account B", async () => {
  window.__CAIC_BOOTSTRAP__ = {
    authProviders: ["github"],
    user: { id: "a", provider: "github", username: "account-a" },
  };
  const accountBRepos = deferred<Repo[]>();
  vi.mocked(api.listRepos).mockResolvedValueOnce([repoA]).mockReturnValueOnce(accountBRepos.promise);
  vi.mocked(api.getConfig).mockResolvedValue({
    imageConstraints: {
      allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
      maxImageBytes: 10485760,
      maxPromptImageBytes: 20971520,
    },
    displayName: "caic",
    tailscaleAvailable: false,
    usbAvailable: false,
    displayAvailable: false,
    sudoAvailable: false,
    gitHubTokenAvailable: false,
    mcpOAuthAvailable: false,
    voiceGateway: { mode: "embedded" },
  } satisfies Config);
  vi.spyOn(api, "logout").mockRejectedValue(new Error("logout response lost"));
  const checkedIdentity = deferred<UserResp>();
  vi.spyOn(api, "getMe").mockReturnValue(checkedIdentity.promise);
  const disconnectVoice = vi.spyOn(voiceSession, "disconnect");
  const injectVoiceText = vi.spyOn(voiceSession, "injectText");

  vi.mocked(api.listHarnesses).mockResolvedValue([
    { name: "claude", models: [], supportsImages: true, supportsCompact: false, supportsModelRefresh: false },
  ]);
  const createPreview = vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:account-a");
  const revokePreview = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  renderApp();
  await waitFor(() => expect(screen.getByTestId("chip-label-repos/a")).toBeInTheDocument());
  const promptForm = screen.getByTestId("prompt-input").closest("form");
  if (!promptForm) throw new Error("Prompt form missing");
  await waitFor(() => expect(within(promptForm).getByTestId("attach-images")).toBeEnabled());
  chooseDraftImage(promptForm, new File(["private image"], "private.png", { type: "image/png" }));
  expect(createPreview).toHaveBeenCalledOnce();
  await waitForTaskEventsSubscription();
  dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ id: "a-task", title: "Account A task" })] });
  expect(await screen.findByText("Account A task")).toBeInTheDocument();
  dispatchWarning({
    id: "same-id",
    category: "ci_poll_failed",
    message: "CI polling failed. CI status may be out of date.",
    details: [{ repo: "repos/a", error: "rate limit exceeded" }],
  });
  expect(screen.getByText("CI polling failed. CI status may be out of date.")).toBeInTheDocument();
  await screen.findByTestId("voice-overlay");
  voiceSession.setState((state) => ({ ...state, connected: true }));
  expect(getVoiceTaskNumber("a-task")).toBe(1);
  dispatchSSE({ kind: "upsert", upsert: makeTask({ id: "a-task", title: "Account A task", state: "running" }) });
  dispatchSSE({ kind: "upsert", upsert: makeTask({ id: "a-task", title: "Account A task", state: "waiting" }) });
  expect(notifications.notify).toHaveBeenCalledWith("a-task", "Account A task is ready", "caic-waiting-a-task", {
    enabled: true,
  });

  const user = userEvent.setup();
  await user.click(screen.getByTitle("account-a"));
  await user.click(screen.getByRole("menuitem", { name: "Sign out" }));
  expect(screen.getByText("Checking your session…")).toBeInTheDocument();
  expect(revokePreview).toHaveBeenCalledWith("blob:account-a");
  expect(screen.queryByText("Account A task")).not.toBeInTheDocument();
  expect(screen.queryByTestId("chip-label-repos/a")).not.toBeInTheDocument();
  expect(screen.queryByText("CI polling failed. CI status may be out of date.")).not.toBeInTheDocument();
  expect(notifications.dismissNotification).toHaveBeenCalledWith("a-task");
  expect(disconnectVoice).toHaveBeenCalled();
  expect(voiceSession.state.connected).toBe(false);
  expect(getVoiceTaskNumber("a-task")).toBeUndefined();

  checkedIdentity.resolve({ id: "b", provider: "github", username: "account-b" });
  await waitFor(() => expect(screen.getByTitle("account-b")).toBeInTheDocument());
  expect(screen.queryByText("Account A task")).not.toBeInTheDocument();
  expect(screen.queryByTestId("chip-label-repos/a")).not.toBeInTheDocument();
  expect(vi.mocked(api.listRepos)).toHaveBeenCalledTimes(2);

  accountBRepos.resolve([repoB]);
  await waitFor(() => expect(screen.getByTestId("chip-label-repos/b")).toBeInTheDocument());
  await screen.findByTestId("voice-overlay");
  injectVoiceText.mockClear();
  dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ id: "b-task", title: "Account B task" })] });
  expect(await screen.findByText("Account B task")).toBeInTheDocument();
  dispatchWarning({
    id: "same-id",
    category: "ci_poll_failed",
    message: "CI polling failed. CI status may be out of date.",
    details: [{ repo: "repos/b", error: "connection refused" }],
  });
  expect(screen.getByText("CI polling failed. CI status may be out of date.")).toBeInTheDocument();
  expect(injectVoiceText).not.toHaveBeenCalled();
  expect(screen.queryByText("Account A task")).not.toBeInTheDocument();
  expect(screen.queryByTestId("chip-label-repos/a")).not.toBeInTheDocument();
});

it("removes account data when the event stream discovers /auth/me returned 404", async () => {
  window.__CAIC_BOOTSTRAP__ = {
    authProviders: ["github"],
    user: { id: "a", provider: "github", username: "account-a" },
  };
  renderApp();
  await waitFor(() => expect(screen.getByTestId("chip-label-repos/a")).toBeInTheDocument());
  await waitForTaskEventsSubscription();
  dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ id: "a-task", title: "Account A task" })] });
  expect(await screen.findByText("Account A task")).toBeInTheDocument();

  const priorFetch = globalThis.fetch;
  vi.spyOn(globalThis, "fetch").mockImplementation((request, init) => {
    if (request === "/auth/me") return Promise.resolve(new Response(null, { status: 404 }));
    return priorFetch(request, init);
  });
  const taskStream = vi.mocked(api.globalTaskEvents).mock.results.at(-1)?.value as FakeEventSource | undefined;
  if (!taskStream) throw new Error("Task stream did not start");
  taskStream.onerror?.(new Event("error"));

  await waitFor(() => expect(screen.getByRole("link", { name: "Sign in with GitHub" })).toBeInTheDocument());
  expect(screen.queryByText("Account A task")).not.toBeInTheDocument();
  expect(screen.queryByTestId("chip-label-repos/a")).not.toBeInTheDocument();
});

it("ignores an old account's delayed /auth/me probe after switching accounts", async () => {
  window.__CAIC_BOOTSTRAP__ = {
    authProviders: ["github"],
    user: { id: "a", provider: "github", username: "account-a" },
  };
  vi.mocked(api.listRepos).mockResolvedValueOnce([repoA]).mockResolvedValueOnce([repoB]);
  vi.spyOn(api, "logout").mockRejectedValue(new Error("logout response lost"));
  vi.spyOn(api, "getMe").mockResolvedValue({ id: "b", provider: "github", username: "account-b" });
  const oldProbe = Promise.withResolvers<Response>();

  renderApp();
  await waitFor(() => expect(screen.getByTestId("chip-label-repos/a")).toBeInTheDocument());
  await waitForTaskEventsSubscription();
  const priorFetch = globalThis.fetch;
  vi.spyOn(globalThis, "fetch").mockImplementation((request, init) => {
    if (request === "/auth/me") return oldProbe.promise;
    return priorFetch(request, init);
  });
  const taskStream = vi.mocked(api.globalTaskEvents).mock.results.at(-1)?.value as FakeEventSource | undefined;
  if (!taskStream) throw new Error("Task stream did not start");
  taskStream.onerror?.(new Event("error"));

  const user = userEvent.setup();
  await user.click(screen.getByTitle("account-a"));
  await user.click(screen.getByRole("menuitem", { name: "Sign out" }));
  await waitFor(() => expect(screen.getByTestId("chip-label-repos/b")).toBeInTheDocument());
  oldProbe.resolve(new Response(null, { status: 404 }));
  await Promise.resolve();

  expect(screen.getByTitle("account-b")).toBeInTheDocument();
  expect(screen.getByTestId("chip-label-repos/b")).toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "Sign in with GitHub" })).not.toBeInTheDocument();
});

describe("App task list loading state", () => {
  it("shows loading until initial data finishes loading", async () => {
    renderApp();

    expect(screen.getByText("Loading...")).toBeInTheDocument();
    expect(screen.queryByText("No tasks yet.")).not.toBeInTheDocument();

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [] });

    await waitFor(() => expect(screen.getByText("No tasks yet.")).toBeInTheDocument());
  });

  it("shows loading (not no tasks) for an empty list while the settled pass is in progress", async () => {
    renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [] });
    dispatchSSE({ kind: "status", status: { loading: true, error: "" } });

    await waitFor(() => {
      expect(screen.getByText("Loading...")).toBeInTheDocument();
      expect(screen.queryByText("No tasks yet.")).not.toBeInTheDocument();
    });
  });
});

describe("App connection word settled states", () => {
  const word = () => screen.getByTestId("new-task-button");
  const status = () => document.getElementById("connection-status");

  it("shows the counted restoration warning while remaining connected after history loads", async () => {
    renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [] });
    dispatchSSE({ kind: "status", status: { loading: true, error: "" } });
    dispatchSSE({
      kind: "warning",
      warning: {
        id: "restore-1",
        category: "runtime_restore_failed",
        message: "2 existing tasks could not be restored.",
        details: [],
      },
    });
    dispatchSSE({ kind: "status", status: { loading: false, error: "" } });
    await waitFor(() => expect(screen.getByText("2 existing tasks could not be restored.")).toBeInTheDocument());
    expect(word().getAttribute("data-status")).toBe("connected");
    expect(status()).toHaveTextContent("Connected");
    fireEvent.click(screen.getByRole("button", { name: "Dismiss warning" }));
    expect(screen.queryByText("2 existing tasks could not be restored.")).not.toBeInTheDocument();
  });

  it("exposes connected status on the caic button", async () => {
    renderApp();
    expect(word().getAttribute("data-status")).toBe("connected");
    expect(word().getAttribute("aria-describedby")).toBe("connection-status");
    expect(status()).toHaveTextContent("Connected");
    expect(screen.queryByTestId("connection-dot")).not.toBeInTheDocument();

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [] });
    dispatchSSE({ kind: "status", status: { loading: false, error: "" } });

    await waitFor(() => expect(word().getAttribute("data-status")).toBe("connected"));
    expect(status()).toHaveTextContent("Connected");
  });

  it("shows loading while the pass is in progress and connected after completion", async () => {
    renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [] });
    dispatchSSE({ kind: "status", status: { loading: true, error: "" } });

    await waitFor(() => expect(word().getAttribute("data-status")).toBe("settled-loading"));
    expect(status()).toHaveTextContent("Restoring tasks…");

    dispatchSSE({ kind: "status", status: { loading: false, error: "" } });

    await waitFor(() => expect(word().getAttribute("data-status")).toBe("connected"));
  });

  it("shows the history error in the button description after a failed status", async () => {
    renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [] });
    dispatchSSE({ kind: "status", status: { loading: true, error: "" } });

    await waitFor(() => expect(word().getAttribute("data-status")).toBe("settled-loading"));

    dispatchSSE({
      kind: "status",
      status: { loading: false, error: "load purged tasks: boom" },
    });

    await waitFor(() => {
      expect(word().getAttribute("data-status")).toBe("settled-error");
      expect(word().getAttribute("title")).toBe("New task — load purged tasks: boom");
      expect(status()).toHaveTextContent("load purged tasks: boom");
    });
  });
});

describe("App task-list SSE recovery", () => {
  it("invalidates task diffs for live changes and evicts authoritative removals", async () => {
    const invalidate = vi.spyOn(taskDiffCache, "invalidate");
    const evictTask = vi.spyOn(taskDiffCache, "evictTask");
    await taskDiffCache.loadIndex("omitted-cache");
    renderApp();
    await waitForTaskEventsSubscription();
    const initial = makeTask({ id: "cache-events", state: "running" });
    dispatchSSE({
      kind: "snapshot",
      snapshot: [initial, makeTask({ id: "omitted-cache" })],
    });

    dispatchSSE({
      kind: "patch",
      patch: {
        id: "cache-events",
        diffStat: [{ path: "file.go", linesAdded: 1, linesDeleted: 0, oldSize: -1, newSize: -1 }],
      },
    });
    dispatchSSE({ kind: "upsert", upsert: { ...initial, state: "waiting" } });
    dispatchSSE({
      kind: "snapshot",
      snapshot: [
        {
          ...initial,
          state: "waiting",
          diffStat: [{ path: "file.go", linesAdded: 2, linesDeleted: 0, oldSize: -1, newSize: -1 }],
        },
      ],
    });

    expect(invalidate).toHaveBeenCalledTimes(3);
    expect(invalidate).toHaveBeenCalledWith("cache-events");
    expect(evictTask).toHaveBeenCalledWith("omitted-cache");
    dispatchSSE({ kind: "delete", delete: "cache-events" });
    expect(evictTask).toHaveBeenCalledWith("cache-events");
  });

  it("evicts task diffs for every authoritative purged-state flow", async () => {
    const evictTask = vi.spyOn(taskDiffCache, "evictTask");
    renderApp();
    await waitForTaskEventsSubscription();
    const patchTask = makeTask({ id: "purged-patch", state: "running" });
    const upsertTask = makeTask({ id: "purged-upsert", state: "running" });
    const snapshotTask = makeTask({ id: "purged-snapshot", state: "running" });
    dispatchSSE({
      kind: "snapshot",
      snapshot: [patchTask, upsertTask, snapshotTask],
    });

    dispatchSSE({
      kind: "patch",
      patch: { id: patchTask.id, state: "purged" },
    });
    dispatchSSE({ kind: "upsert", upsert: { ...upsertTask, state: "purged" } });
    dispatchSSE({
      kind: "snapshot",
      snapshot: [
        { ...patchTask, state: "purged" },
        { ...upsertTask, state: "purged" },
        { ...snapshotTask, state: "purged" },
      ],
    });

    expect(evictTask).toHaveBeenCalledWith("purged-patch");
    expect(evictTask).toHaveBeenCalledWith("purged-upsert");
    expect(evictTask).toHaveBeenCalledWith("purged-snapshot");
  });

  it("reuses a navigation-intent diff prefetch on route mount", async () => {
    vi.spyOn(Date, "now").mockReturnValue(1_000);
    taskDiffCache.evictTask("prefetched");
    const task = makeTask({
      id: "prefetched",
      state: "running",
      diffStat: [{ path: "file.go", linesAdded: 1, linesDeleted: 0, oldSize: -1, newSize: -1 }],
    });
    const { history } = renderApp("/task/@prefetched+prefetched");
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [task] });
    const link = await screen.findByRole("link", { name: /changed file/ });

    fireEvent.focus(link);
    await waitFor(() => expect(api.getTaskDiffIndex).toHaveBeenCalledTimes(1));
    fireEvent.click(link);
    await waitFor(() => expect(history.get()).toBe("/task/@prefetched+prefetched/diff"));
    expect(await screen.findByText("Repository changes")).toBeInTheDocument();
    expect(api.getTaskDiffIndex).toHaveBeenCalledTimes(1);
    taskDiffCache.evictTask("prefetched");
  });

  it("does not alert for a ready task already open in the detail pane", async () => {
    renderApp("/task/@selected+work");
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ id: "selected", state: "running" })] });
    dispatchSSE({
      kind: "upsert",
      upsert: makeTask({ id: "selected", state: "waiting", rateLimit: { blocked: true } }),
    });
    dispatchSSE({
      kind: "upsert",
      upsert: makeTask({ id: "selected", state: "waiting", rateLimit: { blocked: false } }),
    });
    expect(notifications.notify).not.toHaveBeenCalled();

    dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ id: "other", state: "running" })] });
    dispatchSSE({ kind: "upsert", upsert: makeTask({ id: "other", state: "waiting" }) });
    expect(notifications.notify).toHaveBeenCalledWith("other", "do something is ready", "caic-waiting-other", {
      enabled: true,
    });
  });

  it("uses fetched tasks for derived notification and quota recovery bookkeeping", async () => {
    const blockedTask = makeTask({
      id: "recovered",
      state: "waiting",
      rateLimit: { blocked: true },
    });
    vi.mocked(api.getTask).mockResolvedValue(blockedTask);
    renderApp();

    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "patch",
      patch: {
        id: "recovered",
        title: "untrusted partial title",
        state: "waiting",
      },
    });

    await waitFor(() => expect(screen.getByText("do something")).toBeInTheDocument());
    expect(screen.queryByText("untrusted partial title")).not.toBeInTheDocument();

    dispatchSSE({
      kind: "upsert",
      upsert: makeTask({
        id: "recovered",
        state: "waiting",
        rateLimit: { blocked: false },
      }),
    });
    dispatchSSE({
      kind: "patch",
      patch: { id: "recovered", state: "running" },
    });
    dispatchSSE({
      kind: "patch",
      patch: { id: "recovered", state: "waiting" },
    });

    await waitFor(() => {
      expect(notifications.notify).toHaveBeenCalledWith(
        "recovered",
        "do something quota is available",
        "caic-event-recovered",
        {
          enabled: true,
        },
      );
      expect(notifications.notify).toHaveBeenCalledWith(
        "recovered",
        "do something is ready",
        "caic-waiting-recovered",
        {
          enabled: true,
        },
      );
    });
  });

  it("replays patches that arrive during unknown-task recovery", async () => {
    const recoveryFetch = deferred<Task>();
    const recoveredTask = makeTask({
      id: "recovered",
      title: "authoritative task",
      state: "running",
      rateLimit: { blocked: true },
    });
    vi.mocked(api.getTask).mockImplementationOnce(() => recoveryFetch.promise);
    renderApp();

    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "patch",
      patch: { id: "recovered", state: "running" },
    });
    expect(document.querySelector("[data-task-id='recovered']")).not.toBeInTheDocument();

    dispatchSSE({
      kind: "patch",
      patch: {
        id: "recovered",
        state: "waiting",
        rateLimit: { blocked: true },
      },
    });
    dispatchSSE({
      kind: "patch",
      patch: { id: "recovered", rateLimit: { blocked: false } },
    });
    recoveryFetch.resolve(recoveredTask);

    await waitFor(() => expect(document.querySelector("[data-task-id='recovered']")).toHaveTextContent("waiting"));
    expect(api.getTask).toHaveBeenCalledOnce();
    expect(notifications.notify).toHaveBeenCalledWith(
      "recovered",
      "authoritative task is ready",
      "caic-waiting-recovered",
      {
        enabled: true,
      },
    );
    expect(notifications.notify).toHaveBeenCalledWith(
      "recovered",
      "authoritative task quota is available",
      "caic-event-recovered",
      { enabled: true },
    );
  });

  it("does not let recovery fetches overwrite later snapshots", async () => {
    const recoveryFetch = deferred<Task>();
    const runningTask = makeTask({ id: "recovered", state: "running" });
    const snapshotTask = makeTask({ id: "recovered", state: "waiting" });
    vi.mocked(api.getTask).mockImplementationOnce(() => recoveryFetch.promise);
    renderApp();

    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "patch",
      patch: { id: "recovered", state: "running" },
    });
    dispatchSSE({ kind: "snapshot", snapshot: [snapshotTask] });
    recoveryFetch.resolve(runningTask);

    await waitFor(() => expect(document.querySelector("[data-task-id='recovered']")).toHaveTextContent("waiting"));
    expect(api.getTask).toHaveBeenCalledOnce();
  });

  it("applies patches after an upsert supersedes recovery", async () => {
    const recoveryFetch = deferred<Task>();
    const authoritativeTask = makeTask({
      id: "recovered",
      title: "authoritative task",
      state: "running",
    });
    vi.mocked(api.getTask).mockImplementationOnce(() => recoveryFetch.promise);
    renderApp();

    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "patch",
      patch: { id: "recovered", state: "running" },
    });
    dispatchSSE({ kind: "upsert", upsert: authoritativeTask });
    dispatchSSE({
      kind: "patch",
      patch: { id: "recovered", state: "waiting" },
    });

    await waitFor(() => expect(document.querySelector("[data-task-id='recovered']")).toHaveTextContent("waiting"));
    expect(notifications.notify).toHaveBeenCalledWith(
      "recovered",
      "authoritative task is ready",
      "caic-waiting-recovered",
      {
        enabled: true,
      },
    );

    recoveryFetch.resolve(makeTask({ id: "recovered", state: "running" }));
    await Promise.resolve();
    expect(document.querySelector("[data-task-id='recovered']")).toHaveTextContent("waiting");
  });

  it("replays queued updates when recovery fetches fail", async () => {
    const snapshotTask = makeTask({ id: "recovered", state: "running" });
    vi.mocked(api.getTask).mockRejectedValueOnce(apiError(404));
    renderApp();

    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "patch",
      patch: { id: "recovered", state: "running" },
    });
    dispatchSSE({ kind: "snapshot", snapshot: [snapshotTask] });
    dispatchSSE({
      kind: "patch",
      patch: { id: "recovered", state: "waiting" },
    });

    await waitFor(() => expect(document.querySelector("[data-task-id='recovered']")).toHaveTextContent("waiting"));
    expect(api.getTask).toHaveBeenCalledOnce();
  });

  it("dismisses a deep link after a recovery 404 and snapshot absence", async () => {
    const recoveryFetch = deferred<Task>();
    vi.mocked(api.getTask).mockImplementationOnce(() => recoveryFetch.promise);
    const { history } = renderApp("/task/@recovered+gone");

    await waitForTaskEventsSubscription();
    await waitFor(() => expect(api.getTask).toHaveBeenCalledWith("recovered"));
    dispatchSSE({ kind: "snapshot", snapshot: [] });
    recoveryFetch.reject(apiError(404));

    await waitFor(() => expect(history.get()).toBe("/"));
  });

  it("fetches interleaved unknown task patches independently", async () => {
    const firstTask = deferred<Task>();
    const secondTask = deferred<Task>();
    vi.mocked(api.getTask)
      .mockImplementationOnce(() => firstTask.promise)
      .mockImplementationOnce(() => secondTask.promise);
    renderApp();

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "patch", patch: { id: "first", state: "waiting" } });
    dispatchSSE({ kind: "patch", patch: { id: "second", state: "waiting" } });

    expect(api.getTask).toHaveBeenCalledTimes(2);
    expect(api.getTask).toHaveBeenNthCalledWith(1, "first");
    expect(api.getTask).toHaveBeenNthCalledWith(2, "second");

    firstTask.resolve(makeTask({ id: "first", title: "first authoritative task" }));
    secondTask.resolve(makeTask({ id: "second", title: "second authoritative task" }));
    await waitFor(() => expect(screen.getByText("first authoritative task")).toBeInTheDocument());
    expect(screen.getByText("second authoritative task")).toBeInTheDocument();
  });
});

describe("App keyboard shortcuts", () => {
  it("describes F4 when browser voice mode is available", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getConfig).mockResolvedValue({
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: false,
      voiceGateway: { mode: "embedded" },
    });
    renderApp();
    await screen.findByTestId("voice-overlay");

    await user.keyboard("{F1}");

    expect(screen.getByText("F4", { selector: "kbd" })).toBeInTheDocument();
    expect(screen.getByText("Toggle voice mode")).toBeInTheDocument();
  });

  const f3Targets = [
    {
      harnesses: [
        {
          name: "claude",
          models: [],
          supportsImages: false,
          supportsCompact: false,
        },
        {
          name: "codex",
          models: [],
          supportsImages: false,
          supportsCompact: false,
        },
      ],
      targetTestId: "harness-select",
      help: "Focus harness for the new task",
    },
    {
      harnesses: [
        {
          name: "codex",
          models: [{ id: "gpt-5", effortOptions: [] }],
          supportsImages: false,
          supportsCompact: false,
        },
      ],
      targetTestId: "model-select",
      help: "Focus model for the new task",
    },
  ] as const;
  for (const { harnesses, targetTestId, help } of f3Targets) {
    it(`describes the visible ${targetTestId} as the F3 target`, async () => {
      const user = userEvent.setup();
      vi.mocked(api.listHarnesses).mockResolvedValue(harnesses as unknown as HarnessInfo[]);
      renderApp();
      await screen.findByTestId(targetTestId);

      await user.keyboard("{F1}");

      expect(screen.getByText(help)).toBeInTheDocument();
    });
  }

  it("omits F3 from help when there is no dropdown to focus", async () => {
    const user = userEvent.setup();
    renderApp();
    await waitFor(() => expect(api.listHarnesses).toHaveBeenCalledOnce());

    await user.keyboard("{F1}");

    expect(screen.queryByText("F3", { selector: "kbd" })).not.toBeInTheDocument();
  });

  it("uses runtime as the F3 target when no harness or model selector is visible", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getConfig).mockResolvedValue({
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: false,
      voiceGateway: { mode: "disabled" },
      runtimes: [{ name: "docker" }, { name: "podman" }],
    });
    renderApp();
    const runtime = await screen.findByRole("combobox", { name: "Runtime" });

    await user.keyboard("{F1}");
    expect(screen.getByText("Focus runtime for the new task")).toBeInTheDocument();
    (screen.getByTestId("keyboard-shortcuts-dialog") as HTMLDialogElement).close();
    await waitFor(() => expect(screen.queryByTestId("keyboard-shortcuts-dialog")).not.toBeInTheDocument());

    await user.keyboard("{F3}");
    await waitFor(() => expect(runtime).toHaveFocus());
  });

  it("focuses a delayed task-detail prompt on desktop", async () => {
    const taskLoad = deferred<Task>();
    vi.mocked(api.getTask).mockReturnValue(taskLoad.promise);
    renderApp("/task/@task1+do-something");
    expect(screen.queryByTestId("task-detail-prompt")).not.toBeInTheDocument();

    taskLoad.resolve(makeTask());

    await waitFor(() => expect(screen.getByTestId("task-detail-prompt")).toHaveFocus());
  });

  it("does not autofocus a task-detail prompt on touch-primary devices", async () => {
    const matchMedia = window.matchMedia;
    vi.spyOn(window, "matchMedia").mockImplementation((query) => ({
      ...matchMedia(query),
      matches: query === "(hover: none) and (pointer: coarse)",
    }));
    renderApp("/task/@task1+do-something");

    const prompt = await screen.findByTestId("task-detail-prompt");
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

    expect(prompt).not.toHaveFocus();
  });

  it("focuses the first selected repository with F2", async () => {
    const user = userEvent.setup();
    renderApp();
    const prompt = screen.getByTestId("prompt-input");
    prompt.focus();

    await user.keyboard("{F2}");

    await waitFor(() => expect(screen.getByTestId("chip-label-repos/a")).toHaveFocus());
    expect(screen.queryByRole("combobox", { name: "Manage repositories" })).not.toBeInTheDocument();
  });

  it("focuses the repository add button with F2 when none are selected", async () => {
    const user = userEvent.setup();
    renderApp();
    await user.click(await screen.findByTestId("chip-remove-repos/a"));
    screen.getByTestId("prompt-input").focus();

    await user.keyboard("{F2}");

    await waitFor(() => expect(screen.getByTestId("add-repo-button")).toHaveFocus());
    expect(screen.queryByRole("combobox", { name: "Manage repositories" })).not.toBeInTheDocument();
  });

  it("changes harness with F3 and returns focus to the prompt", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "claude",
        models: [],
        supportsImages: false,
        supportsCompact: false,
      },
      {
        name: "codex",
        models: [],
        supportsImages: false,
        supportsCompact: false,
      },
    ] as unknown as HarnessInfo[]);
    renderApp();
    const prompt = screen.getByTestId("prompt-input");
    prompt.focus();

    await user.keyboard("{F3}");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Harness" })).toHaveFocus());
    await user.keyboard("{ArrowDown}{Enter}");

    await waitFor(() => expect(screen.getByRole("combobox", { name: "Harness" })).toHaveValue("codex"));
    await waitFor(() => expect(prompt).toHaveFocus());
  });

  it("focuses the model with F3 when the harness is fixed", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "codex",
        models: [{ id: "gpt-5", effortOptions: [] }],
        supportsImages: false,
        supportsCompact: false,
      },
    ] as unknown as HarnessInfo[]);
    renderApp();

    await user.keyboard("{F3}");

    await waitFor(() => expect(screen.getByRole("button", { name: "Model" })).toHaveFocus());
  });

  it("navigates tasks with Shift+ArrowDown while editing the prompt", async () => {
    const user = userEvent.setup();
    const task = makeTask();
    const { history } = renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [task] });
    const prompt = screen.getByTestId("prompt-input");
    prompt.focus();

    await user.keyboard("{Shift>}{ArrowDown}{/Shift}");

    await waitFor(() => expect(history.get()).toContain("@task1+"));
    await waitFor(() => expect(screen.getByTestId("task-detail-prompt")).toHaveFocus());
  });

  it("navigates with shifted arrows after clicking task-detail content", async () => {
    const user = userEvent.setup();
    const task = makeTask();
    vi.mocked(api.getTask).mockResolvedValue(task);
    renderApp("/task/@task1+do-something");
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [makeTask()] });
    await waitFor(() => expect(screen.getByTestId("task-detail-prompt")).toHaveFocus());

    await user.click(screen.getByTestId("task-detail-form"));
    expect(screen.getByTestId("task-detail-prompt")).not.toHaveFocus();
    await user.keyboard("{Shift>}{ArrowDown}{/Shift}");

    await waitFor(() => expect(screen.getByTestId("task-detail-prompt")).toHaveFocus());
  });

  it("navigates from focused task cards with shifted and unshifted arrows", async () => {
    const user = userEvent.setup();
    const first = makeTask({ id: "first", title: "first task" });
    const second = makeTask({ id: "second", title: "second task" });
    const { history } = renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [first, second] });
    const cards = await waitFor(() => {
      const found = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]"));
      expect(found).toHaveLength(2);
      return found;
    });
    const firstId = cards[0].dataset.taskId;
    const secondId = cards[1].dataset.taskId;
    cards[0].focus();

    await user.keyboard("{ArrowDown}");
    await waitFor(() => expect(history.get()).toContain(`@${secondId}+`));
    await waitFor(() => expect(document.querySelector(`[data-task-id='${secondId}']`)).toHaveFocus());

    await user.keyboard("{Shift>}{ArrowUp}{/Shift}");
    await waitFor(() => expect(history.get()).toContain(`@${firstId}+`));
    await waitFor(() => expect(document.querySelector(`[data-task-id='${firstId}']`)).toHaveFocus());

    await user.keyboard("{Shift>}{ArrowDown}{/Shift}");
    await waitFor(() => expect(history.get()).toContain(`@${secondId}+`));
    await waitFor(() => expect(document.querySelector(`[data-task-id='${secondId}']`)).toHaveFocus());
    await user.keyboard("{ArrowUp}");
    await waitFor(() => expect(history.get()).toContain(`@${firstId}+`));
  });

  it("moves between a task card and its detail prompt with Tab and Shift+Tab", async () => {
    const user = userEvent.setup();
    const task = makeTask();
    const { history } = renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [task] });
    const card = await waitFor(() => {
      const found = document.querySelector<HTMLElement>("[data-task-id='task1']");
      if (!found) throw new Error("Task card was not rendered");
      return found;
    });
    card.focus();

    await user.tab();

    await waitFor(() => expect(history.get()).toContain("@task1+"));
    const detailPrompt = screen.getByTestId("task-detail-prompt");
    await waitFor(() => expect(detailPrompt).toHaveFocus());

    await user.tab({ shift: true });
    await waitFor(() => expect(document.querySelector("[data-task-id='task1']")).toHaveFocus());
  });

  it("keeps one visible task card in the Tab order", async () => {
    const user = userEvent.setup();
    const first = makeTask({ id: "first", title: "first task" });
    const second = makeTask({ id: "second", title: "second task" });
    const { history } = renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [first, second] });
    const cards = await waitFor(() => {
      const found = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]"));
      expect(found).toHaveLength(2);
      return found;
    });
    expect(cards.map((card) => card.tabIndex)).toEqual([0, -1]);

    history.set({ value: `/task/@${cards[1].dataset.taskId}+selected` });
    await waitFor(() => expect(cards.map((card) => card.tabIndex)).toEqual([-1, 0]));

    await user.click(screen.getByTitle("Collapse sidebar"));
    expect(cards.map((card) => card.tabIndex)).toEqual([-1, -1]);
  });

  it("focuses the new-task prompt with Escape", async () => {
    const user = userEvent.setup();
    renderApp();
    screen.getByTestId("new-task-button").focus();

    await user.keyboard("{Escape}");

    await waitFor(() => expect(screen.getByTestId("prompt-input")).toHaveFocus());
  });

  it("focuses the new-task prompt with slash outside an editor", async () => {
    const user = userEvent.setup();
    renderApp();
    screen.getByTestId("new-task-button").focus();

    await user.keyboard("/");

    expect(screen.getByTestId("prompt-input")).toHaveFocus();

    const feature = screen.getByRole("checkbox", { name: "Enable CAIC MCP delegation for this task" });
    feature.focus();
    await user.keyboard("/");
    expect(screen.getByTestId("prompt-input")).toHaveFocus();
  });

  it("focuses the prompt when slash requires Shift on the keyboard layout", () => {
    renderApp();
    const button = screen.getByTestId("new-task-button");
    button.focus();

    fireEvent.keyDown(button, { key: "/", code: "Digit3", shiftKey: true });

    expect(screen.getByTestId("prompt-input")).toHaveFocus();
  });

  it("focuses the selected task's prompt with slash", async () => {
    const user = userEvent.setup();
    const task = makeTask();
    vi.mocked(api.getTask).mockResolvedValue(task);
    const { history } = renderApp("/task/@task1+do-something");
    const prompt = await screen.findByTestId("task-detail-prompt");
    await waitFor(() => expect(prompt).toHaveFocus());
    const newTaskButton = screen.getByTestId("new-task-button");
    newTaskButton.focus();
    expect(newTaskButton).toHaveFocus();

    await user.keyboard("/");

    await waitFor(() => expect(prompt).toHaveFocus());
    expect(history.get()).toContain("/task/@task1");
  });

  it("keeps slash as text when editing a prompt", async () => {
    const user = userEvent.setup();
    renderApp();
    const prompt = screen.getByTestId("prompt-input");
    prompt.focus();

    await user.keyboard("/");

    expect(prompt).toHaveFocus();
    expect(prompt).toHaveTextContent("/");
  });

  it("returns from a task subview to its prompt with slash", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getTask).mockResolvedValue(makeTask());
    const { history } = renderApp("/task/@task1+do-something/stats");
    screen.getByTestId("new-task-button").focus();

    await user.keyboard("/");

    await waitFor(() => expect(history.get()).not.toContain("/stats"));
    await waitFor(() => expect(screen.getByTestId("task-detail-prompt")).toHaveFocus());
  });

  it("leaves slash with an open dialog", async () => {
    const user = userEvent.setup();
    renderApp();
    screen.getByTestId("new-task-button").focus();
    await user.keyboard("{F1}");
    const dialog = screen.getByTestId("keyboard-shortcuts-dialog");
    expect(dialog).toHaveAttribute("open");

    await user.keyboard("/");

    expect(dialog).toHaveAttribute("open");
    expect(screen.getByTestId("prompt-input")).not.toHaveFocus();
  });

  it("returns Escape from a detail prompt to the new-task prompt", async () => {
    const user = userEvent.setup();
    const task = makeTask();
    vi.mocked(api.getTask).mockResolvedValue(task);
    const { history } = renderApp("/task/@task1+do-something");
    const prompt = await screen.findByTestId("task-detail-prompt");
    prompt.focus();

    await user.keyboard("{Escape}");

    await waitFor(() => expect(history.get()).toBe("/"));
    await waitFor(() => expect(screen.getByTestId("prompt-input")).toHaveFocus());
  });

  it("returns Escape from a task card to the new-task prompt", async () => {
    const user = userEvent.setup();
    const task = makeTask();
    vi.mocked(api.getTask).mockResolvedValue(task);
    const { history } = renderApp("/task/@task1+do-something");
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [makeTask()] });
    const card = await waitFor(() => {
      const found = document.querySelector<HTMLElement>("[data-task-id='task1']");
      if (!found) throw new Error("Task card was not rendered");
      return found;
    });
    card.focus();

    await user.keyboard("{Escape}");

    await waitFor(() => expect(history.get()).toBe("/"));
    await waitFor(() => expect(screen.getByTestId("prompt-input")).toHaveFocus());
  });

  it("does not navigate tasks while editing the prompt", async () => {
    const user = userEvent.setup();
    const task = makeTask();
    renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [task] });
    const prompt = screen.getByTestId("prompt-input");
    prompt.focus();

    await user.keyboard("{ArrowDown}r?");

    expect(prompt).toHaveFocus();
    expect(screen.queryByTestId("keyboard-shortcuts-dialog")).not.toBeInTheDocument();
  });

  it("purges the selected task with Shift+Delete while editing its prompt", async () => {
    const user = userEvent.setup();
    const task = makeTask({ state: "running" });
    vi.mocked(api.getTask).mockResolvedValue(task);
    vi.mocked(api.purgeTask).mockResolvedValue({ status: "ok" });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    renderApp("/task/@task1+do-something");

    const prompt = await screen.findByTestId("task-detail-prompt");
    await waitFor(() => expect(api.getPreferences).toHaveBeenCalledOnce());
    await waitFor(() => expect(prompt).toHaveFocus());
    await user.keyboard("{Shift>}{Delete}{/Shift}");

    expect(confirm).not.toHaveBeenCalled();
    expect(api.purgeTask).toHaveBeenCalledWith(task.id);
  });

  it("drops a deleted task's draft before that ID is shown again", async () => {
    const user = userEvent.setup();
    renderApp("/task/@task1+do-something");

    const prompt = await screen.findByTestId("task-detail-prompt");
    await user.type(prompt, "draft that must not survive deletion");
    expect(prompt).toHaveTextContent("draft that must not survive deletion");

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "delete", delete: "task1" });
    await waitFor(() => expect(screen.getByTestId("prompt-input")).toBeInTheDocument());

    dispatchSSE({ kind: "upsert", upsert: makeTask() });
    const card = await waitFor(() => {
      const next = document.querySelector<HTMLElement>("[data-task-id='task1']");
      if (!next) throw new Error("recreated task card was not rendered");
      return next;
    });
    await user.click(card);

    expect(await screen.findByTestId("task-detail-prompt")).toBeEmptyDOMElement();
  });

  it("requires confirmation when purging without a recovery delay", async () => {
    const user = userEvent.setup();
    const task = makeTask({ state: "running" });
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/a" }],
      models: {},
      harness: "",
      settings: { baseImage: "", purgeDelay: 0 },
    } as unknown as PreferencesResp);
    vi.mocked(api.getTask).mockResolvedValue(task);
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderApp("/task/@task1+do-something");

    const prompt = await screen.findByTestId("task-detail-prompt");
    await waitFor(() => expect(api.getPreferences).toHaveBeenCalledOnce());
    await waitFor(() => expect(prompt).toHaveFocus());
    await user.keyboard("{Shift>}{Delete}{/Shift}");

    expect(confirm).toHaveBeenCalledOnce();
    expect(api.purgeTask).not.toHaveBeenCalled();
  });

  it("navigates the avatar menu with arrow keys", async () => {
    const user = userEvent.setup();
    renderApp();
    const menu = screen.getByTitle("Menu");
    menu.focus();

    await user.keyboard("{ArrowDown}");
    await waitFor(() => expect(screen.getByRole("menuitem", { name: "Settings" })).toHaveFocus());

    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("menuitem", { name: "Metrics" })).toHaveFocus();

    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("menuitem", { name: "Usage" })).toHaveFocus();

    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("menuitem", { name: "Keyboard shortcuts" })).toHaveFocus();

    await user.keyboard("{ArrowUp}");
    expect(screen.getByRole("menuitem", { name: "Usage" })).toHaveFocus();

    await user.keyboard("{ArrowUp}");
    expect(screen.getByRole("menuitem", { name: "Metrics" })).toHaveFocus();

    await user.keyboard("{ArrowUp}");
    expect(screen.getByRole("menuitem", { name: "Settings" })).toHaveFocus();
  });

  it("opens keyboard shortcut help from F1, question mark, and the account menu", async () => {
    const user = userEvent.setup();
    renderApp();

    const prompt = screen.getByTestId("prompt-input");
    prompt.focus();
    await user.keyboard("{F1}");
    expect(screen.getByTestId("keyboard-shortcuts-dialog")).toBeInTheDocument();
    (screen.getByTestId("keyboard-shortcuts-dialog") as HTMLDialogElement).close();
    await waitFor(() => expect(prompt).toHaveFocus());

    const newTaskButton = screen.getByTestId("new-task-button");
    newTaskButton.focus();
    await user.keyboard("?");
    expect(screen.getByTestId("keyboard-shortcuts-dialog")).toBeInTheDocument();
    (screen.getByTestId("keyboard-shortcuts-dialog") as HTMLDialogElement).close();
    await waitFor(() => expect(newTaskButton).toHaveFocus());

    await user.click(screen.getByTitle("Menu"));
    await user.click(screen.getByRole("menuitem", { name: "Keyboard shortcuts" }));
    expect(screen.getByTestId("keyboard-shortcuts-dialog")).toBeInTheDocument();
  });
});

describe("App repo chips: No repository", () => {
  it("notifies when a waiting task's backend rate limit clears", async () => {
    const blockedTask = makeTask({
      state: "waiting",
      rateLimit: { blocked: true },
    });
    const recoveredTask = makeTask({
      state: "waiting",
      rateLimit: { blocked: false },
    });
    renderApp();

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [blockedTask] });
    dispatchSSE({ kind: "upsert", upsert: recoveredTask });

    await waitFor(() => {
      expect(notifications.notify).toHaveBeenCalledWith(
        "task1",
        "do something quota is available",
        "caic-event-task1",
        {
          enabled: true,
        },
      );
    });
  });

  it("moves from a purged selected task to the next task needing input first", async () => {
    const user = userEvent.setup();
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const killed = makeTask({
      id: "a3",
      title: "kill me",
      state: "running",
      repos: [{ name: "repos/a", branch: "main" }],
    });
    const running = makeTask({
      id: "a2",
      title: "running next",
      state: "running",
      repos: [{ name: "repos/a", branch: "main" }],
    });
    const asking = makeTask({
      id: "a1",
      title: "asking next",
      state: "asking",
      repos: [{ name: "repos/a", branch: "main" }],
    });
    vi.mocked(api.getTask).mockResolvedValue(killed);
    vi.mocked(api.purgeTask).mockResolvedValue({ status: "ok" });
    const { history } = renderApp("/task/@a3+kill-me");

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [killed, running, asking] });
    await waitFor(() => expect(screen.getByTestId("task-detail-prompt")).toHaveFocus());
    const selectedCard = await waitFor(() => {
      const card = document.querySelector<HTMLElement>("[data-task-id='a3']");
      if (!card) throw new Error("selected task card was not rendered");
      return card;
    });
    selectedCard.focus();

    await user.keyboard("{Shift>}{Delete}{/Shift}");

    expect(api.purgeTask).toHaveBeenCalledWith("a3");
    await waitFor(() => expect(history.get()).toContain("/task/@a1+"));
    await waitFor(() => expect(document.querySelector("[data-task-id='a1']")).toHaveFocus());
  });

  it("moves from a stopped selected task to the next alive task", async () => {
    const killed = makeTask({
      id: "a3",
      title: "stop me",
      state: "waiting",
      repos: [{ name: "repos/a", branch: "main" }],
    });
    const next = makeTask({
      id: "a2",
      title: "next task",
      state: "running",
      repos: [{ name: "repos/a", branch: "main" }],
    });
    vi.mocked(api.getTask).mockResolvedValue(killed);
    vi.mocked(api.stopTask).mockResolvedValue({ status: "stopping" });
    const { history } = renderApp("/task/@a3+stop-me");

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [killed, next] });
    const stopButton = await waitFor(() => {
      const button = document.querySelector<HTMLButtonElement>("[data-task-id='a3'] [data-testid='stop-task']");
      if (!button) throw new Error("selected task stop button was not rendered");
      return button;
    });

    await userEvent.setup().click(stopButton);

    expect(api.stopTask).toHaveBeenCalledWith("a3");
    await waitFor(() => expect(history.get()).toContain("/task/@a2+"));
    await waitFor(() => expect(document.querySelector("[data-task-id='a2']")).toHaveFocus());
  });

  it("returns to the new-task prompt after purging the last alive selected task", async () => {
    const user = userEvent.setup();
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const killed = makeTask({
      id: "a3",
      title: "kill me",
      state: "running",
      repos: [{ name: "repos/a", branch: "main" }],
    });
    vi.mocked(api.getTask).mockResolvedValue(killed);
    vi.mocked(api.purgeTask).mockResolvedValue({ status: "ok" });
    const { history } = renderApp("/task/@a3+kill-me");

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [killed] });
    await waitFor(() => expect(screen.getByTestId("task-detail-prompt")).toHaveFocus());
    const selectedCard = await waitFor(() => {
      const card = document.querySelector<HTMLElement>("[data-task-id='a3']");
      if (!card) throw new Error("selected task card was not rendered");
      return card;
    });
    selectedCard.focus();

    await user.keyboard("{Shift>}{Delete}{/Shift}");

    expect(api.purgeTask).toHaveBeenCalledWith("a3");
    await waitFor(() => expect(history.get()).toBe("/"));
    await waitFor(() => expect(screen.getByTestId("prompt-input")).toHaveFocus());
  });

  const dismissPanes = [
    ["diff", "/task/@task1+do-something/diff", () => vi.mocked(api.getTaskDiffIndex).mockRejectedValue(apiError(404))],
    [
      "processes",
      "/task/@task1+do-something/processes",
      () => vi.mocked(api.getTaskProcesses).mockRejectedValue(apiError(404)),
    ],
    ["info", "/task/@task1+do-something/info", () => vi.mocked(api.getTaskInfo).mockRejectedValue(apiError(404))],
  ] as const;
  for (const [_name, route, rejectRefresh] of dismissPanes) {
    it(`dismisses the ${_name} pane when its task refresh returns 404`, async () => {
      rejectRefresh();
      const { history } = renderApp(route);

      await waitFor(() => expect(history.get()).toBe("/"));
    });
  }

  it("does not dismiss a task pane when its refresh returns 403", async () => {
    vi.mocked(api.getTaskDiffIndex).mockRejectedValue(apiError(403));
    const { history } = renderApp("/task/@task1+do-something/diff");

    await waitFor(() => expect(screen.getByText("HTTP 403")).toBeInTheDocument());
    expect(history.get()).toBe("/task/@task1+do-something/diff");
  });

  it("dismisses a deep-linked task detail only when task lookup returns 404", async () => {
    vi.mocked(api.getTask).mockRejectedValue(apiError(404));
    const { history } = renderApp("/task/@missing+gone");

    await waitFor(() => expect(api.getTask).toHaveBeenCalledWith("missing"));
    await waitFor(() => expect(history.get()).toBe("/"));
  });

  it("keeps a deep-linked task detail when task lookup returns 403", async () => {
    vi.mocked(api.getTask).mockRejectedValue(apiError(403));
    const { history } = renderApp("/task/@secret+denied");

    await waitFor(() => expect(api.getTask).toHaveBeenCalledWith("secret"));
    await Promise.resolve();
    expect(history.get()).toBe("/task/@secret+denied");
  });

  it("syncs harness model and effort from per-model preferences", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/a" }],
      harness: "codex",
      models: { claude: "sonnet", codex: "gpt-5" },
      efforts: {
        claude: { sonnet: "max" },
        codex: { "gpt-5": "high", "gpt-5-mini": "minimal" },
      },
      settings: { baseImage: "" },
    } as unknown as PreferencesResp);
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "claude",
        models: [
          {
            id: "sonnet",
            effortOptions: ["low", "medium", "high", "xhigh", "max"],
          },
        ],
        supportsImages: false,
        supportsCompact: false,
      },
      {
        name: "codex",
        models: [
          { id: "gpt-5", effortOptions: ["minimal", "low", "medium", "high"] },
          {
            id: "gpt-5-mini",
            effortOptions: ["minimal", "low", "medium", "high"],
          },
        ],
        supportsImages: false,
        supportsCompact: false,
      },
    ] as unknown as HarnessInfo[]);

    renderApp();

    const harness = await screen.findByRole("combobox", { name: "Harness" });
    const model = screen.getByRole("button", { name: "Model" });
    const effort = () => screen.getByRole("combobox", { name: "Effort" });
    expect(harness).toHaveValue("codex");
    expect(model).toHaveTextContent("gpt-5");
    expect(effort()).toHaveValue("high");

    await user.click(model);
    await user.click(await screen.findByRole("option", { name: "gpt-5-mini" }));
    expect(effort()).toHaveValue("minimal");

    await user.selectOptions(effort(), "low");
    await user.click(model);
    await user.click(await screen.findByRole("option", { name: "gpt-5" }));
    expect(effort()).toHaveValue("high");

    await user.click(model);
    await user.click(await screen.findByRole("option", { name: "gpt-5-mini" }));
    expect(effort()).toHaveValue("low");

    await user.selectOptions(harness, "claude");
    expect(model).toHaveTextContent("sonnet");
    expect(effort()).toHaveValue("max");

    await user.selectOptions(harness, "codex");
    expect(model).toHaveTextContent("gpt-5-mini");
    expect(effort()).toHaveValue("low");

    await user.selectOptions(harness, "claude");
    expect(model).toHaveTextContent("sonnet");
    expect(effort()).toHaveValue("max");
  });

  it("uses effort options reported by the selected model", async () => {
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/a" }],
      harness: "codex",
      models: { codex: "gpt-5" },
      efforts: { codex: { "gpt-5": "high" } },
      settings: { baseImage: "" },
    } as unknown as PreferencesResp);
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "codex",
        models: [{ id: "gpt-5", effortOptions: ["low"] }],
        supportsImages: false,
        supportsCompact: false,
      },
    ] as unknown as HarnessInfo[]);

    renderApp();

    const effort = await screen.findByRole("combobox", { name: "Effort" });
    expect(effort).toHaveValue("");
    expect(screen.getByRole("option", { name: "low" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "high" })).not.toBeInTheDocument();
  });

  it("hides effort controls when a model reports none", async () => {
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/a" }],
      harness: "codex",
      models: { codex: "gpt-5" },
      efforts: { codex: { "gpt-5": "high" } },
      settings: { baseImage: "" },
    } as unknown as PreferencesResp);
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "codex",
        models: [{ id: "gpt-5", effortOptions: [] }],
        supportsImages: false,
        supportsCompact: false,
      },
    ] as unknown as HarnessInfo[]);

    renderApp();

    await waitFor(() => expect(api.listHarnesses).toHaveBeenCalledOnce());
    expect(screen.queryByRole("combobox", { name: "Effort" })).not.toBeInTheDocument();
  });

  it("opens the model picker on ArrowDown without navigating tasks", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/a" }],
      harness: "codex",
      models: { codex: "gpt-5" },
      efforts: {},
      settings: { baseImage: "" },
    } as unknown as PreferencesResp);
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "codex",
        models: [{ id: "gpt-5", effortOptions: [] }],
        supportsImages: false,
        supportsCompact: false,
      },
    ] as unknown as HarnessInfo[]);

    const { history } = renderApp("/");
    // Seed a task card so the global ArrowUp/Down handler would otherwise navigate.
    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "snapshot",
      snapshot: [makeTask({ id: "taskX", title: "other task" })],
    });
    await screen.findByText("other task");
    const model = await screen.findByRole("button", { name: "Model" });
    model.focus();
    await user.keyboard("{ArrowDown}");
    // The global ArrowUp/Down task-navigation handler must ignore the focused
    // combobox trigger, so the route stays put and the picker opens instead.
    expect(history.get()).toBe("/");
    expect(screen.getByRole("listbox")).toBeInTheDocument();
  });

  it("does not mount browser voice in Go Mode host mode", async () => {
    window.goModeHost = {};

    renderApp();

    await waitFor(() => expect(api.listRepos).toHaveBeenCalledOnce());
    expect(screen.queryByTestId("voice-overlay")).not.toBeInTheDocument();
  });

  it("does not mount browser voice when the route has the Go Mode host marker", async () => {
    renderApp("/?goModeHost=1");

    await waitFor(() => expect(api.listRepos).toHaveBeenCalledOnce());
    expect(screen.queryByTestId("voice-overlay")).not.toBeInTheDocument();
  });

  it("shows task numbers while native Go Mode voice is connected", async () => {
    window.goModeHost = { isVoiceConnected: () => true };
    renderApp();

    await waitFor(() => expect(api.listRepos).toHaveBeenCalledOnce());
    dispatchSSE({
      kind: "snapshot",
      snapshot: [makeTask({ id: "taskX", title: "voice task" })],
    });

    expect(await screen.findByText("#1")).toBeInTheDocument();
  });

  it("opens task details from the mobile voice list", async () => {
    window.goModeHost = { isVoiceConnected: () => true };
    vi.spyOn(window, "matchMedia").mockImplementation((query) => ({
      matches: query === "(max-width: 768px)",
      media: query,
      onchange: null,
      addListener: () => undefined,
      removeListener: () => undefined,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      dispatchEvent: () => false,
    }));
    const { history } = renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ id: "task1", title: "Review output", state: "waiting" })] });

    const normal = screen.getByTestId("normal-content");
    expect(within(normal).getByTestId("task-list")).toBeInTheDocument();
    expect(within(normal).getByTestId("new-task-form")).toBeInTheDocument();
    expect(normal.querySelector("header")).toBeInTheDocument();

    const voiceView = await screen.findByTestId("mobile-voice-tasks");
    expect(normal).toHaveAttribute("aria-hidden", "true");
    const link = within(voiceView).getByRole("link", { name: "Task 1, waiting: Review output" });
    expect(link).toHaveAttribute("href", "/task/@task1");
    expect(executeFrontendVoiceTool("focus_task", { task_number: "#1" })).toEqual({ task_id: "task1" });
    expect(history.get()).toBe("/");
    expect(link).toHaveAttribute("aria-current", "true");
    window.goModeHost = { isVoiceConnected: () => false };
    window.dispatchEvent(new Event("gomodevoicechange"));
    await waitFor(() => expect(focusedVoiceTask()).toBeNull());
    window.goModeHost = { isVoiceConnected: () => true };
    window.dispatchEvent(new Event("gomodevoicechange"));
    const restoredLink = await screen.findByRole("link", { name: "Task 1, waiting: Review output" });
    expect(restoredLink).not.toHaveAttribute("aria-current");
    fireEvent.click(restoredLink);

    await waitFor(() => expect(history.get()).toBe("/task/@task1"));
    await waitFor(() => expect(screen.queryByTestId("mobile-voice-tasks")).not.toBeInTheDocument());
    expect(normal).toHaveAttribute("aria-hidden", "false");
    expect(within(normal).getByTestId("detail-pane")).toBeInTheDocument();
  });

  it("focuses desktop task details through the frontend voice tool", async () => {
    window.goModeHost = { isVoiceConnected: () => true };
    const { history } = renderApp();
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ id: "task1", title: "Review output", state: "waiting" })] });
    expect(executeFrontendVoiceTool("focus_task", { task_number: "missing" })).toHaveProperty("error");
    expect(history.get()).toBe("/");
    expect(executeFrontendVoiceTool("focus_task", { task_number: 1 })).toEqual({ task_id: "task1" });
    await waitFor(() => expect(history.get()).toContain("@task1"));
    expect(screen.getByTestId("detail-pane")).toBeInTheDocument();
  });

  it("does not mount browser voice when the server disables the voice gateway", async () => {
    vi.mocked(api.getConfig).mockResolvedValue({
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: false,
      voiceGateway: { mode: "disabled" },
    });

    renderApp();

    await waitFor(() => expect(api.getConfig).toHaveBeenCalledOnce());
    expect(screen.queryByTestId("voice-overlay")).not.toBeInTheDocument();
  });

  it("refreshes browser voice availability when task events reconnect", async () => {
    const disabledConfig = {
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: false,
      voiceGateway: { mode: "disabled" as const },
    };
    const enabledConfig = {
      ...disabledConfig,
      voiceGateway: { mode: "embedded" as const },
    };
    vi.mocked(api.getConfig).mockResolvedValueOnce(disabledConfig).mockResolvedValueOnce(enabledConfig);

    renderApp();

    await waitFor(() => expect(api.getConfig).toHaveBeenCalledOnce());
    expect(screen.queryByTestId("voice-overlay")).not.toBeInTheDocument();

    dispatchOpen();

    await waitFor(() => expect(api.getConfig).toHaveBeenCalledTimes(2));
    await screen.findByTestId("voice-overlay");
  });

  for (const completion of ["success", "failure"] as const) {
    it(`ignores a retired connection's config ${completion} after the current config`, async () => {
      enableImageDrafts();
      const config: Config = {
        displayName: "Initial config",
        tailscaleAvailable: false,
        usbAvailable: false,
        displayAvailable: false,
        sudoAvailable: false,
        gitHubTokenAvailable: false,
        mcpOAuthAvailable: false,
        voiceGateway: { mode: "disabled" },
        imageConstraints: { allowedMediaTypes: ["image/png"], maxImageBytes: 1024, maxPromptImageBytes: 2048 },
      };
      const retired = deferred<Config>();
      const current = deferred<Config>();
      vi.mocked(api.getConfig)
        .mockResolvedValueOnce(config)
        .mockReturnValueOnce(retired.promise)
        .mockReturnValueOnce(current.promise);
      const view = renderApp();
      try {
        await screen.findByTestId("prompt-input");
        await waitForTaskEventsSubscription();
        dispatchOpen();
        await waitFor(() => expect(api.getConfig).toHaveBeenCalledTimes(2));
        fireEvent(window, new Event("offline"));
        fireEvent(window, new Event("online"));
        await waitFor(() => expect(taskEventSubscriptions).toBe(2));
        dispatchOpen();
        await waitFor(() => expect(api.getConfig).toHaveBeenCalledTimes(3));
        current.resolve({ ...config, displayName: "Current config", voiceGateway: { mode: "embedded" } });
        await screen.findByTestId("voice-overlay");
        if (completion === "success") {
          retired.resolve({
            ...config,
            displayName: "Retired config",
            imageConstraints: { allowedMediaTypes: ["image/png"], maxImageBytes: 1, maxPromptImageBytes: 2048 },
          });
          await retired.promise;
        } else {
          retired.reject(new Error("Retired config failure"));
          await expect(retired.promise).rejects.toThrow("Retired config failure");
        }
        expect(document.title).toBe("Current config — caic");
        expect(screen.getByTestId("voice-overlay")).toBeInTheDocument();
        const form = screen.getByTestId("new-task-form");
        chooseDraftImage(form, new File(["ok"], "current.png", { type: "image/png" }));
        expect(within(form).getByRole("img", { name: "attached" })).toBeInTheDocument();
      } finally {
        view.unmount();
      }
    });
  }

  it("keeps browser voice mounted outside Go Mode host mode when the server enables voice", async () => {
    vi.mocked(api.getConfig).mockResolvedValue({
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: false,
      voiceGateway: { mode: "embedded" },
    });

    renderApp();

    await waitFor(() => expect(screen.getByTestId("voice-overlay")).toBeInTheDocument());
  });

  it("returns to the task list from the caic title", async () => {
    const user = userEvent.setup();
    const { history } = renderApp("/settings");

    await user.click(screen.getByRole("button", { name: "caic" }));

    expect(history.get()).toBe("/");
  });

  it("links to the settings page from the user menu", async () => {
    const user = userEvent.setup();
    const { history } = renderApp("/");

    await user.click(screen.getByRole("button", { name: "Menu" }));
    const settingsLink = screen.getByRole("menuitem", { name: "Settings" });

    expect(settingsLink).toHaveAttribute("href", "/settings");

    await user.click(settingsLink);

    expect(history.get()).toBe("/settings");
    expect(screen.getByRole("heading", { name: "Settings" })).toBeInTheDocument();
  });

  it("renders settings as a routed page instead of a dialog", async () => {
    renderApp("/settings");

    expect(screen.getByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => expect(api.getVersion).toHaveBeenCalledOnce());
  });

  it("links to the metrics page from the user menu", async () => {
    const user = userEvent.setup();
    const { history } = renderApp("/");

    await user.click(screen.getByRole("button", { name: "Menu" }));
    const metricsLink = screen.getByRole("menuitem", { name: "Metrics" });

    expect(metricsLink).toHaveAttribute("href", "/metrics");

    await user.click(metricsLink);

    expect(history.get()).toBe("/metrics");
    expect(screen.getByRole("heading", { name: "Metrics" })).toBeInTheDocument();
  });

  it("links to the usage dashboard from the user menu", async () => {
    const user = userEvent.setup();
    const { history } = renderApp("/");

    await user.click(screen.getByRole("button", { name: "Menu" }));
    const usageLink = screen.getByRole("menuitem", { name: "Usage" });

    expect(usageLink).toHaveAttribute("href", "/usage");

    await user.click(usageLink);

    expect(history.get()).toBe("/usage");
    expect(screen.getByRole("heading", { name: "Usage" })).toBeInTheDocument();
  });

  it("renders operation latency as a routed page", async () => {
    vi.mocked(api.getMetrics).mockResolvedValue({
      since: "2026-01-01T00:00:00Z" as ISOTimestamp,
      resource: { serviceName: "caic", serviceVersion: "1.2.3", host: "host-1" },
      series: [
        {
          name: "container.launch",
          outcome: "ok",
          kind: "histogram",
          unit: "s",
          attrs: { "container.runtime": "podman" },
          count: 12,
          samples: 12,
          sum: 18,
          min: 0.12,
          p50: 1.5,
          p95: 2.4,
          max: 2.6,
          last: 2.6,
        },
        {
          name: "container.disk_size",
          outcome: "ok",
          kind: "histogram",
          unit: "By",
          count: 3,
          samples: 3,
          sum: 12288,
          min: 4096,
          p50: 4096,
          p95: 4096,
          max: 4096,
          last: 4096,
        },
        {
          name: "container.instances",
          outcome: "ok",
          kind: "gauge",
          unit: "1",
          count: 2,
          samples: 2,
          sum: 9,
          min: 3,
          p50: 3,
          p95: 5,
          max: 5,
          last: 4,
        },
      ],
    });

    renderApp("/metrics");

    expect(screen.getByRole("heading", { name: "Metrics" })).toBeInTheDocument();
    expect(await screen.findByText(/caic 1\.2\.3 on host-1\./)).toBeInTheDocument();
    const table = await screen.findByRole("table");
    const createRow = within(table).getByRole("rowheader", { name: "container.launch" }).closest("tr");
    if (!createRow) throw new Error("container.launch row is missing");
    expect(within(createRow).getByText("container.runtime=podman")).toBeInTheDocument();
    expect(within(createRow).getByText("1.5s")).toBeInTheDocument();
    expect(within(createRow).getByText("2.4s")).toBeInTheDocument();
    expect(within(createRow).getByText("12")).toBeInTheDocument();

    const sizeRow = within(table).getByRole("rowheader", { name: "container.disk_size" }).closest("tr");
    if (!sizeRow) throw new Error("container.disk_size row is missing");
    expect(within(sizeRow).getByText("12 KiB")).toBeInTheDocument();
    expect(within(sizeRow).getAllByText("4.0 KiB").length).toBe(4);

    // A gauge has no total, spread, or percentiles, so those cells stay blank
    // instead of reporting the sum of its samples.
    const gaugeRow = within(table).getByRole("rowheader", { name: "container.instances" }).closest("tr");
    if (!gaugeRow) throw new Error("container.instances row is missing");
    const gaugeCells = Array.from(gaugeRow.querySelectorAll("td"), (cell) => cell.textContent);
    expect(gaugeCells).toEqual(["gauge", "ok", "—", "2", "—", "—", "—", "—", "5", "4"]);
    expect(within(gaugeRow).queryByText("9")).toBeNull();
  });

  it("refreshes models from settings", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "opencode",
        models: [],
        supportsImages: true,
        supportsCompact: true,
        supportsModelRefresh: true,
      },
    ] as unknown as HarnessInfo[]);
    vi.mocked(api.refreshHarness).mockResolvedValue({
      name: "opencode",
      models: [{ id: "openrouter/stealth/union-alpha", effortOptions: [] }],
      supportsImages: true,
      supportsCompact: true,
      supportsModelRefresh: true,
    } as unknown as HarnessInfo);
    renderApp("/settings?section=server");
    await waitFor(() => expect(api.listHarnesses).toHaveBeenCalledOnce());

    await user.click(await screen.findByRole("button", { name: "opencode" }));

    await waitFor(() => expect(api.refreshHarness).toHaveBeenCalledWith("opencode", {}));
    expect(await screen.findByRole("status", { name: "Model reload status" })).toHaveTextContent(
      "opencode models refreshed.",
    );
  });

  it("reports saving until queued edits settle, then reports failure and retry", async () => {
    const user = userEvent.setup();
    const first = Promise.withResolvers<PreferencesResp>();
    const second = Promise.withResolvers<PreferencesResp>();
    const retry = Promise.withResolvers<PreferencesResp>();
    vi.mocked(api.updatePreferences)
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise)
      .mockImplementationOnce(() => retry.promise);
    renderApp("/settings");
    await screen.findByDisplayValue("15s");
    const status = screen.getByRole("status", { name: "Settings save status" });
    expect(status.querySelector("svg")).not.toBeInTheDocument();
    await user.click(screen.getByRole("textbox", { name: "Docker image" }));
    await user.tab();
    expect(api.updatePreferences).not.toHaveBeenCalled();
    expect(status.querySelector("svg")).not.toBeInTheDocument();
    await user.click(screen.getByRole("checkbox", { name: "Auto-fix CI failures" }));
    await user.click(screen.getByRole("checkbox", { name: "Review and fix new PRs" }));
    expect(status).toHaveTextContent("Saving settings…");
    const original = await api.getPreferences();
    first.resolve(original);
    await waitFor(() => expect(api.updatePreferences).toHaveBeenCalledTimes(2));
    expect(status).toHaveTextContent("Saving settings…");
    second.reject(new Error("Settings storage unavailable"));
    await waitFor(() => expect(status).toHaveTextContent("Settings not saved"));
    expect(screen.getByRole("alert")).toHaveTextContent("Settings storage unavailable");
    await user.click(screen.getByRole("checkbox", { name: "Auto-fix CI failures" }));
    expect(status).toHaveTextContent("Saving settings…");
    retry.resolve(original);
    await waitFor(() => expect(status).toHaveTextContent("Settings saved"));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("keeps local drafts unsaved when earlier requests complete", async () => {
    const user = userEvent.setup();
    const pending = Promise.withResolvers<PreferencesResp>();
    vi.mocked(api.updatePreferences).mockImplementationOnce(() => pending.promise);
    renderApp("/settings");
    const input = await screen.findByRole("textbox", { name: "Purge delay" });
    await waitFor(() => expect(input).toHaveValue("15s"));
    const status = screen.getByRole("status", { name: "Settings save status" });
    await user.click(screen.getByRole("checkbox", { name: "Auto-fix CI failures" }));
    await user.clear(input);
    await user.type(input, "30m");
    expect(status).toHaveTextContent("Unsaved settings");
    pending.resolve(await api.getPreferences());
    await waitFor(() => expect(api.updatePreferences).toHaveBeenCalledOnce());
    expect(status).toHaveTextContent("Unsaved settings");
    await user.tab();
    await waitFor(() => expect(status).toHaveTextContent("Settings saved"));
    await user.clear(input);
    await user.type(input, "invalid");
    await user.click(screen.getByRole("checkbox", { name: "Review and fix new PRs" }));
    await waitFor(() => expect(api.updatePreferences).toHaveBeenCalledTimes(3));
    expect(status).toHaveTextContent("Unsaved settings");
  });

  it("starts an image refresh from settings", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getConfig).mockResolvedValue({
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: false,
      voiceGateway: { mode: "disabled" },
      runtimes: [{ name: "docker" }, { name: "podman" }],
    });
    vi.mocked(api.startImageRefresh).mockResolvedValue({ state: "succeeded" });
    renderApp("/settings");
    expect(screen.queryByRole("combobox", { name: "Runtime to refresh" })).not.toBeInTheDocument();
    expect(api.updatePreferences).not.toHaveBeenCalled();
    await user.click(await screen.findByRole("button", { name: "Refresh image and coding agents for podman" }));
    await waitFor(() => expect(api.startImageRefresh).toHaveBeenCalledWith("podman", {}));
    await waitFor(() => expect(api.getImageRefresh).toHaveBeenCalledWith("podman"));
    expect(within(screen.getByRole("group", { name: "podman" })).getByRole("status")).toHaveTextContent(
      "Image and coding agents refreshed. New tasks will use the updated image.",
    );
    expect(within(screen.getByRole("group", { name: "docker" })).queryByRole("status")).not.toBeInTheDocument();
  });

  it("shows scheduled image warmup in settings", async () => {
    vi.mocked(api.getConfig).mockResolvedValue({
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: false,
      voiceGateway: { mode: "disabled" },
      runtimes: [{ name: "docker" }, { name: "podman" }],
    });
    vi.mocked(api.getImageRefresh).mockImplementation(async (runtimeName) =>
      runtimeName === "docker" ? { state: "running", scheduled: true } : { state: "idle" },
    );
    renderApp("/settings");
    expect(await screen.findByText("Scheduled image warmup is running on this runtime…")).toBeInTheDocument();
    const refresh = screen.getByRole("button", { name: "Refresh image and coding agents for docker" });
    expect(refresh).toBeDisabled();
    expect(refresh.querySelector("span")).toBeNull();
    expect(screen.getByRole("button", { name: "Refresh image and coding agents for podman" })).toBeEnabled();
  });

  it("waits for image settings to save before refreshing", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getConfig).mockResolvedValue({
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: false,
      voiceGateway: { mode: "disabled" },
      runtimes: [{ name: "docker" }],
    });
    let finishSave: ((value: PreferencesResp) => void) | undefined;
    vi.mocked(api.updatePreferences).mockReturnValueOnce(
      new Promise((resolve) => {
        finishSave = resolve;
      }),
    );
    vi.mocked(api.startImageRefresh).mockResolvedValue({ state: "succeeded" });
    renderApp("/settings");
    const image = await screen.findByRole("textbox", { name: "Docker image" });
    await user.clear(image);
    await user.type(image, "example.com/new-agent:v2");
    await user.click(screen.getByRole("button", { name: "Refresh image and coding agents for docker" }));
    await waitFor(() => expect(api.updatePreferences).toHaveBeenCalled());
    expect(api.startImageRefresh).not.toHaveBeenCalled();
    const settings = vi.mocked(api.updatePreferences).mock.calls.at(-1)?.[0].settings;
    if (!settings || !finishSave) throw new Error("settings save was not started");
    finishSave({ repositories: [], models: {}, harness: "", settings } as PreferencesResp);
    await waitFor(() => expect(api.startImageRefresh).toHaveBeenCalledWith("docker", {}));
  });

  it("saves the task purge delay", async () => {
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [],
      models: {},
      harness: "",
      settings: { baseImage: "", purgeDelay: 90_000_000_000 },
    } as unknown as PreferencesResp);
    renderApp("/settings");

    const input = await screen.findByRole("textbox", { name: "Purge delay" });
    await waitFor(() => expect(input).toHaveValue("1m30s"));
    fireEvent.change(input, { target: { value: "1m31s" } });

    await waitFor(() =>
      expect(api.updatePreferences).toHaveBeenCalledWith({
        settings: expect.objectContaining({ purgeDelay: 91_000_000_000 }),
      }),
    );
  });

  it("does not request or show remote MCP grants when MCP OAuth is unavailable", async () => {
    vi.mocked(api.getConfig).mockResolvedValue({
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: false,
      voiceGateway: { mode: "disabled" },
    });

    renderApp("/settings?section=server");

    await waitFor(() => expect(api.getConfig).toHaveBeenCalledOnce());
    expect(api.listOAuthGrants).not.toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "MCP clients" })).not.toBeInTheDocument();
  });

  it("loads and shows remote MCP grants when MCP OAuth is available", async () => {
    vi.mocked(api.getConfig).mockResolvedValue({
      imageConstraints: {
        allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
        maxImageBytes: 10485760,
        maxPromptImageBytes: 20971520,
      },
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      mcpOAuthAvailable: true,
      voiceGateway: { mode: "disabled" },
    });

    renderApp("/settings?section=server");

    await waitFor(() => expect(api.listOAuthGrants).toHaveBeenCalledOnce());
    expect(screen.getByRole("heading", { name: "MCP clients" })).toBeInTheDocument();
  });

  it("saves read-only custom mounts", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/a" }],
      models: {},
      harness: "",
      settings: {
        baseImage: "",
        customMounts: [
          {
            hostPath: "/host/data",
            containerPath: "/container/data",
            resolvedContainerPath: "/container/data",
            enabled: true,
            readOnly: false,
          },
        ],
      },
    } as unknown as PreferencesResp);

    renderApp("/settings?section=storage");

    await screen.findByDisplayValue("/host/data");
    await user.click(screen.getByRole("checkbox", { name: "Read only" }));

    await waitFor(() =>
      expect(api.updatePreferences).toHaveBeenCalledWith({
        settings: expect.objectContaining({
          customMounts: [
            {
              hostPath: "/host/data",
              containerPath: "/container/data",
              enabled: true,
              readOnly: true,
            },
          ],
        }),
      }),
    );
  });

  it("renders server-derived container-path defaults", async () => {
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [],
      models: {},
      harness: "",
      settings: {
        cacheMappings: [
          {
            hostPath: "~/.cache/example",
            containerPath: "",
            resolvedContainerPath: "/home/user/.cache/example",
            enabled: true,
          },
        ],
        customMounts: [
          {
            hostPath: "~/Documents",
            containerPath: "",
            resolvedContainerPath: "/home/user/Documents",
            enabled: true,
            readOnly: false,
          },
        ],
      },
    } as unknown as PreferencesResp);

    renderApp("/settings?section=storage");

    await screen.findByDisplayValue("~/.cache/example");
    expect(screen.getAllByLabelText("Container path")[0]).toHaveAttribute("placeholder", "~/.cache/example");
    expect(screen.getAllByLabelText("Container path")[1]).toHaveAttribute("placeholder", "~/Documents");
  });

  it("keeps a customized container path when the host path changes", async () => {
    const user = userEvent.setup();
    renderApp("/settings?section=storage");

    const cacheSection = screen.getByRole("heading", {
      name: "Custom caches",
    }).parentElement;
    if (!cacheSection) throw new Error("Custom caches section is missing");
    const caches = within(cacheSection);
    await user.click(caches.getByRole("button", { name: "Add cache" }));
    await user.type(caches.getByLabelText("Host path"), "~/.cache/example");
    await user.clear(caches.getByLabelText("Container path"));
    await user.type(caches.getByLabelText("Container path"), "/var/cache/example");
    await user.clear(caches.getByLabelText("Host path"));
    await user.type(caches.getByLabelText("Host path"), "~/.cache/other");

    expect(caches.getByLabelText("Container path")).toHaveValue("/var/cache/example");
  });

  it("leaves the container path blank for a non-home host path", async () => {
    const user = userEvent.setup();
    renderApp("/settings?section=storage");

    const mountSection = screen.getByRole("heading", {
      name: "Custom mounts",
    }).parentElement;
    if (!mountSection) throw new Error("Custom mounts section is missing");
    const mounts = within(mountSection);
    await user.click(mounts.getByRole("button", { name: "Add mount" }));
    await user.type(mounts.getByLabelText("Host path"), "/srv/shared");

    expect(mounts.getByLabelText("Container path")).toHaveAttribute("placeholder", "Container path");
    expect(mounts.getByLabelText("Container path")).toHaveValue("");
    await user.clear(mounts.getByLabelText("Host path"));
    await user.type(mounts.getByLabelText("Host path"), "~/../../etc");
    expect(mounts.getByLabelText("Container path")).toHaveAttribute("placeholder", "Container path");
  });

  it("shows mapping validation failures and sends only editable mapping fields", async () => {
    const user = userEvent.setup();
    vi.mocked(api.updatePreferences).mockRejectedValueOnce(
      new Error("cacheMappings[0]: host path must be absolute or home-relative"),
    );
    renderApp("/settings?section=storage");

    const cacheSection = screen.getByRole("heading", {
      name: "Custom caches",
    }).parentElement;
    if (!cacheSection) throw new Error("Custom caches section is missing");
    const caches = within(cacheSection);
    await user.click(caches.getByRole("button", { name: "Add cache" }));
    await user.type(caches.getByLabelText("Host path"), "cache");
    await user.tab();

    await waitFor(() =>
      expect(api.updatePreferences).toHaveBeenCalledWith({
        settings: expect.objectContaining({
          cacheMappings: [{ hostPath: "cache", containerPath: "", enabled: true }],
        }),
      }),
    );
    expect(screen.getByRole("alert")).toHaveTextContent(
      "cacheMappings[0]: host path must be absolute or home-relative",
    );
  });

  it("has no chips after removing the last one", async () => {
    const user = userEvent.setup();
    renderApp();

    // Wait for initial load: repos/a chip should appear.
    await waitFor(() => {
      expect(screen.getByTestId("chip-label-repos/a")).toBeInTheDocument();
    });

    // Remove repos/a chip.
    await user.click(screen.getByRole("button", { name: "Remove repos/a" }));

    // No chips remain.
    expect(chipPathValues()).toHaveLength(0);
  });

  it("stays empty after repos SSE event updates CI status", async () => {
    const user = userEvent.setup();
    renderApp();

    await waitFor(() => {
      expect(screen.getByTestId("chip-label-repos/a")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Remove repos/a" }));
    expect(chipPathValues()).toHaveLength(0);

    // Simulate a "repos" SSE event (e.g. CI status update) which triggers setRepos.
    const repoAUpdated: Repo = {
      path: "repos/a",
      branch: "main",
      baseBranch: { name: "main" },
      remoteURL: "",
      ci: "success" as const,
    };
    dispatchSSE({ kind: "repos", repos: [repoAUpdated] });

    await waitFor(() => {
      // No chip should have been added back.
      expect(chipPathValues()).toHaveLength(0);
    });
  });

  it("uses the saved model preference when opening the fork dialog", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/a" }],
      harness: "pi",
      models: { pi: "openai-codex/gpt-5.6-terra" },
      settings: { baseImage: "" },
    } as unknown as PreferencesResp);
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "pi",
        models: [
          { id: "openai-codex/gpt-5.5", effortOptions: [] },
          { id: "openai-codex/gpt-5.6-terra", effortOptions: [] },
        ],
        supportsImages: false,
        supportsCompact: false,
      },
    ] as unknown as HarnessInfo[]);

    renderApp("/task/@task1");
    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "snapshot",
      snapshot: [
        makeTask({
          harness: "pi",
          reportedModel: "openai-codex/gpt-5.5",
          repos: [{ name: "repos/a", branch: "fork-source" }],
        }),
      ],
    });

    await user.click(await screen.findByRole("button", { name: "Context actions" }));
    await user.click(screen.getByRole("menuitem", { name: "Fork" }));

    expect(screen.getByRole("button", { name: "Fork Model" })).toHaveTextContent("openai-codex/gpt-5.6-terra");
  });

  it("generates an editable handoff prompt for a fork", async () => {
    const user = userEvent.setup();
    renderApp("/task/@task1");
    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "snapshot",
      snapshot: [makeTask({ repos: [{ name: "repos/a", branch: "fork-source" }] })],
    });

    await user.click(await screen.findByRole("button", { name: "Context actions" }));
    await user.click(screen.getByRole("menuitem", { name: "Fork" }));
    await user.click(screen.getByTestId("generate-handoff"));

    expect(api.getTaskHandoff).toHaveBeenCalledWith("task1");
    await waitFor(() => expect(screen.getByTestId("fork-prompt-input")).toHaveTextContent("Generated handoff prompt"));
    await user.clear(screen.getByTestId("fork-prompt-input"));
    await user.type(screen.getByTestId("fork-prompt-input"), "Edited handoff prompt");
    expect(screen.getByTestId("fork-prompt-input")).toHaveTextContent("Edited handoff prompt");
  });

  it("keeps ordinary fork harness selection unrestricted and unranked", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "claude",
        models: [],
        supportsImages: false,
        supportsCompact: false,
        quotaGroup: "claudecode",
      },
      {
        name: "codex",
        models: [],
        supportsImages: false,
        supportsCompact: false,
        quotaGroup: "codex",
      },
      { name: "pi", models: [], supportsImages: false, supportsCompact: false },
    ] as unknown as HarnessInfo[]);
    renderApp("/task/@task1");
    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "snapshot",
      snapshot: [makeTask({ repos: [{ name: "repos/a", branch: "fork-source" }] })],
    });

    await user.click(await screen.findByRole("button", { name: "Context actions" }));
    await user.click(screen.getByRole("menuitem", { name: "Fork" }));

    const harnessSelect = screen.getByRole("combobox", {
      name: "Fork Harness",
    });
    expect(
      within(harnessSelect)
        .getAllByRole("option")
        .map((option) => option.textContent),
    ).toEqual(["claude", "codex", "pi"]);
    expect(screen.queryByTestId("fork-target-status")).not.toBeInTheDocument();
  });

  for (const targetModel of ["", "gpt-5"]) {
    it(`opens an editable handoff with the recommended harness ${targetModel || "default"} model and effort`, async () => {
      const user = userEvent.setup();
      vi.mocked(api.getPreferences).mockResolvedValue({
        repositories: [{ path: "repos/a" }],
        harness: "claude",
        models: { claude: "sonnet", codex: targetModel },
        efforts: { claude: { sonnet: "max" }, codex: { "gpt-5": "high" } },
        settings: { baseImage: "" },
      } as unknown as PreferencesResp);
      vi.mocked(api.listHarnesses).mockResolvedValue([
        {
          name: "claude",
          models: [{ id: "sonnet", effortOptions: ["max"] }],
          supportsImages: false,
          supportsCompact: false,
          quotaGroup: "claudecode",
        },
        {
          name: "codex",
          models: [{ id: "gpt-5", effortOptions: ["high"] }],
          supportsImages: false,
          supportsCompact: false,
          quotaGroup: "codex",
        },
        { name: "pi", models: [], supportsImages: false, supportsCompact: false },
      ] as unknown as HarnessInfo[]);
      vi.mocked(api.getUsage).mockResolvedValue({
        local: { windows: [] },
        providers: [
          {
            provider: "codex",
            label: "Codex",
            logoUrl: "",
            authKind: "oauth",
            usageUrl: "",
            fetchStatus: "fresh",
            rateLimits: [{ window: "primary", utilization: 0.25 }],
          },
        ],
      });
      vi.mocked(api.getTaskHandoff).mockResolvedValue({
        prompt: "Quota-aware generated handoff",
      });
      vi.mocked(api.forkTask).mockResolvedValue(
        makeTask({
          id: "forked-task",
          harness: "codex",
          repos: [{ name: "repos/a", branch: "forked-branch" }],
        }),
      );
      renderApp("/task/@task1");
      await waitForTaskEventsSubscription();
      dispatchSSE({
        kind: "snapshot",
        snapshot: [
          makeTask({
            state: "waiting",
            repos: [{ name: "repos/a", branch: "fork-source" }],
            rateLimit: {
              blocked: true,
              quotaGroup: "claudecode",
              window: "5h",
              resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
            },
          }),
        ],
      });

      await user.click(await screen.findByTestId("quota-recovery-detail-action"));

      expect(screen.getByRole("heading", { name: "Continue after quota limit" })).toBeInTheDocument();
      expect(api.getTaskHandoff).toHaveBeenCalledWith("task1");
      await waitFor(() =>
        expect(screen.getByTestId("fork-prompt-input")).toHaveTextContent("Quota-aware generated handoff"),
      );
      await user.clear(screen.getByTestId("fork-prompt-input"));
      await user.type(screen.getByTestId("fork-prompt-input"), "Edited quota recovery handoff");
      const harnessSelect = screen.getByRole("combobox", {
        name: "Fork Harness",
      });
      expect(
        within(harnessSelect)
          .getAllByRole("option")
          .map((option) => option.textContent),
      ).toEqual(["codex — Available · Recommended", "pi — Quota status unknown", "claude — Same exhausted quota"]);
      expect(harnessSelect).toHaveValue("codex");
      expect(screen.getByRole("button", { name: "Fork Model" })).toHaveTextContent(targetModel || "Default model");
      if (targetModel) {
        expect(screen.getByRole("combobox", { name: "Fork Effort" })).toHaveValue("high");
      } else {
        expect(screen.queryByRole("combobox", { name: "Fork Effort" })).not.toBeInTheDocument();
      }
      fireEvent.change(harnessSelect, { target: { value: "pi" } });
      expect(screen.getByTestId("fork-target-status")).toHaveTextContent("Selected harness: Quota status unknown");
      fireEvent.change(harnessSelect, { target: { value: "codex" } });
      await user.click(screen.getByTestId("fork-submit"));

      expect(api.forkTask).toHaveBeenCalledWith(
        "task1",
        expect.objectContaining({
          prompt: { text: "Edited quota recovery handoff" },
          harness: "codex",
          model: targetModel || undefined,
          effort: targetModel ? "high" : undefined,
        }),
      );
    });
  }

  it("adopts a delayed recommendation without overwriting explicit target choices", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listHarnesses).mockResolvedValue([
      {
        name: "claude",
        models: [
          { id: "claude-default", effortOptions: ["low", "high"] },
          { id: "claude-explicit", effortOptions: ["low", "high"] },
        ],
        supportsImages: false,
        supportsCompact: false,
        quotaGroup: "claudecode",
      },
      {
        name: "codex",
        models: [{ id: "codex-default", effortOptions: ["low", "high"] }],
        supportsImages: false,
        supportsCompact: false,
        quotaGroup: "codex",
      },
      { name: "pi", models: [], supportsImages: false, supportsCompact: false },
    ] as unknown as HarnessInfo[]);
    renderApp("/task/@task1");
    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "snapshot",
      snapshot: [
        makeTask({
          state: "waiting",
          repos: [{ name: "repos/a", branch: "fork-source" }],
          rateLimit: {
            blocked: true,
            quotaGroup: "claudecode",
            window: "5h",
            resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
          },
        }),
      ],
    });

    await user.click(await screen.findByTestId("quota-recovery-detail-action"));
    const harnessSelect = screen.getByRole("combobox", {
      name: "Fork Harness",
    });
    expect(harnessSelect).toHaveValue("claude");

    const codexUsage = (fetchStatus: "fresh" | "stale") => ({
      local: { windows: [] },
      providers: [
        {
          provider: "codex",
          label: "Codex",
          logoUrl: "",
          authKind: "oauth",
          usageUrl: "",
          fetchStatus,
          rateLimits: [{ window: "primary", utilization: 0.25 }],
        },
      ],
    });
    dispatchUsageSSE(codexUsage("fresh"));
    await waitFor(() => expect(harnessSelect).toHaveValue("codex"));

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    dispatchUsageSSE(codexUsage("stale"));
    await user.click(screen.getByTestId("quota-recovery-detail-action"));
    const reopenedSelect = screen.getByRole("combobox", {
      name: "Fork Harness",
    });
    expect(reopenedSelect).toHaveValue("claude");
    fireEvent.change(reopenedSelect, { target: { value: "pi" } });
    dispatchUsageSSE(codexUsage("fresh"));
    await Promise.resolve();
    expect(reopenedSelect).toHaveValue("pi");

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    dispatchUsageSSE(codexUsage("stale"));
    await user.click(screen.getByTestId("quota-recovery-detail-action"));
    await user.click(screen.getByRole("button", { name: "Fork Model" }));
    await user.click(
      within(screen.getByRole("listbox", { name: "Fork Model" })).getByRole("option", {
        name: "claude-explicit",
      }),
    );
    dispatchUsageSSE(codexUsage("fresh"));
    await Promise.resolve();
    expect(screen.getByRole("combobox", { name: "Fork Harness" })).toHaveValue("claude");
    expect(screen.getByRole("button", { name: "Fork Model" })).toHaveTextContent("claude-explicit");

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    dispatchUsageSSE(codexUsage("stale"));
    await user.click(screen.getByTestId("quota-recovery-detail-action"));
    fireEvent.change(screen.getByRole("combobox", { name: "Fork Effort" }), {
      target: { value: "high" },
    });
    dispatchUsageSSE(codexUsage("fresh"));
    await Promise.resolve();
    expect(screen.getByRole("combobox", { name: "Fork Harness" })).toHaveValue("claude");
    expect(screen.getByRole("combobox", { name: "Fork Effort" })).toHaveValue("high");
  });

  it("does not apply a closed recovery handoff to a later ordinary fork of the same task", async () => {
    const user = userEvent.setup();
    const staleHandoff = deferred<Awaited<ReturnType<typeof api.getTaskHandoff>>>();
    vi.mocked(api.getTaskHandoff).mockReturnValueOnce(staleHandoff.promise);
    vi.mocked(api.forkTask).mockResolvedValue(
      makeTask({
        id: "ordinary-fork",
        repos: [{ name: "repos/a", branch: "ordinary-fork" }],
      }),
    );
    renderApp("/task/@task1");
    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "snapshot",
      snapshot: [
        makeTask({
          state: "waiting",
          repos: [{ name: "repos/a", branch: "fork-source" }],
          rateLimit: {
            blocked: true,
            window: "5h",
            resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
          },
        }),
      ],
    });

    await user.click(await screen.findByTestId("quota-recovery-detail-action"));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: "Context actions" }));
    await user.click(screen.getByRole("menuitem", { name: "Fork" }));

    staleHandoff.resolve({ prompt: "Stale quota recovery handoff" });
    await staleHandoff.promise;
    await Promise.resolve();

    expect(screen.getByTestId("fork-prompt-input")).toHaveTextContent("");
    await user.type(screen.getByTestId("fork-prompt-input"), "Ordinary fork prompt");
    await user.click(screen.getByTestId("fork-submit"));

    expect(api.forkTask).toHaveBeenCalledWith(
      "task1",
      expect.objectContaining({
        prompt: { text: "Ordinary fork prompt" },
      }),
    );
  });

  it("does not dismiss the fork dialog when it is clicked", async () => {
    const user = userEvent.setup();
    renderApp("/task/@task1");
    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "snapshot",
      snapshot: [makeTask({ repos: [{ name: "repos/a", branch: "fork-source" }] })],
    });

    await user.click(await screen.findByRole("button", { name: "Context actions" }));
    await user.click(screen.getByRole("menuitem", { name: "Fork" }));

    const dialog = screen.getByTestId("fork-dialog");
    await user.click(dialog);

    expect(dialog).toBeInTheDocument();
    expect(dialog).toHaveAttribute("open");
  });

  it("dismisses the fork dialog when its backdrop is clicked", async () => {
    const user = userEvent.setup();
    renderApp("/task/@task1");
    await waitForTaskEventsSubscription();
    dispatchSSE({
      kind: "snapshot",
      snapshot: [makeTask({ repos: [{ name: "repos/a", branch: "fork-source" }] })],
    });

    await user.click(await screen.findByRole("button", { name: "Context actions" }));
    await user.click(screen.getByRole("menuitem", { name: "Fork" }));

    const dialog = screen.getByTestId("fork-dialog");
    vi.spyOn(dialog, "getBoundingClientRect").mockReturnValue(new DOMRect(100, 100, 400, 300));
    fireEvent.click(dialog, { clientX: 50, clientY: 50 });

    expect(screen.queryByTestId("fork-dialog")).not.toBeInTheDocument();
  });

  it("keeps a newer task-list SSE update when the create response arrives late", async () => {
    const user = userEvent.setup();
    const createResponse = deferred<Task>();
    vi.mocked(api.createTask).mockReturnValueOnce(createResponse.promise);
    const { history } = renderApp();

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [] });
    await user.type(screen.getByTestId("prompt-input"), "do something");
    await user.click(screen.getByTestId("submit-task"));
    await waitFor(() => expect(api.createTask).toHaveBeenCalledOnce());

    dispatchSSE({
      kind: "upsert",
      upsert: makeTask({
        state: "waiting",
        stateUpdatedAt: "2026-01-01T00:00:01Z" as ISOTimestamp,
      }),
    });
    createResponse.resolve(
      makeTask({
        state: "pending",
        stateUpdatedAt: "2026-01-01T00:00:00Z" as ISOTimestamp,
      }),
    );

    await waitFor(() => expect(history.get()).toContain("/task/@task1"));
    await expect(screen.getByTestId("state-badge")).toHaveTextContent("waiting");
  });

  it("creates task without repos when no chips are selected", async () => {
    const user = userEvent.setup();
    renderApp();

    await waitFor(() => {
      expect(screen.getByTestId("chip-label-repos/a")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Remove repos/a" }));

    await user.type(screen.getByTestId("prompt-input"), "do something");
    await user.click(screen.getByTestId("submit-task"));

    await waitFor(() => expect(api.createTask).toHaveBeenCalledOnce());
    const call = vi.mocked(api.createTask).mock.calls[0][0];
    expect(call.repos).toBeUndefined();
  });

  it("enables CAIC MCP delegation by default", async () => {
    const user = userEvent.setup();
    renderApp();

    await user.type(screen.getByTestId("prompt-input"), "review the change");
    await user.click(screen.getByTestId("submit-task"));

    await waitFor(() => expect(api.createTask).toHaveBeenCalledOnce());
    expect(vi.mocked(api.createTask).mock.calls[0][0].caicMCP).toBe(true);
  });

  it("omits the CAIC MCP delegation grant when disabled", async () => {
    const user = userEvent.setup();
    renderApp();

    await user.type(screen.getByTestId("prompt-input"), "delegate the review");
    await user.click(screen.getByRole("checkbox", { name: "Enable CAIC MCP delegation for this task" }));
    await user.click(screen.getByTestId("submit-task"));

    await waitFor(() => expect(api.createTask).toHaveBeenCalledOnce());
    expect(vi.mocked(api.createTask).mock.calls[0][0].caicMCP).toBeUndefined();
  });

  it("submits with Ctrl+Enter from a feature toggle without changing the toggle", async () => {
    const user = userEvent.setup();
    renderApp();

    await user.type(screen.getByTestId("prompt-input"), "review the change");
    const toggle = screen.getByRole("checkbox", { name: "Enable CAIC MCP delegation for this task" });
    expect(toggle).toBeChecked();
    toggle.focus();
    await user.keyboard("{Enter}");
    expect(toggle).not.toBeChecked();
    expect(api.createTask).not.toHaveBeenCalled();

    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(api.createTask).toHaveBeenCalledOnce());
    expect(toggle).not.toBeChecked();
    expect(vi.mocked(api.createTask).mock.calls[0][0].initialPrompt.text).toBe("review the change");
    expect(vi.mocked(api.createTask).mock.calls[0][0].caicMCP).toBeUndefined();
  });
});

describe("App repo chip ordering", () => {
  it("defaults to the last-used repo from preferences on load", async () => {
    // getPreferences returns repos/b as MRU first.
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/b" }, { path: "repos/a" }],
      models: {},
      harness: "",
      settings: { baseImage: "" },
    } as unknown as PreferencesResp);
    renderApp();

    await waitFor(() => {
      expect(screen.getByTestId("chip-label-repos/b")).toBeInTheDocument();
      expect(screen.queryByTestId("chip-label-repos/a")).not.toBeInTheDocument();
    });
  });

  it("cloned repo appears in add-dropdown (not Recent) before first task", async () => {
    const user = userEvent.setup();
    renderApp();

    // Wait for initial load: repos/a chip visible.
    await waitFor(() => {
      expect(screen.getByTestId("chip-label-repos/a")).toBeInTheDocument();
    });

    // Clone a new repo.
    await user.click(screen.getByTestId("clone-toggle"));
    await user.type(screen.getByTestId("clone-url"), "https://github.com/org/new.git");
    await user.click(screen.getByTestId("clone-submit"));
    await waitFor(() => expect(screen.queryByTestId("clone-url")).not.toBeInTheDocument());

    // After clone, repos/new is the single selected chip (clone replaces selection).
    await waitFor(() => {
      expect(screen.getByTestId("chip-label-repos/new")).toBeInTheDocument();
    });

    // Remove the repos/new chip so we can inspect the add-dropdown.
    await user.click(screen.getByRole("button", { name: "Remove repos/new" }));

    // Open the add-dropdown.
    await user.click(screen.getByTestId("add-repo-button"));
    const dropdown = screen.getByTestId("add-repo-dropdown");

    // repos/new must appear in "All repositories" section (no Recent label next to it).
    const groupLabels = Array.from(dropdown.children)
      .filter((el) => el.tagName === "DIV")
      .map((el) => el.textContent);
    const options = Array.from(dropdown.querySelectorAll("button")).map((b) => b.textContent);

    // repos/a is recent; repos/new is not — so Recent group should be present.
    expect(groupLabels).toContain("Recent");
    expect(groupLabels).toContain("All repositories");
    expect(options).toContain("repos/new");
    // repos/new should come after repos/a (in All repositories, not Recent).
    const recentIdx = groupLabels.indexOf("Recent");
    const allIdx = groupLabels.indexOf("All repositories");
    expect(allIdx).toBeGreaterThan(recentIdx);
  });

  it("cloned repo moves to Recent section in add-dropdown after first task", async () => {
    const user = userEvent.setup();
    renderApp();

    await waitFor(() => {
      expect(screen.getByTestId("chip-label-repos/a")).toBeInTheDocument();
    });

    // Clone a new repo.
    await user.click(screen.getByTestId("clone-toggle"));
    await user.type(screen.getByTestId("clone-url"), "https://github.com/org/new.git");
    await user.click(screen.getByTestId("clone-submit"));
    await waitFor(() => expect(screen.queryByTestId("clone-url")).not.toBeInTheDocument());

    // Submit a task for repos/new (it's the current chip after clone).
    await user.type(screen.getByTestId("prompt-input"), "do something");
    await user.click(screen.getByTestId("submit-task"));
    await waitFor(() => expect(api.createTask).toHaveBeenCalledOnce());

    // Remove chip to inspect the dropdown.
    await user.click(screen.getByRole("button", { name: "Remove repos/new" }));
    await user.click(screen.getByTestId("add-repo-button"));
    const dropdown = screen.getByTestId("add-repo-dropdown");

    // After first task, repos/new is promoted to Recent.
    const groupLabels = Array.from(dropdown.children)
      .filter((el) => el.tagName === "DIV")
      .map((el) => el.textContent);
    expect(groupLabels).toContain("Recent");

    // repos/new should now appear before the "All repositories" divider (i.e. in Recent).
    const nodes = Array.from(dropdown.children);
    const recentLabelIdx = nodes.findIndex((n) => n.textContent === "Recent");
    const allLabelIdx = nodes.findIndex((n) => n.textContent === "All repositories");
    const newOptionIdx = nodes.findIndex((n) => n.textContent === "repos/new");
    expect(newOptionIdx).toBeGreaterThan(recentLabelIdx);
    if (allLabelIdx >= 0) {
      expect(newOptionIdx).toBeLessThan(allLabelIdx);
    }
  });
});

describe("SSE test harness", () => {
  it("stops delivering events to a closed subscription", () => {
    const received: string[] = [];
    const first = new FakeEventSource();
    first.addEventListener("message", () => received.push("first"));
    const second = new FakeEventSource();
    second.addEventListener("message", () => received.push("second"));

    first.close();
    dispatchSSE({ kind: "upsert" });

    expect(received).toEqual(["second"]);
    second.close();
  });

  it("reports a dispatch no live subscription can receive", () => {
    expect(() => dispatchSSE({ kind: "upsert" })).toThrow(/no live subscription/);
  });
});

it("edits CPU settings independently for each runtime", async () => {
  const user = userEvent.setup();
  const config: Config = {
    imageConstraints: {
      allowedMediaTypes: ["image/png", "image/jpeg", "image/gif", "image/webp"],
      maxImageBytes: 10485760,
      maxPromptImageBytes: 20971520,
    },
    displayName: "test",
    tailscaleAvailable: false,
    usbAvailable: false,
    displayAvailable: false,
    sudoAvailable: false,
    gitHubTokenAvailable: false,
    mcpOAuthAvailable: false,
    voiceGateway: { mode: "disabled" },
    runtimes: [{ name: "docker" }, { name: "podman" }],
  };
  const prefs: PreferencesResp = {
    repositories: [],
    settings: {
      autoFixOnCIFailure: false,
      autoFixOnPROpen: false,
      purgeDelay: 15_000_000_000,
      runtimeName: "docker",
      runtimeSettings: {
        docker: { containerPlatform: "linux/amd64", maxCPUs: 4 },
        podman: { containerPlatform: "linux/arm64", maxCPUs: 2 },
      },
    },
  };
  vi.mocked(api.getConfig).mockResolvedValue(config);
  vi.mocked(api.getPreferences).mockResolvedValue(prefs);
  const view = renderApp("/settings");
  const docker = await screen.findByRole("group", { name: "docker" });
  const podman = await screen.findByRole("group", { name: "podman" });
  await waitFor(() => expect(within(docker).getByRole("spinbutton", { name: "CPU cores" })).toHaveValue(4));
  expect(within(podman).getByRole("spinbutton", { name: "CPU cores" })).toHaveValue(2);
  await user.selectOptions(within(docker).getByRole("combobox", { name: "CPU architecture" }), "linux/arm64");
  const cores = within(docker).getByRole("spinbutton", { name: "CPU cores" });
  await user.clear(cores);
  await user.type(cores, "6");
  await user.tab();
  await waitFor(() =>
    expect(api.updatePreferences).toHaveBeenLastCalledWith(
      expect.objectContaining({
        settings: expect.objectContaining({
          runtimeSettings: {
            docker: { containerPlatform: "linux/arm64", maxCPUs: 6 },
            podman: { containerPlatform: "linux/arm64", maxCPUs: 2 },
          },
        }),
      }),
    ),
  );
  expect(within(podman).getByRole("spinbutton", { name: "CPU cores" })).toHaveValue(2);
  await user.selectOptions(within(podman).getByRole("combobox", { name: "CPU architecture" }), "");
  await waitFor(() =>
    expect(api.updatePreferences).toHaveBeenLastCalledWith(
      expect.objectContaining({
        settings: expect.objectContaining({
          runtimeSettings: {
            docker: { containerPlatform: "linux/arm64", maxCPUs: 6 },
            podman: { containerPlatform: "", maxCPUs: 2 },
          },
        }),
      }),
    ),
  );
  const savedCall = vi.mocked(api.updatePreferences).mock.calls.at(-1);
  if (!savedCall) throw new Error("Settings were not saved");
  const saved = savedCall[0];
  vi.mocked(api.getPreferences).mockResolvedValue({ ...prefs, settings: saved.settings });
  view.unmount();
  renderApp("/settings");
  const restored = await screen.findByRole("group", { name: "docker" });
  await waitFor(() => expect(within(restored).getByRole("spinbutton", { name: "CPU cores" })).toHaveValue(6));
  expect(within(restored).getByRole("combobox", { name: "CPU architecture" })).toHaveValue("linux/arm64");
});

function enableImageDrafts() {
  vi.mocked(api.listHarnesses).mockResolvedValue([
    { name: "claude", models: [], supportsImages: true, supportsCompact: false, supportsModelRefresh: false },
  ]);
  vi.mocked(api.getConfig).mockResolvedValue({
    imageConstraints: {
      allowedMediaTypes: ["image/png", "image/jpeg"],
      maxImageBytes: 10485760,
      maxPromptImageBytes: 20971520,
    },
    displayName: "test",
    tailscaleAvailable: false,
    usbAvailable: false,
    displayAvailable: false,
    sudoAvailable: false,
    gitHubTokenAvailable: false,
    mcpOAuthAvailable: false,
    voiceGateway: { mode: "disabled" },
  });
  const create = vi.spyOn(URL, "createObjectURL").mockImplementation(() => `blob:${crypto.randomUUID()}`);
  const revoke = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  return { create, revoke };
}

function chooseDraftImage(form: HTMLElement, file: File) {
  const input = form.querySelector("input[type=file]");
  if (!input) throw new Error("Image chooser is missing");
  fireEvent.change(input, { target: { files: [file] } });
}

function editDraft(el: HTMLElement, text: string) {
  el.textContent = text;
  fireEvent.input(el);
}

describe("account-owned image drafts", () => {
  it("preserves edits and added images during conversion, sends the snapshot, and releases only submitted URLs", async () => {
    const { create, revoke } = enableImageDrafts();
    const read = deferred<ArrayBuffer>();
    const file = new File(["first"], "first.png", { type: "image/png" });
    const slice = vi.spyOn(file, "slice").mockReturnValue({ arrayBuffer: () => read.promise } as Blob);
    const request = deferred<Task>();
    vi.mocked(api.createTask).mockReturnValue(request.promise);
    const view = renderApp();
    const prompt = await screen.findByTestId("prompt-input");
    const form = prompt.closest("form");
    if (!form) throw new Error("Prompt form missing");
    await waitFor(() => expect(within(form).getByTestId("attach-images")).toBeEnabled());
    editDraft(prompt, "first prompt");
    chooseDraftImage(form, file);
    fireEvent.click(within(form).getByTestId("submit-task"));
    expect(slice).toHaveBeenCalledOnce();
    expect(api.createTask).not.toHaveBeenCalled();
    editDraft(prompt, "new prompt");
    chooseDraftImage(form, new File(["second"], "second.png", { type: "image/png" }));
    read.resolve(new TextEncoder().encode("first").buffer);
    await waitFor(() => expect(api.createTask).toHaveBeenCalledOnce());
    expect(vi.mocked(api.createTask).mock.calls[0][0].initialPrompt).toEqual({
      text: "first prompt",
      images: [{ mediaType: "image/png", data: "Zmlyc3Q=" }],
    });
    request.resolve(makeTask());
    await waitFor(() => expect(revoke).toHaveBeenCalledOnce());
    expect(prompt).toHaveTextContent("new prompt");
    expect(within(form).getAllByRole("img", { name: "attached" })).toHaveLength(1);
    expect(revoke).toHaveBeenCalledWith(create.mock.results[0].value);
    view.unmount();
    expect(revoke).toHaveBeenCalledTimes(2);
  });

  it("retains images and text after a failed send, retries the same wire bytes, and releases on success", async () => {
    const { revoke } = enableImageDrafts();
    vi.mocked(api.createTask).mockRejectedValueOnce(new Error("Offline")).mockResolvedValueOnce(makeTask());
    const view = renderApp();
    const prompt = await screen.findByTestId("prompt-input");
    const form = prompt.closest("form");
    if (!form) throw new Error("Prompt form missing");
    await waitFor(() => expect(within(form).getByTestId("attach-images")).toBeEnabled());
    editDraft(prompt, "retry prompt");
    chooseDraftImage(form, new File(["image"], "image.png", { type: "image/png" }));
    fireEvent.click(within(form).getByTestId("submit-task"));
    await screen.findByText("Task creation failed: Offline");
    expect(revoke).not.toHaveBeenCalled();
    expect(prompt).toHaveTextContent("retry prompt");
    expect(within(form).getByRole("img", { name: "attached" })).toBeInTheDocument();
    fireEvent.click(within(form).getByTestId("submit-task"));
    await waitFor(() => expect(revoke).toHaveBeenCalledOnce());
    expect(vi.mocked(api.createTask).mock.calls[1][0].initialPrompt).toEqual(
      vi.mocked(api.createTask).mock.calls[0][0].initialPrompt,
    );
    expect(prompt).toHaveTextContent("");
    view.unmount();
    expect(revoke).toHaveBeenCalledOnce();
  });

  it("cancels pending conversion on account-provider disposal before calling the API", async () => {
    const { revoke } = enableImageDrafts();
    const read = deferred<ArrayBuffer>();
    const file = new File(["image"], "image.png", { type: "image/png" });
    const slice = vi.spyOn(file, "slice").mockReturnValue({ arrayBuffer: () => read.promise } as Blob);
    const view = renderApp();
    const prompt = await screen.findByTestId("prompt-input");
    const form = prompt.closest("form");
    if (!form) throw new Error("Prompt form missing");
    await waitFor(() => expect(within(form).getByTestId("attach-images")).toBeEnabled());
    chooseDraftImage(form, file);
    fireEvent.click(within(form).getByTestId("submit-task"));
    expect(slice).toHaveBeenCalledOnce();
    view.unmount();
    expect(revoke).toHaveBeenCalledOnce();
    read.resolve(new ArrayBuffer(5));
    await read.promise;
    // The converter's continuation is registered before this continuation.
    expect(api.createTask).not.toHaveBeenCalled();
  });
});

it("preserves the task's newer text and attachments after sending its conversion snapshot", async () => {
  const { create, revoke } = enableImageDrafts();
  vi.mocked(api.getTask).mockResolvedValue(makeTask({ state: "waiting" }));
  const sent = deferred<Awaited<ReturnType<typeof api.sendInput>>>();
  vi.mocked(api.sendInput).mockReturnValue(sent.promise);
  const read = deferred<ArrayBuffer>();
  const file = new File(["first"], "first.png", { type: "image/png" });
  const slice = vi.spyOn(file, "slice").mockReturnValue({ arrayBuffer: () => read.promise } as Blob);
  const view = renderApp("/task/@task1+task");
  await waitForTaskEventsSubscription();
  dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ state: "waiting" })] });
  const form = await screen.findByTestId("task-detail-form");
  await waitFor(() => expect(within(form).getByTestId("attach-images")).toBeEnabled());
  const prompt = within(form).getByRole("textbox");
  editDraft(prompt, "original task prompt");
  chooseDraftImage(form, file);
  fireEvent.click(within(form).getByTestId("send-input"));
  expect(slice).toHaveBeenCalledOnce();
  editDraft(prompt, "next task prompt");
  chooseDraftImage(form, new File(["second"], "second.png", { type: "image/png" }));
  read.resolve(new TextEncoder().encode("first").buffer);
  await waitFor(() =>
    expect(api.sendInput).toHaveBeenCalledWith("task1", {
      prompt: { text: "original task prompt", images: [{ mediaType: "image/png", data: "Zmlyc3Q=" }] },
    }),
  );
  sent.resolve({ status: "ok" });
  await waitFor(() => expect(revoke).toHaveBeenCalledWith(create.mock.results[0].value));
  expect(prompt).toHaveTextContent("next task prompt");
  expect(within(form).getAllByRole("img", { name: "attached" })).toHaveLength(1);
  await waitForTaskEventsSubscription();
  dispatchSSE({ kind: "delete", delete: "task1" });
  expect(revoke).toHaveBeenCalledTimes(2);
  view.unmount();
  expect(revoke).toHaveBeenCalledTimes(2);
});

it("keeps a screenshot started during conversion when the preceding submission succeeds", async () => {
  const { revoke } = enableImageDrafts();
  const devices = Object.getOwnPropertyDescriptor(navigator, "mediaDevices");
  Object.defineProperty(navigator, "mediaDevices", {
    configurable: true,
    value: { getDisplayMedia: async () => ({ getTracks: () => [{ stop: () => {} }] }) },
  });
  vi.spyOn(window.HTMLMediaElement.prototype, "play").mockResolvedValue();
  vi.spyOn(window.HTMLVideoElement.prototype, "videoWidth", "get").mockReturnValue(100);
  vi.spyOn(window.HTMLVideoElement.prototype, "videoHeight", "get").mockReturnValue(100);
  vi.spyOn(window.HTMLCanvasElement.prototype, "getContext").mockReturnValue({
    drawImage: () => {},
  } as unknown as CanvasRenderingContext2D);
  vi.spyOn(globalThis, "requestAnimationFrame").mockImplementation((callback) => {
    callback(0);
    return 0;
  });
  let finishCapture!: BlobCallback;
  const encode = vi.spyOn(window.HTMLCanvasElement.prototype, "toBlob").mockImplementation((callback) => {
    finishCapture = callback;
  });
  const read = deferred<ArrayBuffer>();
  const file = new File(["first"], "first.png", { type: "image/png" });
  vi.spyOn(file, "slice").mockReturnValue({ arrayBuffer: () => read.promise } as Blob);
  const view = renderApp();
  try {
    const prompt = await screen.findByTestId("prompt-input");
    const form = prompt.closest("form");
    if (!form) throw new Error("Prompt form missing");
    await waitFor(() => expect(within(form).getByTestId("attach-images")).toBeEnabled());
    chooseDraftImage(form, file);
    fireEvent.click(within(form).getByTestId("submit-task"));
    fireEvent.click(within(form).getByTestId("attach-images"));
    fireEvent.click(screen.getByTestId("screenshot-menu-item"));
    await waitFor(() => expect(encode).toHaveBeenCalledOnce());
    read.resolve(new TextEncoder().encode("first").buffer);
    await waitFor(() => expect(revoke).toHaveBeenCalledOnce());
    finishCapture(new Blob(["next capture"], { type: "image/jpeg" }));
    await waitFor(() => expect(within(form).getAllByRole("img", { name: "attached" })).toHaveLength(1));
  } finally {
    view.unmount();
    if (devices) Object.defineProperty(navigator, "mediaDevices", devices);
    else Reflect.deleteProperty(navigator, "mediaDevices");
  }
});

it("releases hidden task drafts omitted by a complete reconnect snapshot", async () => {
  const { create, revoke } = enableImageDrafts();
  const first = makeTask({ id: "task1", state: "waiting" });
  const second = makeTask({ id: "task2", state: "waiting" });
  vi.mocked(api.getTask).mockImplementation(async (id) => (id === first.id ? first : second));
  const view = renderApp("/task/@task1+task");
  await waitForTaskEventsSubscription();
  dispatchSSE({ kind: "snapshot", snapshot: [first, second] });
  const form = await screen.findByTestId("task-detail-form");
  await waitFor(() => expect(within(form).getByTestId("attach-images")).toBeEnabled());
  editDraft(within(form).getByRole("textbox"), "removed task draft");
  chooseDraftImage(form, new File(["private image"], "private.png", { type: "image/png" }));
  view.history.set({ value: "/task/@task2+task" });
  await waitFor(() =>
    expect(within(screen.getByTestId("task-detail-form")).getByRole("textbox")).toHaveTextContent(""),
  );
  dispatchSSE({ kind: "snapshot", snapshot: [second] });
  await waitFor(() => expect(revoke).toHaveBeenCalledWith(create.mock.results[0].value));
  dispatchSSE({ kind: "upsert", upsert: first });
  view.history.set({ value: "/task/@task1+task" });
  const restored = await screen.findByTestId("task-detail-form");
  expect(within(restored).getByRole("textbox")).toHaveTextContent("");
  expect(within(restored).queryByRole("img", { name: "attached" })).not.toBeInTheDocument();
  view.unmount();
  expect(revoke).toHaveBeenCalledOnce();
});

it("reports a dispatched message failure after navigation and retains the original task draft for retry", async () => {
  const { revoke } = enableImageDrafts();
  vi.mocked(api.getTask).mockResolvedValue(makeTask({ state: "waiting" }));
  const sent = deferred<Awaited<ReturnType<typeof api.sendInput>>>();
  vi.mocked(api.sendInput).mockReturnValueOnce(sent.promise).mockResolvedValueOnce({ status: "ok" });
  const view = renderApp("/task/@task1+task");
  await waitForTaskEventsSubscription();
  dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ state: "waiting" })] });
  const form = await screen.findByTestId("task-detail-form");
  await waitFor(() => expect(within(form).getByTestId("attach-images")).toBeEnabled());
  editDraft(within(form).getByRole("textbox"), "retry original task");
  chooseDraftImage(form, new File(["image"], "image.png", { type: "image/png" }));
  fireEvent.click(within(form).getByTestId("send-input"));
  await waitFor(() => expect(api.sendInput).toHaveBeenCalledOnce());
  view.history.set({ value: "/" });
  await waitFor(() => expect(screen.queryByTestId("task-detail-form")).not.toBeInTheDocument());
  sent.reject(new Error("Connection interrupted"));
  await screen.findByText("Message send failed: Connection interrupted");
  expect(revoke).not.toHaveBeenCalled();
  view.history.set({ value: "/task/@task1+task" });
  const retry = await screen.findByTestId("task-detail-form");
  expect(within(retry).getByRole("textbox")).toHaveTextContent("retry original task");
  expect(within(retry).getByRole("img", { name: "attached" })).toBeInTheDocument();
  fireEvent.click(within(retry).getByTestId("send-input"));
  await waitFor(() => expect(revoke).toHaveBeenCalledOnce());
  expect(vi.mocked(api.sendInput).mock.calls[1]).toEqual(vi.mocked(api.sendInput).mock.calls[0]);
  view.unmount();
});

it("suppresses an old account's dispatched message failure after provider disposal", async () => {
  const { revoke } = enableImageDrafts();
  vi.mocked(api.getTask).mockResolvedValue(makeTask({ state: "waiting" }));
  const sent = deferred<Awaited<ReturnType<typeof api.sendInput>>>();
  vi.mocked(api.sendInput).mockReturnValue(sent.promise);
  const first = renderApp("/task/@task1+task");
  await waitForTaskEventsSubscription();
  dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ state: "waiting" })] });
  const form = await screen.findByTestId("task-detail-form");
  await waitFor(() => expect(within(form).getByTestId("attach-images")).toBeEnabled());
  chooseDraftImage(form, new File(["private"], "private.png", { type: "image/png" }));
  fireEvent.click(within(form).getByTestId("send-input"));
  await waitFor(() => expect(api.sendInput).toHaveBeenCalledOnce());
  first.unmount();
  expect(revoke).toHaveBeenCalledOnce();
  const next = renderApp();
  await screen.findByTestId("prompt-input");
  sent.reject(new Error("Private old-account failure"));
  await expect(sent.promise).rejects.toThrow("Private old-account failure");
  expect(screen.queryByText(/Private old-account failure/)).not.toBeInTheDocument();
  next.unmount();
});

it("waits for the initial task snapshot before loading a selected detail without adding list membership", async () => {
  enableImageDrafts();
  initialTaskSnapshot = null;
  vi.mocked(api.getTask).mockResolvedValue(makeTask({ state: "waiting", title: "REST detail only" }));
  const view = renderApp("/task/@task1+task");
  try {
    await waitForTaskEventsSubscription();
    expect(api.getTask).not.toHaveBeenCalled();
    dispatchSSE({ kind: "snapshot", snapshot: [], complete: true });
    await waitFor(() => expect(api.getTask).toHaveBeenCalledOnce());
    await screen.findByText("REST detail only");
    expect(document.querySelector("[data-task-id='task1']")).not.toBeInTheDocument();
    expect(within(screen.getByTestId("task-detail-form")).queryByTestId("attach-images")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

it("keeps a newly created task out of list membership and image admission until SSE introduces it", async () => {
  enableImageDrafts();
  initialTaskSnapshot = null;
  const task = makeTask({ id: "fresh-task", state: "waiting", title: "Fresh task" });
  vi.mocked(api.createTask).mockResolvedValue(task);
  vi.mocked(api.getTask).mockResolvedValue(task);
  const view = renderApp();
  try {
    await waitForTaskEventsSubscription();
    editDraft(await screen.findByTestId("prompt-input"), "create before snapshot arrives");
    fireEvent.click(screen.getByTestId("submit-task"));
    await screen.findByTestId("task-detail-header");
    expect(document.querySelector("[data-task-id='fresh-task']")).not.toBeInTheDocument();
    expect(screen.queryByTestId("task-detail-form")).not.toBeInTheDocument();
    dispatchSSE({ kind: "snapshot", snapshot: [], complete: true });
    await waitFor(() => expect(api.getTask).toHaveBeenCalledOnce());
    await screen.findByText("Fresh task");
    expect(within(screen.getByTestId("task-detail-form")).queryByTestId("attach-images")).not.toBeInTheDocument();
    dispatchSSE({ kind: "upsert", upsert: task });
    await waitFor(() =>
      expect(within(screen.getByTestId("task-detail-form")).getByTestId("attach-images")).toBeEnabled(),
    );
    expect(document.querySelector("[data-task-id='fresh-task']")).toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

it("resolves a late creation response for an unseen deleted task through selected-detail 404 without reviving the list", async () => {
  const created = deferred<Task>();
  vi.mocked(api.createTask).mockReturnValue(created.promise);
  vi.mocked(api.getTask).mockRejectedValueOnce(apiError(404));
  const view = renderApp();
  try {
    await waitForTaskEventsSubscription();
    editDraft(await screen.findByTestId("prompt-input"), "created and removed before SSE sees it");
    fireEvent.click(screen.getByTestId("submit-task"));
    await waitFor(() => expect(api.createTask).toHaveBeenCalledOnce());
    dispatchSSE({ kind: "snapshot", snapshot: [], complete: true });
    created.resolve(makeTask({ id: "unseen-deleted-task" }));
    await waitFor(() => expect(api.getTask).toHaveBeenCalledWith("unseen-deleted-task"));
    await waitFor(() => expect(view.history.get()).toBe("/"));
    expect(document.querySelector("[data-task-id='unseen-deleted-task']")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

it("preserves offline drafts through partial restoration, ignores the old connection, and prunes only a complete snapshot", async () => {
  const { revoke } = enableImageDrafts();
  const task = makeTask({ state: "waiting" });
  const view = renderApp("/task/@task1+task");
  try {
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [task] });
    const form = await screen.findByTestId("task-detail-form");
    await waitFor(() => expect(within(form).getByTestId("attach-images")).toBeEnabled());
    editDraft(within(form).getByRole("textbox"), "keep this offline draft");
    chooseDraftImage(form, new File(["private"], "private.png", { type: "image/png" }));
    const oldMessages = [...fakeESListeners];
    initialTaskSnapshot = null;
    fireEvent(window, new Event("offline"));
    fireEvent(window, new Event("online"));
    await waitFor(() => expect(taskEventSubscriptions).toBe(2));
    fakeESListeners.forEach((listener) =>
      listener({ data: JSON.stringify({ kind: "snapshot", snapshot: [], complete: null }) }),
    );
    expect(revoke).not.toHaveBeenCalled();
    expect(within(form).getByRole("img", { name: "attached" })).toBeInTheDocument();
    dispatchSSE({ kind: "snapshot", snapshot: [], complete: false });
    dispatchSSE({ kind: "status", status: { loading: false, error: "Runtime restoration incomplete" } });
    oldMessages.forEach((listener) => listener({ data: JSON.stringify({ kind: "delete", delete: "task1" }) }));
    expect(revoke).not.toHaveBeenCalled();
    expect(within(form).getByRole("textbox")).toHaveTextContent("keep this offline draft");
    expect(within(form).getByRole("img", { name: "attached" })).toBeInTheDocument();
    expect(within(form).getByTestId("attach-images")).toBeEnabled();
    dispatchSSE({ kind: "snapshot", snapshot: [], complete: true });
    await waitFor(() => expect(revoke).toHaveBeenCalledOnce());
    await waitFor(() => expect(view.history.get()).toBe("/"));
  } finally {
    view.unmount();
  }
});

it("rejects retired-connection REST recovery results after the replacement snapshot", async () => {
  initialTaskSnapshot = null;
  const retired = deferred<Task>();
  const current = deferred<Task>();
  vi.mocked(api.getTask).mockReturnValueOnce(retired.promise).mockReturnValueOnce(current.promise);
  const view = renderApp("/task/@task1+task");
  try {
    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [], complete: true });
    await waitFor(() => expect(api.getTask).toHaveBeenCalledOnce());
    fireEvent(window, new Event("offline"));
    fireEvent(window, new Event("online"));
    await waitFor(() => expect(taskEventSubscriptions).toBe(2));
    expect(api.getTask).toHaveBeenCalledOnce();
    dispatchSSE({ kind: "snapshot", snapshot: [], complete: true });
    await waitFor(() => expect(api.getTask).toHaveBeenCalledTimes(2));
    current.resolve(makeTask({ state: "waiting", title: "Current detail" }));
    await screen.findByText("Current detail");
    retired.resolve(makeTask({ state: "waiting", title: "Retired detail" }));
    await retired.promise;
    expect(screen.queryByText("Retired detail")).not.toBeInTheDocument();
    expect(document.querySelector("[data-task-id='task1']")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

for (const completion of ["success", "404"] as const) {
  it(`ignores a disposed account's selected-detail ${completion} completion`, async () => {
    const retired = deferred<Task>();
    vi.mocked(api.getTask)
      .mockReturnValueOnce(retired.promise)
      .mockResolvedValueOnce(makeTask({ state: "waiting", title: "Current account detail" }));
    const first = renderApp("/task/@task1+task");
    await waitFor(() => expect(api.getTask).toHaveBeenCalledOnce());
    first.unmount();
    const current = renderApp("/task/@task1+task");
    try {
      await screen.findByText("Current account detail");
      if (completion === "success") {
        retired.resolve(makeTask({ state: "waiting", title: "Old account detail" }));
        await retired.promise;
      } else {
        retired.reject(apiError(404));
        await expect(retired.promise).rejects.toThrow("HTTP 404");
      }
      expect(current.history.get()).toBe("/task/@task1+task");
      expect(screen.queryByText("Old account detail")).not.toBeInTheDocument();
      expect(document.querySelector("[data-task-id='task1']")).not.toBeInTheDocument();
    } finally {
      current.unmount();
    }
  });
}

it("rejects task image adoption at the owner boundary before SSE introduction and after deletion", async () => {
  const { create, revoke } = enableImageDrafts();
  const observed: { store: AppStore | null } = { store: null };
  const view = render(() => (
    <AuthProvider>
      <MemoryRouter
        root={(props) => (
          <HostModeProvider>
            <AppStateProvider>{props.children}</AppStateProvider>
          </HostModeProvider>
        )}
      >
        <Route
          path="/"
          component={() => {
            observed.store = useAppState();
            return <span>Draft owner ready</span>;
          }}
        />
      </MemoryRouter>
    </AuthProvider>
  ));
  try {
    await waitForTaskEventsSubscription();
    await waitFor(() => expect(observed.store?.imageConstraints()).not.toBeNull());
    const store = observed.store;
    if (!store) throw new Error("Draft owner not mounted");
    const file = new File(["private"], "private.png", { type: "image/png" });
    expect(() => store.addInputImages("task1", [file])).toThrow("Waiting for task updates");
    expect(create).not.toHaveBeenCalled();
    dispatchSSE({ kind: "upsert", upsert: makeTask({ state: "waiting" }) });
    store.addInputImages("task1", [file]);
    expect(create).toHaveBeenCalledOnce();
    dispatchSSE({ kind: "delete", delete: "task1" });
    expect(revoke).toHaveBeenCalledOnce();
    expect(() => store.addInputImages("task1", [file])).toThrow("Waiting for task updates");
    expect(create).toHaveBeenCalledOnce();
  } finally {
    view.unmount();
  }
});
