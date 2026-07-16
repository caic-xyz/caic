// Core voice gateway session manager for the web frontend via WebRTC.

import { createStore, produce } from "solid-js/store";

// TODO: Remove references to "Task".
import type { Task } from "@sdk/types.gen";
// TODO: Cleanup imports.
import * as voicegatewaySDK from "@voicegateway-sdk/api.gen";
import {
  type Error,
  type ContextUpdate,
  type UserMessage,
  type MessageEnvelope,
  type SessionSetup,
  type ToolResult,
  type ToolCall,
  type TranscriptDelta,
  type VoiceRTCClientDiagnostics,
  type VoiceRTCDiagnosticsResp,
  MessageKindContextUpdate,
  MessageKindUserMessage,
  MessageKindError,
  MessageKindInterrupted,
  MessageKindSessionReady,
  MessageKindSessionSetup,
  MessageKindSpeechEnded,
  MessageKindSpeechStarted,
  MessageKindToolCall,
  MessageKindToolResult,
  MessageKindTranscriptDelta,
} from "@voicegateway-sdk/types.gen";

import { TaskNumberMap } from "./TaskNumberMap";
import {
  mcpListTools,
  mcpCallTool,
  mcpServerInstructions,
  type McpToolDescriptor,
} from "./McpClient";

// Constants

/** Max time (ms) to wait for session.ready before timing out. */
const SETUP_TIMEOUT_MS = 15000;
const ICE_GATHERING_TIMEOUT_MS = 10000;
const ICE_HOST_CANDIDATE_GRACE_MS = 1000;
const ICE_DISCONNECTED_GRACE_MS = 5000;
const MAX_RECONNECT_ATTEMPTS = 3;

export const voiceGatewayApi = voicegatewaySDK.createApiClient();

export const {
  voiceRTCOffer,
  diagnoseVoiceRTC,
  closeVoiceRTC,
} = voiceGatewayApi;

// State types

export type TranscriptSpeaker = "user" | "assistant";

export interface TranscriptEntry {
  speaker: TranscriptSpeaker;
  text: string;
  final: boolean;
}

/** Describes a single audio input/output device. Mirrors Android's AudioDevice. */
export interface AudioDevice {
  deviceId: string;
  kind: MediaDeviceKind;
  label: string;
}

export interface VoiceState {
  connectStatus: string | null;
  connected: boolean;
  listening: boolean;
  speaking: boolean;
  muted: boolean;
  activeTool: string | null;
  transcript: TranscriptEntry[];
  micLevel: number;
  error: string | null;
  /** Available audio input devices (microphones). */
  audioInputs: AudioDevice[];
  /** Available audio output devices (speakers, headsets). */
  audioOutputs: AudioDevice[];
  /** Currently selected input device ID, empty string for system default. */
  selectedInputId: string;
  /** Currently selected output device ID, empty string for system default. */
  selectedOutputId: string;
}

// VoiceSession

export class VoiceSession {
  readonly state: VoiceState;
  private readonly _setState: (fn: (s: VoiceState) => VoiceState) => void;

  readonly taskNumberMap = new TaskNumberMap();

  /** Task IDs excluded from AI context (pre-purged at session start). */
  excludedTaskIds: Set<string> = new Set();

  private _pc: RTCPeerConnection | null = null;
  private _dc: RTCDataChannel | null = null;
  private _rtcSessionID: string | null = null;
  private _lastOfferSDP = "";
  private _lastAnswerSDP = "";
  private _audioContext: AudioContext | null = null;
  private _micStream: MediaStream | null = null;
  /** The <audio> element playing remote RTP audio, stored so we can call setSinkId(). */
  private _speakerAudio: HTMLAudioElement | null = null;
  /** True while the model is speaking — injected text is buffered and flushed after the turn ends. */
  private _speakerActive = false;
  /** Text notifications buffered while the model is speaking; flushed on turn end. */
  private _pendingNotifications: string[] = [];
  private _setupTimer: ReturnType<typeof setTimeout> | null = null;
  private _reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private _reconnectArgs: [Task[], string, string, string] | null = null;
  private _reconnectAttempts = 0;

  constructor() {
    const [state, setState] = createStore<VoiceState>({
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
    });
    // eslint-disable-next-line solid/reactivity
    this.state = state;
    this._setState = setState as (fn: (s: VoiceState) => VoiceState) => void;
  }

  // -----------------------------------------------------------------------
  // Public API
  // -----------------------------------------------------------------------

  /** Enumerate available audio devices and auto-select defaults. Call before connect(). */
  async enumerateDevices(): Promise<void> {
    try {
      const devices = await navigator.mediaDevices.enumerateDevices();
      const inputs: AudioDevice[] = [];
      const outputs: AudioDevice[] = [];
      for (const d of devices) {
        if (d.kind === "audioinput") {
          inputs.push({
            deviceId: d.deviceId,
            kind: d.kind,
            label: d.label || "Microphone",
          });
        } else if (d.kind === "audiooutput") {
          outputs.push({
            deviceId: d.deviceId,
            kind: d.kind,
            label: d.label || "Speaker",
          });
        }
      }
      // Auto-select: first available, or keep current selection if still valid.
      const curSel = this.state.selectedInputId;
      const curOut = this.state.selectedOutputId;
      const newInId =
        curSel && inputs.some((d) => d.deviceId === curSel)
          ? curSel
          : (inputs[0]?.deviceId ?? "");
      const newOutId =
        curOut && outputs.some((d) => d.deviceId === curOut)
          ? curOut
          : (outputs[0]?.deviceId ?? "");
      this._update((s) => {
        s.audioInputs = inputs;
        s.audioOutputs = outputs;
        s.selectedInputId = newInId;
        s.selectedOutputId = newOutId;
      });
    } catch {
      // enumerateDevices() can fail if permissions are denied; ignore silently.
    }
  }

  /** Select an audio input device. If connected, replaces the mic track. */
  async selectInputDevice(deviceId: string): Promise<void> {
    this._update((s) => {
      s.selectedInputId = deviceId;
    });
    if (this._pc && this._micStream) {
      try {
        // Stop old mic tracks.
        this._micStream.getTracks().forEach((t) => t.stop());
        // Acquire new mic stream with the selected device.
        const constraints: MediaStreamConstraints = {
          audio: deviceId ? { deviceId: { exact: deviceId } } : true,
          video: false,
        };
        const newStream =
          await navigator.mediaDevices.getUserMedia(constraints);
        this._micStream = newStream;
        // Replace tracks on the PeerConnection.
        const sender = this._pc
          .getSenders()
          .find((s) => s.track?.kind === "audio");
        for (const t of newStream.getAudioTracks()) {
          if (sender) {
            await sender.replaceTrack(t);
          } else {
            this._pc.addTrack(t, newStream);
          }
        }
        // Reconnect AnalyserNode if present.
        if (this._audioContext) {
          const analyser = this._audioContext.createAnalyser();
          analyser.fftSize = 256;
          const source = this._audioContext.createMediaStreamSource(newStream);
          source.connect(analyser);
          const buf = new Uint8Array(analyser.frequencyBinCount);
          const pc = this._pc;
          const pollMicLevel = () => {
            if (this._pc !== pc) return;
            analyser.getByteTimeDomainData(buf);
            let sumSq = 0;
            for (let i = 0; i < buf.length; i++) {
              const v = (buf[i] - 128) / 128;
              sumSq += v * v;
            }
            const rms = Math.sqrt(sumSq / buf.length);
            if (!this.state.muted && !this._speakerActive) {
              this._update((s) => {
                s.micLevel = Math.min(1, Math.sqrt(rms));
              });
            }
            requestAnimationFrame(pollMicLevel);
          };
          requestAnimationFrame(pollMicLevel);
        }
      } catch {
        // If switching fails, leave the previous stream in place.
      }
    }
  }

  /** Select an audio output device. Applies immediately if connected. */
  selectOutputDevice(deviceId: string): void {
    this._update((s) => {
      s.selectedOutputId = deviceId;
    });
    if (this._speakerAudio && "setSinkId" in this._speakerAudio) {
      void (
        this._speakerAudio as HTMLAudioElement & {
          setSinkId: (id: string) => Promise<void>;
        }
      ).setSinkId(deviceId);
    }
  }

  /** Start a new voice session via WebRTC data channel through the caic backend. */
  async connect(
    tasks: Task[],
    _recentRepo: string,
    _defaultHarness: string,
    _defaultModel: string,
  ): Promise<void> {
    this._reconnectArgs = [tasks, _recentRepo, _defaultHarness, _defaultModel];
    if (this._reconnectTimer !== null) {
      clearTimeout(this._reconnectTimer);
      this._reconnectTimer = null;
    }
    this._releaseAll();
    this._lastOfferSDP = "";
    this._lastAnswerSDP = "";
    this._audioContext = new AudioContext({ sampleRate: 16000 });
    this._clearTranscript();
    this._setStatus("Setting up WebRTC…");

    try {
      const [systemInstruction, mcpTools] = await Promise.all([
        mcpServerInstructions(),
        mcpListTools(),
      ]);

      // Keep local task numbering aligned with the server-provided voice prompt.
      const prePurged = new Set(
        tasks
          .filter(
            (t) =>
              t.state === "purged" ||
              t.state === "failed" ||
              t.state === "crashed" ||
              t.state === "stopped" ||
              t.state === "stopping",
          )
          .map((t) => t.id),
      );
      this.excludedTaskIds = prePurged;
      this.taskNumberMap.reset();
      this.taskNumberMap.update(tasks);

      // Create PeerConnection.
      const pc = new RTCPeerConnection({
        iceServers: [{ urls: "stun:stun.l.google.com:19302" }],
      });
      this._pc = pc;

      // Mic audio → RTP track (browser handles Opus encoding + AEC).
      const inputId = this.state.selectedInputId;
      const micConstraints: MediaStreamConstraints = {
        audio: inputId ? { deviceId: { exact: inputId } } : true,
        video: false,
      };
      this._micStream =
        await navigator.mediaDevices.getUserMedia(micConstraints);
      for (const t of this._micStream.getAudioTracks()) {
        pc.addTrack(t, this._micStream);
      }

      // Mic level via AnalyserNode (replaces AudioWorklet RMS in WebRTC mode).
      if (this._audioContext) {
        const analyser = this._audioContext.createAnalyser();
        analyser.fftSize = 256;
        const source = this._audioContext.createMediaStreamSource(
          this._micStream,
        );
        source.connect(analyser);
        const buf = new Uint8Array(analyser.frequencyBinCount);
        const pollMicLevel = () => {
          if (!this._pc || this._pc !== pc) return;
          analyser.getByteTimeDomainData(buf);
          let sumSq = 0;
          for (let i = 0; i < buf.length; i++) {
            const v = (buf[i] - 128) / 128;
            sumSq += v * v;
          }
          const rms = Math.sqrt(sumSq / buf.length);
          if (!this.state.muted && !this._speakerActive) {
            this._update((s) => {
              s.micLevel = Math.min(1, Math.sqrt(rms));
            });
          }
          requestAnimationFrame(pollMicLevel);
        };
        requestAnimationFrame(pollMicLevel);
      }

      // Speaker audio from remote RTP track.
      const outId = this.state.selectedOutputId;
      pc.ontrack = (evt) => {
        const audio = new Audio();
        this._speakerAudio = audio;
        audio.srcObject = evt.streams[0];
        if (outId && "setSinkId" in audio) {
          void (
            audio as HTMLAudioElement & {
              setSinkId: (id: string) => Promise<void>;
            }
          ).setSinkId(outId);
        }
        audio.play().catch(() => {
          // Autoplay may be blocked; user interaction will resume.
        });
      };

      // Create data channel carrying the voice gateway control protocol.
      const dc = pc.createDataChannel("voice-gateway", { ordered: true });
      this._dc = dc;

      dc.onmessage = (evt: MessageEvent<string>) => {
        this._handleMessage(evt.data).catch((err: unknown) => {
          this._setError(
            err instanceof Error ? err.message : "Message handling failed",
          );
        });
      };

      dc.onopen = () => {
        this._reconnectAttempts = 0;
        this._setStatus("Waiting for server…");
        this._sendSetup(mcpTools, systemInstruction);
      };

      dc.onclose = () => {
        if (this._dc !== dc) return;
        if (this.state.connected) {
          this._update((s) => {
            s.connected = false;
            s.listening = false;
            s.speaking = false;
          });
        }
      };

      pc.oniceconnectionstatechange = () => {
        const st = pc.iceConnectionState;
        if (st === "failed") {
          this._scheduleReconnect(pc, `WebRTC ICE ${st}`, 0);
        } else if (st === "disconnected") {
          this._scheduleReconnect(pc, `WebRTC ICE ${st}`, ICE_DISCONNECTED_GRACE_MS);
        } else if (st === "connected" && this._reconnectTimer !== null) {
          clearTimeout(this._reconnectTimer);
          this._reconnectTimer = null;
        } else if (st === "closed") {
          void this._setDiagnosticError(pc, `WebRTC ICE ${st}`);
        }
      };

      // SDP offer/answer exchange.
      this._setStatus("Signaling…");
      const offer = await pc.createOffer();
      const offerSDP = await completeLocalOffer(pc, offer);
      this._lastOfferSDP = offerSDP;
      const resp = await voiceRTCOffer({ sdp: offerSDP });
      this._rtcSessionID = resp.sessionID;
      this._lastAnswerSDP = resp.sdp;
      await pc.setRemoteDescription({ type: "answer", sdp: resp.sdp });
      this._startSetupTimer(pc);

      this._update((s) => {
        s.listening = true;
      });
    } catch (e: unknown) {
      this._setError(
        e instanceof Error ? e.message : "WebRTC connection failed",
      );
    }
  }

  disconnect(): void {
    this._reconnectArgs = null;
    if (this._reconnectTimer !== null) {
      clearTimeout(this._reconnectTimer);
      this._reconnectTimer = null;
    }
    if (this._setupTimer !== null) {
      clearTimeout(this._setupTimer);
      this._setupTimer = null;
    }
    this._releaseAll();
    this._speakerAudio = null;
    // Preserve transcript for review; mark all entries final.
    this._update((s) => {
      s.connected = false;
      s.listening = false;
      s.speaking = false;
      s.muted = false;
      s.connectStatus = null;
      s.activeTool = null;
      s.micLevel = 0;
      s.transcript = s.transcript.map((e) => ({ ...e, final: true }));
    });
  }

  toggleMute(): void {
    this._update((s) => {
      s.muted = !s.muted;
    });
    if (this._micStream) {
      const enabled = !this.state.muted;
      this._micStream.getAudioTracks().forEach((t) => {
        t.enabled = enabled;
      });
    }
  }

  injectText(text: string): void {
    if (this._speakerActive) {
      this._pendingNotifications.push(text);
      return;
    }
    this._send(JSON.stringify(gatewayContextUpdate(text)));
  }

  private _flushPendingNotifications(): void {
    if (this._pendingNotifications.length === 0) return;
    const text = this._pendingNotifications.join("\n");
    this._pendingNotifications = [];
    this.injectText(text);
  }

  clearTranscript(): void {
    this._clearTranscript();
  }

  // -----------------------------------------------------------------------
  // Private helpers
  // -----------------------------------------------------------------------

  /** Send a message via the WebRTC data channel. */
  private _send(msg: string): void {
    if (this._dc && this._dc.readyState === "open") {
      this._dc.send(msg);
    }
  }

  /** Release WebRTC transport and audio resources. */
  private _releaseAll(): void {
    this._dc?.close();
    this._dc = null;
    this._pc?.close();
    this._pc = null;
    this._rtcSessionID = null;
    this._releaseAudio();
  }

  private _setStatus(status: string): void {
    this._update((s) => {
      s.connectStatus = status;
      s.error = null;
    });
  }

  private _scheduleReconnect(pc: RTCPeerConnection, reason: string, delay: number): void {
    if (this._pc !== pc || this._reconnectArgs === null || this._reconnectAttempts >= MAX_RECONNECT_ATTEMPTS) return;
    if (this._reconnectTimer !== null) {
      if (delay !== 0) return;
      clearTimeout(this._reconnectTimer);
    }
    this._setStatus(`${reason}; reconnecting…`);
    this._reconnectTimer = setTimeout(() => {
      this._reconnectTimer = null;
      const args = this._reconnectArgs;
      if (args === null) return;
      this._reconnectAttempts++;
      void this.connect(...args);
    }, delay);
  }

  private _startSetupTimer(pc: RTCPeerConnection): void {
    if (this._setupTimer !== null) clearTimeout(this._setupTimer);
    this._setupTimer = setTimeout(() => {
      if (this._pc === pc && !this.state.connected && !this.state.error) {
        void this._setConnectionTimeoutError(pc);
      }
    }, SETUP_TIMEOUT_MS);
  }

  private async _setConnectionTimeoutError(
    pc: RTCPeerConnection,
  ): Promise<void> {
    await this._setDiagnosticError(
      pc,
      "Connection timed out — server did not respond",
    );
  }

  private async _setDiagnosticError(
    pc: RTCPeerConnection,
    fallback: string,
  ): Promise<void> {
    if (this._pc !== pc || this.state.error) {
      return;
    }
    const sessionID = this._rtcSessionID;
    if (!sessionID) {
      this._setError(fallback);
      return;
    }
    try {
      const diagnostics = await diagnoseVoiceRTC(sessionID, {
        client: this._clientDiagnostics(pc),
      });
      logVoiceRTCDiagnostics(diagnostics, this._lastOfferSDP, this._lastAnswerSDP);
      if (this._pc === pc && !this.state.connected && !this.state.error) {
        this._setError(formatVoiceRTCDiagnostics(diagnostics));
      }
    } catch (e: unknown) {
      const detail = e instanceof Error ? ` (${e.message})` : "";
      if (this._pc === pc && !this.state.connected && !this.state.error) {
        this._setError(`${fallback}${detail}`);
      }
    }
  }

  private _clientDiagnostics(
    pc: RTCPeerConnection,
  ): VoiceRTCClientDiagnostics {
    return {
      iceConnectionState: pc.iceConnectionState,
      iceGatheringState: pc.iceGatheringState,
      connectionState: pc.connectionState,
      signalingState: pc.signalingState,
      dataChannelState: this._dc?.readyState,
    };
  }

  private _setError(message: string): void {
    this._releaseAll();
    this._update((s) => {
      s.connectStatus = null;
      s.connected = false;
      s.listening = false;
      s.speaking = false;
      s.error = message;
    });
  }

  private _clearTranscript(): void {
    this._update((s) => {
      s.transcript = [];
    });
  }

  private _update(fn: (s: VoiceState) => void): void {
    this._setState(produce(fn));
  }

  // -----------------------------------------------------------------------
  // Gateway setup message
  // -----------------------------------------------------------------------

  private _sendSetup(
    tools: McpToolDescriptor[],
    systemInstruction: string,
  ): void {
    const setup = gatewaySessionSetup(
      tools.map((d) => ({
        name: d.name,
        description: d.description,
        parameters: d.inputSchema,
      })),
      systemInstruction,
    );
    this._send(JSON.stringify(setup));
  }

  // -----------------------------------------------------------------------
  // Message handling
  // -----------------------------------------------------------------------

  private async _handleMessage(text: string): Promise<void> {
    let env: MessageEnvelope;
    try {
      env = JSON.parse(text) as MessageEnvelope;
    } catch {
      return;
    }

    if (env.kind === MessageKindSessionReady) {
      if (this._setupTimer !== null) {
        clearTimeout(this._setupTimer);
        this._setupTimer = null;
      }
      this._update((s) => {
        s.connectStatus = null;
        s.connected = true;
        s.error = null;
      });
      this._send(JSON.stringify(gatewayUserMessage("Say exactly one word: Ready")));
      // Audio capture is handled via WebRTC RTP tracks; no separate audio setup needed.
      return;
    }

    if (env.kind === MessageKindTranscriptDelta) {
      this._handleTranscriptDelta(JSON.parse(text) as TranscriptDelta);
      return;
    }

    if (env.kind === MessageKindSpeechStarted) {
      this._speakerActive = true;
      this._update((s) => {
        s.speaking = true;
      });
      return;
    }

    if (env.kind === MessageKindSpeechEnded) {
      this._speakerActive = false;
      this._flushPendingNotifications();
      this._update((s) => {
        s.speaking = false;
        s.transcript = s.transcript.map((e) => ({ ...e, final: true }));
      });
      return;
    }

    if (env.kind === MessageKindInterrupted) {
      this._speakerActive = false;
      this._flushPendingNotifications();
      this._update((s) => {
        s.speaking = false;
        s.activeTool = null;
      });
      return;
    }

    if (env.kind === MessageKindToolCall) {
      await this._handleToolCall(JSON.parse(text) as ToolCall);
      return;
    }

    if (env.kind === MessageKindError) {
      if (this._setupTimer !== null) {
        clearTimeout(this._setupTimer);
        this._setupTimer = null;
      }
      const msg = JSON.parse(text) as Error;
      this._setError(msg.message);
    }
  }

  private _handleTranscriptDelta(msg: TranscriptDelta): void {
    if (msg.speaker !== "user" && msg.speaker !== "assistant") return;
    if (!msg.text) return;
    this._update((s) => {
      s.transcript = appendChunk(s.transcript, msg.speaker, msg.text ?? "");
    });
  }

  private async _handleToolCall(msg: ToolCall): Promise<void> {
    if (!msg.id || !msg.name) return;

    try {
      this._update((s) => {
        s.activeTool = msg.name ?? null;
      });
      const result = await mcpCallTool(msg.name, msg.args ?? {});
      this._update((s) => {
        s.activeTool = null;
      });

      // Surface tool errors in the transcript.
      if (result.isError) {
        const errMsg =
          typeof result.structuredContent["error"] === "string"
            ? result.structuredContent["error"]
            : "Tool error";
        this._update((s) => {
          s.transcript = [
            ...s.transcript,
            {
              speaker: "assistant" as TranscriptSpeaker,
              text: `[${msg.name}] ${errMsg}`,
              final: true,
            },
          ];
        });
      }

      this._sendToolResult(msg.id, msg.name, result.structuredContent);
    } catch (e: unknown) {
      this._update((s) => {
        s.activeTool = null;
      });
      const errMsg = e instanceof Error ? e.message : "Unknown error";
      this._update((s) => {
        s.transcript = [
          ...s.transcript,
          {
            speaker: "assistant" as TranscriptSpeaker,
            text: `[${msg.name}] ${errMsg}`,
            final: true,
          },
        ];
      });
      this._sendToolResult(msg.id, msg.name, { error: errMsg });
    }
  }

  private _sendToolResult(
    id: string,
    name: string,
    result: Record<string, unknown>,
  ): void {
    this._send(JSON.stringify(gatewayToolResult(id, name, result)));
  }

  // -----------------------------------------------------------------------
  // Audio cleanup
  // -----------------------------------------------------------------------

  private _releaseAudio(): void {
    try {
      this._micStream?.getTracks().forEach((t) => t.stop());
      this._micStream = null;
    } catch {
      // ignore
    }
    try {
      void this._audioContext?.close();
      this._audioContext = null;
    } catch {
      // ignore
    }
    this._speakerActive = false;
    this._pendingNotifications = [];
  }
}

function completeLocalOffer(
  pc: RTCPeerConnection,
  offer: RTCSessionDescriptionInit,
): Promise<string> {
  return pc.setLocalDescription(offer).then(async () => {
    await waitForUsableICECandidate(pc);
    const sdp = pc.localDescription?.sdp;
    if (!sdp) {
      throw new Error("WebRTC local offer SDP unavailable after ICE gathering");
    }
    return sdp;
  });
}

// waitForUsableICECandidate does not wait for every configured STUN server.
// A slow or unreachable STUN request must not delay LAN or Tailscale signaling.
function waitForUsableICECandidate(pc: RTCPeerConnection): Promise<void> {
  return new Promise((resolve, reject) => {
    let hostCandidateGrace: number | null = null;
    const timeout = window.setTimeout(() => {
      cleanup();
      reject(new Error("Timed out waiting for a usable WebRTC ICE candidate"));
    }, ICE_GATHERING_TIMEOUT_MS);
    const cleanup = () => {
      window.clearTimeout(timeout);
      if (hostCandidateGrace !== null) window.clearTimeout(hostCandidateGrace);
      pc.removeEventListener("icecandidate", onICECandidate);
      pc.removeEventListener("icegatheringstatechange", onGatheringStateChange);
      pc.removeEventListener("connectionstatechange", onConnectionStateChange);
    };
    const resolveIfUsable = (): boolean => {
      if (!localDescriptionHasUsableICECandidate(pc)) return false;
      cleanup();
      resolve();
      return true;
    };
    const onICECandidate = (event: Event) => {
      const candidate = (event as Event & { candidate: RTCIceCandidate | null }).candidate;
      if (candidate === null || !isUsableICECandidate(candidate.candidate)) return;
      if (iceCandidateType(candidate.candidate) === "srflx") {
        cleanup();
        resolve();
        return;
      }
      if (hostCandidateGrace !== null) return;
      hostCandidateGrace = window.setTimeout(resolveIfUsable, ICE_HOST_CANDIDATE_GRACE_MS);
    };
    const onGatheringStateChange = () => {
      if (resolveIfUsable() || pc.iceGatheringState !== "complete") return;
      cleanup();
      reject(new Error("WebRTC ICE gathering completed without a usable candidate"));
    };
    const onConnectionStateChange = () => {
      if (pc.connectionState !== "closed" && pc.connectionState !== "failed") return;
      cleanup();
      reject(new Error(`WebRTC connection ${pc.connectionState} before ICE candidate gathering completed`));
    };
    pc.addEventListener("icecandidate", onICECandidate);
    pc.addEventListener("icegatheringstatechange", onGatheringStateChange);
    pc.addEventListener("connectionstatechange", onConnectionStateChange);
    if (pc.iceGatheringState === "complete" || localDescriptionHasReflexiveICECandidate(pc)) {
      resolveIfUsable();
    } else if (localDescriptionHasUsableICECandidate(pc)) {
      hostCandidateGrace = window.setTimeout(resolveIfUsable, ICE_HOST_CANDIDATE_GRACE_MS);
    }
  });
}

function localDescriptionHasUsableICECandidate(pc: RTCPeerConnection): boolean {
  return pc.localDescription?.sdp.split("\n").some(isUsableICECandidate) ?? false;
}

function localDescriptionHasReflexiveICECandidate(pc: RTCPeerConnection): boolean {
  return pc.localDescription?.sdp.split("\n").some((candidate) => {
    return isUsableICECandidate(candidate) && iceCandidateType(candidate) === "srflx";
  }) ?? false;
}

function isUsableICECandidate(candidate: string): boolean {
  const fields = candidate.trim().split(/\s+/);
  return fields.length >= 8 && fields[2] === "udp" && isUsableIPv4(fields[4]);
}

function iceCandidateType(candidate: string): string | undefined {
  return candidate.trim().split(/\s+/)[7];
}

function isUsableIPv4(address: string | undefined): boolean {
  if (address === undefined || address.startsWith("169.254.")) return false;
  const octets = address.split(".");
  return octets.length === 4 && octets.every((octet) => /^\d{1,3}$/.test(octet) && Number(octet) <= 255);
}

function logVoiceRTCDiagnostics(
  diagnostics: VoiceRTCDiagnosticsResp,
  offerSDP: string,
  answerSDP: string,
): void {
  console.warn("Voice RTC diagnostics", {
    issue: diagnostics.issue,
    side: diagnostics.side,
    message: diagnostics.message,
    server: diagnostics.server,
    client: diagnostics.client,
    clientOfferCandidates: summarizeSDPCandidates(offerSDP),
    serverAnswerCandidates: summarizeSDPCandidates(answerSDP),
  });
}

export function summarizeSDPCandidates(sdp: string): string {
  const candidates: string[] = [];
  for (const line of sdp.split("\n")) {
    const body = line.trim();
    if (!body.startsWith("a=candidate:")) continue;
    const fields = body.split(/\s+/);
    if (fields.length < 8) continue;
    candidates.push(`${fields[4]}:${fields[5]} ${fields[7]}`);
  }
  return candidates.length === 0 ? "none" : candidates.join(", ");
}

export function formatVoiceRTCDiagnostics(
  diagnostics: VoiceRTCDiagnosticsResp,
): string {
  const side = diagnostics.side === "none" ? "unknown" : diagnostics.side;
  const mappingError = diagnostics.server.udpMappingError
    ? ` UDP mapping: ${diagnostics.server.udpMappingError}`
    : "";
  return `Voice connection failed (${side}: ${diagnostics.issue}) — ${diagnostics.message}${mappingError}`;
}

function gatewayContextUpdate(text: string): ContextUpdate {
  return {
    kind: MessageKindContextUpdate,
    context: { text },
  };
}

function gatewayUserMessage(text: string): UserMessage {
  return {
    kind: MessageKindUserMessage,
    text,
  };
}

function gatewaySessionSetup(
  tools: SessionSetup["tools"],
  systemInstruction: string,
): SessionSetup {
  return {
    kind: MessageKindSessionSetup,
    voice: {
      name: "Orus",
      language: "en",
    },
    tools,
    context: {
      systemInstruction,
    },
  };
}

function gatewayToolResult(
  id: string,
  name: string,
  result: Record<string, unknown>,
): ToolResult {
  return {
    kind: MessageKindToolResult,
    id,
    name,
    result,
  };
}

// Transcript helpers

function appendChunk(
  transcript: TranscriptEntry[],
  speaker: TranscriptSpeaker,
  text: string,
): TranscriptEntry[] {
  const last = transcript[transcript.length - 1];
  if (last && last.speaker === speaker && !last.final) {
    return [
      ...transcript.slice(0, -1),
      { speaker, text: last.text + text, final: false },
    ];
  }
  return [...transcript, { speaker, text, final: false }];
}

// Singleton — lives at module level so the WebRTC connection survives component remounts.
export const voiceSession = new VoiceSession();

// Disconnect on actual page unload (browser close/refresh).
if (typeof window !== "undefined") {
  window.addEventListener("beforeunload", () => {
    voiceSession.disconnect();
  });
}
