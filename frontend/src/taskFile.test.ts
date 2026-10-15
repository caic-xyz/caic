// Tests bounded text and compressed STEP reads, sniffed previews, and transfer cancellation.
import { afterEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { readTaskBinaryFile, readTaskFilePreview } from "./taskFile";

const fetchMock = vi.spyOn(globalThis, "fetch");
afterEach(() => fetchMock.mockReset());

describe("readTaskFilePreview", () => {
  it("identifies PDFs by MIME and cancels the preview stream without retaining PDF bytes", async () => {
    let cancelled = false;
    fetchMock.mockResolvedValue(
      new Response(
        new ReadableStream({
          cancel() {
            cancelled = true;
          },
        }),
        { headers: { "Content-Type": "application/pdf" } },
      ),
    );
    expect(await readTaskFilePreview("abc", "/report.bin", new AbortController().signal)).toEqual({ kind: "pdf" });
    expect(cancelled).toBe(true);
  });

  it("does not treat a text file with a PDF extension as a PDF", async () => {
    fetchMock.mockResolvedValue(new Response("plain text", { headers: { "Content-Type": "text/plain" } }));
    expect(await readTaskFilePreview("abc", "/report.pdf", new AbortController().signal)).toEqual({
      kind: "text",
      text: "plain text",
      truncated: false,
    });
  });

  for (const [size, truncated] of [
    [1_048_575, false],
    [1_048_576, false],
    [1_048_577, true],
  ] as const) {
    it(`bounds the text prefix for a ${size}-byte file`, async () => {
      fetchMock.mockResolvedValue(new Response("x".repeat(size), { headers: { "Content-Type": "text/plain" } }));
      const file = await readTaskFilePreview("abc", "/notes.txt", new AbortController().signal);
      expect(file).toEqual({ kind: "text", text: "x".repeat(Math.min(size, 1_048_576)), truncated });
    });
  }

  for (const [charset, bytes] of [
    ["utf-16le", [255, 254, 65, 0, 169, 3]],
    ["utf-16be", [254, 255, 0, 65, 3, 169]],
  ] as const) {
    it(`decodes the raw endpoint's sniffed ${charset} charset`, async () => {
      fetchMock.mockResolvedValue(
        new Response(new Uint8Array(bytes), { headers: { "Content-Type": `text/plain; charset=${charset}` } }),
      );
      expect(await readTaskFilePreview("abc", "/unicode.txt", new AbortController().signal)).toEqual({
        kind: "text",
        text: "AΩ",
        truncated: false,
      });
    });
  }

  it("cancels a chunked stream after the preview limit without waiting for EOF", async () => {
    let cancelled = false;
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new Uint8Array(1_048_576).fill(120));
        controller.enqueue(new Uint8Array([121]));
      },
      cancel() {
        cancelled = true;
      },
    });
    fetchMock.mockResolvedValue(new Response(body, { headers: { "Content-Type": "text/plain" } }));
    const file = await readTaskFilePreview("abc", "/notes.txt", new AbortController().signal);
    expect(file).toEqual({ kind: "text", text: "x".repeat(1_048_576), truncated: true });
    expect(cancelled).toBe(true);
  });
});

describe("readTaskBinaryFile", () => {
  it("cancels oversized compressed STEP downloads based on actual streamed bytes", async () => {
    for (const path of ["/part.stpz", "/part.step.zip", "/part.stp.gz"]) {
      let cancelled = false;
      let pulls = 0;
      fetchMock.mockResolvedValue(
        new Response(
          new ReadableStream<Uint8Array>({
            pull(controller) {
              pulls++;
              controller.enqueue(new Uint8Array(1024 * 1024));
            },
            cancel() {
              cancelled = true;
            },
          }),
          { headers: { "Content-Length": "1" } },
        ),
      );
      await expect(readTaskBinaryFile("abc", path, new AbortController().signal)).rejects.toThrow("64 MiB");
      expect(cancelled).toBe(true);
      expect(pulls).toBeLessThanOrEqual(66);
    }
  });

  it("preserves bytes and accepts compressed STEP downloads exactly at the limit", async () => {
    const bytes = new Uint8Array(64 * 1024 * 1024);
    bytes[0] = 0x50;
    bytes[bytes.length - 1] = 0xff;
    fetchMock.mockResolvedValue(new Response(bytes));
    const result = await readTaskBinaryFile("abc", "/part.stpz", new AbortController().signal);
    if (!(result instanceof Uint8Array)) throw new Error("Expected a view of the download buffer");
    expect(result.length).toBe(64 * 1024 * 1024);
    expect(result[0]).toBe(0x50);
    expect(result[result.length - 1]).toBe(0xff);
  });

  it("propagates compressed download failures", async () => {
    fetchMock.mockResolvedValue(
      new Response(
        new ReadableStream({
          start(controller) {
            controller.error(new Error("stream failed"));
          },
        }),
      ),
    );
    await expect(readTaskBinaryFile("abc", "/part.stpz", new AbortController().signal)).rejects.toThrow(
      "stream failed",
    );
  });
});
