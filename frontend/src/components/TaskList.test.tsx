// Tests for task-list active-first repository ordering, selection visibility, and navigation.

import { afterEach, beforeEach, describe, it } from "node:test";
import { fireEvent, render, waitFor } from "@solidjs/testing-library";
import { createSignal } from "solid-js";
import { expect, vi } from "@tests/expect";

import type { Task } from "@sdk/types.gen";

import TaskList, { type TaskListProps } from "./TaskList";
import styles from "./TaskList.module.css";

function task(id: string): Task {
  return {
    id,
    initialPrompt: "Do work",
    title: "Do work",
    state: "running",
    stateUpdatedAt: "2026-09-03T00:00:00Z" as Task["stateUpdatedAt"],
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
    const { container } = render(() => <TaskList {...taskListProps(tasks)} selectedId={null} />);

    expect(Array.from(container.querySelectorAll(`.${styles.repoGroupHeader}`), (el) => el.textContent)).toEqual([
      "repo8",
      "repo10",
      "repo1",
      "repo2",
      "repo3",
      "repo4",
      "repo5",
      "repo6",
      "Other",
    ]);
  });

  it("reorders repositories when their last active task stops or revives without replacing other cards", () => {
    const running = { ...task("1"), repos: [{ name: "repo2", branch: "work" }] };
    const other = { ...task("2"), repos: [{ name: "repo10", branch: "work" }] };
    let updateTasks: (tasks: Task[]) => void = () => undefined;
    const { container } = render(() => {
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

    render(() => {
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
    render(() => <TaskList {...taskListProps([task("1"), task("2")])} selectedId="2" />);

    await waitFor(() => expect(scrollIntoView).toHaveBeenCalled());
    scrollIntoView.mockClear();

    resizeCallbacks.forEach((callback) => callback([], {} as ResizeObserver));

    expect(scrollIntoView).toHaveBeenCalledWith({ block: "nearest", inline: "nearest" });
    expect(scrollIntoView.mock.contexts.at(-1)).toBe(document.querySelector("[data-task-id='2']"));
  });

  it("preserves task card DOM identity across live task updates and insertions", () => {
    let updateTasks: (tasks: Task[]) => void = () => undefined;
    render(() => {
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
    const { getByTestId } = render(() => (
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
