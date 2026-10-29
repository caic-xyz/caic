// Streams bounded batches from line-framed JSON history without retaining complete chunk bodies.
import type { HistoryEvent } from "@sdk/types.gen";
import { validateHistoryEvent } from "@sdk/validate.gen";

type FramingState = "opening" | "first" | "next" | "end";

// Immutable history is a valid JSON array: opening bracket on its own line,
// one compact event per line (leading comma after the first), then closing
// bracket. The projection version owns this framing, including its end marker.
export async function* readHistoryBatches(
  body: ReadableStream<Uint8Array>,
  signal: AbortSignal,
): AsyncGenerator<HistoryEvent[]> {
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let phase: FramingState = "opening";
  let parts: string[] = [];
  let batch: HistoryEvent[] = [];
  let finished = false;
  let cancellation: Promise<void> | null = null;
  const abort = () => {
    cancellation = reader.cancel(signal.reason);
  };
  function line(text: string, state: FramingState): FramingState {
    const value = text.trim();
    if (!value) return state;
    if (state === "opening") {
      if (value !== "[") throw new Error("Invalid history array opening");
      return "first";
    } else if (state === "end") throw new Error("Unexpected data after history array");
    else if (value === "]") return "end";
    else {
      if (state === "next" && value[0] !== ",") throw new Error("Missing history output separator");
      batch.push(validateHistoryEvent(JSON.parse(state === "next" ? value.slice(1) : value)));
      return "next";
    }
  }
  signal.addEventListener("abort", abort, { once: true });
  try {
    for (;;) {
      signal.throwIfAborted();
      const chunk = await reader.read();
      signal.throwIfAborted();
      const text = decoder.decode(chunk.value, { stream: !chunk.done });
      let start = 0;
      for (let end = text.indexOf("\n"); end !== -1; end = text.indexOf("\n", start)) {
        const part = text.slice(start, end);
        if (parts.length) {
          parts.push(part);
          phase = line(parts.join(""), phase);
          parts = [];
        } else phase = line(part, phase);
        start = end + 1;
        // Raw-record count does not bound normalized output count. Yield a
        // bounded batch even when one response read contains many outputs.
        if (batch.length === 256) {
          yield batch;
          batch = [];
          signal.throwIfAborted();
        }
      }
      if (start < text.length) parts.push(text.slice(start));
      if (chunk.done) {
        if (parts.length) phase = line(parts.join(""), phase);
        if (phase !== "end") throw new Error("Incomplete history array");
      }
      if (batch.length) {
        yield batch;
        batch = [];
      }
      if (chunk.done) break;
    }
    finished = true;
  } finally {
    signal.removeEventListener("abort", abort);
    parts = [];
    batch = [];
    if (!finished) await (cancellation ?? reader.cancel());
    reader.releaseLock();
  }
}
