// Reads bounded text previews and identifies image/PDF previews or CAD binary data through the raw endpoint.

import { taskRawURL } from "./runtimePath";

const maxTextBytes = 1 << 20;
const imageTypes = new Set(["image/avif", "image/bmp", "image/gif", "image/jpeg", "image/png", "image/webp"]);

export type TaskFilePreview =
  { kind: "text"; text: string; truncated: boolean } | { kind: "image" } | { kind: "pdf" } | { kind: "unsupported" };

async function fetchTaskFile(taskId: string, path: string, signal: AbortSignal): Promise<Response> {
  const res = await fetch(taskRawURL(taskId, path), { signal });
  if (!res.ok) {
    const body: unknown = await res.json().catch(() => null);
    const message = (body as { error?: { message?: unknown } } | null)?.error?.message;
    throw new Error(typeof message === "string" ? message : `HTTP ${res.status}`);
  }
  return res;
}

// MIME comes from the raw endpoint's byte sniffer, not the filename. The viewer
// owns escaped text, sanitized Markdown, inert images, and PDF.js rendering.
export async function readTaskFilePreview(taskId: string, path: string, signal: AbortSignal): Promise<TaskFilePreview> {
  const res = await fetchTaskFile(taskId, path, signal);
  const mediaType = res.headers.get("Content-Type") ?? "application/octet-stream";
  const contentType = mediaType.split(";")[0].trim();
  const textFile = contentType.startsWith("text/");
  if (!textFile) {
    await res.body?.cancel();
    if (contentType === "application/pdf") return { kind: "pdf" };
    return imageTypes.has(contentType) ? { kind: "image" } : { kind: "unsupported" };
  }
  if (!res.body) return { kind: "text", text: "", truncated: false };
  const reader = res.body.getReader();
  const charset = /;\s*charset=([^;\s]+)/i.exec(mediaType)?.[1] ?? "utf-8";
  const decoder = new TextDecoder(charset);
  let text = "";
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return { kind: "text", text: text + decoder.decode(), truncated: false };
      const remaining = maxTextBytes - size;
      text += decoder.decode(value.subarray(0, remaining), { stream: true });
      size += value.byteLength;
      if (size > maxTextBytes) return { kind: "text", text: text + decoder.decode(), truncated: true };
    }
  } finally {
    // Cancellation after a bounded preview must not replace its successful result.
    await reader.cancel().catch(() => undefined);
  }
}

/** Reads the raw binary contents of a task runtime file. */
export async function readTaskBinaryFile(taskId: string, path: string, signal: AbortSignal): Promise<ArrayBuffer> {
  return (await fetchTaskFile(taskId, path, signal)).arrayBuffer();
}
