// Owns task history and resource samples across cached live logs and compressed disk replay.
import { batch, createMemo, createSignal, onCleanup, untrack, type Accessor } from "solid-js";
import type { EventMessage, EventStats } from "@sdk/types.gen";
import { createLiveTaskHistory } from "./liveTaskHistory";
import { RecentTextPreview, type PreviewText } from "./recentTextPreview";
import type { ResultEstimate } from "./taskAnalytics";
import { mergeStats } from "./taskStats";
import { isChildEvent, isSessionBoundary } from "./grouping";
import { api } from "./api";

export interface TaskHistory {
  messages: Accessor<EventMessage[]>;
  preview: Accessor<PreviewText | null>;
  stats: Accessor<EventStats[]>;
  // Replacements change epoch; messages within an epoch only append.
  epoch: Accessor<number>;
  loading: Accessor<boolean>;
  error: Accessor<string | null>;
  retry: () => void;
  // Prices refer to source-owned root result objects, never public raw positions.
  readCosts: (signal: AbortSignal) => Promise<readonly ResultEstimate[]>;
}

export function createTaskEventTimeline(options: {
  taskId: Accessor<string>;
  taskState: Accessor<string>;
}): TaskHistory {
  const [attempt, setAttempt] = createSignal(0);
  const disk = createMemo(() => ["purged", "crashed", "failed", "stopped"].includes(options.taskState()));
  const terminal = createMemo(() => ["purged", "crashed", "failed"].includes(options.taskState()));
  const source = createMemo(() => {
    attempt();
    const id = options.taskId();
    const fromDisk = disk();
    const completed = fromDisk && terminal();
    return untrack(() => (fromDisk ? createDiskHistory(id, completed) : createLiveTaskHistory(id)));
  });
  let revision = 0;
  const epoch = createMemo(() => {
    source().epoch();
    return ++revision;
  });
  return {
    messages: () => source().messages(),
    preview: () => source().preview(),
    stats: () => source().stats(),
    epoch,
    loading: () => source().loading(),
    error: () => source().error(),
    retry: () => setAttempt((previous) => previous + 1),
    readCosts: (signal) => source().readCosts(signal),
  };
}

function createDiskHistory(id: string, terminal: boolean): Omit<TaskHistory, "retry"> {
  const [stats, setStats] = createSignal<EventStats[]>([]);
  const owner = new AbortController();
  const [messages, setMessages] = createSignal<EventMessage[]>([]);
  const [preview, setPreview] = createSignal<PreviewText | null>(null);
  const [loading, setLoading] = createSignal(true);
  const [error, setError] = createSignal<string | null>(null);
  const [epoch, setEpoch] = createSignal(0);

  let historySource: EventSource | null = null;
  let active = true;
  let live = false;
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
    batch(() => {
      if (events.length > 0) setMessages((previous) => [...previous, ...events]);
      setPreview(null);
    });
  }

  function scheduleLiveFlush() {
    if (liveFlushTimer !== null) return;
    liveFlushTimer = setTimeout(() => {
      liveFlushTimer = null;
      flushPendingEvents();
    }, 100);
  }

  function failHistory(failure: { message: string }) {
    if (!active) return;
    active = false;
    clearLiveFlushTimer();
    pendingEvents = [];
    live = false;
    setPreview(null);
    setLoading(false);
    setError(failure.message);
    historySource?.close();
    historySource = null;
  }

  const connected = api.taskEvents(id, "1", {
    onStatsSnapshot: (value) => {
      if (active) setStats(mergeStats([], value.samples));
    },
    onStats: (value) => {
      if (active) setStats((previous) => mergeStats(previous, [value]));
    },
    onMessage: (event) => {
      if (!active) return;
      pendingEvents.push(event);
      if (!live) return;
      if (["result", "ask", "userInput", "error"].includes(event.kind) || isSessionBoundary(event))
        flushPendingEvents();
      else scheduleLiveFlush();
    },
    onError: (err) => {
      if (!active) return;
      failHistory({ message: err instanceof Error ? err.message : String(err) });
    },
    onBackward: (event) => {
      if (!active || live || messages().length > 0) return;
      const excerpt = new RecentTextPreview();
      for (const message of event.messages) excerpt.append(message);
      setPreview(excerpt.value);
    },
    onReady: () => {
      if (!active) return;
      flushPendingEvents();
      live = true;
      setLoading(false);
      setError(null);
    },
    onHistoryError: failHistory,
    onReset: () => {
      if (!active) return;
      clearLiveFlushTimer();
      pendingEvents = [];
      live = false;
      batch(() => {
        setMessages([]);
        setPreview(null);
        setLoading(true);
        setError(null);
        setEpoch((previous) => previous + 1);
      });
    },
  });
  historySource = connected;
  connected.onerror = (event) => {
    if (event instanceof MessageEvent) return;
    if (!active) return;
    const wasLive = live;
    if (wasLive) flushPendingEvents();
    live = false;
    if (wasLive && terminal) {
      active = false;
      connected.close();
      historySource = null;
    } else setError("Task history stream disconnected");
  };

  onCleanup(() => {
    active = false;
    owner.abort();
    clearLiveFlushTimer();
    pendingEvents = [];
    historySource?.close();
    historySource = null;
    setMessages([]);
    setPreview(null);
  });

  return {
    messages,
    preview,
    stats,
    epoch,
    loading,
    error,
    readCosts: async (request) => {
      AbortSignal.any([owner.signal, request]).throwIfAborted();
      return messages().flatMap((event) =>
        event.result && !isChildEvent(event)
          ? [{ result: event.result, model: event.result.usage.reportedModel || null, cost: event.result.cost ?? null }]
          : [],
      );
    },
  };
}
