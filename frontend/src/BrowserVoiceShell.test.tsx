// Tests for caic-owned browser voice task updates and numbering.

import { beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { render, waitFor } from "@solidjs/testing-library";
import { createSignal } from "solid-js";
import type { Task } from "@sdk/types.gen";

import { VoiceTaskUpdates } from "./BrowserVoiceShell";
import { voiceSession } from "./gomode/VoiceSession";
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
  it("seeds existing tasks and numbers new tasks without injecting creation feedback", async () => {
    voiceSession.setState((s) => ({ ...s, connected: true }));
    const [tasks, setTasks] = createSignal([task("a-task", "Existing work")]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} />);
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
    const [tasks, setTasks] = createSignal([task("a-task", "Existing work")]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} />);
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
    const [tasks, setTasks] = createSignal([task("a-task", "First work"), task("b-task", "Second work")]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} />);
    voiceSession.setState((s) => ({ ...s, connected: true }));
    expect(injectTextMock).not.toHaveBeenCalled();

    setTasks((current) => [current[0], { ...current[1], state: "waiting" }]);
    expect(injectTextMock).toHaveBeenCalledWith("[Task #2 (Second work) — waiting]");

    setTasks((current) => [current[0]]);
    setTasks((current) => [{ ...current[0], state: "waiting" }]);
    expect(injectTextMock).toHaveBeenLastCalledWith("[First work — waiting]");
    view.unmount();
  });
});
