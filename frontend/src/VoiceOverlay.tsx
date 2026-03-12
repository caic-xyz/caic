// Voice overlay component: persistent bottom panel with mic button and voice controls.
import { createEffect, createSignal, For, Show, untrack, onCleanup } from "solid-js";
import type { Task } from "@sdk/types.gen";
import { VoiceSession } from "./VoiceSession";
import type { VoiceState, TranscriptEntry } from "./VoiceSession";
import styles from "./VoiceOverlay.module.css";
import MicIcon from "@material-symbols/svg-400/outlined/mic.svg?solid";
import MicOffIcon from "@material-symbols/svg-400/outlined/mic_off.svg?solid";
import CallEndIcon from "@material-symbols/svg-400/outlined/call_end.svg?solid";
import CloseIcon from "@material-symbols/svg-400/outlined/close.svg?solid";

interface Props {
  tasks: () => Task[];
  recentRepo: () => string;
  selectedHarness: () => string;
  selectedModel: () => string;
}

/** Bar transition durations (ms): center reacts fastest, outer bars lag. */
const BAR_DURATIONS = [80, 40, 120];
const BAR_MIN_H = 3;
const BAR_MAX_H = 20;

export default function VoiceOverlay(props: Props) {
  const session = new VoiceSession();

  // Track pre-purged task IDs to exclude from notifications.
  const [prePurgedIds, setPreTerminatedIds] = createSignal(new Set<string>());

  // Previous task states for detecting transitions.
  let prevStates = new Map<string, string>();

  // Detect connected→true transition and build snapshot.
  let wasConnected = false;
  createEffect(() => {
    const connected = session.state.connected;
    if (connected && !wasConnected) {
      const tasks = untrack(() => props.tasks());
      const prePurged = new Set(
        tasks
          .filter((t) => t.state === "purged" || t.state === "failed" || t.state === "stopped" || t.state === "stopping")
          .map((t) => t.id),
      );
      setPreTerminatedIds(prePurged);
      prevStates = new Map(tasks.map((t) => [t.id, t.state]));
    }
    wasConnected = connected;
  });

  // Track task changes and inject notifications while connected.
  createEffect(() => {
    const currentTasks = props.tasks();
    if (session.state.connected) {
      const excluded = prePurgedIds();
      session.taskNumberMap.update(currentTasks.filter((t) => !excluded.has(t.id)));
      for (const task of currentTasks) {
        const prev = prevStates.get(task.id);
        if (prev !== undefined && prev !== task.state) {
          const notification = buildNotification(task, session);
          if (notification !== null) session.injectText(notification);
        }
      }
    }
    prevStates = new Map(currentTasks.map((t) => [t.id, t.state]));
  });

  // Disconnect on component cleanup.
  onCleanup(() => session.disconnect());

  // -----------------------------------------------------------------------
  // Panel state
  // -----------------------------------------------------------------------

  const isActive = () =>
    session.state.connected ||
    session.state.connectStatus !== null ||
    session.state.error !== null;

  // -----------------------------------------------------------------------
  // Event handlers
  // -----------------------------------------------------------------------

  const handleMicClick = () => {
    if (session.state.connected || session.state.connectStatus !== null) {
      session.disconnect();
    } else {
      void session.connect(untrack(() => props.tasks()), untrack(() => props.recentRepo()), untrack(() => props.selectedHarness()), untrack(() => props.selectedModel()));
    }
  };

  // -----------------------------------------------------------------------
  // Render
  // -----------------------------------------------------------------------

  return (
    <div class={styles.panel} role="region" aria-label="Voice assistant">
      {/* Idle state: mic button right-aligned */}
      <Show when={!isActive()}>
        <div class={styles.rowEnd}>
          <button
            type="button"
            class={styles.micButton}
            onClick={() => handleMicClick()}
            title="Connect voice assistant"
            aria-label="Connect voice assistant"
          >
            <MicIcon width="1.1em" height="1.1em" />
          </button>
        </div>
      </Show>

      <Show when={session.state.error !== null && session.state.error} keyed>
        {(err) => (
          <ErrorPanel error={err} onRetry={() => handleMicClick()} />
        )}
      </Show>
      <Show
        when={session.state.error === null && session.state.connectStatus !== null && session.state.connectStatus}
        keyed
      >
        {(status) => <ConnectingPanel status={status} onDisconnect={() => session.disconnect()} />}
      </Show>
      <Show
        when={
          session.state.error === null &&
          session.state.connectStatus === null &&
          (session.state.connected || session.state.listening || session.state.speaking)
        }
      >
        <ActivePanel
          state={session.state}
          onDisconnect={() => session.disconnect()}
          onToggleMute={() => session.toggleMute()}
          onClearTranscript={() => session.clearTranscript()}
        />
      </Show>
    </div>
  );
}

// Sub-panels

function ConnectingPanel(props: { status: string; onDisconnect: () => void }) {
  return (
    <div class={`${styles.row} ${styles.statusConnecting}`}>
      <MicIcon width="1.1em" height="1.1em" />
      <span class={styles.statusText}>{props.status}</span>
      <button
        type="button"
        class={styles.iconButton}
        onClick={() => props.onDisconnect()}
        title="Cancel"
        aria-label="Cancel connection"
      >
        <CloseIcon width="1.1em" height="1.1em" />
      </button>
    </div>
  );
}

function ErrorPanel(props: { error: string; onRetry: () => void }) {
  return (
    <div class={styles.row}>
      <MicIcon width="1.1em" height="1.1em" style={{ color: "var(--color-danger)" }} />
      <span class={styles.statusError}>{props.error}</span>
      <button
        type="button"
        class={`${styles.actionButton} ${styles.actionButtonPrimary}`}
        onClick={() => props.onRetry()}
      >
        Retry
      </button>
    </div>
  );
}

function ActivePanel(props: {
  state: VoiceState;
  onDisconnect: () => void;
  onToggleMute: () => void;
  onClearTranscript: () => void;
}) {
  const statusText = () => {
    if (props.state.activeTool !== null) return props.state.activeTool;
    if (props.state.muted && !props.state.speaking) return "Muted";
    if (props.state.speaking) return "Speaking…";
    return "Listening…";
  };

  const statusClass = () =>
    props.state.activeTool !== null
      ? `${styles.statusText} ${styles.statusTool}`
      : styles.statusText;

  return (
    <>
      <div class={styles.rowSpaced}>
        <MicLevelBars micLevel={props.state.micLevel} />
        <span class={statusClass()}>{statusText()}</span>
        <button
          type="button"
          class={`${styles.iconButton}${props.state.muted ? " " + styles.iconButtonMuted : ""}`}
          onClick={() => props.onToggleMute()}
          title={props.state.muted ? "Unmute" : "Mute"}
          aria-label={props.state.muted ? "Unmute" : "Mute"}
        >
          <Show when={props.state.muted} fallback={<MicIcon width="1.1em" height="1.1em" />}>
            <MicOffIcon width="1.1em" height="1.1em" />
          </Show>
        </button>
        <button
          type="button"
          class={`${styles.iconButton} ${styles.iconButtonEnd}`}
          onClick={() => props.onDisconnect()}
          title="End voice session"
          aria-label="End voice session"
        >
          <CallEndIcon width="1.1em" height="1.1em" />
        </button>
      </div>
      <TranscriptLog
        transcript={props.state.transcript}
        onClear={() => props.onClearTranscript()}
      />
    </>
  );
}

// Mic level bars

function MicLevelBars(barProps: { micLevel: number }) {
  return (
    <div class={styles.micBars} aria-hidden="true">
      <For each={BAR_DURATIONS}>
        {(duration) => {
          const height = () => BAR_MIN_H + barProps.micLevel * (BAR_MAX_H - BAR_MIN_H);
          return (
            <div
              class={styles.micBar}
              style={{
                height: `${height()}px`,
                "transition-duration": `${duration}ms`,
              }}
            />
          );
        }}
      </For>
    </div>
  );
}

// Transcript log

function TranscriptLog(props: { transcript: TranscriptEntry[]; onClear: () => void }) {
  let listRef: HTMLDivElement | undefined;

  // Auto-scroll to bottom when transcript changes.
  createEffect(() => {
    const len = props.transcript.length;
    const last = props.transcript[len - 1]?.text;
    void len;
    void last;
    if (listRef) {
      listRef.scrollTop = listRef.scrollHeight;
    }
  });

  return (
    <>
      <Show when={props.transcript.length > 0}>
        <div class={styles.rowLabel}>
          <span class={styles.transcriptLabel}>Transcript</span>
          <button
            type="button"
            class={styles.clearButton}
            onClick={() => props.onClear()}
            title="Clear transcript"
            aria-label="Clear transcript"
          >
            ×
          </button>
        </div>
        <div class={styles.transcriptList} ref={(el) => { listRef = el; }}>
          <For each={props.transcript}>
            {(entry) => (
              <div class={styles.transcriptEntry}>
                <span
                  class={
                    entry.speaker === "user"
                      ? styles.transcriptLabelUser
                      : styles.transcriptLabelAssistant
                  }
                >
                  {entry.speaker === "user" ? "You:" : "Assistant:"}
                </span>
                {entry.text}
              </div>
            )}
          </For>
        </div>
      </Show>
      <Show when={props.transcript.length === 0}>
        <p class={styles.transcriptPlaceholder}>Transcript will appear here…</p>
      </Show>
    </>
  );
}

// Notification builder (mirrors VoiceViewModel.buildNotification)

function buildNotification(task: Task, session: VoiceSession): string | null {
  const num = session.taskNumberMap.toNumber(task.id);
  if (num === undefined) return null;
  const shortName = task.title || task.id;
  switch (task.state) {
    case "asking":
    case "waiting":
    case "has_plan":
      return `[Task #${num} (${shortName}) — ${task.state}]`;
    case "purged":
      return task.result ? `[Task #${num} (${shortName}) — completed: ${task.result}]` : null;
    case "stopped":
      return `[Task #${num} (${shortName}) — stopped: container died]`;
    case "failed":
      return `[Task #${num} (${shortName}) — failed: ${task.error ?? "unknown"}]`;
    default:
      return null;
  }
}
