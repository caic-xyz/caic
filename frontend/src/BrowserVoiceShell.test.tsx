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
  it("seeds existing tasks and injects newly created tasks while connected", async () => {
    voiceSession.setState((s) => ({ ...s, connected: true }));
    const [tasks, setTasks] = createSignal([task("a-task", "Existing work")]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} />);
    expect(injectTextMock).not.toHaveBeenCalled();
    expect(getVoiceTaskNumber("a-task")).toBe(1);

    setTasks((current) => [...current, task("b-task", "New work")]);
    await waitFor(() => expect(injectTextMock).toHaveBeenCalledWith("[Task #2 created (New work) — running]"));
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
    expect(injectTextMock).toHaveBeenCalledWith("[Task #1 (Existing work) — waiting]");
    expect(injectTextMock).not.toHaveBeenCalledWith("[Task #1 (Existing work) — CI: failure]");
    setTasks((current) => [{ ...current[0], ciStatus: "success" }]);
    setTasks((current) => [{ ...current[0], ciStatus: "failure" }]);
    expect(injectTextMock).toHaveBeenCalledWith("[Task #1 (Existing work) — CI: failure]");

    voiceSession.setState((s) => ({ ...s, connected: false }));
    setTasks((current) => [{ ...current[0], state: "running" }]);
    voiceSession.setState((s) => ({ ...s, connected: true }));
    expect(injectTextMock).toHaveBeenCalledTimes(2);
    view.unmount();
  });
});
