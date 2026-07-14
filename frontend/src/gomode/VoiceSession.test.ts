// Tests for the browser voice gateway session manager.

import { beforeEach, describe, expect, it, vi } from "vitest";

const sdkMocks = vi.hoisted(() => ({
  closeVoiceRTC: vi.fn(),
  diagnoseVoiceRTC: vi.fn(),
  voiceRTCOffer: vi.fn(async () => ({ sdp: "answer-sdp", sessionID: "session-1" })),
}));

vi.mock("@voicegateway-sdk/api.gen", () => ({
  createApiClient: () => sdkMocks,
}));

vi.mock("./McpClient", () => ({
  mcpCallTool: vi.fn(),
  mcpListTools: vi.fn(async () => []),
  mcpServerInstructions: vi.fn(async () => "instructions"),
}));

import { formatVoiceRTCDiagnostics, summarizeSDPCandidates, VoiceSession } from "./VoiceSession";
import {
  VoiceRTCConnectivityIssueUDPUnreachable,
  VoiceRTCConnectivitySideNetwork,
  type VoiceRTCDiagnosticsResp,
} from "@voicegateway-sdk/types.gen";

class FakePeerConnection extends EventTarget {
  static last: FakePeerConnection | null = null;

  connectionState: RTCPeerConnectionState = "new";
  iceConnectionState: RTCIceConnectionState = "new";
  iceGatheringState: RTCIceGatheringState = "new";
  localDescription: RTCSessionDescription | null = null;
  oniceconnectionstatechange: ((this: RTCPeerConnection, ev: Event) => unknown) | null = null;
  ontrack: ((this: RTCPeerConnection, ev: RTCTrackEvent) => unknown) | null = null;

  constructor() {
    super();
    FakePeerConnection.last = this;
  }

  addTrack(): void {}

  createDataChannel(): RTCDataChannel {
    return { close: () => {} } as RTCDataChannel;
  }

  createOffer(): Promise<RTCSessionDescriptionInit> {
    return Promise.resolve({ type: "offer", sdp: "initial-offer" });
  }

  setLocalDescription(): Promise<void> {
    this.iceGatheringState = "gathering";
    this.localDescription = { type: "offer", sdp: "initial-offer" } as RTCSessionDescription;
    queueMicrotask(() => {
      this.localDescription = {
        type: "offer",
        sdp: "v=0\r\na=candidate:1 1 udp 2130706431 192.0.2.2 50000 typ host\r\n",
      } as RTCSessionDescription;
      this.iceGatheringState = "complete";
      this.dispatchEvent(new Event("icegatheringstatechange"));
    });
    return Promise.resolve();
  }

  setRemoteDescription(): Promise<void> {
    return Promise.resolve();
  }

  close(): void {}
}

beforeEach(() => {
  sdkMocks.closeVoiceRTC.mockReset();
  sdkMocks.diagnoseVoiceRTC.mockReset();
  sdkMocks.voiceRTCOffer.mockReset();
  sdkMocks.voiceRTCOffer.mockResolvedValue({ sdp: "answer-sdp", sessionID: "session-1" });
  vi.stubGlobal("RTCPeerConnection", FakePeerConnection as unknown as typeof RTCPeerConnection);
  vi.stubGlobal("AudioContext", FakeAudioContext as unknown as typeof AudioContext);
  vi.stubGlobal("requestAnimationFrame", vi.fn(() => 1));
  Object.defineProperty(navigator, "mediaDevices", {
    configurable: true,
    value: {
      getUserMedia: vi.fn(async () => ({
        getAudioTracks: () => [{}],
        getTracks: () => [],
      })),
    },
  });
});

class FakeAudioContext {
  createAnalyser(): AnalyserNode {
    return {
      fftSize: 0,
      frequencyBinCount: 1,
      getByteTimeDomainData: () => {},
    } as unknown as AnalyserNode;
  }

  createMediaStreamSource(): MediaStreamAudioSourceNode {
    return { connect: () => {} } as unknown as MediaStreamAudioSourceNode;
  }

  close(): Promise<void> {
    return Promise.resolve();
  }
}

describe("VoiceSession", () => {
  it("sends the complete local SDP after ICE gathering", async () => {
    const session = new VoiceSession();

    await session.connect([], "", "", "");

    expect(sdkMocks.voiceRTCOffer).toHaveBeenCalledWith({
      sdp: "v=0\r\na=candidate:1 1 udp 2130706431 192.0.2.2 50000 typ host\r\n",
    });
  });

  it("does not start setup timeout before signaling completes", async () => {
    vi.useFakeTimers();
    let resolveOffer: (value: { sdp: string; sessionID: string }) => void = () => {};
    sdkMocks.voiceRTCOffer.mockReturnValue(
      new Promise((resolve) => {
        resolveOffer = resolve;
      }),
    );
    const session = new VoiceSession();

    try {
      const connect = session.connect([], "", "", "");
      await vi.waitFor(() => expect(sdkMocks.voiceRTCOffer).toHaveBeenCalled());

      await vi.advanceTimersByTimeAsync(15_000);

      expect(session.state.error).toBeNull();
      resolveOffer({ sdp: "answer-sdp", sessionID: "session-1" });
      await connect;
    } finally {
      session.disconnect();
      vi.useRealTimers();
    }
  });
});

describe("summarizeSDPCandidates", () => {
  it("summarizes candidate host, port, and type", () => {
    const sdp = "v=0\r\na=candidate:1 1 udp 2130706431 70.51.33.231 42602 typ srflx raddr 192.168.1.123 rport 42602\r\na=candidate:2 1 udp 2130706431 192.168.1.123 42602 typ host\r\n";

    expect(summarizeSDPCandidates(sdp)).toBe(
      "70.51.33.231:42602 srflx, 192.168.1.123:42602 host",
    );
  });

  it("reports none when SDP has no candidates", () => {
    expect(summarizeSDPCandidates("v=0\r\n")).toBe("none");
  });
});

describe("formatVoiceRTCDiagnostics", () => {
  it("surfaces UDP mapping errors", () => {
    const diagnostics: VoiceRTCDiagnosticsResp = {
      sessionID: "voice-session",
      issue: VoiceRTCConnectivityIssueUDPUnreachable,
      side: VoiceRTCConnectivitySideNetwork,
      message: "server is waiting for a WebRTC data channel",
      server: {
        sessionFound: true,
        udpMappingError: "refresh UPnP UDP mapping 40000 -> 3478: timeout",
      },
    };

    expect(formatVoiceRTCDiagnostics(diagnostics)).toContain(
      "UDP mapping: refresh UPnP UDP mapping 40000 -> 3478: timeout",
    );
  });
});
