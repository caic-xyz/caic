// Shared task-diff cache with estimated reusable-payload budgets: deduplicates index and patch loads, supports stale refresh, and evicts deleted tasks.

import type { FileDiffResp, TaskDiffIndexResp } from "@sdk/types.gen";
import { api } from "./api";

export interface FileDiffSelector {
  taskId: string;
  repository: string;
  commit: string;
  path: string;
  originalPath: string;
}

export interface DiffIndexSnapshot {
  data: TaskDiffIndexResp | null;
  error: unknown | null;
  loading: boolean;
  stale: boolean;
  version: number;
}

interface IndexEntry extends DiffIndexSnapshot {
  evictionEpoch: number;
  generation: number;
  listeners: Set<() => void>;
  request: Promise<TaskDiffIndexResp> | null;
  used: number;
  validatedAt: number;
  payloadBytes: number;
}

interface PatchEntry {
  // Null until settlement; pending requests remain available for coalescing.
  payloadBytes: number | null;
  selector: FileDiffSelector;
  request: Promise<string>;
  used: number;
  generation: number;
}

interface DiffCacheOptions {
  indexLimit: number;
  patchLimit: number;
  freshnessMs: number;
  loadIndex: (taskId: string) => Promise<TaskDiffIndexResp>;
  loadPatch: (selector: FileDiffSelector) => Promise<FileDiffResp>;
  maxPatchCharacters: number;
  maxRetainedBytes: number;
}

export class DiffCache {
  private readonly indexes = new Map<string, IndexEntry>();
  private readonly patches = new Map<string, PatchEntry>();
  private used = 0;

  constructor(private readonly options: DiffCacheOptions) {}

  snapshot(taskId: string): DiffIndexSnapshot {
    const entry = this.indexes.get(taskId);
    if (!entry)
      return {
        data: null,
        error: null,
        loading: false,
        stale: true,
        version: 0,
      };
    this.touch(entry);
    return {
      data: entry.data,
      error: entry.error,
      loading: entry.loading,
      stale: entry.stale,
      version: entry.version,
    };
  }

  subscribe(taskId: string, listener: () => void): () => void {
    const entry = this.indexEntry(taskId);
    entry.listeners.add(listener);
    this.prune();
    return () => {
      entry.listeners.delete(listener);
      this.prune();
    };
  }

  loadIndex(taskId: string): Promise<TaskDiffIndexResp> {
    const entry = this.indexEntry(taskId);
    this.touch(entry);
    if (!entry.stale && entry.data) return Promise.resolve(entry.data);
    if (entry.request) return entry.request;

    entry.loading = true;
    entry.error = null;
    const generation = entry.generation;
    const evictionEpoch = entry.evictionEpoch;
    this.emit(entry);
    const request = this.options
      .loadIndex(taskId)
      .then((data) => {
        if (entry.evictionEpoch === evictionEpoch) {
          entry.data = data;
          entry.payloadBytes = estimatePayloadBytes(data);
          entry.error = null;
          entry.version++;
          this.dropWorkingPatches(taskId);
          if (entry.generation === generation) {
            entry.stale = false;
            entry.validatedAt = Date.now();
          }
        }
        return data;
      })
      .catch((error: unknown) => {
        if (entry.generation === generation) entry.error = error;
        throw error;
      })
      .finally(() => {
        if (entry.request !== request) return;
        entry.loading = false;
        entry.request = null;
        this.touch(entry);
        this.emit(entry);
        this.prune();
        if (entry.generation !== generation && entry.listeners.size) {
          void this.loadIndex(taskId).catch(() => undefined);
        }
      });
    entry.request = request;
    return request;
  }

  revalidateIndex(taskId: string): Promise<TaskDiffIndexResp> {
    const entry = this.indexes.get(taskId);
    if (entry?.request) return entry.request;
    if (entry?.data && !entry.stale && Date.now() - entry.validatedAt <= this.options.freshnessMs) {
      this.touch(entry);
      return Promise.resolve(entry.data);
    }
    this.invalidate(taskId);
    return this.loadIndex(taskId);
  }

  loadPatch(selector: FileDiffSelector): Promise<string> {
    const key = patchKey(selector);
    const cached = this.patches.get(key);
    const generation = this.indexes.get(selector.taskId)?.generation ?? 0;
    if (cached && (selector.commit !== "" || cached.generation === generation)) {
      this.touch(cached);
      return cached.request;
    }

    const entry: PatchEntry = {
      selector,
      payloadBytes: null,
      request: Promise.resolve(""),
      used: ++this.used,
      generation,
    };
    entry.request = this.options
      .loadPatch(selector)
      .then((response) => {
        if (this.patches.get(key) === entry) {
          entry.payloadBytes = response.diff.length * 2;
          if (response.diff.length > this.options.maxPatchCharacters) this.patches.delete(key);
          this.prune();
        }
        return response.diff;
      })
      .catch((error: unknown) => {
        if (this.patches.get(key) === entry) this.patches.delete(key);
        throw error;
      });
    this.patches.set(key, entry);
    this.prune();
    return entry.request;
  }

  invalidate(taskId: string): void {
    const entry = this.indexes.get(taskId);
    if (entry) {
      entry.generation++;
      entry.stale = true;
      this.emit(entry);
    }
    if (entry?.listeners.size) void this.loadIndex(taskId).catch(() => undefined);
  }

  evictTask(taskId: string): void {
    const entry = this.indexes.get(taskId);
    if (entry?.listeners.size) {
      entry.data = null;
      entry.payloadBytes = 0;
      entry.error = null;
      entry.evictionEpoch++;
      entry.generation++;
      entry.loading = false;
      entry.request = null;
      entry.stale = true;
      entry.validatedAt = 0;
      entry.version++;
      this.emit(entry);
    } else {
      this.indexes.delete(taskId);
    }
    for (const [key, patch] of this.patches) {
      if (patch.selector.taskId === taskId) this.patches.delete(key);
    }
  }

  /** Discard all account-scoped data and invalidate pending index responses. */
  clear(): void {
    for (const entry of this.indexes.values()) {
      entry.evictionEpoch++;
      entry.listeners.clear();
    }
    this.indexes.clear();
    this.patches.clear();
  }

  private indexEntry(taskId: string): IndexEntry {
    const cached = this.indexes.get(taskId);
    if (cached) return cached;
    const entry: IndexEntry = {
      data: null,
      error: null,
      evictionEpoch: 0,
      generation: 0,
      listeners: new Set(),
      loading: false,
      request: null,
      stale: true,
      used: ++this.used,
      validatedAt: 0,
      payloadBytes: 0,
      version: 0,
    };
    this.indexes.set(taskId, entry);
    return entry;
  }

  private emit(entry: IndexEntry): void {
    for (const listener of entry.listeners) listener();
  }

  private touch(entry: { used: number }): void {
    entry.used = ++this.used;
  }

  // Active indexes own the currently displayed metadata and remain pinned.
  // Their estimate participates in the budget, but an oversized active view
  // can exceed it. All reusable payloads then go, and unsubscribe releases an
  // oversized index. This is not a bound on live views or pending responses.
  private prune(): void {
    let retained = 0;
    for (const entry of this.indexes.values()) retained += entry.payloadBytes;
    for (const entry of this.patches.values()) retained += entry.payloadBytes ?? 0;
    if (
      retained <= this.options.maxRetainedBytes &&
      this.indexes.size <= this.options.indexLimit &&
      this.patches.size <= this.options.patchLimit
    )
      return;
    const indexes = [...this.indexes.entries()]
      .filter(([, entry]) => entry.listeners.size === 0 && !entry.request)
      .sort((a, b) => a[1].used - b[1].used);
    for (const [key, entry] of indexes) {
      if (entry.payloadBytes > this.options.maxRetainedBytes || this.indexes.size > this.options.indexLimit) {
        this.indexes.delete(key);
        retained -= entry.payloadBytes;
      }
    }
    const patches = [...this.patches.entries()]
      .filter(([, entry]) => entry.payloadBytes !== null)
      .sort((a, b) => a[1].used - b[1].used);
    for (const [key, entry] of patches) {
      if (this.patches.size <= this.options.patchLimit) break;
      this.patches.delete(key);
      retained -= entry.payloadBytes ?? 0;
    }
    const candidates = [
      ...indexes
        .filter(([key]) => this.indexes.has(key))
        .map(([key, entry]) => ({
          used: entry.used,
          bytes: entry.payloadBytes,
          remove: () => this.indexes.delete(key),
        })),
      ...patches
        .filter(([key]) => this.patches.has(key))
        .map(([key, entry]) => ({
          used: entry.used,
          bytes: entry.payloadBytes ?? 0,
          remove: () => this.patches.delete(key),
        })),
    ].sort((a, b) => a.used - b.used);
    for (const entry of candidates) {
      if (retained <= this.options.maxRetainedBytes) break;
      entry.remove();
      retained -= entry.bytes;
    }
  }

  private dropWorkingPatches(taskId: string): void {
    for (const [key, patch] of this.patches) {
      if (patch.selector.taskId === taskId && patch.selector.commit === "") this.patches.delete(key);
    }
  }
}

// estimatePayloadBytes counts UTF-16 string code units and scalar payloads,
// without serializing the index into a second complete string. It is a
// conservative string estimate, not measured heap: engine string storage,
// object overhead, and shared references vary.
function estimatePayloadBytes(value: unknown): number {
  if (typeof value === "string") return value.length * 2;
  if (typeof value === "number" || typeof value === "boolean") return 8;
  if (Array.isArray(value)) return value.reduce<number>((sum, item: unknown) => sum + estimatePayloadBytes(item), 0);
  if (value !== null && typeof value === "object") {
    let bytes = 0;
    for (const item of Object.values(value)) bytes += estimatePayloadBytes(item);
    return bytes;
  }
  return 0;
}

function patchKey(selector: FileDiffSelector): string {
  return [selector.taskId, selector.repository, selector.commit, selector.path, selector.originalPath].join("\0");
}

export const taskDiffCache = new DiffCache({
  freshnessMs: 1_500,
  indexLimit: 20,
  patchLimit: 100,
  // Late-bound api calls: tests spy on the api object, so the cache must read the
  // methods through it instead of capturing the function references at import time.
  loadIndex: (taskId) => api.getTaskDiffIndex(taskId),
  maxPatchCharacters: 1_000_000,
  maxRetainedBytes: 16 << 20,
  loadPatch: (selector) =>
    api.getTaskFileDiff(selector.taskId, selector.repository, selector.commit, selector.path, selector.originalPath),
});

export const prefetchTaskDiff = (taskId: string) => taskDiffCache.loadIndex(taskId);
export const invalidateTaskDiff = (taskId: string) => taskDiffCache.invalidate(taskId);
export const evictTaskDiff = (taskId: string) => taskDiffCache.evictTask(taskId);
