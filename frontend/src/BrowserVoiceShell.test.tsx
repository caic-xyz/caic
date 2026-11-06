// Tests for caic-owned browser voice task updates and numbering.

import assert from "node:assert/strict";
import { beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { render, waitFor } from "@solidjs/testing-library";
import { createSignal } from "solid-js";
import type { Task } from "@sdk/types.gen";

import { VoiceTaskUpdates } from "./BrowserVoiceShell";
import { voiceSession } from "@maruel/gomode/web/VoiceSession";
import { getVoiceTaskNumber } from "./voiceTaskState";

const injectTextMock = vi.spyOn(voiceSession, "injectText");

beforeEach(() => {
  vi.clearAllMocks();
  voiceSession.setState((s) => ({ ...s, connected: false }));
});

function task(id: string, title: string): Task {
  return { id, state: "running", title } as Task;
}

describe("VoiceTaskUpdates", () => {
  it("reports running tasks renumbered by removal, once and before state updates", () => {
    voiceSession.setState((s) => ({ ...s, connected: true }));
    const [tasks, setTasks] = createSignal<Task[]>([
      task("a-task", "First work"),
      task("b-task", "Second work"),
      task("c-task", "Third work"),
    ]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} session={voiceSession} />);
    expect(injectTextMock).not.toHaveBeenCalled();

    setTasks((current) => current.slice(1).map((t) => (t.id === "c-task" ? { ...t, state: "waiting" } : t)));
    expect(injectTextMock.mock.calls).toEqual([
      ["[Task numbers changed:\n- Second work (b-task): #2 → #1\n- Third work (c-task): #3 → #2]"],
      ["[Task #2 (Third work) — waiting]"],
    ]);
    setTasks((current) => [...current]);
    expect(injectTextMock).toHaveBeenCalledTimes(2);

    setTasks((current) => current.slice(1));
    expect(injectTextMock).toHaveBeenLastCalledWith("[Task numbers changed:\n- Third work (c-task): #2 → #1]");
    view.unmount();
  });

  it("reports active/inactive reordering without replaying changes made while disconnected", () => {
    const [tasks, setTasks] = createSignal<Task[]>([task("a-task", "First work"), task("b-task", "Second work")]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} session={voiceSession} />);
    voiceSession.setState((s) => ({ ...s, connected: true }));
    setTasks((current) => current.map((t) => (t.id === "a-task" ? { ...t, state: "stopped" } : t)));
    expect(injectTextMock.mock.calls).toEqual([
      ["[Task numbers changed:\n- First work (a-task): #1 → #2\n- Second work (b-task): #2 → #1]"],
      ["[Task #2 (First work) — stopped]"],
    ]);

    voiceSession.setState((s) => ({ ...s, connected: false }));
    setTasks((current) => current.map((t) => ({ ...t, state: "running" })));
    voiceSession.setState((s) => ({ ...s, connected: true }));
    expect(injectTextMock).toHaveBeenCalledTimes(2);
    expect(getVoiceTaskNumber("a-task")).toBe(1);
    view.unmount();
  });

  it("seeds existing tasks and numbers new tasks without injecting creation feedback", async () => {
    voiceSession.setState((s) => ({ ...s, connected: true }));
    const [tasks, setTasks] = createSignal<[Task, ...Task[]]>([task("a-task", "Existing work")]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} session={voiceSession} />);
    expect(injectTextMock).not.toHaveBeenCalled();
    expect(getVoiceTaskNumber("a-task")).toBe(1);

    setTasks((current) => [...current, task("b-task", "New work")]);
    await waitFor(() => expect(getVoiceTaskNumber("b-task")).toBe(2));
    expect(injectTextMock).not.toHaveBeenCalled();
    expect(getVoiceTaskNumber("b-task")).toBe(2);
    view.unmount();
    expect(getVoiceTaskNumber("b-task")).toBeUndefined();
  });

  it("reports attention and CI changes without replaying the connection baseline", () => {
    const [tasks, setTasks] = createSignal<[Task, ...Task[]]>([task("a-task", "Existing work")]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} session={voiceSession} />);
    voiceSession.setState((s) => ({ ...s, connected: true }));
    expect(injectTextMock).not.toHaveBeenCalled();

    setTasks((current) => [{ ...current[0], state: "waiting", ciStatus: "failure" }]);
    expect(injectTextMock).toHaveBeenCalledWith("[Existing work — waiting]");
    expect(injectTextMock).not.toHaveBeenCalledWith("[Existing work — CI: failure]");
    setTasks((current) => [{ ...current[0], ciStatus: "success" }]);
    setTasks((current) => [{ ...current[0], ciStatus: "failure" }]);
    expect(injectTextMock).toHaveBeenCalledWith("[Existing work — CI: failure]");

    voiceSession.setState((s) => ({ ...s, connected: false }));
    setTasks((current) => [{ ...current[0], state: "running" }]);
    voiceSession.setState((s) => ({ ...s, connected: true }));
    expect(injectTextMock).toHaveBeenCalledTimes(2);
    view.unmount();
  });

  it("numbers task updates only while multiple tasks are alive", () => {
    const [tasks, setTasks] = createSignal<[Task, ...Task[]]>([
      task("a-task", "First work"),
      task("b-task", "Second work"),
    ]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} session={voiceSession} />);
    voiceSession.setState((s) => ({ ...s, connected: true }));
    expect(injectTextMock).not.toHaveBeenCalled();

    setTasks((current) => {
      const second = current[1];
      assert.ok(second !== undefined);
      return [current[0], { ...second, state: "waiting" }];
    });
    expect(injectTextMock).toHaveBeenCalledWith("[Task #2 (Second work) — waiting]");

    setTasks((current) => [current[0]]);
    setTasks((current) => [{ ...current[0], state: "waiting" }]);
    expect(injectTextMock).toHaveBeenLastCalledWith("[First work — waiting]");
    view.unmount();
  });
});
