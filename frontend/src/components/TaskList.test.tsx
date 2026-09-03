// Tests for task-list selection visibility and navigation behavior.

import { render, waitFor } from "@solidjs/testing-library";
import { createSignal } from "solid-js";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { Task } from "@sdk/types.gen";

import TaskList, { type TaskListProps } from "./TaskList";

vi.mock("./TaskCard", () => ({
  default: (props: { id: string }) => <div data-task-id={props.id} />,
}));

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
    onError: () => undefined,
    supportsCompact: () => false,
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

  beforeEach(() => {
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
});
