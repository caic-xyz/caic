// Tests UI task numbering and lifecycle around server-owned browser voice context.

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
});
