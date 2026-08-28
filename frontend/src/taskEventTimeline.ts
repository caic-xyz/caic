// SolidJS task event timeline primitive: owns SSE lifecycle, replay buffering, and live publication.

import { batch, createEffect, createMemo, createSignal, onCleanup, untrack, type Accessor } from "solid-js";

import type { EventMessage } from "@sdk/types.gen";

import { taskEventStream } from "./api";
import { isSessionBoundary } from "./grouping";

const liveFlushDelayMs = 100;

interface TaskEventTimelineOptions {
  taskId: Accessor<string>;
  taskState: Accessor<string>;
  onError: (message: string) => void;
}

interface TaskEventTimeline {
  messages: Accessor<EventMessage[]>;
  epoch: Accessor<number>;
}

function isTerminalTaskState(state: string): boolean {
  return state === "purged" || state === "crashed" || state === "failed";
}

function shouldFlushBufferedEvent(ev: EventMessage): boolean {
  return ev.kind === "result"
    || ev.kind === "ask"
    || ev.kind === "userInput"
    || ev.kind === "error"
    || isSessionBoundary(ev);
}

export function createTaskEventTimeline(options: TaskEventTimelineOptions): TaskEventTimeline {
  const [messages, setMessages] = createSignal<EventMessage[]>([]);
  const [epoch, setEpoch] = createSignal(0);
  const terminal = createMemo(() => isTerminalTaskState(options.taskState()));
  let renderedTaskId = "";

  createEffect(() => {
    const id = options.taskId();
    const taskIsTerminal = terminal();
    if (id !== renderedTaskId) {
      renderedTaskId = id;
      setMessages([]);
    }

    let source: EventSource | null = null;
    let active = true;
    let historyFailed = false;
    let live = false;
    let replaceOnNextFlush = true;
    let liveFlushTimer: ReturnType<typeof setTimeout> | null = null;
    let pendingEvents: EventMessage[] = [];

    function clearLiveFlushTimer() {
      if (liveFlushTimer === null) return;
      clearTimeout(liveFlushTimer);
      liveFlushTimer = null;
    }

    function flushPendingEvents() {
      clearLiveFlushTimer();
      const events = pendingEvents;
      pendingEvents = [];
      if (replaceOnNextFlush) {
        batch(() => {
          setEpoch((previous) => previous + 1);
          setMessages(events);
        });
        replaceOnNextFlush = false;
      } else if (events.length > 0) {
        setMessages((previous) => [...previous, ...events]);
      }
    }

    function scheduleLiveFlush() {
      if (liveFlushTimer !== null) return;
      liveFlushTimer = setTimeout(flushPendingEvents, liveFlushDelayMs);
    }

    const connected = taskEventStream(id, {
      onMessage: (event) => {
        if (!active || historyFailed) return;
        pendingEvents.push(event);
        if (!live) return;
        if (shouldFlushBufferedEvent(event)) flushPendingEvents();
        else scheduleLiveFlush();
      },
      onError: (err) => {
        if (!active || historyFailed) return;
        const message = err instanceof Error ? err.message : String(err);
        untrack(() => options.onError(`Task event error: ${message}`));
      },
      onReady: () => {
        if (!active || historyFailed) return;
        flushPendingEvents();
        live = true;
      },
      onHistoryError: (error) => {
        if (!active || historyFailed) return;
        historyFailed = true;
        clearLiveFlushTimer();
        pendingEvents = [];
        live = false;
        source?.close();
        source = null;
        untrack(() => options.onError(`Task history error: ${error.message}`));
      },
      onReset: () => {
        if (!active || historyFailed) return;
        clearLiveFlushTimer();
        pendingEvents = [];
        live = false;
        replaceOnNextFlush = true;
      },
    });
    source = connected;
    connected.onerror = () => {
      if (!active || historyFailed) return;
      const wasLive = live;
      if (wasLive) flushPendingEvents();
      live = false;
      if (!wasLive || !taskIsTerminal) return;
      active = false;
      connected.close();
      if (source === connected) source = null;
    };

    onCleanup(() => {
      active = false;
      clearLiveFlushTimer();
      pendingEvents = [];
      source?.close();
      source = null;
    });
  });

  return { messages, epoch };
}
