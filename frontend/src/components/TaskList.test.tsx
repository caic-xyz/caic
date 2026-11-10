// Tests task-list repository ordering, compact purged disclosures, selection visibility, and navigation.

import { afterEach, beforeEach, describe, it } from "node:test";
import { fireEvent, render, waitFor } from "@solidjs/testing-library";
import { createSignal, type JSX } from "solid-js";
import { MemoryRouter, Route } from "@solidjs/router";
import { expect, vi } from "@tests/expect";

import type { Task } from "@sdk/types.gen";

import TaskList, { type TaskListProps } from "./TaskList";
import styles from "./TaskList.module.css";

function task(id: string): Task {
  return {
    id,
    pendingInput: null,
    initialPrompt: "Do work",
    title: "Do work",
    state: "running",
    stateUpdatedAt: "2026-09-03T00:00:00Z" as Task["stateUpdatedAt"],
    costUSD: 0,
    costKnown: false,
    costComplete: false,
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
    autoCompactTokenLimit: 0,
    harness: "claude",
    runtime: { id: "runtime" },
  };
}

function taskListProps(tasks: Task[]): Omit<TaskListProps, "selectedId"> {
  return {
    tasks: () => tasks,
    tasksLoading: () => false,
    settledLoading: () => false,
    repos: () => [],
    usage: () => null,
    sidebarOpen: () => true,
    setSidebarOpen: () => undefined,
    now: () => Date.now(),
    onSelect: () => undefined,
    onStop: () => undefined,
    onPurge: () => undefined,
    onRevive: () => undefined,
    onFork: () => undefined,
    onQuotaRecovery: () => undefined,
    onError: () => undefined,
    supportsCompact: () => false,
    harnessLogoUrl: () => undefined,
    actionId: () => null,
    autoFixCI: () => false,
    autoFixPR: () => false,
    voiceConnected: () => false,
    getTaskNumber: () => undefined,
  };
}

function renderList(component: () => JSX.Element) {
  return render(() => (
    <MemoryRouter>
      <Route path="*" component={component} />
    </MemoryRouter>
  ));
}

describe("TaskList", () => {
  const scrollIntoView = vi.fn();
  const originalScrollIntoView = HTMLElement.prototype.scrollIntoView;
  // TaskList and the real TaskCard each create a ResizeObserver; capture every callback.
  const resizeCallbacks: ResizeObserverCallback[] = [];

  beforeEach(() => {
    vi.stubGlobal(
      "ResizeObserver",
      class ResizeObserverMock {
        constructor(callback: ResizeObserverCallback) {
          resizeCallbacks.push(callback);
        }

        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
      callback(0);
      return 0;
    });
    vi.stubGlobal("cancelAnimationFrame", () => undefined);
    Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
      configurable: true,
      value: scrollIntoView,
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    if (originalScrollIntoView) {
      Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
        configurable: true,
        value: originalScrollIntoView,
      });
    } else {
      Reflect.deleteProperty(HTMLElement.prototype, "scrollIntoView");
    }
    scrollIntoView.mockReset();
  });

  it("puts repositories with active tasks first, with natural ordering within each tier", () => {
    const inactive: Task["state"][] = ["stopped", "stopping", "crashed", "purged", "purging", "failed"];
    const tasks: Task[] = inactive.map((state, index) => ({
      ...task(String(index + 1)),
      state,
      repos: [{ name: `repo${index + 1}`, branch: "work" }],
    }));
    tasks.push(
      { ...task("7"), repos: [{ name: "repo10", branch: "work" }] },
      { ...task("8"), state: "waiting", repos: [{ name: "repo8", branch: "work" }] },
      { ...task("9"), state: "stopped", repos: [{ name: "repo8", branch: "old" }] },
      task("A"),
    );
    const { container } = renderList(() => <TaskList {...taskListProps(tasks)} selectedId={null} />);

    expect(Array.from(container.querySelectorAll(`.${styles.repoGroupHeader}`), (el) => el.textContent)).toEqual([
      "repo8",
      "repo10",
      "repo1",
      "repo2",
      "repo3",
      "repo4 (1)",
      "repo5 (1)",
      "repo6 (1)",
      "Other",
    ]);
  });

  it("uses the repo name to disclose purged-only tasks, including Other", () => {
    const tasks: Task[] = [
      { ...task("1"), state: "purged", repos: [{ name: "archive", branch: "work" }] },
      { ...task("2"), state: "failed", repos: [{ name: "archive", branch: "work" }] },
      { ...task("3"), state: "purged" },
    ];
    const { container, getByRole, queryByRole } = renderList(() => (
      <TaskList {...taskListProps(tasks)} selectedId={null} />
    ));
    const disclosure = getByRole("button", { name: "archive: Purged (2)" });
    expect(disclosure).toHaveTextContent("archive (2)");
    expect(disclosure).toHaveAttribute("aria-expanded", "false");
    expect(queryByRole("button", { name: /^Purged/ })).toBeNull();
    expect(container.querySelector("[data-task-id='1']")).toBeNull();

    fireEvent.click(disclosure);
    expect(disclosure).toHaveAttribute("aria-expanded", "true");
    expect(container.querySelector("[data-task-id='1']")).toBeTruthy();
    expect(container.querySelector("[data-task-id='2']")).toBeTruthy();

    fireEvent.click(disclosure);
    expect(disclosure).toHaveAttribute("aria-expanded", "false");
    expect(container.querySelector("[data-task-id='1']")).toBeNull();
    fireEvent.click(getByRole("button", { name: "Other: Purged (1)" }));
    expect(container.querySelector("[data-task-id='3']")).toBeTruthy();
  });

  it("preserves separate purged rows for running or stopped repos and expands selected purged tasks", () => {
    const tasks: Task[] = [
      { ...task("1"), repos: [{ name: "running", branch: "work" }] },
      { ...task("2"), state: "purged", repos: [{ name: "running", branch: "work" }] },
      { ...task("3"), state: "stopped", repos: [{ name: "stopped", branch: "work" }] },
      { ...task("4"), state: "purged", repos: [{ name: "stopped", branch: "work" }] },
      { ...task("5"), state: "purged", repos: [{ name: "archive", branch: "work" }] },
    ];
    const { container, getAllByRole, getByRole } = renderList(() => (
      <TaskList {...taskListProps(tasks)} selectedId="5" />
    ));
    expect(getAllByRole("button", { name: "Purged (1)" })).toHaveLength(2);
    expect(getByRole("button", { name: "Stopped (1)" })).toBeTruthy();
    expect(getByRole("button", { name: "archive: Purged (1)" })).toHaveAttribute("aria-expanded", "true");
    expect(container.querySelector("[data-task-id='5']")).toBeTruthy();
  });

  it("distinguishes a reported zero from unknown cost across task updates", () => {
    const [tasks, setTasks] = createSignal([{ ...task("1"), costKnown: true, costComplete: true }, task("2")]);
    const { container } = renderList(() => <TaskList {...taskListProps([])} tasks={tasks} selectedId={null} />);
    const first = container.querySelector('[data-task-id="1"]');
    const second = container.querySelector('[data-task-id="2"]');
    expect(first?.querySelector('[data-testid="task-card-cost"]')).toHaveTextContent("$0.00");
    expect(second?.querySelector('[data-testid="task-card-cost"]')).toBeNull();
    setTasks([task("1"), { ...task("2"), costKnown: true, costComplete: true }]);
    expect(first?.querySelector('[data-testid="task-card-cost"]')).toBeNull();
    expect(second?.querySelector('[data-testid="task-card-cost"]')).toHaveTextContent("$0.00");
  });

  it("reorders repositories when their last active task stops or revives without replacing other cards", () => {
    const running = { ...task("1"), repos: [{ name: "repo2", branch: "work" }] };
    const other = { ...task("2"), repos: [{ name: "repo10", branch: "work" }] };
    let updateTasks: (tasks: Task[]) => void = () => undefined;
    const { container } = renderList(() => {
      const [tasks, setTasks] = createSignal([running, other]);
      updateTasks = setTasks;
      return <TaskList {...taskListProps([])} tasks={tasks} selectedId="2" />;
    });
    const repoOrder = () =>
      Array.from(container.querySelectorAll(`.${styles.repoGroupHeader}`), (el) => el.textContent);
    const card = container.querySelector<HTMLElement>("[data-task-id='2']");
    if (!card) throw new Error("task card not rendered");
    card.focus();
    expect(repoOrder()).toEqual(["repo2", "repo10"]);

    updateTasks([{ ...running, state: "stopped" }, other]);

    expect(repoOrder()).toEqual(["repo10", "repo2"]);
    expect(container.querySelector("[data-task-id='2']")).toBe(card);
    expect(card).toHaveFocus();

    updateTasks([running, other]);

    expect(repoOrder()).toEqual(["repo2", "repo10"]);
    expect(container.querySelector("[data-task-id='2']")).toBe(card);
    expect(card).toHaveFocus();
  });

  it("scrolls the summary bar to a newly selected task", async () => {
    let selectTask: (id: string) => void = () => undefined;
    const tasks = [task("1"), task("2")];

    renderList(() => {
      const [selectedId, setSelectedId] = createSignal("1");
      selectTask = setSelectedId;
      return <TaskList {...taskListProps(tasks)} selectedId={selectedId()} />;
    });

    await waitFor(() => expect(scrollIntoView).toHaveBeenCalled());
    scrollIntoView.mockClear();

    selectTask("2");

    await waitFor(() => expect(scrollIntoView).toHaveBeenCalledWith({ block: "nearest", inline: "nearest" }));
    expect(scrollIntoView.mock.contexts.at(-1)).toBe(document.querySelector("[data-task-id='2']"));
  });

  it("keeps the selected task visible when the open sidebar resizes", async () => {
    renderList(() => <TaskList {...taskListProps([task("1"), task("2")])} selectedId="2" />);

    await waitFor(() => expect(scrollIntoView).toHaveBeenCalled());
    scrollIntoView.mockClear();

    resizeCallbacks.forEach((callback) => callback([], {} as ResizeObserver));

    expect(scrollIntoView).toHaveBeenCalledWith({ block: "nearest", inline: "nearest" });
    expect(scrollIntoView.mock.contexts.at(-1)).toBe(document.querySelector("[data-task-id='2']"));
  });

  it("preserves task card DOM identity across live task updates and insertions", () => {
    let updateTasks: (tasks: Task[]) => void = () => undefined;
    renderList(() => {
      const [tasks, setTasks] = createSignal([task("1")]);
      updateTasks = setTasks;
      return <TaskList {...taskListProps([])} tasks={tasks} selectedId="1" />;
    });
    const card = document.querySelector("[data-task-id='1']");
    if (!card) throw new Error("task card not rendered");

    updateTasks([{ ...task("1"), duration: 1 }]);

    expect(document.querySelector("[data-task-id='1']")).toBe(card);

    updateTasks([task("1"), task("2")]);

    expect(document.querySelector("[data-task-id='1']")).toBe(card);
  });

  it("previews purge while Shift is held, including in an editing control", () => {
    const { getByTestId } = renderList(() => (
      <>
        <textarea data-testid="editor" />
        <TaskList {...taskListProps([task("1")])} selectedId="1" />
      </>
    ));
    const card = document.querySelector("[data-task-id='1']");
    if (!card) throw new Error("task card not rendered");

    fireEvent.keyDown(document.body, { key: "Shift" });
    expect(card.querySelector("[data-testid='stop-task-icon']")).toBeNull();
    expect(card.querySelector("[data-testid='purge-task-icon']")).toBeTruthy();
    fireEvent.keyUp(document.body, { key: "Shift" });
    expect(card.querySelector("[data-testid='purge-task-icon']")).toBeNull();

    const editor = getByTestId("editor");
    editor.focus();
    fireEvent.keyDown(editor, { key: "Shift" });
    expect(card.querySelector("[data-testid='purge-task-icon']")).toBeTruthy();
  });
});
