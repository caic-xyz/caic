// Builds line-framed immutable history responses for frontend network fixtures.
import type { HistoryEvent } from "@sdk/types.gen";

export function historyArray(events: readonly HistoryEvent[]): string {
  const lines = events.map((event) => JSON.stringify(event));
  return "[\n" + lines.join("\n,") + (lines.length ? "\n" : "") + "]\n";
}

export function historyResponse(events: readonly HistoryEvent[]): Response {
  return new Response(historyArray(events), {
    headers: { "content-type": "application/json" },
  });
}
