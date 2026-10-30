// Derives resource-counter rates and merges bounded task resource observations.

import type { EventStats } from "@sdk/types.gen";

export interface ToolTimingSummary {
  name: string;
  calls: number;
  durationMs: number;
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

// mergeStats keeps the newest revision of each timestamp in the bounded ring.
export function mergeStats(first: readonly EventStats[], latest: readonly EventStats[]): EventStats[] {
  const values = new Map(first.map((sample) => [sample.ts, sample]));
  for (const sample of latest) values.set(sample.ts, sample);
  return [...values.values()].sort((a, b) => a.ts - b.ts).slice(-60);
}
