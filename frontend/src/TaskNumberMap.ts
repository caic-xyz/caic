// Bidirectional task ID ↔ stable 1-based number mapping for voice mode,
// parallel to android/voice/TaskNumberMap.kt.
import type { Task } from "@sdk/types.gen";

export class TaskNumberMap {
  private readonly idToNumber = new Map<string, number>();
  private readonly numberToId = new Map<number, string>();
  private nextNumber = 1;

  /** Sync with current task list. Existing tasks keep their number; new ones get the next (ordered by creation time via KSID). */
  update(tasks: Task[]): void {
    const currentIds = new Set(tasks.map((t) => t.id));
    for (const [id, num] of this.idToNumber) {
      if (!currentIds.has(id)) {
        this.idToNumber.delete(id);
        this.numberToId.delete(num);
      }
    }
    // Sort new tasks by ID ascending (KSID encodes creation time) so that
    // the oldest task gets the lowest number.
    const newTasks = tasks.filter((t) => !this.idToNumber.has(t.id));
    newTasks.sort((a, b) => {
      const lc = a.id.length - b.id.length;
      if (lc !== 0) return lc;
      return a.id > b.id ? 1 : a.id < b.id ? -1 : 0;
    });
    for (const task of newTasks) {
      this.idToNumber.set(task.id, this.nextNumber);
      this.numberToId.set(this.nextNumber, task.id);
      this.nextNumber++;
    }
  }

  reset(): void {
    this.idToNumber.clear();
    this.numberToId.clear();
    this.nextNumber = 1;
  }

  toId(number: number): string | undefined {
    return this.numberToId.get(number);
  }

  toNumber(id: string): number | undefined {
    return this.idToNumber.get(id);
  }
}
