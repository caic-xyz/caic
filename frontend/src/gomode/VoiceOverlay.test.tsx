// Tests for VoiceOverlay voice session connection.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

const { connectMock, disconnectMock, voiceState } = vi.hoisted(() => ({
  connectMock: vi.fn(),
  disconnectMock: vi.fn(),
  voiceState: {
    connectStatus: null,
    connected: false,
    listening: false,
    speaking: false,
    muted: false,
    activeTool: null,
    transcript: [],
    micLevel: 0,
    error: null,
    audioInputs: [],
    audioOutputs: [],
    selectedInputId: "",
    selectedOutputId: "",
  },
}));

vi.mock("./VoiceSession", () => ({
  voiceSession: {
    state: voiceState,
    taskNumberMap: { update: vi.fn(), reset: vi.fn() },
    excludedTaskIds: new Set<string>(),
    connect: connectMock,
    disconnect: disconnectMock,
    toggleMute: vi.fn(),
    injectText: vi.fn(),
    clearTranscript: vi.fn(),
    enumerateDevices: vi.fn(),
    selectInputDevice: vi.fn(),
    selectOutputDevice: vi.fn(),
  },
}));

vi.mock("./notifications", () => ({
  setVoiceActive: vi.fn(),
}));

vi.mock("./VoiceState", () => ({
  voiceConnected: vi.fn(() => false),
  setVoiceConnected: vi.fn(),
  setVoiceTaskNumberMap: vi.fn(),
  getVoiceTaskNumber: vi.fn(() => undefined),
}));

// Stub SVG imports.
vi.mock("@material-symbols/svg-400/outlined/mic.svg?solid", () => ({ default: () => null }));
vi.mock("@material-symbols/svg-400/outlined/mic_off.svg?solid", () => ({ default: () => null }));
vi.mock("@material-symbols/svg-400/outlined/call_end.svg?solid", () => ({ default: () => null }));
vi.mock("@material-symbols/svg-400/outlined/close.svg?solid", () => ({ default: () => null }));

import VoiceOverlay from "./VoiceOverlay";

beforeEach(() => {
  vi.clearAllMocks();
  voiceState.connected = false;
});

describe("VoiceOverlay connection", () => {
  it("calls connect() on mic button click", async () => {
    const user = userEvent.setup();
    render(() => (
      <VoiceOverlay
        tasks={() => []}
        recentRepo={() => "my-repo"}
        selectedHarness={() => "claude"}
        selectedModel={() => "opus"}
      />
    ));

    const micButton = screen.getByRole("button", { name: /voice/i });
    await user.click(micButton);

    expect(connectMock).toHaveBeenCalledOnce();
  });

  it("starts voice mode when F4 is pressed", async () => {
    const user = userEvent.setup();
    render(() => (
      <VoiceOverlay
        tasks={() => []}
        recentRepo={() => "my-repo"}
        selectedHarness={() => "claude"}
        selectedModel={() => "opus"}
      />
    ));

    await user.keyboard("{F4}");

    expect(connectMock).toHaveBeenCalledOnce();
  });

  it("starts voice mode with F4 while an editor is focused", async () => {
    const user = userEvent.setup();
    render(() => (
      <>
        <input aria-label="Prompt" />
        <VoiceOverlay
          tasks={() => []}
          recentRepo={() => "my-repo"}
          selectedHarness={() => "claude"}
          selectedModel={() => "opus"}
        />
      </>
    ));

    await user.click(screen.getByRole("textbox", { name: "Prompt" }));
    await user.keyboard("{F4}");

    expect(connectMock).toHaveBeenCalledOnce();
  });

  it("stops voice mode when F4 is pressed while connected", async () => {
    const user = userEvent.setup();
    voiceState.connected = true;
    render(() => (
      <VoiceOverlay
        tasks={() => []}
        recentRepo={() => "my-repo"}
        selectedHarness={() => "claude"}
        selectedModel={() => "opus"}
      />
    ));

    await user.keyboard("{F4}");

    expect(disconnectMock).toHaveBeenCalledOnce();
    expect(connectMock).not.toHaveBeenCalled();
  });
});
