// Core voice gateway session manager for the web frontend via WebRTC. Keep in sync with android/caic/src/main/java/com/fghbuild/caic/voice/VoiceSession.kt
import { createStore, produce } from "solid-js/store";
import { voiceRTCOffer } from "./api";
import type { Task } from "@sdk/types.gen";
import { TaskNumberMap } from "./TaskNumberMap";
import { mcpListTools, mcpCallTool, type McpToolDescriptor } from "./McpClient";
import { formatElapsed, formatCost } from "./formatting";
import {
  type Error,
  type ContextUpdate,
  type MessageEnvelope,
  type SessionSetup,
  type ToolResult,
  type ToolCall,
  type TranscriptDelta,
  MessageKindContextUpdate,
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

// Constants

/** Max time (ms) to wait for session.ready before timing out. */
const SETUP_TIMEOUT_MS = 15000;

const SYSTEM_INSTRUCTION =
  "You are a voice assistant for caic, a system for managing AI coding agents.\n\n" +
  "## What caic does\n" +
  "caic runs coding agents (Claude Code, Codex, etc) inside isolated containers " +
  "on a remote server. Each agent works autonomously on a git branch, writing " +
  "code, running tests, and committing changes. The user is a software engineer " +
  "who supervises multiple agents concurrently — often while away from the " +
  "screen — and controls them by voice.\n\n" +
  "## Task lifecycle\n" +
  "A task has a prompt (what to build), a repo, a branch, and a state:\n" +
  "- pending: task is queued, waiting to start\n" +
  "- branching: creating git branch\n" +
  "- provisioning: starting container\n" +
  "- starting: launching agent session\n" +
  "- running: agent is actively working\n" +
  "- waiting: agent completed a turn, awaiting user input\n" +
  "- asking: agent asked a question, needs the user to answer\n" +
  "- has_plan: agent produced a plan, awaiting approval\n" +
  "- pulling: pulling changes from container\n" +
  "- pushing: pushing changes to remote\n" +
  "- purging: cleanup in progress, container being deleted\n" +
  "- purged: container deleted; result contains the outcome\n" +
  "- failed: agent crashed or was aborted; error has the reason\n\n" +
  "## Context you have\n" +
  "At session start you receive a snapshot of all current tasks. Use it to " +
  "answer questions about task status without calling tasks_list first. Call " +
  "task_get_detail when the user asks for specifics (recent events, diffs).\n\n" +
  "## On connection\n" +
  'When the session starts, say exactly one word: "Ready". ' +
  "Do not say anything else — no greeting, no summary, no explanation. " +
  'After saying "Ready", stop and remain silent until the user speaks. ' +
  "Always speak fast.\n\n" +
  "## Behavior guidelines\n" +
  "- Do not ask follow-up questions like 'would you like me to…' " +
  "or 'should I also…'. Answer the user's request and stop. " +
  "Only ask a question if the user's request is genuinely ambiguous " +
  "or you misunderstood something critical — then ask the single " +
  "clarifying question needed and nothing else.\n" +
  "- Be concise. The user is often away from the screen.\n" +
  "- Summarize task status: state and what the agent is doing. " +
  "Only mention elapsed time or cost when the user specifically asks.\n" +
  "- When an agent is asking, read the question and options clearly, wait for " +
  "the verbal answer, then call task_answer_question.\n" +
  "- When creating a task, use the default repo, harness, and model from the " +
  "session context unless the user specifies otherwise. " +
  "Confirm repo and prompt before creating.\n" +
  "- Refer to tasks by its title.\n" +
  "- Proactively notify the user when tasks finish or need input.\n" +
  "- Free tools: agent_last_message, tasks_list, task_get_detail, get_usage. Call them whenever useful without asking.\n" +
  "- When the user asks for a status update, call agent_last_message for each waiting/asking task to get latest output.\n" +
  "- For safety issues during sync, describe each issue and ask whether to force.";

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
  private _audioContext: AudioContext | null = null;
  private _micStream: MediaStream | null = null;
  /** The <audio> element playing remote RTP audio, stored so we can call setSinkId(). */
  private _speakerAudio: HTMLAudioElement | null = null;
  /** True while the model is speaking — injected text is buffered and flushed after the turn ends. */
  private _speakerActive = false;
  /** Text notifications buffered while the model is speaking; flushed on turn end. */
  private _pendingNotifications: string[] = [];
  /** Snapshot to inject after session.ready. */
  private _pendingSnapshot: string | null = null;
  private _setupTimer: ReturnType<typeof setTimeout> | null = null;

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
    recentRepo: string,
    defaultHarness: string,
    defaultModel: string,
  ): Promise<void> {
    this._releaseAll();
    this._audioContext = new AudioContext({ sampleRate: 16000 });
    this._clearTranscript();
    this._setStatus("Setting up WebRTC…");

    try {
      const mcpTools = await mcpListTools();

      // Build task snapshot.
      const prePurged = new Set(
        tasks
          .filter(
            (t) =>
              t.state === "purged" ||
              t.state === "failed" ||
              t.state === "stopped" ||
              t.state === "stopping",
          )
          .map((t) => t.id),
      );
      this.excludedTaskIds = prePurged;
      const active = tasks
        .filter((t) => !prePurged.has(t.id))
        .sort((a, b) => {
          const lc = a.id.length - b.id.length;
          if (lc !== 0) return lc;
          return a.id > b.id ? 1 : a.id < b.id ? -1 : 0;
        });
      this.taskNumberMap.reset();
      this.taskNumberMap.update(active);
      this._pendingSnapshot = buildSnapshot(
        active,
        recentRepo,
        this.taskNumberMap,
        defaultHarness,
        defaultModel,
      );

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
        this._setStatus("Waiting for server…");
        this._sendSetup(mcpTools);
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
        if (st === "failed" || st === "disconnected" || st === "closed") {
          this._setError(`WebRTC ICE ${st}`);
        }
      };

      // Setup timeout.
      if (this._setupTimer !== null) clearTimeout(this._setupTimer);
      this._setupTimer = setTimeout(() => {
        if (this._pc === pc && !this.state.connected && !this.state.error) {
          this._setError("Connection timed out — server did not respond");
        }
      }, SETUP_TIMEOUT_MS);

      // SDP offer/answer exchange.
      this._setStatus("Signaling…");
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      const resp = await voiceRTCOffer({ sdp: offer.sdp ?? "" });
      this._rtcSessionID = resp.sessionID;
      await pc.setRemoteDescription({ type: "answer", sdp: resp.sdp });

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
    if (this._setupTimer !== null) {
      clearTimeout(this._setupTimer);
      this._setupTimer = null;
    }
    this._releaseAll();
    this._speakerAudio = null;
    this._pendingSnapshot = null;
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

  private _sendSetup(tools: McpToolDescriptor[]): void {
    const setup = gatewaySessionSetup(
      tools.map((d) => ({
        name: d.name,
        description: d.description,
        parameters: d.inputSchema,
      })),
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
      // Audio capture is handled via WebRTC RTP tracks; no separate audio setup needed.
      // Inject snapshot so the provider backend knows current task state.
      if (this._pendingSnapshot) {
        this.injectText(this._pendingSnapshot);
        this._pendingSnapshot = null;
      }
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

function gatewayContextUpdate(text: string): ContextUpdate {
  return {
    kind: MessageKindContextUpdate,
    context: { text },
  };
}

function gatewaySessionSetup(
  tools: SessionSetup["tools"],
): SessionSetup {
  return {
    kind: MessageKindSessionSetup,
    voice: {
      name: "Orus",
      language: "en",
    },
    tools,
    context: {
      systemInstruction: SYSTEM_INSTRUCTION,
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

// Snapshot builder (mirrors VoiceViewModel.buildSnapshot)

function buildSnapshot(
  tasks: Task[],
  recentRepo: string,
  map: TaskNumberMap,
  defaultHarness?: string,
  defaultModel?: string,
): string {
  const parts: string[] = [];
  if (recentRepo) parts.push(`[Default repo: ${recentRepo}]`);
  if (defaultHarness) parts.push(`[Default harness: ${defaultHarness}]`);
  if (defaultModel) parts.push(`[Default model: ${defaultModel}]`);
  if (tasks.length > 0) {
    const lines = tasks.map((t) => {
      const num = map.toNumber(t.id) ?? 0;
      const shortName = t.title || t.id;
      return `- Task #${num}: ${shortName} (${t.state}, ${formatElapsed(t.duration * 1000)}, ${formatCost(t.costUSD)}, ${t.harness})`;
    });
    parts.push(`[Current tasks at session start]\n${lines.join("\n")}`);
  } else if (parts.length === 0) {
    return "[No active tasks]";
  }
  return parts.join("\n");
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
