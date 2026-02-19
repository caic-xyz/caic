// Helpers for converting image files to base64 ImageData payloads.
import type { ImageData as APIImageData } from "@sdk/types.gen";

const ALLOWED_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);

/** Convert a File to an APIImageData, or null if the type is unsupported. */
export async function fileToImageData(file: File): Promise<APIImageData | null> {
  if (!ALLOWED_TYPES.has(file.type)) return null;
  const buf = await file.arrayBuffer();
  const bytes = new Uint8Array(buf);
  // Chunk the conversion to avoid blowing the call stack on large files
  // (mobile photos can be multi-MB; spreading into String.fromCharCode
  // passes one argument per byte which exceeds stack limits).
  const chunks: string[] = [];
  for (let i = 0; i < bytes.length; i += 65536) {
    chunks.push(String.fromCharCode(...bytes.subarray(i, i + 65536)));
  }
  const data = btoa(chunks.join(""));
  return { mediaType: file.type, data };
}

/** Capture a screenshot via getDisplayMedia and return it as JPEG base64, or null on cancel/error. */
export async function captureScreen(): Promise<APIImageData | null> {
  let stream: MediaStream;
  try {
    stream = await navigator.mediaDevices.getDisplayMedia({ video: true });
  } catch {
    return null; // User cancelled the picker.
  }
  const video = document.createElement("video");
  video.srcObject = stream;
  video.muted = true;
  await video.play();
  // Wait one frame for the video to render.
  await new Promise((r) => requestAnimationFrame(r));
  const canvas = document.createElement("canvas");
  canvas.width = video.videoWidth;
  canvas.height = video.videoHeight;
  const ctx = canvas.getContext("2d");
  if (!ctx) {
    stream.getTracks().forEach((t) => t.stop());
    return null;
  }
  ctx.drawImage(video, 0, 0);
  stream.getTracks().forEach((t) => t.stop());
  const dataURL = canvas.toDataURL("image/jpeg", 0.9);
  const data = dataURL.split(",")[1];
  return { mediaType: "image/jpeg", data };
}

/** Extract image files from a paste event and convert them. */
export async function imagesFromClipboard(e: ClipboardEvent): Promise<APIImageData[]> {
  const items = e.clipboardData?.items;
  if (!items) return [];
  const files: File[] = [];
  for (const item of items) {
    if (item.kind === "file" && ALLOWED_TYPES.has(item.type)) {
      const f = item.getAsFile();
      if (f) files.push(f);
    }
  }
  const results = await Promise.all(files.map(fileToImageData));
  return results.filter((r): r is APIImageData => r !== null);
}
