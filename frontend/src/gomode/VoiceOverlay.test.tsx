// Tests for VoiceOverlay voice session connection.

import { beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { fireEvent, render, screen, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { createSignal } from "solid-js";

import type { Task } from "@sdk/types.gen";

import VoiceOverlay from "./VoiceOverlay";
import { voiceSession } from "./VoiceSession";

// Spies on the real voice session singleton replace the former module mocks.
const connectMock = vi.spyOn(voiceSession, "connect").mockResolvedValue(undefined as never);
const disconnectMock = vi.spyOn(voiceSession, "disconnect");
const injectTextMock = vi.spyOn(voiceSession, "injectText");
vi.spyOn(voiceSession.taskNumberMap, "toNumber").mockImplementation((id: string) => (id === "new-task" ? 2 : 1));

beforeEach(() => {
  vi.clearAllMocks();
  voiceSession.setState((s) => ({ ...s, connected: false }));
});

function task(id: string, title: string): Task {
  return { id, state: "running", title } as Task;
}

describe("VoiceOverlay connection", () => {
  it("calls connect() on mic button click", async () => {
    const user = userEvent.setup();
    render(() => <VoiceOverlay tasks={() => []} />);

    const micButton = screen.getByRole("button", { name: /voice/i });
    await user.click(micButton);

    expect(connectMock).toHaveBeenCalledOnce();
  });

  it("starts voice mode when F4 is pressed", async () => {
    const user = userEvent.setup();
    render(() => <VoiceOverlay tasks={() => []} />);

    await user.keyboard("{F4}");

    expect(connectMock).toHaveBeenCalledOnce();
  });

  it("starts voice mode with F4 while an editor is focused", async () => {
    const user = userEvent.setup();
    render(() => (
      <>
        <input aria-label="Prompt" />
        <VoiceOverlay tasks={() => []} />
      </>
    ));

    await user.click(screen.getByRole("textbox", { name: "Prompt" }));
    await user.keyboard("{F4}");

    expect(connectMock).toHaveBeenCalledOnce();
  });

  it("stops voice mode when F4 is pressed while connected", async () => {
    voiceSession.setState((s) => ({ ...s, connected: true }));
    render(() => <VoiceOverlay tasks={() => []} />);

    fireEvent.keyDown(document, { key: "F4" });
    await waitFor(() => expect(voiceSession.state.connected).toBe(false));

    expect(disconnectMock).toHaveBeenCalledOnce();
    expect(connectMock).not.toHaveBeenCalled();
  });

  it("injects newly created tasks into an active voice session", async () => {
    voiceSession.setState((s) => ({ ...s, connected: true }));
    const [tasks, setTasks] = createSignal([task("existing-task", "Existing work")]);
    render(() => <VoiceOverlay tasks={tasks} />);

    expect(injectTextMock).not.toHaveBeenCalled();
    setTasks((current) => [...current, task("new-task", "New work")]);

    await waitFor(() => {
      expect(injectTextMock).toHaveBeenCalledWith("[Task #2 created (New work) — running]");
    });
  });
});
