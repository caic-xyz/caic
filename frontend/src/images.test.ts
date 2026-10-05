// Tests Blob draft admission, URL ownership, bounded encoding, and cancellation.

import { afterEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { ImageDraftOwner, imagesToAPI } from "./images";

const limits = { allowedMediaTypes: ["image/png"], maxImageBytes: 10485760, maxPromptImageBytes: 20971520 };
const blob = (size: number, type: string) => new Blob([new Uint8Array(size)], { type });
afterEach(() => vi.restoreAllMocks());

function owner() {
  const create = vi.spyOn(URL, "createObjectURL").mockImplementation(() => `blob:${crypto.randomUUID()}`);
  const revoke = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  return { drafts: new ImageDraftOwner(() => limits), create, revoke };
}

describe("ImageDraftOwner", () => {
  it("accepts exact byte limits and rejects the entire overflowing or unsupported selection before URLs/read", () => {
    const { drafts, create } = owner();
    const image = blob(10485760, "image/png");
    const read = vi.spyOn(image, "arrayBuffer");
    const current = drafts.adopt([], [image, image]);
    expect(current).toHaveLength(2);
    expect(read).not.toHaveBeenCalled();
    expect(create).toHaveBeenCalledTimes(2);
    expect(() => drafts.adopt(current, [blob(1, "image/png")])).toThrow(/total/);
    expect(() => drafts.adopt([], [image, blob(10485761, "image/png")])).toThrow(/Each image/);
    expect(() => drafts.adopt([], [image, blob(1, "image/bmp")])).toThrow(/type/);
    expect(() => drafts.adopt([], [blob(0, "image/png")])).toThrow(/empty/);
    expect(create).toHaveBeenCalledTimes(2);
    drafts.dispose();
  });
  it("releases removed and disposed previews once and prevents late adoption", () => {
    const { drafts, revoke } = owner();
    const images = drafts.adopt([], [blob(1, "image/png"), blob(2, "image/png")]);
    drafts.release([images[0]]);
    drafts.release([images[0]]);
    drafts.dispose();
    expect(revoke.mock.calls.map((call) => call[0])).toEqual(images.map((image) => image.previewURL));
    expect(() => drafts.adopt([], [blob(1, "image/png")])).toThrow(/closed/);
  });
  it("releases already allocated previews if URL creation fails", () => {
    const { drafts, create, revoke } = owner();
    create.mockReturnValueOnce("blob:first").mockImplementationOnce(() => {
      throw new Error("URL allocation failed");
    });
    expect(() => drafts.adopt([], [blob(1, "image/png"), blob(1, "image/png")])).toThrow(/allocation/);
    expect(revoke).toHaveBeenCalledWith("blob:first");
    drafts.dispose();
    expect(revoke).toHaveBeenCalledOnce();
  });
  it("requires server policy without reading files", () => {
    const drafts = new ImageDraftOwner(() => null);
    expect(() => drafts.adopt([], [blob(1, "image/png")])).toThrow(/server limits/);
  });
});

describe("imagesToAPI", () => {
  it("encodes all bytes across chunk boundaries and all padding cases using bounded slices", async () => {
    for (const size of [49151, 49152, 49153, 98306]) {
      const bytes = Uint8Array.from({ length: size }, (_, i) => i % 251);
      const source = new Blob([bytes], { type: "image/png" });
      const fullRead = vi.spyOn(source, "arrayBuffer");
      const slice = vi.spyOn(source, "slice");
      const result = await imagesToAPI(
        [{ id: "test", blob: source, previewURL: "blob:test" }],
        new AbortController().signal,
      );
      expect(result).toEqual([{ mediaType: "image/png", data: Buffer.from(bytes).toString("base64") }]);
      expect(fullRead).not.toHaveBeenCalled();
      for (const [start, end] of slice.mock.calls) expect((end ?? size) - (start ?? 0)).toBeLessThanOrEqual(65536);
    }
  });
  it("stops after a pending slice when cancelled and does not start another image", async () => {
    const controller = new AbortController();
    const first = blob(100000, "image/png");
    const next = blob(1, "image/png");
    const nextSlice = vi.spyOn(next, "slice");
    let finish!: (value: ArrayBuffer) => void;
    const pending = new Promise<ArrayBuffer>((resolve) => {
      finish = resolve;
    });
    const slice = vi.spyOn(first, "slice").mockReturnValue({ arrayBuffer: () => pending } as Blob);
    const result = imagesToAPI(
      [first, next].map((image, i) => ({ id: String(i), blob: image, previewURL: "blob:test" })),
      controller.signal,
    );
    controller.abort(new Error("Draft closed"));
    finish(new ArrayBuffer(49152));
    await expect(result).rejects.toThrow("Draft closed");
    expect(slice).toHaveBeenCalledOnce();
    expect(nextSlice).not.toHaveBeenCalled();
  });
  it("propagates file read failure without a partial result", async () => {
    const source = blob(1, "image/png");
    vi.spyOn(source, "slice").mockReturnValue({
      arrayBuffer: () => Promise.reject(new Error("File unavailable")),
    } as Blob);
    await expect(
      imagesToAPI([{ id: "test", blob: source, previewURL: "blob:test" }], new AbortController().signal),
    ).rejects.toThrow("File unavailable");
  });
});

describe("screen capture", () => {
  it("stops tracks on playback failure and surfaces the failure", async () => {
    const { captureScreen } = await import("./images");
    const devices = Object.getOwnPropertyDescriptor(navigator, "mediaDevices");
    const stop = vi.fn();
    Object.defineProperty(navigator, "mediaDevices", {
      configurable: true,
      value: { getDisplayMedia: async () => ({ getTracks: () => [{ stop }] }) },
    });
    vi.spyOn(window.HTMLMediaElement.prototype, "play").mockRejectedValue(new Error("Playback failed"));
    try {
      await expect(captureScreen(new AbortController().signal)).rejects.toThrow("Playback failed");
      expect(stop).toHaveBeenCalledOnce();
    } finally {
      if (devices) Object.defineProperty(navigator, "mediaDevices", devices);
      else Reflect.deleteProperty(navigator, "mediaDevices");
    }
  });
  it("stops a stream that arrives after cancellation without starting playback", async () => {
    const { captureScreen } = await import("./images");
    const devices = Object.getOwnPropertyDescriptor(navigator, "mediaDevices");
    let grant!: (stream: MediaStream) => void;
    const permission = new Promise<MediaStream>((r) => {
      grant = r;
    });
    Object.defineProperty(navigator, "mediaDevices", {
      configurable: true,
      value: { getDisplayMedia: () => permission },
    });
    const play = vi.spyOn(window.HTMLMediaElement.prototype, "play").mockResolvedValue();
    const stop = vi.fn();
    try {
      const controller = new AbortController();
      const result = captureScreen(controller.signal);
      controller.abort(new Error("Capture cancelled"));
      grant({ getTracks: () => [{ stop }] } as unknown as MediaStream);
      await expect(result).rejects.toThrow("Capture cancelled");
      expect(stop).toHaveBeenCalledOnce();
      expect(play).not.toHaveBeenCalled();
    } finally {
      if (devices) Object.defineProperty(navigator, "mediaDevices", devices);
      else Reflect.deleteProperty(navigator, "mediaDevices");
    }
  });
});
