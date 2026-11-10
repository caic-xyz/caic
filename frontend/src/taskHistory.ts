// Publishes coherent current-context snapshots while owning exact live handoff, disclosure reads, and recovery.
import { observeNetworkChanges } from "@maruel/gomode/web/NetworkChanges";
import { batch, createSignal, onCleanup, untrack } from "solid-js";
import type {
  EventMessage,
  EventStats,
  HistoryEvent,
  HistoryManifest,
  HistoryPosition,
  HistorySection,
} from "@sdk/types.gen";
import { createApiClient } from "@sdk/api.gen";
import { isChildEvent } from "./grouping";
import { mergeStats } from "./taskStats";
import type { TaskHistory } from "./taskEventTimeline";
import { api } from "./api";
import { readHistoryBatches } from "./historyStream";
import { compareHistoryPosition, historyChunkFirst } from "./historyPosition";

export function createTaskHistory(id: string): TaskHistory {
  const [messages, setMessages] = createSignal<EventMessage[]>([], { equals: false });
  const [manifest, setManifest] = createSignal<HistoryManifest | null>(null);
  const [stats, setStats] = createSignal<EventStats[]>([]);
  const [through, setThrough] = createSignal(0);
  const [epoch, setEpoch] = createSignal(0);
  const [loading, setLoading] = createSignal(true);
  const [error, setError] = createSignal<string | null>(null);
  let positions = new WeakMap<EventMessage, HistoryPosition>();
  let pending: HistoryEvent[] = [];
  let discovery: HistoryManifest | null = null;
  let currentID: string | null = null;
  let lastLive: HistoryPosition | null = null;
  let source: EventSource | null = null;
  let controller = new AbortController();
  let publishedReads = new AbortController();
  let bodyRequest: AbortController | null = null;
  let active = true;
  let complete = false;
  let waitingManifest = false;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let retryDelay = 500;
  let watchdog: ReturnType<typeof setTimeout> | null = null;
  let incomingStats: EventStats[] | null = null;
  let observedThrough = 0;

  const current = (signal: AbortSignal) => active && !signal.aborted && signal === controller.signal;
  const client = (signal: AbortSignal) => createApiClient((url, init) => fetch(url, { ...init, signal }));
  function fail(value: unknown) {
    setError(value instanceof Error ? value.message : String(value));
  }
  async function resource(url: string, signal: AbortSignal): Promise<HistoryEvent[]> {
    const response = await fetch(url, { signal, headers: { Accept: "application/json" } });
    signal.throwIfAborted();
    if (response.status === 409) {
      void discover();
      throw new Error("Task history changed; refreshing history");
    }
    if (!response.ok) throw new Error(`History request failed (${response.status})`);
    if (!response.body) throw new Error("Task history response has no body");
    const events: HistoryEvent[] = [];
    for await (const outputs of readHistoryBatches(response.body, signal))
      for (const event of outputs) events.push(event);
    return events;
  }
  async function sectionBody(value: HistoryManifest, section: HistorySection, signal: AbortSignal) {
    const events: HistoryEvent[] = [];
    if (!section.body) return events;
    const body = section.body;
    const size = value.bootstrap.recordsPerChunk;
    // Stable raw-record pages share browser cache entries as the current context grows.
    // Only the first and last pages use exact section output boundaries.
    let first = body.first.recordID;
    while (first <= body.last.recordID) {
      const group = new AbortController();
      const reading = AbortSignal.any([signal, group.signal]);
      try {
        const requests: Promise<HistoryEvent[]>[] = [];
        for (let count = 0; count < 4 && first <= body.last.recordID; count++) {
          const last = Math.min(historyChunkFirst(first, size) + size - 1, body.last.recordID);
          const firstIndex = first === body.first.recordID ? body.first.index : 0;
          const lastIndex = last === body.last.recordID ? String(body.last.index) : "end";
          requests.push(
            resource(`${value.bootstrap.resourceBase}/slice/${first}/${firstIndex}/${last}/${lastIndex}`, reading),
          );
          first = last + 1;
        }
        for (const page of await Promise.all(requests))
          for (const event of page) {
            if (compareHistoryPosition(event, body.first) < 0 || compareHistoryPosition(event, body.last) > 0)
              throw new Error("History output is outside its requested section");
            const previous = events.at(-1);
            if (previous && compareHistoryPosition(event, previous) <= 0)
              throw new Error("History outputs are out of order");
            events.push(event);
          }
      } finally {
        group.abort();
      }
    }
    return events;
  }
  function awaitingBoundary() {
    return pending.some(
      (event) =>
        !isChildEvent(event.message) &&
        (event.message.init ||
          event.message.system?.subtype === "compact_boundary" ||
          event.message.system?.subtype === "context_cleared") &&
        event.recordID > (discovery?.bootstrap.through ?? 0),
    );
  }
  function publish() {
    if (!complete || !discovery) return;
    // A boundary changes the owning section. Wait for its paired manifest so old
    // transcripts never briefly enter the current conversation DOM.
    if (awaitingBoundary()) return;
    const added = pending;
    pending = [];
    if (!added.length) return;
    setMessages((previous) => {
      for (const event of added) {
        positions.set(event.message, { recordID: event.recordID, index: event.index });
        previous.push(event.message);
      }
      return previous;
    });
  }
  function reconcile(value: HistoryManifest) {
    if (discovery && value.bootstrap.generation !== discovery.bootstrap.generation) {
      void discover();
      return;
    }
    if (discovery && value.bootstrap.through < discovery.bootstrap.through) return;
    const section = value.sections?.at(-1);
    const changed = section?.id !== currentID;
    discovery = value;
    observedThrough = Math.max(observedThrough, value.bootstrap.through);
    batch(() => {
      if (!complete) {
        if (changed || waitingManifest) {
          waitingManifest = false;
          void loadCurrent(value);
        }
        return;
      }
      setManifest(value);
      setThrough(observedThrough);
      if (changed) {
        const first = section?.body?.first;
        const retained: EventMessage[] = [];
        const nextPositions = new WeakMap<EventMessage, HistoryPosition>();
        for (const message of messages()) {
          const position = positions.get(message);
          if (first && position && compareHistoryPosition(position, first) >= 0) {
            retained.push(message);
            nextPositions.set(message, position);
          }
        }
        pending = pending.filter((event) => first && compareHistoryPosition(event, first) >= 0);
        positions = nextPositions;
        currentID = section?.id ?? null;
        setEpoch((revision) => revision + 1);
        setMessages(retained);
      }
      publish();
    });
  }
  async function loadCurrent(value: HistoryManifest) {
    bodyRequest?.abort();
    const request = new AbortController();
    bodyRequest = request;
    const signal = AbortSignal.any([controller.signal, request.signal]);
    const section = value.sections?.at(-1);
    currentID = section?.id ?? null;
    try {
      const events = section ? await sectionBody(value, section, signal) : [];
      signal.throwIfAborted();
      if (!active || bodyRequest !== request) return;
      if (awaitingBoundary()) {
        waitingManifest = true;
        return;
      }
      const previous = untrack(messages);
      const published = untrack(manifest);
      const sameScope = published?.bootstrap.resourceBase === value.bootstrap.resourceBase;
      const sameContext = sameScope && published?.sections?.at(-1)?.id === section?.id;
      const retained = new Map<string, EventMessage>();
      if (sameContext)
        for (const message of previous) {
          const position = positions.get(message);
          if (position) retained.set(`${position.recordID}:${position.index}`, message);
        }
      positions = new WeakMap();
      const decoded: EventMessage[] = [];
      for (const event of events) {
        const message = retained.get(`${event.recordID}:${event.index}`) ?? event.message;
        positions.set(message, { recordID: event.recordID, index: event.index });
        decoded.push(message);
      }
      pending = pending.filter((event) => event.recordID > value.bootstrap.through);
      complete = true;
      if (!sameScope) {
        publishedReads.abort();
        publishedReads = new AbortController();
      }
      batch(() => {
        if (
          !sameContext ||
          decoded.length < previous.length ||
          previous.some((message, index) => decoded[index] !== message)
        )
          setEpoch((revision) => revision + 1);
        setManifest(discovery ?? value);
        setStats(incomingStats ?? (sameScope ? untrack(stats) : []));
        setThrough(observedThrough);
        setMessages(decoded);
        publish();
        setLoading(false);
        setError(null);
      });
      retryDelay = 500;
    } catch (err) {
      if (signal.aborted || !active) return;
      fail(err);
      setLoading(false);
      scheduleRetry(controller.signal);
    }
  }
  function scheduleRetry(signal: AbortSignal) {
    if (!current(signal) || !navigator.onLine) return;
    retire();
    controller = new AbortController();
    const retrySignal = controller.signal;
    retryTimer = setTimeout(() => {
      retryTimer = null;
      if (current(retrySignal)) void discover();
    }, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 10_000);
  }
  function clearWatchdog() {
    if (watchdog !== null) clearTimeout(watchdog);
    watchdog = null;
  }
  function alive(signal: AbortSignal) {
    if (!current(signal)) return;
    clearWatchdog();
    // The server repeats the current checkpoint every 15 seconds. Three missed
    // frames replace a connection that silently stopped delivering data.
    watchdog = setTimeout(() => {
      if (current(signal)) void discover();
    }, 45_000);
  }
  function retire() {
    clearWatchdog();
    controller.abort();
    bodyRequest?.abort();
    source?.close();
    source = null;
    if (timer !== null) clearTimeout(timer);
    if (retryTimer !== null) clearTimeout(retryTimer);
    timer = retryTimer = null;
  }
  async function discover() {
    retire();
    if (!navigator.onLine) {
      setLoading(false);
      return;
    }
    controller = new AbortController();
    const signal = controller.signal;
    complete = false;
    waitingManifest = false;
    currentID = null;
    discovery = null;
    lastLive = null;
    pending = [];
    incomingStats = null;
    observedThrough = 0;
    batch(() => {
      setLoading(true);
      setError(null);
    });
    try {
      const value = await client(signal).getHistoryManifest(id);
      if (!current(signal)) return;
      discovery = value;
      currentID = value.sections?.at(-1)?.id ?? null;
      lastLive = { recordID: value.bootstrap.through, index: Number.MAX_SAFE_INTEGER };
      observedThrough = value.bootstrap.through;
      source = api.historyEvents(id, value.bootstrap.cursor, {
        onMessage: (event) => {
          if (!current(signal) || (lastLive && compareHistoryPosition(event, lastLive) <= 0)) return;
          alive(signal);
          lastLive = { recordID: event.recordID, index: event.index };
          pending.push(event);
          if (event.message.ask || event.message.error || event.message.userInput) publish();
          if (timer === null)
            timer = setTimeout(() => {
              timer = null;
              if (current(signal)) publish();
            }, 100);
        },
        onManifest: (next) => {
          if (current(signal)) {
            alive(signal);
            reconcile(next);
          }
        },
        onCheckpoint: (checkpoint) => {
          if (!current(signal)) return;
          alive(signal);
          observedThrough = Math.max(observedThrough, checkpoint.through);
          if (complete) setThrough(observedThrough);
          if (complete) setError(null);
        },
        onStatsSnapshot: (snapshot) => {
          if (current(signal)) {
            alive(signal);
            incomingStats = mergeStats([], snapshot.samples ?? []);
            if (complete) setStats(incomingStats);
          }
        },
        onStats: (sample) => {
          if (current(signal)) {
            alive(signal);
            incomingStats = mergeStats(incomingStats ?? [], [sample]);
            if (complete) setStats(incomingStats);
          }
        },
        onReset: () => {
          if (current(signal)) void discover();
        },
        onError: (err) => {
          if (current(signal)) {
            fail(err);
            scheduleRetry(signal);
          }
        },
        onHistoryError: (err) => {
          if (current(signal)) {
            fail(err.message);
            scheduleRetry(signal);
          }
        },
      });
      source.onopen = () => {
        if (current(signal)) alive(signal);
      };
      source.onerror = (event) => {
        if (current(signal) && !(event instanceof MessageEvent)) {
          // Native reconnect may hang before response headers. Its last-activity
          // deadline remains armed until data arrives; terminal HTTP/auth errors
          // close EventSource and must not be turned into rediscovery loops.
          if (source?.readyState === source?.CLOSED) clearWatchdog();
          setError("Task history stream disconnected");
        }
      };
      alive(signal);
      void loadCurrent(discovery ?? value);
    } catch (err) {
      if (!current(signal)) return;
      fail(err);
      setLoading(false);
      scheduleRetry(signal);
    }
  }
  function capture(signal: AbortSignal) {
    const value = untrack(manifest);
    if (!value) throw new Error("Task history is still loading");
    return { value, signal: AbortSignal.any([signal, publishedReads.signal]) };
  }
  const stopObserving = observeNetworkChanges(() => {
    if (navigator.onLine) void discover();
    else {
      retire();
      setLoading(false);
    }
  });
  onCleanup(() => {
    active = false;
    publishedReads.abort();
    stopObserving();
    retire();
    pending = [];
    discovery = null;
    positions = new WeakMap();
    batch(() => {
      setMessages([]);
      setManifest(null);
      setStats([]);
    });
  });
  void discover();
  return {
    messages,
    manifest,
    stats,
    through,
    epoch,
    loading,
    error,
    retry: () => {
      void discover();
    },
    position: (message) => positions.get(message) ?? null,
    readSection: (section, request) => {
      const { value, signal } = capture(request);
      return sectionBody(value, section, signal);
    },
    readSetup: (request) => {
      const { value, signal } = capture(request);
      const end = Math.max(value.metadata.setup?.last.recordID ?? 0, value.metadata.initialInit?.recordID ?? 0);
      return end ? resource(`${value.bootstrap.resourceBase}/setup/${end}`, signal) : Promise.resolve([]);
    },
    readActivity: (activity, end, after, request) => {
      const { value, signal } = capture(request);
      const query = after ? `?afterRecordID=${after.recordID}&afterIndex=${after.index}` : "";
      return resource(
        `${value.bootstrap.resourceBase}/activity/${activity.kind}/${encodeURIComponent(activity.id)}/${end}${query}`,
        signal,
      );
    },
    readCosts: (request) => {
      const { value, signal } = capture(request);
      return through() === 0
        ? Promise.resolve([])
        : client(signal)
            .getHistoryCosts(id, value.bootstrap.generation, String(through()))
            .then((costs) => costs ?? []);
    },
  };
}
