// Tests for the browser voice gateway session manager.

import { beforeEach, describe, expect, it, vi } from "vitest";

const sdkMocks = vi.hoisted(() => ({
  closeVoiceRTC: vi.fn(),
  diagnoseVoiceRTC: vi.fn(),
  voiceRTCOffer: vi.fn(async () => ({ sdp: "answer-sdp", sessionID: "session-1" })),
}));

const mcpMocks = vi.hoisted(() => ({
  mcpCallTool: vi.fn(),
  mcpListTools: vi.fn(async () => []),
  mcpReadAdvertisedTextResource: vi.fn(async () => '{"items":[]}'),
  mcpServerInstructions: vi.fn(async () => "instructions"),
}));

vi.mock("@voicegateway-sdk/api.gen", () => ({
  createApiClient: () => sdkMocks,
}));

vi.mock("./McpClient", () => ({
  mcpCallTool: mcpMocks.mcpCallTool,
  mcpListTools: mcpMocks.mcpListTools,
  mcpReadAdvertisedTextResource: mcpMocks.mcpReadAdvertisedTextResource,
  mcpServerInstructions: mcpMocks.mcpServerInstructions,
}));

import {
  buildRecoveryContext,
  formatVoiceRTCDiagnostics,
  MAX_RECOVERY_CONTEXT_CHARS,
  summarizeSDPCandidates,
  VoiceSession,
  voiceToolDeclarations,
} from "./VoiceSession";
import {
  MessageKindToolCall,
  type ToolCall,
  VoiceRTCConnectivityIssueUDPUnreachable,
  VoiceRTCConnectivitySideNetwork,
  type VoiceRTCDiagnosticsResp,
} from "@voicegateway-sdk/types.gen";

type VoiceSessionToolCallHandler = {
  _handleToolCall(msg: ToolCall): Promise<void>;
};

class FakePeerConnection extends EventTarget {
  static completeICE = true;
  static reflexiveCandidateDelayMs: number | null = null;
  static last: FakePeerConnection | null = null;
  static instances: FakePeerConnection[] = [];
  static dataChannels: FakeDataChannel[] = [];

  connectionState: RTCPeerConnectionState = "new";
  iceConnectionState: RTCIceConnectionState = "new";
  iceGatheringState: RTCIceGatheringState = "new";
  localDescription: RTCSessionDescription | null = null;
  oniceconnectionstatechange: ((this: RTCPeerConnection, ev: Event) => unknown) | null = null;
  ontrack: ((this: RTCPeerConnection, ev: RTCTrackEvent) => unknown) | null = null;

  constructor() {
    super();
    FakePeerConnection.last = this;
    FakePeerConnection.instances.push(this);
  }

  addTrack(): void {}

  createDataChannel(): RTCDataChannel {
    const channel = new FakeDataChannel();
    FakePeerConnection.dataChannels.push(channel);
    return channel as unknown as RTCDataChannel;
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
      const candidateEvent = new Event("icecandidate");
      Object.defineProperty(candidateEvent, "candidate", {
        value: { candidate: "candidate:1 1 udp 2130706431 192.0.2.2 50000 typ host" },
      });
      this.dispatchEvent(candidateEvent);
      if (FakePeerConnection.reflexiveCandidateDelayMs !== null) {
        window.setTimeout(() => {
          this.localDescription = {
            type: "offer",
            sdp: "v=0\r\na=candidate:1 1 udp 2130706431 192.0.2.2 50000 typ host\r\na=candidate:2 1 udp 1694498815 203.0.113.2 50000 typ srflx raddr 192.0.2.2 rport 50000\r\n",
          } as RTCSessionDescription;
          const reflexiveCandidateEvent = new Event("icecandidate");
          Object.defineProperty(reflexiveCandidateEvent, "candidate", {
            value: {
              candidate: "candidate:2 1 udp 1694498815 203.0.113.2 50000 typ srflx raddr 192.0.2.2 rport 50000",
            },
          });
          this.dispatchEvent(reflexiveCandidateEvent);
        }, FakePeerConnection.reflexiveCandidateDelayMs);
      }
      if (FakePeerConnection.completeICE) {
        this.iceGatheringState = "complete";
        this.dispatchEvent(new Event("icegatheringstatechange"));
      }
    });
    return Promise.resolve();
  }

  setRemoteDescription(): Promise<void> {
    return Promise.resolve();
  }

  close(): void {}
}

class FakeDataChannel {
  readonly send = vi.fn();
  readyState: RTCDataChannelState = "open";
  onclose: (() => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onopen: (() => void) | null = null;

  close(): void {
    this.readyState = "closed";
  }
}

beforeEach(() => {
  FakePeerConnection.completeICE = true;
  FakePeerConnection.reflexiveCandidateDelayMs = null;
  FakePeerConnection.last = null;
  FakePeerConnection.instances = [];
  FakePeerConnection.dataChannels = [];
  sdkMocks.closeVoiceRTC.mockReset();
  sdkMocks.diagnoseVoiceRTC.mockReset();
  sdkMocks.voiceRTCOffer.mockReset();
  sdkMocks.voiceRTCOffer.mockResolvedValue({ sdp: "answer-sdp", sessionID: "session-1" });
  mcpMocks.mcpCallTool.mockReset();
  mcpMocks.mcpListTools.mockReset();
  mcpMocks.mcpListTools.mockResolvedValue([]);
  mcpMocks.mcpReadAdvertisedTextResource.mockReset();
  mcpMocks.mcpReadAdvertisedTextResource.mockResolvedValue('{"items":[]}');
  mcpMocks.mcpServerInstructions.mockReset();
  mcpMocks.mcpServerInstructions.mockResolvedValue("instructions");
  vi.stubGlobal("RTCPeerConnection", FakePeerConnection as unknown as typeof RTCPeerConnection);
  vi.stubGlobal("AudioContext", FakeAudioContext as unknown as typeof AudioContext);
  vi.stubGlobal(
    "requestAnimationFrame",
    vi.fn(() => 1),
  );
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
  it("keeps hang_up local to voice and rejects MCP name conflicts", async () => {
    const tools = voiceToolDeclarations([
      {
        name: "tasks_list",
        description: "List tasks",
        inputSchema: { type: "object", properties: {} },
      },
    ]);
    expect(tools.map((tool) => tool.name)).toEqual(["hang_up", "tasks_list"]);

    expect(() =>
      voiceToolDeclarations([
        {
          name: "hang_up",
          description: "Unexpected MCP tool",
          inputSchema: { type: "object", properties: {} },
        },
      ]),
    ).toThrow('MCP tool "hang_up" conflicts with the reserved voice command.');

    const session = new VoiceSession();
    await (session as unknown as VoiceSessionToolCallHandler)._handleToolCall({
      kind: MessageKindToolCall,
      id: "hang-up-1",
      name: "hang_up",
      args: {},
    });

    expect(mcpMocks.mcpCallTool).not.toHaveBeenCalled();
    expect(session.state.connected).toBe(false);
  });

  it("sends the complete local SDP after ICE gathering", async () => {
    const session = new VoiceSession();

    await session.connect();

    expect(sdkMocks.voiceRTCOffer).toHaveBeenCalledWith({
      sdp: "v=0\r\na=candidate:1 1 udp 2130706431 192.0.2.2 50000 typ host\r\n",
    });
  });

  it("includes the current bounded service items in session setup", async () => {
    mcpMocks.mcpReadAdvertisedTextResource.mockResolvedValue(
      '{"items":[{"id":"1","reference":"Task #1","title":"Build feature","state":"running","needsAttention":false}]}',
    );
    const session = new VoiceSession();

    await session.connect();
    FakePeerConnection.dataChannels[0]?.onopen?.();

    const sent = FakePeerConnection.dataChannels[0]?.send.mock.calls[0]?.[0];
    expect(JSON.parse(sent as string)).toMatchObject({
      kind: "session.setup",
      context: {
        systemInstruction: "instructions",
        text: "Current service items:\n- Task #1: Build feature (running)",
      },
    });
  });

  it("starts with an empty baseline when service-item loading fails", async () => {
    mcpMocks.mcpReadAdvertisedTextResource.mockRejectedValue(new Error("resource unavailable"));
    const session = new VoiceSession();

    await session.connect();
    FakePeerConnection.dataChannels[0]?.onopen?.();

    expect(sdkMocks.voiceRTCOffer).toHaveBeenCalled();
    const sent = FakePeerConnection.dataChannels[0]?.send.mock.calls[0]?.[0];
    expect(JSON.parse(sent as string).context.text).toBe("No visible service items.");
  });

  it("waits briefly for a reflexive candidate without waiting for ICE completion", async () => {
    vi.useFakeTimers();
    FakePeerConnection.completeICE = false;
    FakePeerConnection.reflexiveCandidateDelayMs = 50;
    const session = new VoiceSession();

    try {
      const connect = session.connect();
      await vi.advanceTimersByTimeAsync(49);
      expect(sdkMocks.voiceRTCOffer).not.toHaveBeenCalled();

      await vi.advanceTimersByTimeAsync(1);
      await connect;

      expect(sdkMocks.voiceRTCOffer).toHaveBeenCalledWith({
        sdp: "v=0\r\na=candidate:1 1 udp 2130706431 192.0.2.2 50000 typ host\r\na=candidate:2 1 udp 1694498815 203.0.113.2 50000 typ srflx raddr 192.0.2.2 rport 50000\r\n",
      });
      expect(FakePeerConnection.last?.iceGatheringState).toBe("gathering");
    } finally {
      session.disconnect();
      vi.useRealTimers();
    }
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
      const connect = session.connect();
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

function triggerIceState(state: RTCIceConnectionState): void {
  const pc = FakePeerConnection.last;
  if (!pc) throw new Error("Expected a peer connection");
  pc.iceConnectionState = state;
  pc.oniceconnectionstatechange?.call(pc as unknown as RTCPeerConnection, new Event("iceconnectionstatechange"));
}

describe("voice network recovery", () => {
  it("fetches a fresh service-item snapshot for reconnect setup", async () => {
    vi.useFakeTimers();
    mcpMocks.mcpReadAdvertisedTextResource
      .mockResolvedValueOnce('{"items":[{"id":"1","title":"Old state","state":"running","needsAttention":false}]}')
      .mockResolvedValueOnce('{"items":[{"id":"1","title":"Fresh state","state":"waiting","needsAttention":true}]}');
    const session = new VoiceSession();
    try {
      await session.connect();
      FakePeerConnection.dataChannels[0]?.onopen?.();

      triggerIceState("failed");
      await vi.advanceTimersByTimeAsync(0);
      await vi.waitFor(() => expect(FakePeerConnection.dataChannels).toHaveLength(2));
      FakePeerConnection.dataChannels[1]?.onopen?.();

      const firstSetup = FakePeerConnection.dataChannels[0]?.send.mock.calls[0]?.[0];
      const secondSetup = FakePeerConnection.dataChannels[1]?.send.mock.calls[0]?.[0];
      expect(JSON.parse(firstSetup as string).context.text).toContain("Old state");
      expect(JSON.parse(secondSetup as string).context.text).toContain("Fresh state");
    } finally {
      session.disconnect();
      vi.useRealTimers();
    }
  });

  it("cancels the disconnected grace recovery when ICE reconnects", async () => {
    vi.useFakeTimers();
    const session = new VoiceSession();
    try {
      await session.connect();
      triggerIceState("disconnected");
      expect(session.state.connectStatus).toContain("reconnecting");

      await vi.advanceTimersByTimeAsync(4_999);
      triggerIceState("connected");
      await vi.advanceTimersByTimeAsync(1);

      expect(FakePeerConnection.instances).toHaveLength(1);
    } finally {
      session.disconnect();
      vi.useRealTimers();
    }
  });

  it("recovers immediately after ICE failure and stops after three retries", async () => {
    vi.useFakeTimers();
    const session = new VoiceSession();
    try {
      await session.connect();
      for (let attempt = 1; attempt <= 3; attempt++) {
        triggerIceState("failed");
        await vi.advanceTimersByTimeAsync(0);
        await vi.waitFor(() => expect(FakePeerConnection.instances).toHaveLength(attempt + 1));
      }

      triggerIceState("failed");
      expect(session.state.error).toBe("Voice connection lost after 3 recovery attempts");
      expect(FakePeerConnection.instances).toHaveLength(4);
    } finally {
      session.disconnect();
      vi.useRealTimers();
    }
  });

  it("cancels a pending recovery when manually disconnected", async () => {
    vi.useFakeTimers();
    const session = new VoiceSession();
    try {
      await session.connect();
      triggerIceState("disconnected");
      session.disconnect();
      await vi.advanceTimersByTimeAsync(5_000);

      expect(FakePeerConnection.instances).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("buildRecoveryContext", () => {
  it("preserves finalized chronological transcript and bounded service context", () => {
    const context = buildRecoveryContext([
      { speaker: "user", text: "first", final: true },
      { speaker: "assistant", text: "second", final: true },
      { speaker: "user", text: "partial", final: false },
    ]);

    expect(context).toContain("do not treat this as a new user turn");
    expect(context).toContain("user: first\nassistant: second");
    expect(context).not.toContain("partial");
  });

  it("bounds recovery messages without splitting finalized history order", () => {
    const context = buildRecoveryContext(
      Array.from({ length: 20 }, (_, index) => ({
        speaker: "user" as const,
        text: `${index}: ${"x".repeat(600)}`,
        final: true,
      })),
    );

    expect(context.length).toBeLessThanOrEqual(MAX_RECOVERY_CONTEXT_CHARS);
    expect(context.indexOf("user: 19:")).toBeGreaterThan(context.indexOf("user: 18:"));
  });
});

describe("summarizeSDPCandidates", () => {
  it("summarizes candidate host, port, and type", () => {
    const sdp =
      "v=0\r\na=candidate:1 1 udp 2130706431 70.51.33.231 42602 typ srflx raddr 192.168.1.123 rport 42602\r\na=candidate:2 1 udp 2130706431 192.168.1.123 42602 typ host\r\n";

    expect(summarizeSDPCandidates(sdp)).toBe("70.51.33.231:42602 srflx, 192.168.1.123:42602 host");
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
