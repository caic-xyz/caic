// Owns cached history, resource samples, source-associated prices, and an exact live tail.
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
import { isChildEvent } from "./grouping";
import { mergeStats } from "./taskStats";
import type { ResultEstimate } from "./taskAnalytics";
import type { TaskHistory } from "./taskEventTimeline";
import { api } from "./api";
import { RecentTextPreview, type PreviewText } from "./recentTextPreview";
import { readHistoryArray } from "./historyStream";
import { compareHistoryPosition, historyChunkFirst, historyPositionKey } from "./historyPosition";

export function createLiveTaskHistory(id: string): Omit<TaskHistory, "retry"> {
  const [messages, setMessages] = createSignal<EventMessage[]>([]);
  const [preview, setPreview] = createSignal<PreviewText | null>(null);
  const [stats, setStats] = createSignal<EventStats[]>([]);
  const [epoch, setEpoch] = createSignal(0);
  const [loading, setLoading] = createSignal(true);
  const [error, setError] = createSignal<string | null>(null);
  let events: HistoryEvent[] = [];
  let discovery: HistoryBootstrap | null = null;
  let failure: "history" | "connection" | null = null;
  let published = 0;
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
    const added = events.slice(replace ? 0 : published).map((event) => event.message);
    published = events.length;
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
    const result = await response(`${bootstrap.resourceBase}/chunks/${first}/${last}`, signal);
    if (!result.body) throw new Error("Task history response has no body");
    const decoded: HistoryEvent[] = [];
    for await (const event of readHistoryArray(result.body, signal)) {
      if (event.recordID < first || event.recordID > last)
        throw new Error("History output is outside its requested range");
      const previous = decoded.at(-1);
      if (!previous || compareHistoryPosition(event, previous) > 0) decoded.push(event);
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
    lastLive = event;
    events.push(event);
    if (!complete) excerpt.append(event.message);
    if (timer === null)
      timer = setTimeout(() => {
        timer = null;
        if (current(signal)) publish(false);
      }, 100);
    if (event.message.kind === "result" || event.message.kind === "ask" || event.message.kind === "userInput") {
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
    events = [];
    discovery = null;
    failure = null;
    published = 0;
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
        events = [...tail, ...events];
        excerpt = new RecentTextPreview();
        for (const event of events) excerpt.append(event.message);
        if (firstRecent > 1) publish(false);
        // Decode each immutable range once, then replace the preview atomically.
        // The active transcript needs the complete backlog; only one chunk's
        // transport/decoder buffers are added to that owned transcript at a time.
        const prefix: HistoryEvent[] = [];
        for (let first = 1; first < firstRecent; first += bootstrap.recordsPerChunk) {
          const last = Math.min(first + bootstrap.recordsPerChunk - 1, firstRecent - 1);
          const chunk = await range(bootstrap, first, last, signal);
          if (!current(signal)) return;
          for (const event of chunk) prefix.push(event);
        }
        if (!current(signal)) return;
        events = [...prefix, ...events];
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
    if (!complete || !bootstrap) throw new Error("Task history is still loading");
    const through = Math.max(bootstrap.through, lastLive?.recordID ?? 0);
    const results = new Map<string, EventResult>();
    for (const event of events)
      if (event.message.result && !isChildEvent(event.message))
        results.set(historyPositionKey(event), event.message.result);
    const estimates =
      through === 0 ? [] : await client(signal).getHistoryCosts(id, bootstrap.generation, String(through));
    signal.throwIfAborted();
    return estimates.flatMap((estimate) => {
      const result = results.get(historyPositionKey(estimate));
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
    events = [];
    discovery = null;
    excerpt = new RecentTextPreview();
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
