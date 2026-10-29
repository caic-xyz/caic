// Owns bounded cached backfill, canonical task messages, root-result pricing positions, resources, and an exact live tail.
import { batch, createSignal, onCleanup } from "solid-js";
import type {
  EventMessage,
  EventResult,
  EventStats,
  HistoryBootstrap,
  HistoryEvent,
  HistoryPosition,
} from "@sdk/types.gen";
import { createApiClient } from "@sdk/api.gen";
import { isChildEvent, isSessionBoundary } from "./grouping";
import { mergeStats } from "./taskStats";
import type { ResultEstimate } from "./taskAnalytics";
import type { TaskHistory } from "./taskEventTimeline";
import { api } from "./api";
import { RecentTextPreview, type PreviewText } from "./recentTextPreview";
import { readHistoryArray } from "./historyStream";
import { compareHistoryPosition, historyChunkFirst, historyPositionKey } from "./historyPosition";

const backfillChunks = 4;

export function createTaskHistory(id: string): Omit<TaskHistory, "retry"> {
  const [messages, setMessages] = createSignal<EventMessage[]>([]);
  const [preview, setPreview] = createSignal<PreviewText | null>(null);
  const [stats, setStats] = createSignal<EventStats[]>([]);
  const [epoch, setEpoch] = createSignal(0);
  const [loading, setLoading] = createSignal(true);
  const [error, setError] = createSignal<string | null>(null);
  let pending: EventMessage[] = [];
  let results = new Map<string, EventResult>();
  let discovery: HistoryBootstrap | null = null;
  let failure: "history" | "connection" | null = null;
  let source: EventSource | null = null;
  let controller = new AbortController();
  let active = true;
  let complete = false;
  let excerpt = new RecentTextPreview();
  let lastLive: HistoryPosition | null = null;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let retryDelay = 500;

  function current(signal: AbortSignal): boolean {
    return active && !signal.aborted && signal === controller.signal;
  }

  function client(signal: AbortSignal) {
    return createApiClient((url, init) =>
      fetch(url, {
        ...init,
        signal: init?.signal ? AbortSignal.any([signal, init.signal]) : signal,
      }),
    );
  }

  function fail(value: unknown) {
    failure = "history";
    setError(value instanceof Error ? value.message : String(value));
  }

  function publish(replace: boolean) {
    if (!complete) {
      setPreview(excerpt.value);
      return;
    }
    const added = pending;
    pending = [];
    excerpt.value = null;
    batch(() => {
      setPreview(null);
      if (replace) {
        setEpoch((previous) => previous + 1);
        setMessages(added);
      } else if (added.length) setMessages((previous) => [...previous, ...added]);
    });
  }

  async function response(url: string, signal: AbortSignal) {
    const result = await fetch(url, { signal, headers: { Accept: "application/json" } });
    signal.throwIfAborted();
    if (result.status === 409) {
      void discover();
      throw new Error("Task history changed; refreshing history");
    }
    if (!result.ok) throw new Error(`History request failed (${result.status})`);
    return result;
  }

  async function range(bootstrap: HistoryBootstrap, first: number, last: number, signal: AbortSignal) {
    const positions = results;
    const result = await response(`${bootstrap.resourceBase}/chunks/${first}/${last}`, signal);
    if (!result.body) throw new Error("Task history response has no body");
    const decoded: EventMessage[] = [];
    let previous: HistoryPosition | null = null;
    for await (const event of readHistoryArray(result.body, signal)) {
      if (event.recordID < first || event.recordID > last)
        throw new Error("History output is outside its requested range");
      if (previous && compareHistoryPosition(event, previous) <= 0) continue;
      previous = event;
      decoded.push(event.message);
      if (event.message.result && !isChildEvent(event.message))
        positions.set(historyPositionKey(event), event.message.result);
    }
    return decoded;
  }

  function scheduleRetry(signal: AbortSignal) {
    if (!current(signal)) return;
    if (retryTimer !== null) clearTimeout(retryTimer);
    retryTimer = setTimeout(() => {
      retryTimer = null;
      if (current(signal)) void discover();
    }, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 10_000);
  }

  function live(event: HistoryEvent, signal: AbortSignal) {
    if (lastLive && compareHistoryPosition(event, lastLive) <= 0) return;
    lastLive = { recordID: event.recordID, index: event.index };
    pending.push(event.message);
    if (event.message.result && !isChildEvent(event.message))
      results.set(historyPositionKey(event), event.message.result);
    if (!complete) excerpt.append(event.message);
    if (timer === null)
      timer = setTimeout(() => {
        timer = null;
        if (current(signal)) publish(false);
      }, 100);
    if (
      event.message.kind === "result" ||
      event.message.kind === "ask" ||
      event.message.kind === "userInput" ||
      event.message.kind === "error" ||
      isSessionBoundary(event.message)
    ) {
      clearTimeout(timer);
      timer = null;
      publish(false);
    }
  }

  async function discover() {
    controller.abort();
    controller = new AbortController();
    source?.close();
    source = null;
    if (timer !== null) clearTimeout(timer);
    timer = null;
    if (retryTimer !== null) clearTimeout(retryTimer);
    retryTimer = null;
    const signal = controller.signal;
    pending = [];
    results = new Map();
    discovery = null;
    failure = null;
    complete = false;
    excerpt = new RecentTextPreview();
    lastLive = null;
    batch(() => {
      setMessages([]);
      setPreview(null);
      setStats([]);
      setLoading(true);
      setError(null);
      setEpoch((previous) => previous + 1);
    });
    try {
      const bootstrap = await client(signal).getHistoryBootstrap(id);
      if (!current(signal)) return;
      discovery = bootstrap;
      lastLive = { recordID: bootstrap.through, index: Number.MAX_SAFE_INTEGER };
      source = api.historyEvents(id, bootstrap.cursor, {
        onMessage: (event) => {
          if (current(signal)) live(event, signal);
        },
        onStatsSnapshot: (value) => {
          if (current(signal)) setStats(mergeStats([], value.samples));
        },
        onCheckpoint: () => {
          if (current(signal) && failure === "connection") {
            failure = null;
            setError(null);
          }
        },
        onStats: (value) => {
          if (current(signal)) setStats((previous) => mergeStats(previous, [value]));
        },
        onReset: () => {
          if (current(signal)) void discover();
        },
        onError: (err) => {
          if (!current(signal)) return;
          source?.close();
          source = null;
          fail(err);
          scheduleRetry(signal);
        },
        onHistoryError: (err) => {
          if (!current(signal)) return;
          source?.close();
          source = null;
          fail(err.message);
          scheduleRetry(signal);
        },
      });
      source.onerror = (event) => {
        if (!current(signal) || event instanceof MessageEvent) return;
        if (failure !== "history") {
          failure = "connection";
          setError("Task history stream disconnected");
        }
      };
      if (bootstrap.through > 0) {
        const firstRecent = historyChunkFirst(bootstrap.through, bootstrap.recordsPerChunk);
        const tail = await range(bootstrap, firstRecent, bootstrap.through, signal);
        if (!current(signal)) return;
        // SSE is connected before the first immutable request. Its outputs are
        // strictly beyond the bootstrap cursor and remain visible during backfill.
        pending = [...tail, ...pending];
        excerpt = new RecentTextPreview();
        for (const message of pending) excerpt.append(message);
        if (firstRecent > 1) publish(false);
        // Fixed batches bound both active readers and completed chunks waiting
        // behind a slow earlier range. Canonical history publishes once, in order.
        const prefix: EventMessage[] = [];
        for (let first = 1; first < firstRecent; first += bootstrap.recordsPerChunk * backfillChunks) {
          const group = new AbortController();
          const reading = AbortSignal.any([signal, group.signal]);
          try {
            const requests: Promise<EventMessage[]>[] = [];
            for (let i = 0; i < backfillChunks; i++) {
              const start = first + i * bootstrap.recordsPerChunk;
              if (start >= firstRecent) break;
              const last = Math.min(start + bootstrap.recordsPerChunk - 1, firstRecent - 1);
              requests.push(range(bootstrap, start, last, reading));
            }
            const chunks = await Promise.all(requests);
            if (!current(signal)) return;
            for (const chunk of chunks) for (const message of chunk) prefix.push(message);
          } finally {
            group.abort();
          }
        }
        if (!current(signal)) return;
        pending = [...prefix, ...pending];
      }
      if (!current(signal)) return;
      complete = true;
      publish(true);
      retryDelay = 500;
      setLoading(false);
    } catch (err) {
      if (!current(signal)) return;
      fail(err);
      setLoading(false);
      scheduleRetry(signal);
    }
  }

  async function readCosts(request: AbortSignal): Promise<readonly ResultEstimate[]> {
    const owner = controller.signal;
    const signal = AbortSignal.any([owner, request]);
    signal.throwIfAborted();
    const bootstrap = discovery;
    const boundary = lastLive;
    if (!complete || !bootstrap || !boundary) throw new Error("Task history is still loading");
    // Reset replaces the index; live appends can add later outputs from the
    // same raw record while this request is pending, beyond its exact boundary.
    const positions = results;
    const estimates =
      boundary.recordID === 0
        ? []
        : await client(signal).getHistoryCosts(id, bootstrap.generation, String(boundary.recordID));
    signal.throwIfAborted();
    return estimates.flatMap((estimate) => {
      if (compareHistoryPosition(estimate, boundary) > 0) return [];
      const result = positions.get(historyPositionKey(estimate));
      return result ? [{ result, model: estimate.model ?? null, cost: estimate.cost ?? null }] : [];
    });
  }

  onCleanup(() => {
    active = false;
    controller.abort();
    source?.close();
    source = null;
    if (timer !== null) clearTimeout(timer);
    if (retryTimer !== null) clearTimeout(retryTimer);
    pending = [];
    results = new Map();
    discovery = null;
    excerpt.value = null;
    lastLive = null;
    batch(() => {
      setMessages([]);
      setPreview(null);
      setStats([]);
    });
  });
  void discover();
  return {
    messages,
    preview,
    stats,
    epoch,
    loading,
    error,
    readCosts,
  };
}
