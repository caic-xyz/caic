// Incrementally folds canonical native activity across turns, compaction, and replaced
// history. A terminal observation's result supersedes an earlier interim one.

import type { EventMessage, EventNativeSubagent } from "@sdk/types.gen";

export interface NativeActivity extends EventNativeSubagent {
  startedAt: number | null;
  endedAt: number | null;
}

export function nativeTerminal(status: EventNativeSubagent["status"]): boolean {
  return status === "completed" || status === "failed" || status === "interrupted";
}

// Missing terminal evidence is not success or interruption. A settled task only
// makes its last running observation stale; it never invents a child outcome.
export function nativeActivityStatus(activity: NativeActivity, settled: boolean): string {
  if (settled && activity.status === "running") return "Last observed running · outcome unknown";
  if (activity.status === "unknown") return "Status unknown";
  return activity.status[0].toUpperCase() + activity.status.slice(1);
}

export class NativeActivityTracker {
  private previous: EventMessage[] = [];
  private byID = new Map<string, NativeActivity>();
  private snapshot: NativeActivity[] = [];

  derive(messages: EventMessage[]): NativeActivity[] {
    const reset =
      messages.length < this.previous.length ||
      (this.previous.length > 0 &&
        (messages[0] !== this.previous[0] ||
          messages[this.previous.length - 1] !== this.previous[this.previous.length - 1]));
    let changed = reset;
    if (reset) {
      this.byID.clear();
      this.previous = [];
    }
    for (let i = this.previous.length; i < messages.length; i++) {
      const ev = messages[i];
      const s = ev.nativeSubagent;
      if (ev.kind !== "nativeSubagent" || !s?.id) continue;
      const old = this.byID.get(s.id);
      const status =
        old && (nativeTerminal(old.status) || s.status === "unknown") ? old.status : s.status;
      const next: NativeActivity = {
        ...s,
        toolUseID: old?.toolUseID || s.toolUseID,
        scope: old?.scope || s.scope,
        groupID: old?.groupID || s.groupID,
        label: old?.label || s.label,
        prompt: old?.prompt || s.prompt,
        // A terminal observation upgrades an interim result, but terminal
        // results do not overwrite each other.
        result:
          s.result && (!old?.result || (!nativeTerminal(old.status) && nativeTerminal(s.status)))
            ? s.result
            : old?.result || s.result,
        status,
        startedAt: old?.startedAt ?? (s.status === "running" && ev.ts > 0 ? ev.ts : null),
        endedAt: old?.endedAt ?? (nativeTerminal(s.status) && ev.ts > 0 ? ev.ts : null),
      };
      if (
        old &&
        Object.keys(next).every(
          (key) => next[key as keyof NativeActivity] === old[key as keyof NativeActivity],
        )
      )
        continue;
      this.byID.set(s.id, next);
      changed = true;
    }
    this.previous = messages.slice();
    if (changed) this.snapshot = [...this.byID.values()];
    return this.snapshot;
  }
}
