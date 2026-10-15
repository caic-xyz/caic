// Tests compressed STEP selection, container detection, corruption, and expansion limits.

import { describe, it } from "node:test";
import { gzipSync } from "node:zlib";
import { zipSync, strToU8 } from "three/addons/libs/fflate.module.js";
import { expect } from "@tests/expect";
import { decompressStep } from "./stepCompression";

describe("decompressStep", () => {
  it("preserves plain STEP buffers", async () => {
    const buffer = new ArrayBuffer(0);
    expect(await decompressStep(buffer, "part.step", null)).toBe(buffer);
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
      corrupt[trailer] ^= 1;
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
    gzip[gzip.length - 8] ^= 1;
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
    zip[dataOffset] ^= 1;
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

  it("rejects oversized ZIP directory claims before inflating", async () => {
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
