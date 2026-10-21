// Streams bounded STEP downloads with progress and reads task-file previews and other CAD formats.

import { isCompressedStepPath, taskRawURL } from "./runtimePath";

const maxTextBytes = 1 << 20;
const imageTypes = new Set(["image/avif", "image/bmp", "image/gif", "image/jpeg", "image/png", "image/webp"]);

export type TaskFilePreview =
  | { kind: "text"; text: string; truncated: boolean }
  | { kind: "audio" }
  | { kind: "image" }
  | { kind: "pdf" }
  | { kind: "video" }
  | { kind: "unsupported" };

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
// owns escaped text, sanitized Markdown, inert images, native media, and PDF.js rendering.
export async function readTaskFilePreview(taskId: string, path: string, signal: AbortSignal): Promise<TaskFilePreview> {
  const res = await fetchTaskFile(taskId, path, signal);
  const mediaType = res.headers.get("Content-Type") ?? "application/octet-stream";
  const contentType = mediaType
    .slice(0, mediaType.indexOf(";") === -1 ? mediaType.length : mediaType.indexOf(";"))
    .trim();
  const textFile = contentType.startsWith("text/");
  if (!textFile) {
    await res.body?.cancel();
    if (contentType.startsWith("audio/")) return { kind: "audio" };
    if (contentType.startsWith("video/")) return { kind: "video" };
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

function contentLength(res: Response): number | null {
  const advertised = res.headers.get("Content-Encoding") ? null : res.headers.get("Content-Length");
  return advertised !== null && /^\d+$/.test(advertised) && Number.isSafeInteger(Number(advertised))
    ? Number(advertised)
    : null;
}

// Other CAD formats need a complete buffer; STEP uses the bounded consumer below.
export async function readTaskBinaryFile(
  taskId: string,
  path: string,
  signal: AbortSignal,
  progress: ((completed: number, total: number | null) => void) | null,
): Promise<ArrayBuffer> {
  const res = await fetchTaskFile(taskId, path, signal);
  signal.throwIfAborted();
  progress?.(0, contentLength(res));
  const buffer = await res.arrayBuffer();
  signal.throwIfAborted();
  progress?.(buffer.byteLength, buffer.byteLength);
  return buffer;
}

// The consumer runs synchronously before the next read. There is no application
// queue of compressed chunks; inflate and indexing apply network backpressure.
export async function streamTaskStepFile(
  taskId: string,
  path: string,
  signal: AbortSignal,
  consume: (chunk: Uint8Array<ArrayBuffer>) => void,
  progress: (completed: number, total: number | null) => void,
): Promise<number> {
  const started = performance.now();
  const res = await fetchTaskFile(taskId, path, signal);
  let waitingMs = performance.now() - started;
  signal.throwIfAborted();
  const total = contentLength(res);
  progress(0, total);
  if (!res.body) return waitingMs;
  const reader = res.body.getReader();
  const compressed = isCompressedStepPath(path);
  const limit = (compressed ? 64 : 512) * 1024 * 1024;
  let size = 0;
  try {
    for (;;) {
      const before = performance.now();
      const { done, value } = await reader.read();
      waitingMs += performance.now() - before;
      signal.throwIfAborted();
      if (done) break;
      size += value.byteLength;
      if (size > limit)
        throw new Error(compressed ? "Compressed STEP file exceeds 64 MiB" : "STEP file exceeds 512 MiB");
      if (!compressed && total !== null && size > total) throw new Error("STEP download exceeds Content-Length");
      // Cap each consumer call even when a mocked response or browser provides
      // a large chunk. The archive decoder further bounds inflate input slices.
      for (let offset = 0; offset < value.length; offset += 65536) {
        signal.throwIfAborted();
        consume(value.subarray(offset, offset + 65536));
        // Network receipt and decoding progress are independent: a browser
        // chunk may already contain the full file while indexing still grows.
        progress(size, total !== null && size <= total ? total : null);
        signal.throwIfAborted();
      }
    }
  } finally {
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
  return waitingMs;
}
