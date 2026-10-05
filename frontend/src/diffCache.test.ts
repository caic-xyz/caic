// Tests shared task-diff cache deduplication, stale refresh, selective invalidation, and bounded eviction.

import { afterEach, beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import type { FileDiffResp, TaskDiffIndexResp } from "@sdk/types.gen";

import { DiffCache, type FileDiffSelector } from "./diffCache";

const emptyIndex = (): TaskDiffIndexResp => ({ repositories: [] });
const selector = (taskId: string, commit: string): FileDiffSelector => ({
  taskId,
  repository: "0",
  commit,
  path: "file.go",
  originalPath: "",
});

describe("DiffCache", () => {
  const loadIndex = vi.fn<(taskId: string) => Promise<TaskDiffIndexResp>>();
  const loadPatch = vi.fn<(selector: FileDiffSelector) => Promise<FileDiffResp>>();
  let cache: DiffCache;

  beforeEach(() => {
    loadIndex.mockReset();
    loadPatch.mockReset();
    cache = new DiffCache({
      freshnessMs: 1_000,
      indexLimit: 2,
      loadIndex,
      loadPatch,
      maxRetainedBytes: 16 << 20,
      maxPatchCharacters: 1_000,
      patchLimit: 2,
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("drops cached data and ignores a prior account's late index response", async () => {
    const oldRequest = Promise.withResolvers<TaskDiffIndexResp>();
    const newRequest = Promise.withResolvers<TaskDiffIndexResp>();
    const oldIndex = emptyIndex();
    const newIndex = emptyIndex();
    loadIndex.mockReturnValueOnce(oldRequest.promise).mockReturnValueOnce(newRequest.promise);

    const stale = cache.loadIndex("same-task-id");
    cache.clear();
    expect(cache.snapshot("same-task-id").data).toBeNull();

    const current = cache.loadIndex("same-task-id");
    newRequest.resolve(newIndex);
    await current;
    oldRequest.resolve(oldIndex);
    await stale;
    expect(cache.snapshot("same-task-id").data).toBe(newIndex);
    expect(loadIndex).toHaveBeenCalledTimes(2);
  });

  it("deduplicates concurrent index requests and reuses prefetched data", async () => {
    let resolve: (data: TaskDiffIndexResp) => void = () => undefined;
    loadIndex.mockReturnValueOnce(
      new Promise((done) => {
        resolve = done;
      }),
    );

    const prefetched = cache.loadIndex("task-1");
    const navigated = cache.loadIndex("task-1");
    expect(loadIndex).toHaveBeenCalledTimes(1);

    resolve(emptyIndex());
    await Promise.all([prefetched, navigated]);
    await cache.revalidateIndex("task-1");
    expect(loadIndex).toHaveBeenCalledTimes(1);
  });

  it("revalidates cached navigation data after the intent freshness window", async () => {
    const now = vi.spyOn(Date, "now").mockReturnValue(1_000);
    loadIndex.mockResolvedValue(emptyIndex());
    await cache.loadIndex("task-1");
    now.mockReturnValue(2_001);

    await cache.revalidateIndex("task-1");
    expect(loadIndex).toHaveBeenCalledTimes(2);
  });

  it("keeps stale data visible and refreshes active subscribers after invalidation", async () => {
    const initial = emptyIndex();
    const refreshed: TaskDiffIndexResp = {
      repositories: [
        {
          name: "repo",
          branch: "main",
          ahead: 0,
          behind: 0,
          commits: [],
          uncommitted: [],
        },
      ],
    };
    let resolveRefresh: (data: TaskDiffIndexResp) => void = () => undefined;
    loadIndex.mockResolvedValueOnce(initial).mockReturnValueOnce(
      new Promise((done) => {
        resolveRefresh = done;
      }),
    );
    await cache.loadIndex("task-1");
    const listener = vi.fn();
    const unsubscribe = cache.subscribe("task-1", listener);

    cache.invalidate("task-1");
    expect(cache.snapshot("task-1")).toMatchObject({
      data: initial,
      stale: true,
    });
    expect(loadIndex).toHaveBeenCalledTimes(2);
    resolveRefresh(refreshed);
    await vi.waitFor(() =>
      expect(cache.snapshot("task-1")).toMatchObject({
        data: refreshed,
        stale: false,
      }),
    );
    expect(listener).toHaveBeenCalled();
    unsubscribe();
  });

  it("does not let an invalidated in-flight response become fresh", async () => {
    let resolveFirst: (data: TaskDiffIndexResp) => void = () => undefined;
    loadIndex
      .mockReturnValueOnce(
        new Promise((done) => {
          resolveFirst = done;
        }),
      )
      .mockResolvedValueOnce({ repositories: [] });
    const unsubscribe = cache.subscribe("task-1", () => undefined);
    const first = cache.loadIndex("task-1");
    cache.invalidate("task-1");
    resolveFirst(emptyIndex());
    await first;
    await vi.waitFor(() => expect(loadIndex).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(cache.snapshot("task-1").stale).toBe(false));
    unsubscribe();
  });

  it("surfaces a failed refresh without retrying continuously", async () => {
    loadIndex.mockRejectedValueOnce(new Error("unavailable"));
    const unsubscribe = cache.subscribe("task-1", () => undefined);

    await expect(cache.loadIndex("task-1")).rejects.toThrow("unavailable");
    expect(cache.snapshot("task-1")).toMatchObject({
      loading: false,
      stale: true,
    });
    expect(loadIndex).toHaveBeenCalledTimes(1);
    unsubscribe();
  });

  it("invalidates uncommitted patches but retains committed content", async () => {
    loadIndex.mockResolvedValue(emptyIndex());
    loadPatch.mockImplementation(async (value) => ({
      diff: value.commit || "working",
    }));
    await cache.loadIndex("task-1");
    await cache.loadPatch(selector("task-1", "abc123"));
    await cache.loadPatch(selector("task-1", ""));
    cache.invalidate("task-1");
    await cache.loadPatch(selector("task-1", "abc123"));
    await cache.loadPatch(selector("task-1", ""));

    expect(loadPatch).toHaveBeenCalledTimes(3);
  });

  it("bounds least-recently-used index and patch entries", async () => {
    loadIndex.mockResolvedValue(emptyIndex());
    loadPatch.mockResolvedValue({ diff: "patch" });
    await cache.loadIndex("task-1");
    await cache.loadIndex("task-2");
    cache.snapshot("task-1");
    await cache.loadIndex("task-3");
    await cache.loadIndex("task-2");
    expect(loadIndex).toHaveBeenCalledTimes(4);

    await cache.loadPatch(selector("task-1", "one"));
    await cache.loadPatch(selector("task-1", "two"));
    await cache.loadPatch(selector("task-1", "one"));
    await cache.loadPatch(selector("task-1", "three"));
    await cache.loadPatch(selector("task-1", "two"));
    expect(loadPatch).toHaveBeenCalledTimes(4);
  });

  it("evicts all retained data for a removed task", async () => {
    loadIndex.mockResolvedValue(emptyIndex());
    loadPatch.mockResolvedValue({ diff: "patch" });
    await cache.loadIndex("task-1");
    await cache.loadPatch(selector("task-1", "abc123"));
    cache.evictTask("task-1");
    await cache.loadIndex("task-1");
    await cache.loadPatch(selector("task-1", "abc123"));

    expect(loadIndex).toHaveBeenCalledTimes(2);
    expect(loadPatch).toHaveBeenCalledTimes(2);
  });

  it("rejects a pre-eviction response after a subscribed entry is reused", async () => {
    let resolveOld: (data: TaskDiffIndexResp) => void = () => undefined;
    loadIndex
      .mockReturnValueOnce(
        new Promise((resolve) => {
          resolveOld = resolve;
        }),
      )
      .mockRejectedValueOnce(new Error("replacement unavailable"));
    const unsubscribe = cache.subscribe("task-1", () => undefined);
    const oldRequest = cache.loadIndex("task-1");
    cache.evictTask("task-1");
    await expect(cache.loadIndex("task-1")).rejects.toThrow("replacement unavailable");

    resolveOld({
      repositories: [
        {
          name: "obsolete",
          branch: "main",
          ahead: 0,
          behind: 0,
          commits: [],
          uncommitted: [],
        },
      ],
    });
    await oldRequest;
    expect(cache.snapshot("task-1").data).toBeNull();
    unsubscribe();
  });

  it("budgets index and patch payloads together using shared LRU order", async () => {
    cache = new DiffCache({
      freshnessMs: 1_000,
      indexLimit: 10,
      patchLimit: 10,
      maxPatchCharacters: 1_000,
      maxRetainedBytes: 128,
      loadIndex,
      loadPatch,
    });
    loadIndex.mockResolvedValue({
      repositories: [
        { name: "index-metadata-".repeat(3), branch: "main", ahead: 0, behind: 0, commits: [], uncommitted: [] },
      ],
    });
    loadPatch.mockResolvedValue({ diff: "unique-patch-body-".repeat(3) });
    await cache.loadIndex("indexed");
    await cache.loadPatch(selector("patched", "one"));
    expect(cache.snapshot("indexed").data).toBeNull();
    await cache.loadPatch(selector("patched", "one"));
    expect(loadPatch).toHaveBeenCalledTimes(1);
  });

  it("keeps an oversized active index visible but releases it on unsubscribe", async () => {
    cache = new DiffCache({
      freshnessMs: 1_000,
      indexLimit: 10,
      patchLimit: 10,
      maxPatchCharacters: 1_000,
      maxRetainedBytes: 128,
      loadIndex,
      loadPatch,
    });
    const large: TaskDiffIndexResp = {
      repositories: [
        { name: "oversized-metadata-".repeat(20), branch: "main", ahead: 0, behind: 0, commits: [], uncommitted: [] },
      ],
    };
    loadIndex.mockResolvedValue(large);
    loadPatch.mockResolvedValue({ diff: "patch" });
    await cache.loadPatch(selector("reusable", "one"));
    const unsubscribe = cache.subscribe("active", () => undefined);
    await cache.loadIndex("active");
    expect(cache.snapshot("active").data).toBe(large);
    await cache.loadPatch(selector("reusable", "one"));
    expect(loadPatch).toHaveBeenCalledTimes(2);
    unsubscribe();
    expect(cache.snapshot("active").data).toBeNull();
    await cache.loadIndex("active");
    expect(cache.snapshot("active").data).toBeNull();
    expect(loadIndex).toHaveBeenCalledTimes(2);
  });

  it("releases an oversized index settling after its last subscriber leaves", async () => {
    cache = new DiffCache({
      freshnessMs: 1_000,
      indexLimit: 10,
      patchLimit: 10,
      maxPatchCharacters: 1_000,
      maxRetainedBytes: 128,
      loadIndex,
      loadPatch,
    });
    const pending = Promise.withResolvers<TaskDiffIndexResp>();
    loadIndex.mockReturnValueOnce(pending.promise);
    const unsubscribe = cache.subscribe("task", () => undefined);
    const load = cache.loadIndex("task");
    unsubscribe();
    expect(cache.loadIndex("task")).toBe(load);
    pending.resolve({
      repositories: [
        { name: "oversized-metadata-".repeat(20), branch: "main", ahead: 0, behind: 0, commits: [], uncommitted: [] },
      ],
    });
    await load;
    expect(cache.snapshot("task").data).toBeNull();
    expect(loadIndex).toHaveBeenCalledTimes(1);
  });

  it("coalesces pending requests even when their settled payload cannot be retained", async () => {
    const first = Promise.withResolvers<FileDiffResp>();
    loadPatch.mockReturnValueOnce(first.promise).mockResolvedValue({ diff: "x".repeat(1_001) });
    const one = cache.loadPatch(selector("task", "large"));
    const two = cache.loadPatch(selector("task", "large"));
    expect(one).toBe(two);
    first.resolve({ diff: "x".repeat(1_001) });
    await one;
    await cache.loadPatch(selector("task", "large"));
    expect(loadPatch).toHaveBeenCalledTimes(2);
  });

  it("does not repopulate cleared cache retention from a late patch", async () => {
    const old = Promise.withResolvers<FileDiffResp>();
    loadPatch.mockReturnValueOnce(old.promise).mockResolvedValue({ diff: "new-account" });
    const oldLoad = cache.loadPatch(selector("task", "commit"));
    cache.clear();
    await cache.loadPatch(selector("task", "commit"));
    old.resolve({ diff: "old-account" });
    await oldLoad;
    expect(await cache.loadPatch(selector("task", "commit"))).toBe("new-account");
    expect(loadPatch).toHaveBeenCalledTimes(2);
  });

  it("does not retain oversized patches", async () => {
    loadPatch.mockResolvedValue({ diff: "x".repeat(1_001) });
    await cache.loadPatch(selector("task-1", "large"));
    await cache.loadPatch(selector("task-1", "large"));
    expect(loadPatch).toHaveBeenCalledTimes(2);
  });
});
