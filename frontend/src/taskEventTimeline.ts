// SolidJS task event timeline primitive: owns SSE lifecycle, replay buffering, and live publication.

import { batch, createEffect, createMemo, createSignal, onCleanup, untrack, type Accessor } from "solid-js";

import type { EventMessage } from "@sdk/types.gen";

import { isSessionBoundary } from "./grouping";
import { api } from "./api";

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
  return (
    ev.kind === "result" || ev.kind === "ask" || ev.kind === "userInput" || ev.kind === "error" || isSessionBoundary(ev)
  );
}

export function createTaskEventTimeline(options: TaskEventTimelineOptions): TaskEventTimeline {
  const [canonicalMessages, setCanonicalMessages] = createSignal<EventMessage[]>([]);
  const [backwardMessages, setBackwardMessages] = createSignal<EventMessage[] | null>(null);
  const messages = createMemo(() => {
    const latest = backwardMessages();
    return latest === null ? canonicalMessages() : latest;
  });
  const [epoch, setEpoch] = createSignal(0);
  const terminal = createMemo(() => isTerminalTaskState(options.taskState()));
  let renderedTaskId = "";

  createEffect(() => {
    const id = options.taskId();
    const initialTaskIsTerminal = terminal();
    if (id !== renderedTaskId) {
      renderedTaskId = id;
      batch(() => {
        setCanonicalMessages([]);
        setBackwardMessages(null);
      });
    }

    let historySource: EventSource | null = null;
    let active = true;
    let historyFailed = false;
    let live = false;
    let backfillReady = false;
    let tailSource: EventSource | null = null;
    let tailLive = false;
    let tailReplace = false;
    let tailEvents: EventMessage[] = [];
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
          setCanonicalMessages(events);
          setBackwardMessages(null);
        });
        replaceOnNextFlush = false;
      } else if (events.length > 0) {
        setCanonicalMessages((previous) => [...previous, ...events]);
      }
    }

    function scheduleLiveFlush() {
      if (liveFlushTimer !== null) return;
      liveFlushTimer = setTimeout(() => {
        liveFlushTimer = null;
        if (tailSource !== null && backfillReady) flushTailEvents();
        else flushPendingEvents();
      }, liveFlushDelayMs);
    }

    function flushTailEvents() {
      if (tailEvents.length === 0 && !tailReplace) return;
      const events = tailEvents;
      tailEvents = [];
      if (tailReplace) {
        batch(() => {
          setEpoch((previous) => previous + 1);
          setCanonicalMessages(events);
          setBackwardMessages(null);
        });
        tailReplace = false;
      } else if (events.length > 0) {
        setCanonicalMessages((previous) => [...previous, ...events]);
      }
    }

    function connectTail(eventId: string) {
      if (tailSource !== null) return;
      tailSource = api.taskEventStreamFromLastEventID(id, eventId, {
        onMessage: (event) => {
          if (!active || historyFailed) return;
          tailEvents.push(event);
          if (!backfillReady) {
            setBackwardMessages((previous) => [...(previous ?? []), event]);
            return;
          }
          if (!tailLive) return;
          if (shouldFlushBufferedEvent(event)) flushTailEvents();
          else scheduleLiveFlush();
        },
        onError: (err) => {
          if (!active || historyFailed) return;
          const message = err instanceof Error ? err.message : String(err);
          untrack(() => options.onError(`Task event error: ${message}`));
        },
        onReady: () => {
          if (!active || historyFailed) return;
          tailLive = true;
          if (backfillReady) flushTailEvents();
        },
        onReset: () => {
          if (!active || historyFailed) return;
          historySource?.close();
          historySource = null;
          pendingEvents = [];
          tailEvents = [];
          tailLive = false;
          tailReplace = true;
          backfillReady = true;
          setBackwardMessages(null);
        },
        onHistoryError: failHistory,
      });
    }

    function failHistory(error: { message: string }) {
      if (!active || historyFailed) return;
      historyFailed = true;
      clearLiveFlushTimer();
      pendingEvents = [];
      tailEvents = [];
      live = false;
      tailLive = false;
      setBackwardMessages(null);
      historySource?.close();
      tailSource?.close();
      historySource = null;
      tailSource = null;
      untrack(() => options.onError(`Task history error: ${error.message}`));
    }

    const connected = api.taskEventBackfill(id, {
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
      onBackward: (event) => {
        if (!active || historyFailed || live || !replaceOnNextFlush) return;
        setBackwardMessages([event.message]);
        connectTail(event.eventId);
      },
      onReady: () => {
        if (!active || historyFailed) return;
        flushPendingEvents();
        live = true;
        backfillReady = true;
        if (tailSource !== null) {
          historySource?.close();
          historySource = null;
          flushTailEvents();
        }
      },
      onHistoryError: failHistory,
      onReset: () => {
        if (!active || historyFailed) return;
        clearLiveFlushTimer();
        pendingEvents = [];
        live = false;
        replaceOnNextFlush = true;
      },
    });
    historySource = connected;
    connected.onerror = () => {
      if (!active || historyFailed) return;
      const wasLive = live;
      if (wasLive) flushPendingEvents();
      live = false;
      if (!wasLive || !initialTaskIsTerminal) return;
      active = false;
      connected.close();
    };

    onCleanup(() => {
      active = false;
      clearLiveFlushTimer();
      pendingEvents = [];
      tailEvents = [];
      historySource?.close();
      tailSource?.close();
      historySource = null;
      tailSource = null;
    });
  });

  return { messages, epoch };
}
