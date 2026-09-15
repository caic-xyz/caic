// Benchmarks shared task-diff cache request coalescing across navigation and live refresh bursts.

import { bench, describe } from "vitest";

import type { TaskDiffIndexResp } from "@sdk/types.gen";

import { DiffCache } from "./diffCache";

const index: TaskDiffIndexResp = { repositories: [] };

describe("diff cache request scheduling", () => {
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
});
