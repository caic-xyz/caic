// Benchmarks shared task-diff cache request coalescing and account cleanup.

import { bench } from "@tests/bench";

import type { TaskDiffIndexResp } from "@sdk/types.gen";

import { DiffCache } from "./diffCache";

const index: TaskDiffIndexResp = { repositories: [] };

bench(
  "coalesces navigation and live-refresh request bursts",
  async () => {
    let loads = 0;
    const cache = new DiffCache({
      freshnessMs: 1_500,
      indexLimit: 64,
      patchLimit: 64,
      loadIndex: async () => {
        loads++;
        return index;
      },
      maxRetainedBytes: 16 << 20,
      maxPatchCharacters: 1_000_000,
      loadPatch: async () => ({ diff: "patch" }),
    });

    await Promise.all([cache.loadIndex("task-1"), cache.loadIndex("task-1")]);
    const unsubscribe = cache.subscribe("task-1", () => undefined);
    cache.invalidate("task-1");
    const refresh = cache.loadIndex("task-1");
    for (let event = 1; event < 1_000; event++) {
      cache.invalidate("task-1");
    }
    await refresh;
    await cache.loadIndex("task-1");
    unsubscribe();

    if (loads !== 3) {
      throw new Error(`expected 3 index loads, received ${loads}`);
    }
  },
  { time: 1_000, warmupTime: 200 },
);

bench(
  "clears account-scoped task diffs",
  async () => {
    const cache = new DiffCache({
      freshnessMs: 1_500,
      indexLimit: 64,
      patchLimit: 64,
      loadIndex: async () => index,
      maxRetainedBytes: 16 << 20,
      maxPatchCharacters: 1_000_000,
      loadPatch: async () => ({ diff: "patch" }),
    });
    await Promise.all(
      Array.from({ length: 32 }, (_, task) => {
        const taskId = `task-${task}`;
        return Promise.all([
          cache.loadIndex(taskId),
          cache.loadPatch({ taskId, repository: "0", commit: "", path: "file.go", originalPath: "" }),
        ]);
      }),
    );
    cache.clear();
    if (cache.snapshot("task-0").data !== null) throw new Error("account cache was not cleared");
  },
  { time: 1_000, warmupTime: 200 },
);

bench(
  "loads eighty distinct file patches within the payload budget",
  async () => {
    const cache = new DiffCache({
      freshnessMs: 1_500,
      indexLimit: 20,
      patchLimit: 100,
      maxPatchCharacters: 1_000_000,
      maxRetainedBytes: 16 << 20,
      loadIndex: async () => index,
      loadPatch: async () => ({ diff: "bounded-payload".repeat(320) }),
    });
    await cache.loadIndex("task");
    for (let file = 0; file < 80; file++) {
      await cache.loadPatch({
        taskId: "task",
        repository: "0",
        commit: "immutable",
        path: `file-${file}`,
        originalPath: "",
      });
    }
  },
  { time: 1_000, warmupTime: 200 },
);
