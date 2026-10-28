// Incrementally folds root-owned skill loads by context section across compaction and clear.

import type { EventMessage, EventSkillLoad } from "@sdk/types.gen";
import { isChildEvent } from "./grouping";

export interface ContextSkills {
  current: EventSkillLoad[];
  closed: ReadonlyMap<EventMessage, EventSkillLoad[]>;
}

export class ContextSkillsTracker {
  private taskId = "";
  private epoch = -1;
  private processed = 0;
  private loads: EventSkillLoad[] = [];
  private snapshot: ContextSkills = { current: [], closed: new Map() };

  clear(): void {
    this.processed = 0;
    this.loads = [];
    this.snapshot = { current: [], closed: new Map() };
  }

  derive(taskId: string, epoch: number, messages: EventMessage[]): ContextSkills {
    let changed = false;
    if (taskId !== this.taskId || epoch !== this.epoch || messages.length < this.processed) {
      this.taskId = taskId;
      this.epoch = epoch;
      this.clear();
    }
    let current = this.snapshot.current;
    let closed = this.snapshot.closed;
    for (let i = this.processed; i < messages.length; i++) {
      const ev = messages[i];
      if (!ev) throw new Error("Missing timeline event");
      if (isChildEvent(ev)) continue;
      if (
        ev.kind === "system" &&
        (ev.system?.subtype === "compact_boundary" || ev.system?.subtype === "context_cleared")
      ) {
        closed = new Map(closed).set(ev, current);
        this.loads = [];
        current = [];
        changed = true;
      } else if (ev.kind === "skillLoad" && ev.skillLoad) {
        const load = ev.skillLoad;
        if (this.loads.some((seen) => (load.path ? seen.path === load.path : !seen.path && seen.skill === load.skill)))
          continue;
        this.loads.push(load);
        // Preserve unresolved names: a unique path enriches them, but multiple
        // paths with the same name cannot establish which document was loaded.
        current = [];
        for (const entry of this.loads) {
          const paths = entry.path ? [] : this.loads.filter((known) => known.path && known.skill === entry.skill);
          const [path] = paths;
          const resolved = paths.length === 1 && path ? path : entry;
          if (
            !current.some((seen) =>
              resolved.path ? seen.path === resolved.path : !seen.path && seen.skill === resolved.skill,
            )
          )
            current.push(resolved);
        }
        changed = true;
      }
    }
    this.processed = messages.length;
    if (changed) this.snapshot = { current, closed };
    return this.snapshot;
  }
}
