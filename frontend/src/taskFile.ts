// Reads a bounded text prefix of a task runtime file through the raw task-file endpoint.

import { taskFileURL } from "./runtimePath";

/** Largest prefix read; a longer file is cut and reported as truncated. */
const maxTextBytes = 1 << 20;

export interface TaskTextFile {
  text: string;
  truncated: boolean;
}

export async function readTaskTextFile(taskId: string, path: string, signal: AbortSignal): Promise<TaskTextFile> {
  const res = await fetch(taskFileURL(taskId, path), { signal });
  if (!res.ok) {
    const body: unknown = await res.json().catch(() => null);
    const message = (body as { error?: { message?: unknown } } | null)?.error?.message;
    throw new Error(typeof message === "string" ? message : `HTTP ${res.status}`);
  }
  // The endpoint serves only plain text inline; any other type is a download.
  if (!res.headers.get("Content-Type")?.startsWith("text/plain") || !res.body) {
    throw new Error("not a text file");
  }
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let text = "";
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return { text: text + decoder.decode(), truncated: false };
      size += value.byteLength;
      text += decoder.decode(value, { stream: true });
      if (size >= maxTextBytes) return { text: text + decoder.decode(), truncated: true };
    }
  } finally {
    // Stops the transfer after a cut; a cancel rejection cannot change the result.
    await reader.cancel().catch(() => undefined);
  }
}
