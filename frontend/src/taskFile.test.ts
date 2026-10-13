// Tests bounded text reads, sniffed PDF previews, exact-limit truncation, and transfer cancellation.
import { afterEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { readTaskFilePreview } from "./taskFile";

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
