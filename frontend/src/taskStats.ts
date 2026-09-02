// Task analytics derivation for tool timing and cold-input attribution.

import type { EventMessage } from "@sdk/types.gen";

import { toolResultDurationMs } from "./grouping";

export interface ToolTimingSummary {
  name: string;
  calls: number;
  durationMs: number;
}

interface ToolStart {
  name: string;
  ts: number;
}

export class IncrementalToolTimingTracker {
  private processed = 0;
  private lastProcessed: EventMessage | null = null;
  private starts = new Map<string, ToolStart>();
  private totals = new Map<string, ToolTimingSummary>();
  private summaries: ToolTimingSummary[] = [];
  private dirty = false;

  derive(events: readonly EventMessage[]): ToolTimingSummary[] {
    if (events.length < this.processed || (this.processed > 0 && events[this.processed - 1] !== this.lastProcessed)) {
      this.clear();
    }
    for (let i = this.processed; i < events.length; i++) this.append(events[i]);
    this.processed = events.length;
    this.lastProcessed = events.at(-1) ?? null;
    if (this.dirty) {
      this.summaries = Array.from(this.totals.values(), (summary) => ({ ...summary }))
        .sort((a, b) => b.durationMs - a.durationMs || a.name.localeCompare(b.name));
      this.dirty = false;
    }
    return this.summaries;
  }

  private append(event: EventMessage) {
    if (event.kind === "toolUse" && event.toolUse) {
      this.starts.set(event.toolUse.toolUseID, { name: event.toolUse.name, ts: event.ts });
      return;
    }
    if (event.kind !== "toolResult" || !event.toolResult) return;

    const start = this.starts.get(event.toolResult.toolUseID);
    if (!start) return;
    this.starts.delete(event.toolResult.toolUseID);
    const durationMs = toolResultDurationMs(event.toolResult, start.ts, event.ts);
    if (durationMs === null) return;

    const summary = this.totals.get(start.name) ?? { name: start.name, calls: 0, durationMs: 0 };
    summary.calls += 1;
    summary.durationMs += durationMs;
    this.totals.set(start.name, summary);
    this.dirty = true;
  }

  private clear() {
    this.processed = 0;
    this.lastProcessed = null;
    this.starts = new Map<string, ToolStart>();
    this.totals = new Map<string, ToolTimingSummary>();
    this.summaries = [];
    this.dirty = false;
  }
}

export function deriveToolTimingSummaries(events: readonly EventMessage[]): ToolTimingSummary[] {
  return new IncrementalToolTimingTracker().derive(events);
}
