// Folds the records of each push attempt into one entry anchored in the transcript, outside elidable turns.

import type { EventMessage, EventSyncActivityStatus } from "@sdk/types.gen";

export interface SyncEntry {
  id: string;
  // Destination branch and sync target, known once the start record is seen.
  branch: string | null;
  target: string | null;
  // Hook output in arrival order, including the server's truncation notice.
  output: string;
  // null while the attempt has no end record.
  status: EventSyncActivityStatus | null;
  error: string | null;
  durationMs: number | null;
  // Timestamp of the first record, in ms; 0 when no record carried one. Anchors
  // the entry like a native activity card: it stays where the push started.
  anchorTs: number;
}

// SyncActivityTracker folds appended sync records incrementally. Each changed
// entry is a new object, so unchanged entries keep their identity.
export class SyncActivityTracker {
  private processed = 0;
  private firstEvent: EventMessage | null = null;
  private lastEvent: EventMessage | null = null;
  private byID = new Map<string, SyncEntry>();
  private snapshot: SyncEntry[] = [];

  derive(messages: readonly EventMessage[]): SyncEntry[] {
    let changed = false;
    if (
      messages.length < this.processed ||
      (this.processed > 0 && (messages[0] !== this.firstEvent || messages[this.processed - 1] !== this.lastEvent))
    ) {
      this.byID.clear();
      this.processed = 0;
      changed = true;
    }
    for (let i = this.processed; i < messages.length; i++) {
      const event = messages[i];
      if (!event) throw new Error("Missing timeline event");
      const record = event.syncActivity;
      if (event.kind !== "syncActivity" || !record) continue;
      const old = this.byID.get(record.syncID);
      const base: SyncEntry = old ?? {
        id: record.syncID,
        branch: null,
        target: null,
        output: "",
        status: null,
        error: null,
        durationMs: null,
        anchorTs: event.ts,
      };
      let next: SyncEntry;
      switch (record.phase) {
        case "start":
          next = { ...base, branch: record.branch ?? null, target: record.target ?? null };
          break;
        case "output":
          next = { ...base, output: base.output + (record.output ?? "") };
          break;
        case "end":
          next = {
            ...base,
            status: record.status ?? "failed",
            error: record.error ?? null,
            durationMs: record.durationMS ?? null,
          };
          break;
      }
      this.byID.set(record.syncID, next);
      changed = true;
    }
    this.processed = messages.length;
    this.firstEvent = messages[0] ?? null;
    this.lastEvent = messages.at(-1) ?? null;
    if (changed) this.snapshot = [...this.byID.values()];
    return this.snapshot;
  }
}
