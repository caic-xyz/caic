// Tests for app-shell task creation, repo selection, and harness preferences.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import type { Repo, PreferencesResp, HarnessInfo, Task, ISOTimestamp } from "@sdk/types.gen";

// Minimal complete Task, matching what the backend now returns from createTask
// so the app can seed its store and render the detail view immediately.
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
    contextWindowLimit: 0,
    harness: "claude",
    runtime: { id: "rt1" },
    ...overrides,
  };
}

// Stub EventSource to prevent real SSE connections.
// FakeEventSource captures message listeners so tests can push SSE events.
type MessageListener = (e: { data: string }) => void;
type OpenListener = () => void;
const fakeESListeners: MessageListener[] = [];
const fakeESOpenListeners: OpenListener[] = [];

class FakeEventSource {
  addEventListener = vi.fn((type: string, handler: MessageListener | OpenListener) => {
    if (type === "message") fakeESListeners.push(handler as MessageListener);
    if (type === "open") fakeESOpenListeners.push(handler as OpenListener);
  });
  close = vi.fn();
  onerror: ((e: Event) => void) | null = null;
}

vi.mock("./api", () => ({
  listRepos: vi.fn(),
  getPreferences: vi.fn(),
  updatePreferences: vi.fn(),
  listHarnesses: vi.fn(),
  listCaches: vi.fn(() => Promise.resolve(null)),
  getCacheSizes: vi.fn(() => Promise.resolve(null)),
  getConfig: vi.fn(),
  getVersion: vi.fn(),
  triggerUpdate: vi.fn(),
  getUsage: vi.fn(),
  listRepoBranches: vi.fn(),
  cloneRepo: vi.fn(),
  createTask: vi.fn(),
  getTask: vi.fn(),
  botFixCI: vi.fn(),
  stopTask: vi.fn(),
  purgeTask: vi.fn(),
  reviveTask: vi.fn(),
  globalTaskEvents: vi.fn((cb: (e: unknown) => void) => {
    const es = new FakeEventSource();
    // Mirror the real client: parse the SSE payload before invoking the handler.
    es.addEventListener("message", (e: { data: string }) => cb(JSON.parse(e.data)));
    return es;
  }),
  globalUsageEvents: vi.fn(() => new FakeEventSource()),
  // Used by TaskDetail once a created task navigates into its detail route.
  taskEventStream: vi.fn(() => new FakeEventSource()),
  sendInput: vi.fn(),
  restartTask: vi.fn(),
  clearContext: vi.fn(() => Promise.resolve({ status: "cleared" })),
  compactContext: vi.fn(() => Promise.resolve({ status: "compacting" })),
  syncTask: vi.fn(),
  getTaskDiff: vi.fn(),
}));

vi.mock("./AuthContext", () => ({
  // eslint-disable-next-line solid/reactivity
  AuthProvider: (props: { children: unknown }) => props.children,
  useAuth: () => ({
    ready: () => true,
    providers: () => [],
    user: () => null,
    logout: async () => {},
  }),
}));

vi.stubGlobal("EventSource", FakeEventSource);

// Stub VoiceOverlay to avoid WebRTC/WebSocket connections in tests.
vi.mock("./gomode/VoiceOverlay", () => ({ default: () => <div data-testid="voice-overlay" /> }));

function dispatchSSE(data: unknown) {
  const payload = { data: JSON.stringify(data) };
  fakeESListeners.forEach((fn) => fn(payload));
}

function dispatchOpen() {
  fakeESOpenListeners.forEach((fn) => fn());
}

async function waitForTaskEventsSubscription() {
  await waitFor(() => expect(fakeESListeners.length).toBeGreaterThan(0));
}

// Imports must follow vi.mock declarations.
import { MemoryRouter, createMemoryHistory } from "@solidjs/router";
import { appRoutes } from "./routes";
import * as api from "./api";

/** Render the full app at an initial route, returning the memory history for assertions. */
function renderApp(initial = "/") {
  const history = createMemoryHistory();
  history.set({ value: initial });
  const utils = render(() => <MemoryRouter history={history}>{appRoutes()}</MemoryRouter>);
  return { history, ...utils };
}

const repoA: Repo = { path: "repos/a", branch: "main", baseBranch: { name: "main" }, remoteURL: "" };
const repoB: Repo = { path: "repos/b", branch: "main", baseBranch: { name: "main" }, remoteURL: "" };
const newRepo: Repo = { path: "repos/new", branch: "main", baseBranch: { name: "main" }, remoteURL: "" };

function chipPathValues(): string[] {
  const btns = screen.queryAllByTestId("repo-chips")[0]
    ?.querySelectorAll<HTMLButtonElement>("button[data-testid^='chip-label-']");
  return Array.from(btns ?? []).map((b) => b.dataset["testid"]?.replace("chip-label-", "") ?? "");
}

beforeEach(() => {
  vi.clearAllMocks();
  fakeESListeners.length = 0;
  fakeESOpenListeners.length = 0;
  window.history.replaceState(null, "", "/");
  delete window.goModeHost;
  vi.mocked(api.listRepos).mockResolvedValue([repoA, repoB]);
  vi.mocked(api.getPreferences).mockResolvedValue({
    repositories: [{ path: "repos/a" }],
    models: {},
    harness: "",
    settings: { baseImage: "" },
  } as unknown as PreferencesResp);
  vi.mocked(api.updatePreferences).mockResolvedValue({
    repositories: [{ path: "repos/a" }],
    models: {},
    harness: "",
    settings: { baseImage: "" },
  } as unknown as PreferencesResp);
  vi.mocked(api.listHarnesses).mockResolvedValue([
    { name: "claude", models: [], supportsImages: false, supportsCompact: false },
  ] as unknown as HarnessInfo[]);
  vi.mocked(api.getConfig).mockRejectedValue(new Error("no config"));
  vi.mocked(api.getVersion).mockResolvedValue({
    current: "0.0.1",
    latest: "0.0.1",
    updateAvailable: false,
    autoUpdateEnabled: false,
  });
  vi.mocked(api.getUsage).mockRejectedValue(new Error("no usage"));
  vi.mocked(api.listRepoBranches).mockResolvedValue({ branches: [{ name: "main" }, { name: "dev", remote: "origin" }] });
  vi.mocked(api.cloneRepo).mockResolvedValue(newRepo);
  vi.mocked(api.createTask).mockResolvedValue(makeTask());
  vi.mocked(api.getTask).mockResolvedValue(makeTask());
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("App repo chips: No repository", () => {
  it("moves from a purged selected task to the next task needing input first", async () => {
    const user = userEvent.setup();
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const killed = makeTask({ id: "a3", title: "kill me", state: "running", repos: [{ name: "repos/a", branch: "main" }] });
    const running = makeTask({ id: "a2", title: "running next", state: "running", repos: [{ name: "repos/a", branch: "main" }] });
    const asking = makeTask({ id: "a1", title: "asking next", state: "asking", repos: [{ name: "repos/a", branch: "main" }] });
    vi.mocked(api.getTask).mockResolvedValue(killed);
    vi.mocked(api.purgeTask).mockResolvedValue(undefined);
    const { history } = renderApp("/task/@a3+kill-me");

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [killed, running, asking] });
    await waitFor(() => expect(document.querySelector("[data-task-id='a3']")).toBeInTheDocument());

    await user.keyboard("{Shift>}{Delete}{/Shift}");

    expect(api.purgeTask).toHaveBeenCalledWith("a3");
    await waitFor(() => expect(history.get()).toContain("/task/@a1+"));
    await waitFor(() => expect(document.querySelector("[data-task-id='a1']")).toHaveFocus());
  });

  it("returns to the new-task prompt after purging the last alive selected task", async () => {
    const user = userEvent.setup();
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const killed = makeTask({ id: "a3", title: "kill me", state: "running", repos: [{ name: "repos/a", branch: "main" }] });
    vi.mocked(api.getTask).mockResolvedValue(killed);
    vi.mocked(api.purgeTask).mockResolvedValue(undefined);
    const { history } = renderApp("/task/@a3+kill-me");

    await waitForTaskEventsSubscription();
    dispatchSSE({ kind: "snapshot", snapshot: [killed] });
    await waitFor(() => expect(document.querySelector("[data-task-id='a3']")).toBeInTheDocument());

    await user.keyboard("{Shift>}{Delete}{/Shift}");

    expect(api.purgeTask).toHaveBeenCalledWith("a3");
    await waitFor(() => expect(history.get()).toBe("/"));
    await waitFor(() => expect(screen.getByTestId("prompt-input")).toHaveFocus());
  });

  it("syncs harness model and effort from per-model preferences", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/a" }],
      harness: "codex",
      models: { claude: "sonnet", codex: "gpt-5" },
      efforts: { claude: { sonnet: "max" }, codex: { "gpt-5": "high", "gpt-5-mini": "minimal" } },
      settings: { baseImage: "" },
    } as unknown as PreferencesResp);
    vi.mocked(api.listHarnesses).mockResolvedValue([
      { name: "claude", models: ["sonnet"], supportsImages: false, supportsCompact: false },
      { name: "codex", models: ["gpt-5", "gpt-5-mini"], supportsImages: false, supportsCompact: false },
    ] as unknown as HarnessInfo[]);

    renderApp();

    const harness = await screen.findByRole("combobox", { name: "Harness" });
    const model = screen.getByRole("button", { name: "Model" });
    const effort = screen.getByRole("combobox", { name: "Effort" });
    expect(harness).toHaveValue("codex");
    expect(model).toHaveTextContent("gpt-5");
    expect(effort).toHaveValue("high");

    await user.click(model);
    await user.click(await screen.findByRole("option", { name: "gpt-5-mini" }));
    expect(effort).toHaveValue("minimal");

    await user.selectOptions(effort, "low");
    await user.click(model);
    await user.click(await screen.findByRole("option", { name: "gpt-5" }));
    expect(effort).toHaveValue("high");

    await user.click(model);
    await user.click(await screen.findByRole("option", { name: "gpt-5-mini" }));
    expect(effort).toHaveValue("low");

    await user.selectOptions(harness, "claude");
    expect(model).toHaveTextContent("sonnet");
    expect(effort).toHaveValue("max");

    await user.selectOptions(harness, "codex");
    expect(model).toHaveTextContent("gpt-5-mini");
    expect(effort).toHaveValue("low");

    await user.selectOptions(harness, "claude");
    expect(model).toHaveTextContent("sonnet");
    expect(effort).toHaveValue("max");
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
      { name: "codex", models: ["gpt-5"], supportsImages: false, supportsCompact: false },
    ] as unknown as HarnessInfo[]);

    const { history } = renderApp("/");
    // Seed a task card so the global ArrowUp/Down handler would otherwise navigate.
    dispatchSSE({ kind: "snapshot", snapshot: [makeTask({ id: "taskX", title: "other task" })] });
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

  it("does not mount browser voice when the server disables the voice gateway", async () => {
    vi.mocked(api.getConfig).mockResolvedValue({
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      voiceGateway: { mode: "disabled" },
    });

    renderApp();

    await waitFor(() => expect(api.getConfig).toHaveBeenCalledOnce());
    expect(screen.queryByTestId("voice-overlay")).not.toBeInTheDocument();
  });

  it("refreshes browser voice availability when task events reconnect", async () => {
    const disabledConfig = {
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
      voiceGateway: { mode: "disabled" as const },
    };
    const enabledConfig = { ...disabledConfig, voiceGateway: { mode: "embedded" as const } };
    vi.mocked(api.getConfig)
      .mockResolvedValueOnce(disabledConfig)
      .mockResolvedValueOnce(enabledConfig);

    renderApp();

    await waitFor(() => expect(api.getConfig).toHaveBeenCalledOnce());
    expect(screen.queryByTestId("voice-overlay")).not.toBeInTheDocument();

    dispatchOpen();

    await waitFor(() => expect(api.getConfig).toHaveBeenCalledTimes(2));
    expect(screen.getByTestId("voice-overlay")).toBeInTheDocument();
  });

  it("keeps browser voice mounted outside Go Mode host mode when the server enables voice", async () => {
    vi.mocked(api.getConfig).mockResolvedValue({
      displayName: "test",
      tailscaleAvailable: false,
      usbAvailable: false,
      displayAvailable: false,
      sudoAvailable: false,
      gitHubTokenAvailable: false,
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

  it("navigates to the settings page from the user menu", async () => {
    const user = userEvent.setup();
    const { history } = renderApp("/");

    await user.click(screen.getByRole("button", { name: "Menu" }));
    await user.click(screen.getByRole("button", { name: "Settings" }));

    expect(history.get()).toBe("/settings");
    expect(screen.getByRole("heading", { name: "Settings" })).toBeInTheDocument();
  });

  it("renders settings as a routed page instead of a dialog", async () => {
    renderApp("/settings");

    expect(screen.getByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => expect(api.getVersion).toHaveBeenCalledOnce());
  });

  it("saves read-only custom mounts", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getPreferences).mockResolvedValue({
      repositories: [{ path: "repos/a" }],
      models: {},
      harness: "",
      settings: {
        baseImage: "",
        customMounts: [{ hostPath: "/host/data", containerPath: "/container/data", enabled: true, readOnly: false }],
      },
    } as unknown as PreferencesResp);

    renderApp("/settings");

    await screen.findByDisplayValue("/host/data");
    await user.click(screen.getByRole("checkbox", { name: "Read only" }));

    await waitFor(() => expect(api.updatePreferences).toHaveBeenCalledWith({
      settings: expect.objectContaining({
        customMounts: [{ hostPath: "/host/data", containerPath: "/container/data", enabled: true, readOnly: true }],
      }),
    }));
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
    const repoAUpdated: Repo = { path: "repos/a", branch: "main", baseBranch: { name: "main" }, remoteURL: "", ci: "success" as const };
    dispatchSSE({ kind: "repos", repos: [repoAUpdated] });

    await waitFor(() => {
      // No chip should have been added back.
      expect(chipPathValues()).toHaveLength(0);
    });
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
    const groupLabels = Array.from(dropdown.children).filter((el) => el.tagName === "DIV").map(
      (el) => el.textContent,
    );
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
    const groupLabels = Array.from(dropdown.children).filter((el) => el.tagName === "DIV").map(
      (el) => el.textContent,
    );
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
