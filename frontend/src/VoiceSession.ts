// Core Gemini Live voice session manager for the web frontend. Keep in sync with android/app/src/main/java/com/fghbuild/caic/voice/VoiceSession.kt
import { createStore, produce } from "solid-js/store";
import { getVoiceToken, listHarnesses, listRepos } from "./api";
import type { Task } from "@sdk/types.gen";
import { FunctionHandlers } from "./FunctionHandlers";
import { TaskNumberMap } from "./TaskNumberMap";
import { buildFunctionDeclarations } from "./FunctionDeclarations";
import { formatElapsed, formatCost } from "./formatting";

// Constants

const MODEL_NAME = "models/gemini-2.5-flash-native-audio-preview-12-2025";
const PLAYBACK_SAMPLE_RATE = 24000;

/** Scheduling hint map derived from function declarations (same as Android). */
const FUNCTION_SCHEDULING = new Map<string, string>(
  buildFunctionDeclarations([]).flatMap((fd) =>
    fd.scheduling ? [[fd.name, fd.scheduling]] : [],
  ),
);

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
  "When the session starts, say exactly one word: \"Ready\". " +
  "Do not say anything else — no greeting, no summary, no explanation. " +
  "After saying \"Ready\", stop and remain silent until the user speaks. " +
  "Always speak fast.\n\n" +
  "## Behavior guidelines\n" +
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
  "- For safety issues during sync, describe each issue and ask whether to force.";

// AudioWorklet — inline blob to avoid Vite build complications

const WORKLET_CODE = `
class PCMCapture extends AudioWorkletProcessor {
  constructor() {
    super();
    this._buf = [];
    this._CHUNK = 1600; // ~100ms at 16 kHz
    this.port.onmessage = (e) => {
      if (e.data === "drain") this._buf = [];
    };
  }
  process(inputs) {
    const ch = inputs[0]?.[0];
    if (!ch) return true;
    for (let i = 0; i < ch.length; i++) this._buf.push(ch[i]);
    while (this._buf.length >= this._CHUNK) {
      const chunk = this._buf.splice(0, this._CHUNK);
      const int16 = new Int16Array(this._CHUNK);
      let sumSq = 0;
      for (let i = 0; i < this._CHUNK; i++) {
        const s = Math.max(-1, Math.min(1, chunk[i]));
        const v = Math.round(s < 0 ? s * 32768 : s * 32767);
        int16[i] = v;
        sumSq += v * v;
      }
      const rms = Math.sqrt(sumSq / this._CHUNK) / 32768;
      const micLevel = Math.min(1, Math.sqrt(rms));
      this.port.postMessage({ pcm: int16.buffer, micLevel }, [int16.buffer]);
    }
    return true;
  }
}
registerProcessor("pcm-capture", PCMCapture);
`;

// State types

export type TranscriptSpeaker = "user" | "assistant";

export interface TranscriptEntry {
  speaker: TranscriptSpeaker;
  text: string;
  final: boolean;
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
}

// VoiceSession

export class VoiceSession {
  readonly state: VoiceState;
  private readonly _setState: (fn: (s: VoiceState) => VoiceState) => void;

  readonly taskNumberMap = new TaskNumberMap();

  /** Task IDs excluded from AI context (pre-purged at session start). */
  excludedTaskIds: Set<string> = new Set();

  private _ws: WebSocket | null = null;
  private _audioContext: AudioContext | null = null;
  private _micStream: MediaStream | null = null;
  private _workletNode: AudioWorkletNode | null = null;
  private _nextPlayTime = 0;
  /** True while the model is speaking — mic audio is discarded to prevent echo. */
  private _speakerActive = false;
  /** Text notifications buffered while the model is speaking; flushed on turn end. */
  private _pendingNotifications: string[] = [];
  private _functions: FunctionHandlers | null = null;
  /** Snapshot to inject after setupComplete. */
  private _pendingSnapshot: string | null = null;

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
    });
    // eslint-disable-next-line solid/reactivity
    this.state = state;
    this._setState = setState as (fn: (s: VoiceState) => VoiceState) => void;
  }

  // -----------------------------------------------------------------------
  // Public API
  // -----------------------------------------------------------------------

  /** Start a new voice session with current task context. */
  async connect(tasks: Task[], recentRepo: string, defaultHarness = "", defaultModel = ""): Promise<void> {
    this._ws?.close(1000, "Reconnecting");
    this._ws = null;
    // Release audio before any await so we can recreate AudioContext while still
    // within the user gesture handler (Chrome requires gesture context for running state).
    this._releaseAudio();
    // Create AudioContext now — synchronously within the user gesture, before any await.
    // After an await, Chrome may start the context suspended and refuse to resume it.
    this._audioContext = new AudioContext({ sampleRate: 16000 });
    this._clearTranscript();
    this._setStatus("Fetching token…");

    try {
      const [tokenResp, harnesses, repos] = await Promise.all([
        getVoiceToken(),
        listHarnesses().catch(() => []),
        listRepos().catch(() => []),
      ]);

      const harnessNames = harnesses.map((h) => h.name);
      const repoPaths = repos.map((r) => r.path);

      // Build snapshot before resetting the map.
      const prePurged = new Set(
        tasks
          .filter((t) => t.state === "purged" || t.state === "failed" || t.state === "stopped" || t.state === "stopping")
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
      this._pendingSnapshot = buildSnapshot(active, recentRepo, this.taskNumberMap, defaultHarness, defaultModel);

      this._functions = new FunctionHandlers(
        this.taskNumberMap,
        () => this.excludedTaskIds,
        defaultHarness,
        defaultModel,
      );

      const wsUrl = tokenResp.ephemeral
        ? `wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContentConstrained?access_token=${encodeURIComponent(tokenResp.token)}`
        : `wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent?key=${encodeURIComponent(tokenResp.token)}`;

      this._setStatus("Connecting…");
      const ws = new WebSocket(wsUrl);
      this._ws = ws;

      ws.onopen = () => {
        if (ws !== this._ws) return;
        this._setStatus("Waiting for server…");
        this._sendSetup(harnessNames, repoPaths, defaultHarness);
      };

      // Set binaryType so binary frames arrive as ArrayBuffer, not opaque Blob.
      // Gemini may send protocol messages as binary WebSocket frames; without this,
      // JSON.parse(blob) silently fails and setupComplete is never processed.
      ws.binaryType = "arraybuffer";
      ws.onmessage = (e: MessageEvent<string | ArrayBuffer>) => {
        if (ws !== this._ws) return;
        const text =
          e.data instanceof ArrayBuffer
            ? new TextDecoder().decode(e.data)
            : (e.data as string);
        this._handleMessage(text).catch((err: unknown) => {
          this._setError(err instanceof Error ? err.message : "Message handling failed");
        });
      };

      ws.onerror = () => {
        if (ws !== this._ws) return;
        this._setError("WebSocket connection failed");
      };

      ws.onclose = (e) => {
        if (ws !== this._ws) return;
        if (!this.state.connected) {
          this._setError(formatCloseReason(e.code, e.reason));
        } else {
          this._update((s) => {
            s.connected = false;
            s.listening = false;
            s.speaking = false;
          });
        }
      };
    } catch (e: unknown) {
      this._setError(e instanceof Error ? e.message : "Connection failed");
    }
  }

  disconnect(): void {
    this._releaseAudio();
    this._ws?.close(1000, "User disconnected");
    this._ws = null;
    this._functions = null;
    this._pendingSnapshot = null;
    // Preserve transcript for review; mark all entries final.
    this._update((s) => {
      s.connected = false;
      s.listening = false;
      s.speaking = false;
      s.connectStatus = null;
      s.activeTool = null;
      s.micLevel = 0;
      s.transcript = s.transcript.map((e) => ({ ...e, final: true }));
    });
  }

  toggleMute(): void {
    this._update((s) => {
      s.muted = !s.muted;
      if (!s.muted) {
        // Drain worklet buffer so stale audio isn't sent.
        this._workletNode?.port.postMessage("drain");
      }
    });
  }

  injectText(text: string): void {
    if (this._speakerActive) {
      this._pendingNotifications.push(text);
      return;
    }
    if (!this._ws || this._ws.readyState !== WebSocket.OPEN) return;
    this._ws.send(
      JSON.stringify({
        clientContent: {
          turns: [{ role: "user", parts: [{ text }] }],
          turnComplete: true,
        },
      }),
    );
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

  private _setStatus(status: string): void {
    this._update((s) => {
      s.connectStatus = status;
      s.error = null;
    });
  }

  private _setError(message: string): void {
    this._releaseAudio();
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
  // WebSocket setup
  // -----------------------------------------------------------------------

  private _sendSetup(harnesses: string[], repos: string[], defaultHarness: string): void {
    if (!this._ws) return;
    const decls = buildFunctionDeclarations(harnesses, repos, defaultHarness || undefined);
    const setup = {
      setup: {
        model: MODEL_NAME,
        generationConfig: {
          responseModalities: ["AUDIO"],
          speechConfig: {
            voiceConfig: { prebuiltVoiceConfig: { voiceName: "ORUS" } },
          },
        },
        systemInstruction: { parts: [{ text: SYSTEM_INSTRUCTION }] },
        tools: [
          {
            functionDeclarations: decls.map((fd) => ({
              name: fd.name,
              description: fd.description,
              parameters: fd.parameters,
              ...(fd.behavior ? { behavior: fd.behavior } : {}),
            })),
          },
        ],
        realtimeInputConfig: {
          activityHandling: "START_OF_ACTIVITY_INTERRUPTS",
        },
        inputAudioTranscription: {},
        outputAudioTranscription: {},
      },
    };
    this._ws.send(JSON.stringify(setup));
  }

  // -----------------------------------------------------------------------
  // Message handling
  // -----------------------------------------------------------------------

  private async _handleMessage(text: string): Promise<void> {
    let msg: Record<string, unknown>;
    try {
      msg = JSON.parse(text) as Record<string, unknown>;
    } catch {
      return;
    }

    if ("setupComplete" in msg) {
      this._update((s) => {
        s.connectStatus = null;
        s.connected = true;
        s.error = null;
      });
      // Start audio capture.
      await this._startAudio();
      // Inject snapshot so Gemini knows current task state.
      if (this._pendingSnapshot) {
        this.injectText(this._pendingSnapshot);
        this._pendingSnapshot = null;
      }
      return;
    }

    if ("serverContent" in msg) {
      this._handleServerContent(msg["serverContent"] as ServerContent);
      return;
    }

    if ("toolCall" in msg) {
      const toolCall = msg["toolCall"] as ToolCall;
      await this._handleToolCall(toolCall);
      return;
    }

    if ("toolCallCancellation" in msg) {
      this._update((s) => {
        s.activeTool = null;
      });
      return;
    }

    // Surface Gemini error responses (e.g. auth failure, invalid model).
    const error = msg["error"] as { message?: string } | undefined;
    if (error?.message) {
      this._setError(error.message);
    }
  }

  private _handleServerContent(content: ServerContent): void {
    const parts = content.modelTurn?.parts ?? [];
    for (const part of parts) {
      if (part.inlineData?.data) {
        this._speakerActive = true;
        this._playPCM(part.inlineData.data);
        this._update((s) => {
          s.speaking = true;
          s.micLevel = 0;
        });
      }
    }

    if (content.inputTranscription?.text) {
      const chunk = content.inputTranscription.text;
      this._update((s) => {
        s.transcript = appendChunk(s.transcript, "user", chunk);
      });
    }

    if (content.outputTranscription?.text) {
      const chunk = content.outputTranscription.text;
      this._update((s) => {
        s.transcript = appendChunk(s.transcript, "assistant", chunk);
      });
    }

    if (content.interrupted) {
      this._speakerActive = false;
      this._workletNode?.port.postMessage("drain");
      this._flushPendingNotifications();
      this._update((s) => {
        s.speaking = false;
      });
    }

    if (content.turnComplete) {
      this._speakerActive = false;
      this._workletNode?.port.postMessage("drain");
      this._flushPendingNotifications();
      this._update((s) => {
        s.speaking = false;
        s.transcript = s.transcript.map((e) => ({ ...e, final: true }));
      });
    }
  }

  private async _handleToolCall(toolCall: ToolCall): Promise<void> {
    const fns = this._functions;
    if (!fns) return;

    const responses: FunctionResponse[] = [];
    for (const fc of toolCall.functionCalls ?? []) {
      try {
        const scheduling = FUNCTION_SCHEDULING.get(fc.name);
        this._update((s) => {
          s.activeTool = fc.name;
        });
        const result = await fns.handle(fc.name, fc.args ?? {});
        this._update((s) => {
          s.activeTool = null;
        });

        // Surface tool errors in the transcript.
        if (typeof result["error"] === "string") {
          const errMsg = result["error"];
          this._update((s) => {
            s.transcript = [
              ...s.transcript,
              { speaker: "assistant" as TranscriptSpeaker, text: `[${fc.name}] ${errMsg}`, final: true },
            ];
          });
        }

        const response: Record<string, unknown> =
          scheduling !== undefined ? { ...result, scheduling } : result;
        responses.push({ id: fc.id, name: fc.name, response });
      } catch (e: unknown) {
        this._update((s) => {
          s.activeTool = null;
        });
        const errMsg = e instanceof Error ? e.message : "Unknown error";
        this._update((s) => {
          s.transcript = [
            ...s.transcript,
            { speaker: "assistant" as TranscriptSpeaker, text: `[${fc.name}] ${errMsg}`, final: true },
          ];
        });
        responses.push({ id: fc.id, name: fc.name, response: { error: errMsg } });
      }
    }

    if (this._ws?.readyState === WebSocket.OPEN) {
      this._ws.send(JSON.stringify({ toolResponse: { functionResponses: responses } }));
    }
  }

  // -----------------------------------------------------------------------
  // Audio capture
  // -----------------------------------------------------------------------

  private async _startAudio(): Promise<void> {
    try {
      // AudioContext was pre-created in connect() within the user gesture handler.
      // Recreating it here would be outside the gesture context and suspended in Chrome.
      if (!this._audioContext) {
        this._setError("AudioContext not initialized");
        return;
      }
      this._micStream = await navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: true },
      });

      const blobUrl = URL.createObjectURL(
        new Blob([WORKLET_CODE], { type: "application/javascript" }),
      );
      await this._audioContext.audioWorklet.addModule(blobUrl);
      URL.revokeObjectURL(blobUrl);

      const source = this._audioContext.createMediaStreamSource(this._micStream);
      this._workletNode = new AudioWorkletNode(this._audioContext, "pcm-capture");
      this._workletNode.port.onmessage = (e: MessageEvent<{ pcm: ArrayBuffer; micLevel: number }>) => {
        if (this.state.muted || this._speakerActive) return;
        this._sendAudio(new Int16Array(e.data.pcm));
        this._update((s) => {
          s.micLevel = e.data.micLevel;
        });
      };
      source.connect(this._workletNode);
      // Don't connect worklet to destination — we only want the port messages.

      this._update((s) => {
        s.listening = true;
      });
    } catch (e: unknown) {
      this._setError(e instanceof Error ? e.message : "Microphone setup failed");
    }
  }

  private _releaseAudio(): void {
    try {
      this._workletNode?.disconnect();
      this._workletNode = null;
    } catch {
      // ignore
    }
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
    this._nextPlayTime = 0;
    this._speakerActive = false;
    this._pendingNotifications = [];
  }

  // -----------------------------------------------------------------------
  // Audio send (capture → Gemini)
  // -----------------------------------------------------------------------

  private _sendAudio(int16: Int16Array): void {
    if (!this._ws || this._ws.readyState !== WebSocket.OPEN) return;
    const base64 = arrayBufferToBase64(int16.buffer as ArrayBuffer);
    this._ws.send(
      JSON.stringify({
        realtimeInput: {
          audio: { mimeType: "audio/pcm;rate=16000", data: base64 },
        },
      }),
    );
  }

  // -----------------------------------------------------------------------
  // Audio playback (Gemini → speaker)
  // -----------------------------------------------------------------------

  private _playPCM(base64: string): void {
    const ctx = this._audioContext;
    if (!ctx) return;

    const binaryStr = atob(base64);
    const bytes = new Uint8Array(binaryStr.length);
    for (let i = 0; i < binaryStr.length; i++) {
      bytes[i] = binaryStr.charCodeAt(i);
    }
    const int16 = new Int16Array(bytes.buffer);

    const float32 = new Float32Array(int16.length);
    for (let i = 0; i < int16.length; i++) {
      float32[i] = int16[i] / 32768;
    }

    const buffer = ctx.createBuffer(1, float32.length, PLAYBACK_SAMPLE_RATE);
    buffer.getChannelData(0).set(float32);

    const source = ctx.createBufferSource();
    source.buffer = buffer;
    source.connect(ctx.destination);

    const now = ctx.currentTime;
    const startAt = Math.max(now, this._nextPlayTime);
    source.start(startAt);
    this._nextPlayTime = startAt + buffer.duration;
  }
}

// Snapshot builder (mirrors VoiceViewModel.buildSnapshot)

function buildSnapshot(tasks: Task[], recentRepo: string, map: TaskNumberMap, defaultHarness?: string, defaultModel?: string): string {
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
    return [...transcript.slice(0, -1), { speaker, text: last.text + text, final: false }];
  }
  return [...transcript, { speaker, text, final: false }];
}

// Base64 helper

function arrayBufferToBase64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = "";
  for (let i = 0; i < bytes.byteLength; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary);
}

// WebSocket close reason formatting

function formatCloseReason(code: number, reason: string): string {
  if (reason.includes("unregistered callers")) {
    return "Voice auth failed — check that GEMINI_API_KEY is set on the server";
  }
  return reason || `Connection closed (code ${code})`;
}

// Protocol types (subset needed for deserialization)

interface ServerContent {
  modelTurn?: {
    parts?: Array<{
      inlineData?: { mimeType?: string; data: string };
    }>;
  };
  turnComplete?: boolean;
  interrupted?: boolean;
  inputTranscription?: { text?: string };
  outputTranscription?: { text?: string };
}

interface FunctionCall {
  id: string;
  name: string;
  args?: Record<string, unknown>;
}

interface FunctionResponse {
  id: string;
  name: string;
  response: Record<string, unknown>;
}

interface ToolCall {
  functionCalls?: FunctionCall[];
}
