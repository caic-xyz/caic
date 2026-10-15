// Decodes bounded ZIP and gzip STEP files before geometry import.

import { Gunzip, Unzip, UnzipInflate, unzipSync } from "three/addons/libs/fflate.module.js";
import { isCompressedStepPath } from "./runtimePath";

// Bound allocations independently of the archive's compression ratio.
const maxStepBytes = 512 * 1024 * 1024;

export async function decompressStep(
  buffer: ArrayBuffer | Uint8Array<ArrayBuffer>,
  filename: string,
  progress: ((completed: number, total: number) => void) | null,
): Promise<ArrayBuffer | Uint8Array<ArrayBuffer>> {
  if (!isCompressedStepPath(filename)) return buffer;
  const bytes = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
  // Identify the container by its signature, including gzip-based .stpz files.
  if (bytes[0] === 0x1f && bytes[1] === 0x8b) {
    // Count and verify every member before allocating one exact-sized output.
    // The final trailer only gives the last member's size, not their total.
    const gzipSize = inflateGzip(bytes, null, progress);
    const gzipResult = new Uint8Array(gzipSize);
    inflateGzip(bytes, gzipResult, progress);
    return gzipResult;
  }
  if (bytes[0] !== 0x50 || bytes[1] !== 0x4b) throw new Error("Invalid compressed STEP file: expected ZIP or gzip");

  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let directoryOffset = zipDirectoryOffset(view);
  let expectedCrc = 0;
  let name: string | null = null;
  let expectedSize = 0;
  // Inspect the directory before allocating any decompressed data. A single
  // model is supported; split STEP assemblies need external-reference support.
  unzipSync(bytes, {
    filter: (file) => {
      if (view.getUint32(directoryOffset, true) !== 0x02014b50) throw new Error("Invalid ZIP directory");
      const checksum = view.getUint32(directoryOffset + 16, true);
      directoryOffset +=
        46 +
        view.getUint16(directoryOffset + 28, true) +
        view.getUint16(directoryOffset + 30, true) +
        view.getUint16(directoryOffset + 32, true);
      // STEP ZIP roots commonly use ISO-10303.p21 instead of a CAD extension.
      if (!/\.(step|stp|p21)$/i.test(file.name)) return false;
      if (name !== null) throw new Error("STEP archive contains multiple models");
      if (file.originalSize > maxStepBytes || file.size > maxStepBytes) {
        throw new Error("Decompressed STEP file exceeds 512 MiB");
      }
      name = file.name;
      expectedSize = file.originalSize;
      expectedCrc = checksum;
      return false;
    },
  });
  if (name === null) throw new Error("STEP archive contains no .step, .stp, or .p21 model");
  const result = new Uint8Array(expectedSize);
  let size = 0;
  let complete = false;
  let crc = 0xffffffff;
  let matches = 0;
  const archive = new Unzip((file) => {
    if (file.name !== name) return;
    if (++matches > 1) throw new Error("STEP archive contains multiple models");
    file.ondata = (error, chunk, final) => {
      if (error) throw error;
      size += chunk.byteLength;
      if (size > maxStepBytes) throw new Error("Decompressed STEP file exceeds 512 MiB");
      if (size > expectedSize) throw new Error("Invalid STEP archive model size");
      result.set(chunk, size - chunk.byteLength);
      for (const byte of chunk) crc = (crc >>> 8) ^ crcTable[(crc ^ byte) & 0xff];

      complete = final;
    };
    file.start();
  });
  archive.register(UnzipInflate);
  // Small input chunks bound temporary inflate allocations even when directory
  // sizes are forged. The running output limit is authoritative.
  for (let inputOffset = 0; inputOffset < bytes.length; inputOffset += 1024) {
    archive.push(bytes.subarray(inputOffset, inputOffset + 1024), inputOffset + 1024 >= bytes.length);
    progress?.(Math.min(inputOffset + 1024, bytes.length), bytes.length);
  }
  if (!complete || size !== expectedSize) throw new Error("Incomplete STEP archive or invalid model size");
  if ((crc ^ 0xffffffff) >>> 0 !== expectedCrc) throw new Error("Corrupt STEP archive: CRC checksum mismatch");
  return result;
}

// CRC-32/ISO-HDLC, as required by ZIP for the uncompressed entry bytes.
const crcTable = Uint32Array.from({ length: 256 }, (_, index) => {
  let crc = index;
  for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0);
  return crc >>> 0;
});

// Locate the central directory, whose CRC is present even for streamed ZIPs
// that leave the local header checksum empty and use a data descriptor.
function zipDirectoryOffset(view: DataView): number {
  for (let end = view.byteLength - 22; end >= Math.max(0, view.byteLength - 65558); end--) {
    if (view.getUint32(end, true) !== 0x06054b50) continue;
    if (end + 22 + view.getUint16(end + 20, true) !== view.byteLength) continue;
    const offset = view.getUint32(end + 16, true);
    if (offset !== 0xffffffff) return offset;
    if (end < 20 || view.getUint32(end - 20, true) !== 0x07064b50) break;
    const zip64 = Number(view.getBigUint64(end - 12, true));
    if (!Number.isSafeInteger(zip64) || zip64 < 0 || zip64 + 56 > view.byteLength) break;
    if (view.getUint32(zip64, true) !== 0x06064b50) break;
    const offset64 = Number(view.getBigUint64(zip64 + 48, true));
    if (!Number.isSafeInteger(offset64) || offset64 < 0 || offset64 >= view.byteLength) break;
    return offset64;
  }
  throw new Error("Invalid ZIP directory");
}

// fflate accepts concatenated gzip members but does not validate their CRCs.
// The first pass counts and verifies; the second fills the only output buffer.
function inflateGzip(
  bytes: Uint8Array<ArrayBuffer>,
  output: Uint8Array | null,
  progress: ((completed: number, total: number) => void) | null,
): number {
  if (bytes.length < 18) throw new Error("Incomplete gzip STEP file");
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let total = 0;
  let memberSize = 0;
  let crc = 0xffffffff;
  function finishMember(end: number) {
    if (end < 8 || end > bytes.length) throw new Error("Invalid gzip member trailer");
    const expectedSize = view.getUint32(end - 4, true);
    if (expectedSize > maxStepBytes) throw new Error("Decompressed STEP file exceeds 512 MiB");
    if (memberSize !== expectedSize) throw new Error("Invalid gzip STEP size");
    if (output === null && (crc ^ 0xffffffff) >>> 0 !== view.getUint32(end - 8, true)) {
      throw new Error("Corrupt gzip STEP file: CRC checksum mismatch");
    }
    memberSize = 0;
    crc = 0xffffffff;
  }
  const inflater = new Gunzip((chunk) => {
    if (total + chunk.length > maxStepBytes) throw new Error("Decompressed STEP file exceeds 512 MiB");
    if (output === null) {
      for (const byte of chunk) crc = (crc >>> 8) ^ crcTable[(crc ^ byte) & 0xff];
    } else {
      output.set(chunk, total);
    }
    total += chunk.length;
    memberSize += chunk.length;
  });
  inflater.onmember = finishMember;
  for (let offset = 0; offset < bytes.length; offset += 1024) {
    inflater.push(bytes.subarray(offset, offset + 1024), false);
    progress?.((output === null ? 0 : bytes.length) + Math.min(offset + 1024, bytes.length), bytes.length * 2);
  }
  // Process all buffered members before finalizing; a final data push in
  // fflate stops after the first member remaining in that push.
  inflater.push(new Uint8Array(0), true);
  finishMember(bytes.length);
  return total;
}
