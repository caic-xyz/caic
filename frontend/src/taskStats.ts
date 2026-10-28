// Inclusive task analytics with source-scoped tool timing and resource-counter rates.

import type { EventMessage, EventStats } from "@sdk/types.gen";

import { toolIdentity, toolResultDurationMs } from "./grouping";

export interface ToolTimingSummary {
  name: string;
  calls: number;
  durationMs: number;
}

interface ToolStart {
  name: string;
  ts: number;
}

export interface NetworkRateSample {
  ts: number;
  rxBytesPerSecond: number | null;
  txBytesPerSecond: number | null;
}

export function deriveNetworkRates(stats: readonly EventStats[]): NetworkRateSample[] {
  return stats.map((sample, i) => {
    const previous = stats[i - 1];
    const elapsedSeconds = previous ? (sample.ts - previous.ts) / 1000 : 0;
    return {
      ts: sample.ts,
      rxBytesPerSecond:
        previous && elapsedSeconds > 0 && sample.netRx >= previous.netRx
          ? (sample.netRx - previous.netRx) / elapsedSeconds
          : null,
      txBytesPerSecond:
        previous && elapsedSeconds > 0 && sample.netTx >= previous.netTx
          ? (sample.netTx - previous.netTx) / elapsedSeconds
          : null,
    };
  });
}

export class ToolTimingTracker {
  private starts = new Map<string, ToolStart>();
  private totals = new Map<string, ToolTimingSummary>();
  private summaries: ToolTimingSummary[] = [];
  private dirty = false;

  snapshot(): ToolTimingSummary[] {
    if (this.dirty) {
      this.summaries = Array.from(this.totals.values(), (summary) => ({ ...summary })).sort(
        (a, b) => b.durationMs - a.durationMs || a.name.localeCompare(b.name),
      );
      this.dirty = false;
    }
    return this.summaries;
  }

  append(event: EventMessage): boolean {
    if (event.kind === "toolUse" && event.toolUse) {
      this.starts.set(toolIdentity(event.source, event.toolUse.toolUseID), { name: event.toolUse.name, ts: event.ts });
      return false;
    }
    if (event.kind !== "toolResult" || !event.toolResult) return false;

    const key = toolIdentity(event.source, event.toolResult.toolUseID);
    const start = this.starts.get(key);
    if (!start) return false;
    this.starts.delete(key);
    const durationMs = toolResultDurationMs(event.toolResult, start.ts, event.ts);
    if (durationMs === null) return false;

    const summary = this.totals.get(start.name) ?? { name: start.name, calls: 0, durationMs: 0 };
    summary.calls += 1;
    summary.durationMs += durationMs;
    this.totals.set(start.name, summary);
    this.dirty = true;
    return true;
  }
}

// mergeStats keeps the newest revision of each timestamp in the bounded ring.
export function mergeStats(first: readonly EventStats[], latest: readonly EventStats[]): EventStats[] {
  const values = new Map(first.map((sample) => [sample.ts, sample]));
  for (const sample of latest) values.set(sample.ts, sample);
  return [...values.values()].sort((a, b) => a.ts - b.ts).slice(-60);
}
