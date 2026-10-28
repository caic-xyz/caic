// Streams validated history outputs one event at a time without materializing oversized chunk arrays.
import type { HistoryEvent } from "@sdk/types.gen";
import { validateHistoryEvent } from "@sdk/validate.gen";

export async function* readHistoryArray(
  body: ReadableStream<Uint8Array>,
  signal: AbortSignal,
): AsyncGenerator<HistoryEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let state: "array" | "value" | "event" | "separator" | "end" = "array";
  let depth = 0;
  let quoted = false;
  let escaped = false;
  let allowEnd = true;
  let parts: string[] = [];
  let finished = false;
  let cancellation: Promise<void> | null = null;
  const abort = () => {
    cancellation = reader.cancel(signal.reason);
  };
  signal.addEventListener("abort", abort, { once: true });
  try {
    for (;;) {
      signal.throwIfAborted();
      const chunk = await reader.read();
      signal.throwIfAborted();
      const text = decoder.decode(chunk.value, { stream: !chunk.done });
      let start = state === "event" ? 0 : -1;
      for (let i = 0; i < text.length; i++) {
        const ch = text[i];
        if (state === "event") {
          if (quoted) {
            if (escaped) escaped = false;
            else if (ch === "\\") escaped = true;
            else if (ch === '"') quoted = false;
          } else if (ch === '"') quoted = true;
          else if (ch === "{" || ch === "[") depth++;
          else if (ch === "}" || ch === "]") depth--;
          if (depth === 0) {
            parts.push(text.slice(start, i + 1));
            const json = parts.join("");
            parts = [];
            state = "separator";
            start = -1;
            yield validateHistoryEvent(JSON.parse(json));
          }
          continue;
        }
        if (/\s/.test(ch ?? "")) continue;
        if (state === "array" && ch === "[") state = "value";
        else if (state === "value" && ch === "{") {
          state = "event";
          start = i;
          depth = 1;
        } else if ((state === "separator" || (state === "value" && allowEnd)) && ch === "]") state = "end";
        else if (state === "separator" && ch === ",") {
          state = "value";
          allowEnd = false;
        } else throw new Error("Invalid history JSON array");
      }
      if (state === "event" && start >= 0) parts.push(text.slice(start));
      if (chunk.done) break;
    }
    if (state !== "end") throw new Error("Incomplete history JSON array");
    finished = true;
  } finally {
    signal.removeEventListener("abort", abort);
    parts = [];
    if (!finished) await (cancellation ?? reader.cancel());
    reader.releaseLock();
  }
}
