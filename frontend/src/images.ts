// Owns Blob image drafts, preview URLs, bounded submission encoding, and browser captures.

import type { ImageConstraints, ImageData as APIImageData } from "@sdk/types.gen";

export interface DraftImage {
  readonly id: string;
  readonly blob: Blob;
  readonly previewURL: string;
}

/** Owns preview URLs for one account's drafts; navigation does not dispose drafts. */
export class ImageDraftOwner {
  private readonly images = new Set<DraftImage>();
  private disposed = false;
  private nextID = 0;

  constructor(private readonly constraints: () => ImageConstraints | null) {}

  /** Validate the entire selection before allocating previews or reading any bytes. */
  adopt(current: readonly DraftImage[], blobs: readonly Blob[]): DraftImage[] {
    if (this.disposed) throw new Error("This attachment draft is closed.");
    const limits = this.constraints();
    if (!limits) throw new Error("Image attachments are unavailable until server limits load.");
    let total = current.reduce((sum, img) => sum + img.blob.size, 0);
    for (const blob of blobs) {
      if (!limits.allowedMediaTypes.includes(blob.type)) throw new Error("This image type is not supported.");
      if (!blob.size) throw new Error("The image is empty.");
      if (blob.size > limits.maxImageBytes) {
        throw new Error(`Each image must be ${limits.maxImageBytes / (1024 * 1024)} MiB or smaller.`);
      }
      total += blob.size;
      if (total > limits.maxPromptImageBytes) {
        throw new Error(`Attachments must total ${limits.maxPromptImageBytes / (1024 * 1024)} MiB or less.`);
      }
    }
    const added: DraftImage[] = [];
    try {
      for (const blob of blobs) {
        const img = { id: String(this.nextID++), blob, previewURL: URL.createObjectURL(blob) };
        added.push(img);
        this.images.add(img);
      }
    } catch (err) {
      this.release(added);
      throw err;
    }
    return [...current, ...added];
  }

  release(images: readonly DraftImage[]): void {
    for (const img of images) {
      if (this.images.delete(img)) URL.revokeObjectURL(img.previewURL);
    }
  }

  dispose(): void {
    this.disposed = true;
    this.release([...this.images]);
  }
}

/** Encode images serially; only the final wire values grow with the bounded prompt.
 * Raw reads and binary strings are limited to 48 KiB. A multiple of three keeps
 * independently encoded chunks padding-free except for the final slice.
 * Preview decoding and the final base64/JSON request are outside this work budget.
 */
export async function imagesToAPI(images: readonly DraftImage[], signal: AbortSignal): Promise<APIImageData[]> {
  const result: APIImageData[] = [];
  for (const img of images) {
    let data = "";
    for (let offset = 0; offset < img.blob.size; offset += 48 * 1024) {
      signal.throwIfAborted();
      const bytes = new Uint8Array(await img.blob.slice(offset, offset + 48 * 1024).arrayBuffer());
      signal.throwIfAborted();
      data += btoa(String.fromCharCode(...bytes));
    }
    result.push({ mediaType: img.blob.type, data });
  }
  signal.throwIfAborted();
  return result;
}

export function canvasToBlob(canvas: HTMLCanvasElement): Promise<Blob> {
  return new Promise((resolve, reject) => {
    canvas.toBlob(
      (blob) => {
        if (blob) resolve(blob);
        else reject(new Error("Could not capture the image. Try again."));
      },
      "image/jpeg",
      0.9,
    );
  });
}

/** Capture a screenshot as a Blob; permission cancellation produces no image. */
export async function captureScreen(signal: AbortSignal): Promise<Blob | null> {
  let stream: MediaStream;
  try {
    stream = await navigator.mediaDevices.getDisplayMedia({ video: true });
  } catch (err) {
    if (err instanceof DOMException && err.name === "NotAllowedError") return null;
    throw err;
  }
  const stopTracks = () => stream.getTracks().forEach((track) => track.stop());
  signal.addEventListener("abort", stopTracks, { once: true });
  try {
    signal.throwIfAborted();
    const video = document.createElement("video");
    video.srcObject = stream;
    video.muted = true;
    await video.play();
    signal.throwIfAborted();
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    signal.throwIfAborted();
    const canvas = document.createElement("canvas");
    canvas.width = video.videoWidth;
    canvas.height = video.videoHeight;
    const ctx = canvas.getContext("2d");
    if (!ctx) throw new Error("Could not capture the screen. Try again.");
    ctx.drawImage(video, 0, 0);
    const blob = await canvasToBlob(canvas);
    signal.throwIfAborted();
    return blob;
  } finally {
    signal.removeEventListener("abort", stopTracks);
    stopTracks();
  }
}

/** Collect clipboard image files without reading or encoding their contents. */
export function imagesFromClipboard(e: ClipboardEvent): File[] {
  const files: File[] = [];
  for (const item of e.clipboardData?.items ?? []) {
    if (item.kind !== "file") continue;
    const file = item.getAsFile();
    if (file) files.push(file);
  }
  return files;
}
