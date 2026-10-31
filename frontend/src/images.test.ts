// Tests Blob draft admission, URL ownership, and cancellation.

import assert from "node:assert/strict";
import { afterEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { ImageDraftOwner } from "./images";

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
    assert.ok(images[0] !== undefined);
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
