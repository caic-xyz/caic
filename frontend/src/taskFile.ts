// Streams bounded STEP downloads with progress and reads task-file previews and other CAD formats.

import { isCompressedStepPath, isStepPath, taskRawURL } from "./runtimePath";

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
export async function readTaskBinaryFile(
  taskId: string,
  path: string,
  signal: AbortSignal,
  progress: ((completed: number, total: number | null) => void) | null,
): Promise<ArrayBuffer | Uint8Array<ArrayBuffer>> {
  const res = await fetchTaskFile(taskId, path, signal);
  signal.throwIfAborted();
  const advertised = res.headers.get("Content-Encoding") ? null : res.headers.get("Content-Length");
  const total =
    advertised !== null && /^\d+$/.test(advertised) && Number.isSafeInteger(Number(advertised))
      ? Number(advertised)
      : null;
  progress?.(0, total);
  if (!isStepPath(path)) {
    const buffer = await res.arrayBuffer();
    signal.throwIfAborted();
    progress?.(buffer.byteLength, buffer.byteLength);
    return buffer;
  }
  if (!res.body) return new ArrayBuffer(0);
  const reader = res.body.getReader();
  // Keep one destination allocation and return a view of the received prefix;
  // slicing or Blob assembly would copy the complete compressed file.
  const compressed = isCompressedStepPath(path);
  const limit = (compressed ? 64 : 512) * 1024 * 1024;
  const capacity = !compressed && total !== null && total > 0 && total <= limit ? total : limit;
  const bytes = new Uint8Array(capacity);
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      signal.throwIfAborted();
      if (size > limit)
        throw new Error(compressed ? "Compressed STEP file exceeds 64 MiB" : "STEP file exceeds 512 MiB");
      if (size > capacity) throw new Error("STEP download exceeds Content-Length");
      bytes.set(value, size - value.byteLength);
      progress?.(size, total !== null && size <= total ? total : null);
      signal.throwIfAborted();
    }
  } finally {
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
  return bytes.subarray(0, size);
}
