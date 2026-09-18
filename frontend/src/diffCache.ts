// Shared bounded task-diff cache: deduplicates index and patch loads, supports stale refresh, and evicts deleted tasks.

import type { FileDiffResp, TaskDiffIndexResp } from "@sdk/types.gen";

import { getTaskDiffIndex, getTaskFileDiff } from "./api";

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
}

interface PatchEntry {
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
    this.pruneIndexes();
    return () => {
      entry.listeners.delete(listener);
      this.pruneIndexes();
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
        this.pruneIndexes();
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
      request: Promise.resolve(""),
      used: ++this.used,
      generation,
    };
    entry.request = this.options
      .loadPatch(selector)
      .then((response) => {
        if (
          response.diff.length > this.options.maxPatchCharacters &&
          this.patches.get(key) === entry
        ) {
          this.patches.delete(key);
        }
        return response.diff;
      })
      .catch((error: unknown) => {
        if (this.patches.get(key) === entry) this.patches.delete(key);
        throw error;
      });
    this.patches.set(key, entry);
    this.prunePatches();
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

  private pruneIndexes(): void {
    while (this.indexes.size > this.options.indexLimit) {
      const candidate = [...this.indexes.entries()]
        .filter(([, entry]) => entry.listeners.size === 0 && !entry.request)
        .sort((a, b) => a[1].used - b[1].used)[0];
      if (!candidate) return;
      this.indexes.delete(candidate[0]);
    }
  }

  private prunePatches(): void {
    while (this.patches.size > this.options.patchLimit) {
      const candidate = [...this.patches.entries()].sort((a, b) => a[1].used - b[1].used)[0];
      if (!candidate) return;
      this.patches.delete(candidate[0]);
    }
  }

  private dropWorkingPatches(taskId: string): void {
    for (const [key, patch] of this.patches) {
      if (patch.selector.taskId === taskId && patch.selector.commit === "")
        this.patches.delete(key);
    }
  }
}

function patchKey(selector: FileDiffSelector): string {
  return [
    selector.taskId,
    selector.repository,
    selector.commit,
    selector.path,
    selector.originalPath,
  ].join("\0");
}

export const taskDiffCache = new DiffCache({
  freshnessMs: 1_500,
  indexLimit: 20,
  patchLimit: 100,
  loadIndex: getTaskDiffIndex,
  maxPatchCharacters: 1_000_000,
  loadPatch: (selector) =>
    getTaskFileDiff(
      selector.taskId,
      selector.repository,
      selector.commit,
      selector.path,
      selector.originalPath,
    ),
});

export const prefetchTaskDiff = (taskId: string) => taskDiffCache.loadIndex(taskId);
export const invalidateTaskDiff = (taskId: string) => taskDiffCache.invalidate(taskId);
export const evictTaskDiff = (taskId: string) => taskDiffCache.evictTask(taskId);
