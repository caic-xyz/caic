// Publishes coherent history snapshots through owned bootstrap attempts, live handoff, and resource-bound disclosures.
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

type Bootstrap =
  | { kind: "discovering" }
  | { kind: "reading"; controller: AbortController }
  | { kind: "waiting" }
  | { kind: "failed" }
  | { kind: "live" };

interface HistoryAttempt {
  controller: AbortController;
  manifest: HistoryManifest | null;
  bootstrap: Bootstrap;
  pending: HistoryEvent[];
  lastLive: HistoryPosition | null;
  source: EventSource | null;
  publishTimer: ReturnType<typeof setTimeout> | null;
  retryTimer: ReturnType<typeof setTimeout> | null;
  watchdog: ReturnType<typeof setTimeout> | null;
  incomingStats: EventStats[] | null;
  observedThrough: number;
}

export function createTaskHistory(id: string): TaskHistory {
  const [messages, setMessages] = createSignal<EventMessage[]>([], { equals: false });
  const [manifest, setManifest] = createSignal<HistoryManifest | null>(null);
  const [stats, setStats] = createSignal<EventStats[]>([]);
  const [through, setThrough] = createSignal(0);
  const [epoch, setEpoch] = createSignal(0);
  const [loading, setLoading] = createSignal(true);
  const [error, setError] = createSignal<string | null>(null);
  let positions = new WeakMap<EventMessage, HistoryPosition>();
  let publishedReads = new AbortController();
  let attempt: HistoryAttempt | null = null;
  let disposed = false;
  let retryDelay = 500;

  const current = (owner: HistoryAttempt) => attempt === owner && !owner.controller.signal.aborted;
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
  function awaitingBoundary(owner: HistoryAttempt) {
    return owner.pending.some(
      (event) =>
        !isChildEvent(event.message) &&
        (event.message.init ||
          event.message.system?.subtype === "compact_boundary" ||
          event.message.system?.subtype === "context_cleared") &&
        event.recordID > (owner.manifest?.bootstrap.through ?? 0),
    );
  }
  function publish(owner: HistoryAttempt) {
    if (!current(owner) || owner.bootstrap.kind !== "live") return;
    // A boundary changes the owning section. Wait for its paired manifest so old
    // transcripts never briefly enter the current conversation DOM.
    if (awaitingBoundary(owner)) return;
    const added = owner.pending;
    owner.pending = [];
    if (!added.length) return;
    setMessages((previous) => {
      for (const event of added) {
        positions.set(event.message, { recordID: event.recordID, index: event.index });
        previous.push(event.message);
      }
      return previous;
    });
  }
  function reconcile(owner: HistoryAttempt, value: HistoryManifest) {
    const previous = owner.manifest;
    if (previous && value.bootstrap.generation !== previous.bootstrap.generation) {
      void discover();
      return;
    }
    if (previous && value.bootstrap.through < previous.bootstrap.through) return;
    const section = value.sections?.at(-1);
    const changed = section?.id !== previous?.sections?.at(-1)?.id;
    owner.manifest = value;
    owner.observedThrough = Math.max(owner.observedThrough, value.bootstrap.through);
    batch(() => {
      if (owner.bootstrap.kind !== "live") {
        if (
          changed ||
          owner.bootstrap.kind === "discovering" ||
          (owner.bootstrap.kind === "waiting" && !awaitingBoundary(owner))
        )
          void loadCurrent(owner, value);
        return;
      }
      setManifest(value);
      setThrough(owner.observedThrough);
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
        owner.pending = owner.pending.filter((event) => first && compareHistoryPosition(event, first) >= 0);
        positions = nextPositions;
        setEpoch((before) => before + 1);
        setMessages(retained);
      }
      publish(owner);
    });
  }
  async function loadCurrent(owner: HistoryAttempt, value: HistoryManifest) {
    if (owner.bootstrap.kind === "reading") owner.bootstrap.controller.abort();
    const reading: Bootstrap & { kind: "reading" } = { kind: "reading", controller: new AbortController() };
    owner.bootstrap = reading;
    const signal = AbortSignal.any([owner.controller.signal, reading.controller.signal]);
    const section = value.sections?.at(-1);
    try {
      const events = section ? await sectionBody(value, section, signal) : [];
      signal.throwIfAborted();
      if (!current(owner) || owner.bootstrap !== reading) return;
      if (awaitingBoundary(owner)) {
        owner.bootstrap = { kind: "waiting" };
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
      owner.pending = owner.pending.filter((event) => event.recordID > value.bootstrap.through);
      owner.bootstrap = { kind: "live" };
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
        setManifest(owner.manifest ?? value);
        setStats(owner.incomingStats ?? (sameScope ? untrack(stats) : []));
        setThrough(owner.observedThrough);
        setMessages(decoded);
        publish(owner);
        setLoading(false);
        setError(null);
      });
      retryDelay = 500;
    } catch (err) {
      if (signal.aborted || !current(owner)) return;
      owner.bootstrap = { kind: "failed" };
      fail(err);
      setLoading(false);
      scheduleRetry(owner);
    }
  }
  function scheduleRetry(owner: HistoryAttempt) {
    if (!current(owner) || !navigator.onLine) return;
    retire();
    owner.retryTimer = setTimeout(() => {
      owner.retryTimer = null;
      if (attempt === owner && !disposed) void discover();
    }, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 10_000);
  }
  function clearWatchdog(owner: HistoryAttempt) {
    if (owner.watchdog !== null) clearTimeout(owner.watchdog);
    owner.watchdog = null;
  }
  function alive(owner: HistoryAttempt) {
    if (!current(owner)) return;
    clearWatchdog(owner);
    // The server repeats the current checkpoint every 15 seconds. Three missed
    // frames replace a connection that silently stopped delivering data.
    owner.watchdog = setTimeout(() => {
      if (current(owner)) void discover();
    }, 45_000);
  }
  function retire() {
    const owner = attempt;
    if (!owner) return;
    clearWatchdog(owner);
    owner.controller.abort();
    owner.source?.close();
    owner.source = null;
    if (owner.publishTimer !== null) clearTimeout(owner.publishTimer);
    if (owner.retryTimer !== null) clearTimeout(owner.retryTimer);
    owner.publishTimer = owner.retryTimer = null;
    owner.pending = [];
  }
  async function discover() {
    if (disposed) return;
    retire();
    if (!navigator.onLine) {
      setLoading(false);
      return;
    }
    const owner: HistoryAttempt = {
      controller: new AbortController(),
      manifest: null,
      bootstrap: { kind: "discovering" },
      pending: [],
      lastLive: null,
      source: null,
      publishTimer: null,
      retryTimer: null,
      watchdog: null,
      incomingStats: null,
      observedThrough: 0,
    };
    attempt = owner;
    const signal = owner.controller.signal;
    batch(() => {
      setLoading(true);
      setError(null);
    });
    try {
      const value = await client(signal).getHistoryManifest(id);
      if (!current(owner)) return;
      owner.manifest = value;
      owner.lastLive = { recordID: value.bootstrap.through, index: Number.MAX_SAFE_INTEGER };
      owner.observedThrough = value.bootstrap.through;
      owner.source = api.historyEvents(id, value.bootstrap.cursor, {
        onMessage: (event) => {
          if (!current(owner) || (owner.lastLive && compareHistoryPosition(event, owner.lastLive) <= 0)) return;
          alive(owner);
          owner.lastLive = { recordID: event.recordID, index: event.index };
          owner.pending.push(event);
          if (event.message.ask || event.message.error || event.message.userInput) publish(owner);
          if (owner.publishTimer === null)
            owner.publishTimer = setTimeout(() => {
              owner.publishTimer = null;
              publish(owner);
            }, 100);
        },
        onManifest: (next) => {
          if (current(owner)) {
            alive(owner);
            reconcile(owner, next);
          }
        },
        onCheckpoint: (checkpoint) => {
          if (!current(owner)) return;
          alive(owner);
          owner.observedThrough = Math.max(owner.observedThrough, checkpoint.through);
          if (owner.bootstrap.kind === "live") setThrough(owner.observedThrough);
          if (owner.bootstrap.kind === "live") setError(null);
        },
        onStatsSnapshot: (snapshot) => {
          if (current(owner)) {
            alive(owner);
            owner.incomingStats = mergeStats([], snapshot.samples ?? []);
            if (owner.bootstrap.kind === "live") setStats(owner.incomingStats);
          }
        },
        onStats: (sample) => {
          if (current(owner)) {
            alive(owner);
            owner.incomingStats = mergeStats(owner.incomingStats ?? [], [sample]);
            if (owner.bootstrap.kind === "live") setStats(owner.incomingStats);
          }
        },
        onReset: () => {
          if (current(owner)) void discover();
        },
        onError: (err) => {
          if (current(owner)) {
            fail(err);
            scheduleRetry(owner);
          }
        },
        onHistoryError: (err) => {
          if (current(owner)) {
            fail(err.message);
            scheduleRetry(owner);
          }
        },
      });
      owner.source.onopen = () => {
        if (current(owner)) alive(owner);
      };
      owner.source.onerror = (event) => {
        if (current(owner) && !(event instanceof MessageEvent)) {
          // Keep the deadline while native reconnect waits for headers. A
          // terminal HTTP/auth failure must not become a rediscovery loop.
          if (owner.source?.readyState === owner.source?.CLOSED) clearWatchdog(owner);
          setError("Task history stream disconnected");
        }
      };
      alive(owner);
      // A synchronous stream manifest can already have admitted a newer body.
      if (owner.bootstrap.kind === "discovering") void loadCurrent(owner, owner.manifest);
    } catch (err) {
      if (!current(owner)) return;
      owner.bootstrap = { kind: "failed" };
      fail(err);
      setLoading(false);
      scheduleRetry(owner);
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
    disposed = true;
    publishedReads.abort();
    stopObserving();
    retire();
    attempt = null;
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
        `${value.bootstrap.resourceBase}/activity/${encodeURIComponent(activity.id)}/${end}${query}`,
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
