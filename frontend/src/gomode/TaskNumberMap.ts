// Bidirectional task ID ↔ active-first 1-based number mapping for web voice mode.

import type { Task } from "@sdk/types.gen";

export class TaskNumberMap {
  private readonly idToNumber = new Map<string, number>();
  private readonly numberToId = new Map<number, string>();

  /** Sync with the current task list, giving active tasks sequential numbers before inactive tasks. */
  update(tasks: Task[]): void {
    const orderedTasks = [...tasks].sort((a, b) => {
      const aInactive = isInactive(a);
      const bInactive = isInactive(b);
      if (aInactive !== bInactive) return aInactive ? 1 : -1;

      // Sort by ID ascending (KSID encodes creation time) so that the oldest
      // task within each activity group gets the lowest number.
      const lc = a.id.length - b.id.length;
      if (lc !== 0) return lc;
      return a.id > b.id ? 1 : a.id < b.id ? -1 : 0;
    });
    this.idToNumber.clear();
    this.numberToId.clear();
    for (const [index, task] of orderedTasks.entries()) {
      const number = index + 1;
      this.idToNumber.set(task.id, number);
      this.numberToId.set(number, task.id);
    }
  }

  reset(): void {
    this.idToNumber.clear();
    this.numberToId.clear();
  }

  toId(number: number): string | undefined {
    return this.numberToId.get(number);
  }

  toNumber(id: string): number | undefined {
    return this.idToNumber.get(id);
  }
}

function isInactive(task: Task): boolean {
  return (
    task.state === "stopping" ||
    task.state === "stopped" ||
    task.state === "purging" ||
    task.state === "crashed" ||
    task.state === "failed" ||
    task.state === "purged"
  );
}
