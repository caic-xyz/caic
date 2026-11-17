// Tests that Performance links follow the task's path without reacting to task-list updates that keep it.

import { describe, it } from "node:test";
import { createComputed, createRoot, createSignal } from "solid-js";
import { expect } from "@tests/expect";

import type { Task } from "@sdk/types.gen";
import { createPerformanceLinks } from "./PerformancePage";

describe("createPerformanceLinks", () => {
  it("recomputes a turn link only when the task's path changes", () => {
    const base = { id: "abc", title: "Fix the build", repos: [{ name: "org/repo", branch: "main" }] } as Task;
    createRoot((dispose) => {
      const [task, setTask] = createSignal<Task | undefined>(base);
      const links = createPerformanceLinks(
        () => "abc",
        task,
        () => "?full=1",
      );
      const seen: string[] = [];
      createComputed(() => seen.push(links.turnHref(900)));
      expect(seen).toHaveLength(1);
      expect(seen[0]).toMatch(/^\/task\/@abc\+repo-main-fix-the-build\/perf\/turns\/900\?full=1$/);

      // A task-list update replaces the task object but keeps its path.
      setTask({ ...base, state: "running" } as Task);
      expect(seen).toHaveLength(1);

      setTask({ ...base, title: "Ship it" } as Task);
      expect(seen).toHaveLength(2);
      expect(seen[1]).toMatch(/ship-it\/perf\/turns\/900\?full=1$/);
      dispose();
    });
  });
});
