// Tests task numbering for the Go Mode voice session.

import { describe, expect, it } from "vitest";

import type { Task } from "@sdk/types.gen";

import { TaskNumberMap } from "./TaskNumberMap";

function task(id: string, state: Task["state"]): Task {
  return { id, state } as Task;
}

describe("TaskNumberMap", () => {
  it("numbers active tasks before inactive tasks on every update", () => {
    const map = new TaskNumberMap();
    const inactive = task("01", "stopped");
    const firstActive = task("02", "running");
    const secondActive = task("03", "waiting");

    map.update([inactive, firstActive, secondActive]);

    expect(map.toNumber(firstActive.id)).toBe(1);
    expect(map.toNumber(secondActive.id)).toBe(2);
    expect(map.toNumber(inactive.id)).toBe(3);

    map.update([inactive, { ...firstActive, state: "stopped" }, secondActive]);

    expect(map.toNumber(secondActive.id)).toBe(1);
    expect(map.toNumber(inactive.id)).toBe(2);
    expect(map.toNumber(firstActive.id)).toBe(3);
  });
});
