// Tests for VoiceOverlay WebRTC transport selection.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

const connectWebSocketMock = vi.fn();
const connectWebRTCMock = vi.fn();

vi.mock("./VoiceSession", () => ({
  VoiceSession: class {
    state = {
      connectStatus: null,
      connected: false,
      listening: false,
      speaking: false,
      muted: false,
      activeTool: null,
      transcript: [],
      micLevel: 0,
      error: null,
      transport: "websocket" as const,
    };
    taskNumberMap = { update: vi.fn(), reset: vi.fn() };
    excludedTaskIds = new Set<string>();
    connectWebSocket = connectWebSocketMock;
    connectWebRTC = connectWebRTCMock;
    disconnect = vi.fn();
    toggleMute = vi.fn();
    injectText = vi.fn();
    clearTranscript = vi.fn();
  },
}));

vi.mock("./notifications", () => ({
  setVoiceActive: vi.fn(),
}));

// Stub SVG imports.
vi.mock("@material-symbols/svg-400/outlined/mic.svg?solid", () => ({ default: () => null }));
vi.mock("@material-symbols/svg-400/outlined/mic_off.svg?solid", () => ({ default: () => null }));
vi.mock("@material-symbols/svg-400/outlined/call_end.svg?solid", () => ({ default: () => null }));
vi.mock("@material-symbols/svg-400/outlined/close.svg?solid", () => ({ default: () => null }));

import VoiceOverlay from "./VoiceOverlay";

beforeEach(() => {
  vi.clearAllMocks();
});

describe("VoiceOverlay transport selection", () => {
  it("calls connect() when webrtcAvailable is false", async () => {
    const user = userEvent.setup();
    render(() => (
      <VoiceOverlay
        tasks={() => []}
        recentRepo={() => "my-repo"}
        selectedHarness={() => "claude"}
        selectedModel={() => "opus"}
        webrtcAvailable={() => false}
      />
    ));

    const micButton = screen.getByRole("button", { name: /voice/i });
    await user.click(micButton);

    expect(connectWebSocketMock).toHaveBeenCalledOnce();
    expect(connectWebRTCMock).not.toHaveBeenCalled();
  });

  it("calls connectWebRTC() when webrtcAvailable is true", async () => {
    const user = userEvent.setup();
    render(() => (
      <VoiceOverlay
        tasks={() => []}
        recentRepo={() => "my-repo"}
        selectedHarness={() => "claude"}
        selectedModel={() => "opus"}
        webrtcAvailable={() => true}
      />
    ));

    const micButton = screen.getByRole("button", { name: /voice/i });
    await user.click(micButton);

    expect(connectWebRTCMock).toHaveBeenCalledOnce();
    expect(connectWebSocketMock).not.toHaveBeenCalled();
  });
});
