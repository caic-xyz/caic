// Addresses fixed raw-record chunks and compares canonical output positions.
import type { HistoryPosition } from "@sdk/types.gen";

export function compareHistoryPosition(a: HistoryPosition, b: HistoryPosition): number {
  return a.recordID - b.recordID || a.index - b.index;
}

export function historyPositionKey(position: HistoryPosition): string {
  return `${position.recordID}:${position.index}`;
}

export function historyChunkFirst(recordID: number, recordsPerChunk: number): number {
  return Math.floor((recordID - 1) / recordsPerChunk) * recordsPerChunk + 1;
}
