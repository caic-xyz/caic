// Tests caic CI context and UI task numbering around shared browser voice.

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

  it("reports CI failures without replaying the connection baseline", () => {
    const [tasks, setTasks] = createSignal<[Task, ...Task[]]>([task("a-task", "Existing work")]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} session={voiceSession} />);
    voiceSession.setState((s) => ({ ...s, connected: true }));
    expect(injectTextMock).not.toHaveBeenCalled();

    setTasks((current) => [{ ...current[0], state: "waiting", ciStatus: "failure" }]);
    expect(injectTextMock).not.toHaveBeenCalledWith("[Existing work — CI: failure; taskID: a-task]");
    setTasks((current) => [{ ...current[0], ciStatus: "success" }]);
    setTasks((current) => [{ ...current[0], ciStatus: "failure" }]);
    expect(injectTextMock).toHaveBeenCalledWith("[Existing work — CI: failure; taskID: a-task]");

    voiceSession.setState((s) => ({ ...s, connected: false }));
    setTasks((current) => [{ ...current[0], state: "running" }]);
    voiceSession.setState((s) => ({ ...s, connected: true }));
    expect(injectTextMock).toHaveBeenCalledTimes(1);
    view.unmount();
  });

  it("uses current task numbers for CI failures and omits a lone task number", () => {
    const [tasks, setTasks] = createSignal<[Task, ...Task[]]>([
      { ...task("a-task", "First work"), ciStatus: "success" },
      { ...task("b-task", "Second work"), ciStatus: "success" },
    ]);
    const view = render(() => <VoiceTaskUpdates tasks={tasks} session={voiceSession} />);
    voiceSession.setState((s) => ({ ...s, connected: true }));
    expect(injectTextMock).not.toHaveBeenCalled();

    setTasks((current) => {
      const second = current[1];
      assert.ok(second !== undefined);
      return [
        { ...current[0], state: "stopped" },
        { ...second, ciStatus: "failure" },
      ];
    });
    expect(getVoiceTaskNumber("b-task")).toBe(1);
    expect(injectTextMock).toHaveBeenCalledWith("[Task #1 (Second work) — CI: failure; taskID: b-task]");

    setTasks((current) => [current[0]]);
    setTasks((current) => [{ ...current[0], ciStatus: "failure" }]);
    expect(injectTextMock).toHaveBeenLastCalledWith("[First work — CI: failure; taskID: a-task]");
    view.unmount();
  });
});
