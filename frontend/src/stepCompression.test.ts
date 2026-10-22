// Tests compressed STEP selection, container detection, corruption, and expansion limits.

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { gzipSync } from "node:zlib";
import { Zip, ZipDeflate, zipSync, strToU8 } from "three/addons/libs/fflate.module.js";
import { expect } from "@tests/expect";
import { StepDecompressor } from "./stepCompression";

async function decompressStep(
  buffer: ArrayBuffer | Uint8Array<ArrayBuffer>,
  filename: string,
  _progress: null,
): Promise<Uint8Array<ArrayBuffer>> {
  const bytes = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
  const chunks: Uint8Array<ArrayBuffer>[] = [];
  const decoder = new StepDecompressor(filename, (chunk) => chunks.push(chunk));
  for (let offset = 0; offset < bytes.length; offset += 7919) decoder.push(bytes.subarray(offset, offset + 7919));
  decoder.finish();
  const result = new Uint8Array(chunks.reduce((size, chunk) => size + chunk.length, 0));
  let outputOffset = 0;
  for (const chunk of chunks) {
    result.set(chunk, outputOffset);
    outputOffset += chunk.length;
  }
  return result;
}

function flipByte(bytes: Uint8Array, index: number) {
  const value = bytes[index];
  assert.ok(value !== undefined);
  bytes[index] = value ^ 1;
}
describe("StepDecompressor", () => {
  it("accepts an empty plain STEP stream", async () => {
    const buffer = new ArrayBuffer(0);
    expect((await decompressStep(buffer, "part.step", null)).length).toBe(0);
  });

  it("detects ZIP and gzip signatures for stpz and ignores unrelated archive entries", async () => {
    const text = "ISO-10303-21;\nEND-ISO-10303-21;";
    const bytes = strToU8(text);
    const gzip = Uint8Array.from(gzipSync(bytes));
    const zip = zipSync({ "README.txt": strToU8("metadata"), "nested/part.STEP": bytes });
    for (const compressed of [zip, gzip]) {
      const result = await decompressStep(compressed.buffer as ArrayBuffer, "part.STPZ", null);
      expect(new TextDecoder().decode(result)).toBe(text);
    }
  });

  it("accepts Part 21 archive roots and nested model names", async () => {
    const text = "ISO-10303-21;\nEND-ISO-10303-21;";
    for (const name of ["ISO-10303.p21", "ISO10303.p21", "nested/model.P21"]) {
      const zip = zipSync({ "README.txt": strToU8("metadata"), [name]: strToU8(text) });
      expect(new TextDecoder().decode(await decompressStep(zip, "part.stpz", null))).toBe(text);
    }
    const ambiguous = zipSync({ "ISO-10303.p21": strToU8(text), "another.step": strToU8(text) });
    await expect(decompressStep(ambiguous, "part.stpz", null)).rejects.toThrow("multiple models");
  });

  it("joins concatenated gzip members and checks each member's CRC", async () => {
    const first = gzipSync("STEP ");
    const last = gzipSync("data");
    const gzip = Uint8Array.from(Buffer.concat([first, last]));
    expect(new TextDecoder().decode(await decompressStep(gzip, "part.stpz", null))).toBe("STEP data");
    for (const trailer of [first.length - 8, gzip.length - 8]) {
      const corrupt = gzip.slice();
      flipByte(corrupt, trailer);
      await expect(decompressStep(corrupt, "part.stp.gz", null)).rejects.toThrow("CRC checksum mismatch");
    }
  });

  it("bounds the aggregate output of concatenated gzip members", async () => {
    const member = gzipSync(new Uint8Array(130 * 1024 * 1024));
    const gzip = Uint8Array.from(Buffer.concat([member, member, member, member]));
    await expect(decompressStep(gzip, "part.step.gz", null)).rejects.toThrow("512 MiB");
  });

  it("rejects missing and ambiguous models", async () => {
    for (const empty of [zipSync({}), zipSync({ "note.txt": strToU8("hello") })]) {
      await expect(decompressStep(empty.buffer as ArrayBuffer, "part.stpz", null)).rejects.toThrow("contains no");
    }
    const zip = zipSync({ "a.step": strToU8("a"), "nested/b.stp": strToU8("b") });
    await expect(decompressStep(zip.buffer as ArrayBuffer, "part.step.zip", null)).rejects.toThrow("multiple models");
  });

  it("rejects corrupt and truncated containers", async () => {
    for (const bytes of [
      strToU8("not compressed"),
      new Uint8Array([0x50, 0x4b, 3, 4]),
      new Uint8Array([0x1f, 0x8b, 8]),
    ]) {
      await expect(decompressStep(bytes.buffer as ArrayBuffer, "part.stpz", null)).rejects.toThrow();
    }
    const gzip = Uint8Array.from(gzipSync("STEP data"));
    flipByte(gzip, gzip.length - 8);
    await expect(decompressStep(gzip.buffer, "part.stp.gz", null)).rejects.toThrow();
  });

  it("rejects oversized gzip expansion", async () => {
    const gzip = Uint8Array.from(gzipSync("STEP data"));
    new DataView(gzip.buffer).setUint32(gzip.length - 4, 512 * 1024 * 1024 + 1, true);
    await expect(decompressStep(gzip.buffer, "part.step.gz", null)).rejects.toThrow("512 MiB");
  });

  it("rejects ZIP payload corruption even when the model size is unchanged", async () => {
    const zip = zipSync({ "part.stp": strToU8("STEP data") }, { level: 0 });
    const view = new DataView(zip.buffer);
    const dataOffset = 30 + view.getUint16(26, true) + view.getUint16(28, true);
    flipByte(zip, dataOffset);
    await expect(decompressStep(zip.buffer as ArrayBuffer, "part.stpz", null)).rejects.toThrow("CRC checksum mismatch");
  });

  it("rejects actual ZIP expansion beyond its declared size", async () => {
    const zip = zipSync({ "part.stp": new Uint8Array(1024 * 1024) });
    const view = new DataView(zip.buffer);
    view.setUint32(22, 1, true);
    for (let offset = 0; offset < zip.length - 4; offset++) {
      if (view.getUint32(offset, true) === 0x02014b50) {
        view.setUint32(offset + 24, 1, true);
        break;
      }
    }
    await expect(decompressStep(zip.buffer as ArrayBuffer, "part.stpz", null)).rejects.toThrow(
      "Invalid STEP archive model size",
    );
  });

  it("supports models larger than 128 MiB and archive subviews", async () => {
    const zip = zipSync({ "part.stp": new Uint8Array(144 * 1024 * 1024) });
    const storage = new Uint8Array(zip.length + 32);
    storage.set(zip, 16);
    const output = await decompressStep(storage.subarray(16, 16 + zip.length), "part.stpz", null);
    expect(output.byteLength).toBe(144 * 1024 * 1024);
    expect(output instanceof Uint8Array).toBe(true);
  });

  it("rejects oversized ZIP directory claims before successful completion", async () => {
    const zip = zipSync({ "part.stp": strToU8("STEP data") });
    const view = new DataView(zip.buffer);
    for (let offset = 0; offset < zip.length - 4; offset++) {
      if (view.getUint32(offset, true) === 0x02014b50) {
        view.setUint32(offset + 24, 512 * 1024 * 1024 + 1, true);
        break;
      }
    }
    await expect(decompressStep(zip.buffer as ArrayBuffer, "part.stpz", null)).rejects.toThrow("512 MiB");
  });
});

function streamed(bytes: Uint8Array<ArrayBuffer>, filename: string, chunkSize: number): string {
  const chunks: Uint8Array<ArrayBuffer>[] = [];
  const decoder = new StepDecompressor(filename, (chunk) => chunks.push(chunk));
  for (let offset = 0; offset < bytes.length; offset += chunkSize)
    decoder.push(bytes.subarray(offset, offset + chunkSize));
  decoder.finish();
  return Buffer.concat(chunks).toString("utf8");
}

function zip64(bytes: Uint8Array<ArrayBuffer>): Uint8Array<ArrayBuffer> {
  const original = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const oldEnd = bytes.length - 22;
  const directoryOffset = original.getUint32(oldEnd + 16, true);
  const extraOffset = directoryOffset + 46 + original.getUint16(directoryOffset + 28, true);
  const result = new Uint8Array(bytes.length + 28 + 76);
  result.set(bytes.subarray(0, extraOffset));
  result.set(bytes.subarray(extraOffset, oldEnd), extraOffset + 28);
  const view = new DataView(result.buffer);
  view.setUint16(directoryOffset + 30, 28, true);
  view.setUint32(directoryOffset + 20, 0xffffffff, true);
  view.setUint32(directoryOffset + 24, 0xffffffff, true);
  view.setUint32(directoryOffset + 42, 0xffffffff, true);
  view.setUint16(extraOffset, 1, true);
  view.setUint16(extraOffset + 2, 24, true);
  view.setBigUint64(extraOffset + 4, BigInt(original.getUint32(directoryOffset + 24, true)), true);
  view.setBigUint64(extraOffset + 12, BigInt(original.getUint32(directoryOffset + 20, true)), true);
  view.setBigUint64(extraOffset + 20, 0n, true);
  const end64 = oldEnd + 28;
  view.setUint32(end64, 0x06064b50, true);
  view.setBigUint64(end64 + 4, 44n, true);
  view.setUint16(end64 + 12, 45, true);
  view.setUint16(end64 + 14, 45, true);
  view.setBigUint64(end64 + 24, 1n, true);
  view.setBigUint64(end64 + 32, 1n, true);
  view.setBigUint64(end64 + 40, BigInt(oldEnd - directoryOffset + 28), true);
  view.setBigUint64(end64 + 48, BigInt(directoryOffset), true);
  view.setUint32(end64 + 56, 0x07064b50, true);
  view.setBigUint64(end64 + 64, BigInt(end64), true);
  view.setUint32(end64 + 72, 1, true);
  result.set(bytes.subarray(oldEnd), end64 + 76);
  view.setUint16(end64 + 76 + 8, 0xffff, true);
  view.setUint16(end64 + 76 + 10, 0xffff, true);
  view.setUint32(end64 + 76 + 12, 0xffffffff, true);
  view.setUint32(end64 + 76 + 16, 0xffffffff, true);
  return result;
}

describe("streaming archive boundaries", () => {
  it("delivers decoded data before archive completion across every byte boundary", () => {
    for (const bytes of [
      zipSync({ "part.step": strToU8("STEP streamed data") }),
      Uint8Array.from(gzipSync("STEP streamed data")),
    ]) {
      for (const size of [1, 2, 3, 7, 31, 1024]) expect(streamed(bytes, "part.stpz", size)).toBe("STEP streamed data");
      let received = "";
      const decoder = new StepDecompressor("part.stpz", (chunk) => {
        received += new TextDecoder().decode(chunk);
      });
      decoder.push(bytes.subarray(0, bytes.length - 8));
      expect(received).toBe("STEP streamed data");
      decoder.push(bytes.subarray(bytes.length - 8));
      decoder.finish();
    }
  });

  it("preserves gzip member trailers across a long next-member filename", () => {
    const first = gzipSync("first ");
    const last = gzipSync("last");
    const filename = Buffer.alloc(8000, 97);
    const header = Buffer.from(last.subarray(0, 10));
    header.writeUInt8(header.readUInt8(3) | 8, 3);
    const bytes = Uint8Array.from(Buffer.concat([first, header, filename, Buffer.from([0]), last.subarray(10)]));
    for (const size of [1, 17, 1024, 65536]) expect(streamed(bytes, "part.stpz", size)).toBe("first last");
    flipByte(bytes, first.length - 8);
    expect(() => streamed(bytes, "part.stpz", 17)).toThrow("CRC checksum mismatch");
  });

  it("validates empty concatenated gzip members and rejects a truncated final trailer", () => {
    const bytes = Uint8Array.from(Buffer.concat([gzipSync(""), gzipSync("middle"), gzipSync("")]));
    expect(streamed(bytes, "part.stpz", 1)).toBe("middle");
    for (let missing = 1; missing <= 8; missing++)
      expect(() => streamed(bytes.subarray(0, bytes.length - missing), "part.stpz", 1)).toThrow();
  });

  it("requires final ZIP metadata even after all model data has been delivered", () => {
    const bytes = zipSync({ "part.step": strToU8("STEP data") });
    let size = 0;
    const decoder = new StepDecompressor("part.stpz", (chunk) => {
      size += chunk.length;
    });
    decoder.push(bytes.subarray(0, bytes.length - 22));
    expect(size).toBe(9);
    expect(() => decoder.finish()).toThrow();
    for (const delta of [8, 10, 12, 16]) {
      const corrupt = bytes.slice();
      flipByte(corrupt, corrupt.length - 22 + delta);
      expect(() => streamed(corrupt, "part.stpz", 1)).toThrow();
    }
  });

  it("supports streamed ZIP data descriptors without trusting local size fields", () => {
    const chunks: Uint8Array<ArrayBuffer>[] = [];
    const archive = new Zip((error, chunk) => {
      if (error) throw error;
      chunks.push(chunk);
    });
    const file = new ZipDeflate("part.step");
    archive.add(file);
    file.push(strToU8("streamed descriptor"), true);
    archive.end();
    const bytes = Uint8Array.from(Buffer.concat(chunks));
    for (const size of [1, 2, 17, 1024]) expect(streamed(bytes, "part.stpz", size)).toBe("streamed descriptor");
    const corrupt = bytes.slice();
    for (let offset = 0; offset < corrupt.length - 4; offset++) {
      if (new DataView(corrupt.buffer).getUint32(offset, true) === 0x02014b50) {
        flipByte(corrupt, offset + 16);
        break;
      }
    }
    expect(() => streamed(corrupt, "part.stpz", 1)).toThrow("CRC checksum mismatch");
  });

  it("validates ZIP64 directory offsets, sizes, and entry metadata", () => {
    const bytes = zip64(zipSync({ "part.step": strToU8("ZIP64 model") }));
    for (const size of [1, 3, 1024]) expect(streamed(bytes, "part.stpz", size)).toBe("ZIP64 model");
    const corrupt = bytes.slice();
    const locator = corrupt.length - 22 - 20;
    flipByte(corrupt, locator + 8);
    expect(() => streamed(corrupt, "part.stpz", 1)).toThrow();
  });

  it("handles directories larger than the EOCD search suffix and ignores unsupported metadata compression", () => {
    const entries: Record<string, Uint8Array<ArrayBuffer>> = {};
    for (let index = 0; index < 1500; index++) entries[`metadata-${index}.txt`] = new Uint8Array(0);
    entries["part.step"] = strToU8("large directory");
    expect(streamed(zipSync(entries), "part.stpz", 997)).toBe("large directory");
    const bytes = zipSync({ "ignored.bin": strToU8("metadata"), "part.step": strToU8("model") });
    const view = new DataView(bytes.buffer);
    view.setUint16(8, 99, true);
    for (let offset = 0; offset < bytes.length - 4; offset++) {
      if (view.getUint32(offset, true) === 0x02014b50) {
        view.setUint16(offset + 10, 99, true);
        break;
      }
    }
    expect(streamed(bytes, "part.stpz", 1)).toBe("model");
  });

  it("does not trust a central-directory signature embedded in stored payload bytes", () => {
    const model = "before PK\u0001\u0002 after";
    expect(streamed(zipSync({ "part.step": strToU8(model) }, { level: 0 }), "part.stpz", 1)).toBe(model);
  });

  it("bounds compressed input independently of expansion and rejects pushes after finish", () => {
    const chunk = new Uint8Array(1024 * 1024);
    let plainSize = 0;
    const plain = new StepDecompressor("part.step", (data) => {
      plainSize += data.length;
    });
    for (let index = 0; index < 65; index++) plain.push(chunk);
    plain.finish();
    expect(plainSize).toBe(65 * 1024 * 1024);
    const decoder = new StepDecompressor("part.stpz", () => undefined);
    // A stored metadata entry exercises the compressed-input limit without
    // retaining or inflating a giant model in the test.
    const header = new Uint8Array(41);
    const view = new DataView(header.buffer);
    view.setUint32(0, 0x04034b50, true);
    view.setUint32(18, 64 * 1024 * 1024, true);
    view.setUint32(22, 64 * 1024 * 1024, true);
    view.setUint16(26, 11, true);
    header.set(strToU8("ignored.bin"), 30);
    decoder.push(header);
    for (let index = 0; index < 63; index++) decoder.push(chunk);
    expect(() => decoder.push(chunk)).toThrow("64 MiB");
    const complete = new StepDecompressor("part.step", () => undefined);
    complete.finish();
    expect(() => complete.push(new Uint8Array(0))).toThrow("finished");
  });
});
