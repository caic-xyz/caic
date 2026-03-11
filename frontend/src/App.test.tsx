// Tests for repo chip selection after clone and task creation.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import type { Repo, PreferencesResp, HarnessInfo } from "@sdk/types.gen";

const navigateMock = vi.fn();

vi.mock("@solidjs/router", () => ({
  useNavigate: () => navigateMock,
  useLocation: () => ({ pathname: "/" }),
}));

vi.mock("./api", () => ({
  listRepos: vi.fn(),
  getPreferences: vi.fn(),
  listHarnesses: vi.fn(),
  getConfig: vi.fn(),
  getUsage: vi.fn(),
  listRepoBranches: vi.fn(),
  cloneRepo: vi.fn(),
  createTask: vi.fn(),
  terminateTask: vi.fn(),
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

// Stub EventSource to prevent real SSE connections.
// FakeEventSource captures message listeners so tests can push SSE events.
type MessageListener = (e: { data: string }) => void;
const fakeESListeners: MessageListener[] = [];

class FakeEventSource {
  addEventListener = vi.fn((type: string, handler: MessageListener) => {
    if (type === "message") fakeESListeners.push(handler);
  });
  close = vi.fn();
  onerror: ((e: Event) => void) | null = null;
}
vi.stubGlobal("EventSource", FakeEventSource);

function dispatchSSE(data: unknown) {
  const payload = { data: JSON.stringify(data) };
  fakeESListeners.forEach((fn) => fn(payload));
}

// Imports must follow vi.mock declarations.
import App from "./App";
import * as api from "./api";

const repoA: Repo = { path: "repos/a", baseBranch: "main", remoteURL: "" };
const repoB: Repo = { path: "repos/b", baseBranch: "main", remoteURL: "" };
const newRepo: Repo = { path: "repos/new", baseBranch: "main", remoteURL: "" };

function chipPathValues(): string[] {
  const btns = screen.queryAllByTestId("repo-chips")[0]
    ?.querySelectorAll<HTMLButtonElement>("button[data-testid^='chip-label-']");
  return Array.from(btns ?? []).map((b) => b.dataset["testid"]?.replace("chip-label-", "") ?? "");
}

beforeEach(() => {
  vi.clearAllMocks();
  navigateMock.mockClear();
  fakeESListeners.length = 0;
  vi.mocked(api.listRepos).mockResolvedValue([repoA, repoB]);
  vi.mocked(api.getPreferences).mockResolvedValue({
    repositories: [{ path: "repos/a" }],
    models: {},
    harness: "",
    baseImage: "",
  } as PreferencesResp);
  vi.mocked(api.listHarnesses).mockResolvedValue([
    { name: "claude", models: [], supportsImages: false },
  ] as HarnessInfo[]);
  vi.mocked(api.getConfig).mockRejectedValue(new Error("no config"));
  vi.mocked(api.getUsage).mockRejectedValue(new Error("no usage"));
  vi.mocked(api.listRepoBranches).mockResolvedValue({ branches: ["main", "dev"] });
  vi.mocked(api.cloneRepo).mockResolvedValue(newRepo);
  vi.mocked(api.createTask).mockResolvedValue({ id: "task1" });
});

describe("App repo chips: No repository", () => {
  it("has no chips after removing the last one", async () => {
    const user = userEvent.setup();
    render(() => <App />);

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
    render(() => <App />);

    await waitFor(() => {
      expect(screen.getByTestId("chip-label-repos/a")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Remove repos/a" }));
    expect(chipPathValues()).toHaveLength(0);

    // Simulate a "repos" SSE event (e.g. CI status update) which triggers setRepos.
    const repoAUpdated: Repo = { path: "repos/a", baseBranch: "main", remoteURL: "", defaultBranchCIStatus: "success" as const };
    dispatchSSE({ kind: "repos", repos: [repoAUpdated] });

    await waitFor(() => {
      // No chip should have been added back.
      expect(chipPathValues()).toHaveLength(0);
    });
  });

  it("creates task without repos when no chips are selected", async () => {
    const user = userEvent.setup();
    render(() => <App />);

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
      baseImage: "",
    } as PreferencesResp);
    render(() => <App />);

    await waitFor(() => {
      expect(screen.getByTestId("chip-label-repos/b")).toBeInTheDocument();
      expect(screen.queryByTestId("chip-label-repos/a")).not.toBeInTheDocument();
    });
  });

  it("cloned repo appears in add-dropdown (not Recent) before first task", async () => {
    const user = userEvent.setup();
    render(() => <App />);

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
    const groupLabels = Array.from(dropdown.querySelectorAll("[class*='dropdownGroupLabel']")).map(
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
    render(() => <App />);

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
    const groupLabels = Array.from(dropdown.querySelectorAll("[class*='dropdownGroupLabel']")).map(
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
