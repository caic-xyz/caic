// Streams bounded ZIP and gzip STEP payloads, validating containers before successful completion.

import { Gunzip, Unzip, UnzipInflate, UnzipPassThrough, strFromU8 } from "three/addons/libs/fflate.module.js";

import { isCompressedStepPath } from "./runtimePath";

const maxInputBytes = 64 * 1024 * 1024;
const maxStepBytes = 512 * 1024 * 1024;
type Bytes = Uint8Array<ArrayBuffer>;
type Sink = (chunk: Bytes) => void;

// Synchronous delivery means the caller completes indexing before pulling more
// network input. No decoded queue or complete compressed archive is required.
export class StepDecompressor {
  private readonly compressed: boolean;
  private readonly ondata: Sink;
  private decoder: ZipStep | GzipStep | null = null;
  private prefix = new Uint8Array(0);
  private inputSize = 0;
  private outputSize = 0;
  private finished = false;

  constructor(filename: string, ondata: Sink) {
    this.compressed = isCompressedStepPath(filename);
    this.ondata = ondata;
  }

  push(chunk: Bytes): void {
    if (this.finished) throw new Error("STEP decompression has finished");
    this.inputSize += chunk.length;
    if (this.compressed && this.inputSize > maxInputBytes) throw new Error("STEP file exceeds 64 MiB download limit");
    if (!this.compressed) {
      this.emit(chunk);
      return;
    }
    if (this.decoder === null) {
      const needed = Math.min(2 - this.prefix.length, chunk.length);
      const prefix = new Uint8Array(this.prefix.length + needed);
      prefix.set(this.prefix);
      prefix.set(chunk.subarray(0, needed), this.prefix.length);
      this.prefix = prefix;
      chunk = chunk.subarray(needed);
      if (prefix.length < 2) return;
      if (prefix[0] === 0x1f && prefix[1] === 0x8b) this.decoder = new GzipStep((data) => this.emit(data));
      else if (prefix[0] === 0x50 && prefix[1] === 0x4b) this.decoder = new ZipStep((data) => this.emit(data));
      else throw new Error("Invalid compressed STEP file: expected ZIP or gzip");
      this.decoder.push(prefix);
      this.prefix = new Uint8Array(0);
    }
    // An adversarial high-ratio block must not grow a temporary inflate buffer
    // from a large network chunk before the decoded-size guard can run.
    for (let offset = 0; offset < chunk.length; offset += 1024)
      this.decoder.push(chunk.subarray(offset, offset + 1024));
  }

  finish(): void {
    if (this.finished) throw new Error("STEP decompression has finished");
    this.finished = true;
    if (!this.compressed) return;
    if (this.decoder === null) throw new Error("Incomplete compressed STEP file");
    this.decoder.finish();
  }

  private emit(chunk: Bytes): void {
    this.outputSize += chunk.length;
    if (this.outputSize > maxStepBytes) throw new Error("Decompressed STEP file exceeds 512 MiB");
    if (chunk.length) this.ondata(chunk);
  }
}

// A retained compressed suffix with absolute offsets. Individual buffers stay
// chunked, so validating metadata never requires copying the archive wholesale.
class History {
  private chunks: { start: number; bytes: Bytes }[] = [];
  private first = 0;
  end = 0;

  push(bytes: Bytes): void {
    if (bytes.length) this.chunks.push({ start: this.end, bytes });
    this.end += bytes.length;
  }

  discardBefore(offset: number): void {
    while (
      this.first < this.chunks.length &&
      this.chunks[this.first].start + this.chunks[this.first].bytes.length <= offset
    ) {
      this.first++;
    }
    if (this.first > 128 && this.first > this.chunks.length / 2) {
      this.chunks = this.chunks.slice(this.first);
      this.first = 0;
    }
  }

  byte(offset: number): number {
    let low = this.first;
    let high = this.chunks.length - 1;
    while (low <= high) {
      const middle = (low + high) >>> 1;
      const chunk = this.chunks[middle];
      if (offset < chunk.start) high = middle - 1;
      else if (offset >= chunk.start + chunk.bytes.length) low = middle + 1;
      else return chunk.bytes[offset - chunk.start];
    }
    throw new Error("Invalid compressed STEP metadata offset");
  }

  uint16(offset: number): number {
    return this.byte(offset) | (this.byte(offset + 1) << 8);
  }

  uint32(offset: number): number {
    return (this.uint16(offset) | (this.uint16(offset + 2) << 16)) >>> 0;
  }

  uint64(offset: number): number {
    const value = this.uint32(offset) + this.uint32(offset + 4) * 0x100000000;
    if (!Number.isSafeInteger(value)) throw new Error("Invalid ZIP64 offset or size");
    return value;
  }

  bytes(offset: number, length: number): Bytes {
    const result = new Uint8Array(length);
    for (let index = 0; index < length; index++) result[index] = this.byte(offset + index);
    return result;
  }
}

class GzipStep {
  private readonly inflater: Gunzip;
  private readonly history = new History();
  private size = 0;
  private crc = 0xffffffff;
  private produced = false;
  private lastMember = 0;

  constructor(ondata: Sink) {
    this.inflater = new Gunzip((chunk) => {
      this.size += chunk.length;
      if (this.size > maxStepBytes) throw new Error("Decompressed STEP file exceeds 512 MiB");
      if (chunk.length) this.produced = true;
      this.crc = updateCrc(this.crc, chunk);
      ondata(chunk);
    });
    this.inflater.onmember = (end) => this.finishMember(end);
  }

  push(chunk: Bytes): void {
    const start = this.history.end;
    this.history.push(chunk);
    this.produced = false;
    this.inflater.push(chunk, false);
    // onmember arrives after the NEXT header is complete. Retain the suffix
    // since the last push producing output, including its preceding trailer.
    // Long optional headers or empty deflate blocks can retain up to the 64 MiB
    // input bound; ordinary input retains approximately one chunk plus 8 bytes.
    if (this.produced) this.history.discardBefore(start - 8);
  }

  finish(): void {
    if (this.history.end < 18) throw new Error("Incomplete gzip STEP file");
    // Gunzip does not expose the last member boundary and does not reject a
    // shortened all-zero trailer. A known empty member forces its public
    // onmember callback to report the exact preceding boundary. It must equal
    // the real input length; no synthetic bytes may complete a broken member.
    const end = this.history.end;
    const sentinel = new Uint8Array([31, 139, 8, 0, 0, 0, 0, 0, 0, 3, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0]);
    this.history.push(sentinel);
    this.inflater.push(sentinel, false);
    if (this.lastMember !== end) throw new Error("Incomplete gzip STEP file");
    this.inflater.push(new Uint8Array(0), true);
  }

  private finishMember(end: number): void {
    if (end < this.lastMember + 18 || end > this.history.end) throw new Error("Invalid gzip member trailer");
    const expectedSize = this.history.uint32(end - 4);
    if (expectedSize > maxStepBytes) throw new Error("Decompressed STEP file exceeds 512 MiB");
    if (this.size !== expectedSize) throw new Error("Invalid gzip STEP size");
    if ((this.crc ^ 0xffffffff) >>> 0 !== this.history.uint32(end - 8)) {
      throw new Error("Corrupt gzip STEP file: CRC checksum mismatch");
    }
    this.lastMember = end;
    this.size = 0;
    this.crc = 0xffffffff;
  }
}

// Start ignored entries with a decoder that consumes compressed input directly.
// Leaving UnzipFile.start() uncalled makes fflate retain that entry's chunks.
class Discard {
  static compression = 0;
  ondata: InstanceType<typeof UnzipPassThrough>["ondata"] = () => undefined;

  push(_chunk: Uint8Array, final: boolean): void {
    if (final) this.ondata(null, new Uint8Array(0), true);
  }
}

class ZipStep {
  private readonly archive: Unzip;
  private readonly history = new History();
  private directoryStart: number | null = null;
  private signature = 0;
  private name: string | null = null;
  private compression = 0;
  private declaredSize: number | null = null;
  private declaredCompressedSize: number | null = null;
  private size = 0;
  private crc = 0xffffffff;
  private complete = false;

  constructor(ondata: Sink) {
    this.archive = new Unzip((file) => {
      if (!/\.(step|stp|p21)$/i.test(file.name)) {
        Discard.compression = file.compression;
        this.archive.register(Discard);
        file.ondata = (error) => {
          if (error) throw error;
        };
        file.start();
        return;
      }
      if (this.name !== null) throw new Error("STEP archive contains multiple models");
      this.name = file.name;
      this.compression = file.compression;
      this.declaredSize = file.originalSize ?? null;
      this.declaredCompressedSize = file.size ?? null;
      if (this.declaredSize !== null && this.declaredSize > maxStepBytes) {
        throw new Error("Decompressed STEP file exceeds 512 MiB");
      }
      this.archive.register(UnzipInflate);
      this.archive.register(UnzipPassThrough);
      file.ondata = (error, chunk, final) => {
        if (error) throw error;
        this.size += chunk.length;
        if (this.size > maxStepBytes) throw new Error("Decompressed STEP file exceeds 512 MiB");
        if (this.declaredSize !== null && this.size > this.declaredSize)
          throw new Error("Invalid STEP archive model size");
        this.crc = updateCrc(this.crc, chunk);
        ondata(chunk);
        this.complete = final;
      };
      file.start();
    });
  }

  push(chunk: Bytes): void {
    const start = this.history.end;
    this.history.push(chunk);
    for (let index = 0; index < chunk.length; index++) {
      this.signature = ((this.signature >>> 8) | (chunk[index] << 24)) >>> 0;
      if (this.directoryStart === null && this.signature === 0x02014b50) this.directoryStart = start + index - 3;
    }
    // Keep enough suffix for EOCD's maximum comment. Once a possible central
    // signature appears, retain it until the authoritative EOF directory offset
    // validates it. A false signature in compressed bytes can retain up to the
    // 64 MiB input limit; normal archives retain only their directory and tail.
    this.history.discardBefore(this.directoryStart ?? this.history.end - 65558);
    this.archive.push(chunk, false);
  }

  finish(): void {
    this.archive.push(new Uint8Array(0), true);
    const directory = this.directory();
    let offset = directory.offset;
    let modelCount = 0;
    for (let entry = 0; entry < directory.count; entry++) {
      if (offset + 46 > directory.end || this.history.uint32(offset) !== 0x02014b50)
        throw new Error("Invalid ZIP directory");
      const flags = this.history.uint16(offset + 8);
      const compression = this.history.uint16(offset + 10);
      const checksum = this.history.uint32(offset + 16);
      let compressedSize = this.history.uint32(offset + 20);
      let size = this.history.uint32(offset + 24);
      const nameLength = this.history.uint16(offset + 28);
      const extraLength = this.history.uint16(offset + 30);
      const commentLength = this.history.uint16(offset + 32);
      let disk = this.history.uint16(offset + 34);
      let local = this.history.uint32(offset + 42);
      const extra = offset + 46 + nameLength;
      const next = extra + extraLength + commentLength;
      if (next > directory.end) throw new Error("Invalid ZIP directory");
      if (compressedSize === 0xffffffff || size === 0xffffffff || local === 0xffffffff || disk === 0xffff) {
        let cursor = extra;
        let found = false;
        while (cursor + 4 <= extra + extraLength) {
          const tag = this.history.uint16(cursor);
          const length = this.history.uint16(cursor + 2);
          const end = cursor + 4 + length;
          if (end > extra + extraLength) throw new Error("Invalid ZIP64 extra field");
          if (tag === 1) {
            cursor += 4;
            const read = () => {
              if (cursor + 8 > end) throw new Error("Invalid ZIP64 extra field");
              const value = this.history.uint64(cursor);
              cursor += 8;
              return value;
            };
            if (size === 0xffffffff) size = read();
            if (compressedSize === 0xffffffff) compressedSize = read();
            if (local === 0xffffffff) local = read();
            if (disk === 0xffff) {
              if (cursor + 4 > end) throw new Error("Invalid ZIP64 extra field");
              disk = this.history.uint32(cursor);
            }
            found = true;
            break;
          }
          cursor = end;
        }
        if (!found) throw new Error("Missing ZIP64 extra field");
      }
      if (disk !== 0 || local >= directory.offset || compressedSize > directory.offset - local)
        throw new Error("Invalid ZIP entry offset");
      const name = strFromU8(this.history.bytes(offset + 46, nameLength), !(flags & 2048));
      if (/\.(step|stp|p21)$/i.test(name)) {
        if (++modelCount > 1) throw new Error("STEP archive contains multiple models");
        if (size > maxStepBytes || compressedSize > maxStepBytes)
          throw new Error("Decompressed STEP file exceeds 512 MiB");
        if (flags & 1) throw new Error("Encrypted STEP archives are unsupported");
        if (name !== this.name || compression !== this.compression) throw new Error("Invalid STEP archive model entry");
        if (
          size !== this.size ||
          (this.declaredSize !== null && this.declaredSize !== size) ||
          (this.declaredCompressedSize !== null && this.declaredCompressedSize !== compressedSize)
        )
          throw new Error("Invalid STEP archive model size");
        if ((this.crc ^ 0xffffffff) >>> 0 !== checksum) throw new Error("Corrupt STEP archive: CRC checksum mismatch");
      }
      offset = next;
    }
    if (offset !== directory.end) throw new Error("Invalid ZIP directory size");
    if (modelCount === 0) throw new Error("STEP archive contains no .step, .stp, or .p21 model");
    if (!this.complete) throw new Error("Incomplete STEP archive or invalid model size");
  }

  private directory(): { offset: number; end: number; count: number } {
    const size = this.history.end;
    for (let end = size - 22; end >= Math.max(0, size - 65558); end--) {
      if (this.history.uint32(end) !== 0x06054b50) continue;
      if (end + 22 + this.history.uint16(end + 20) !== size) continue;
      if (this.history.uint16(end + 4) !== 0 || this.history.uint16(end + 6) !== 0)
        throw new Error("Multidisk ZIP archives are unsupported");
      let count = this.history.uint16(end + 10);
      let length = this.history.uint32(end + 12);
      let offset = this.history.uint32(end + 16);
      let directoryEnd = end;
      if (count === 0xffff || length === 0xffffffff || offset === 0xffffffff) {
        const locator = end - 20;
        if (
          locator < 0 ||
          this.history.uint32(locator) !== 0x07064b50 ||
          this.history.uint32(locator + 4) !== 0 ||
          this.history.uint32(locator + 16) !== 1
        )
          throw new Error("Invalid ZIP64 directory");
        const zip64 = this.history.uint64(locator + 8);
        if (
          zip64 + 56 > locator ||
          this.history.uint32(zip64) !== 0x06064b50 ||
          zip64 + 12 + this.history.uint64(zip64 + 4) !== locator
        )
          throw new Error("Invalid ZIP64 directory");
        if (this.history.uint32(zip64 + 16) !== 0 || this.history.uint32(zip64 + 20) !== 0)
          throw new Error("Multidisk ZIP archives are unsupported");
        count = this.history.uint64(zip64 + 32);
        if (this.history.uint64(zip64 + 24) !== count) throw new Error("Invalid ZIP64 entry count");
        length = this.history.uint64(zip64 + 40);
        offset = this.history.uint64(zip64 + 48);
        directoryEnd = zip64;
      } else if (this.history.uint16(end + 8) !== count) throw new Error("Invalid ZIP entry count");
      if (offset + length !== directoryEnd || count > Math.floor(length / 46)) throw new Error("Invalid ZIP directory");
      return { offset, end: directoryEnd, count };
    }
    throw new Error("Invalid ZIP directory");
  }
}

// CRC-32/ISO-HDLC, required by ZIP entries and each gzip member.
const crcTable = Uint32Array.from({ length: 256 }, (_, index) => {
  let crc = index;
  for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0);
  return crc >>> 0;
});

function updateCrc(crc: number, chunk: Uint8Array): number {
  for (const byte of chunk) crc = (crc >>> 8) ^ crcTable[(crc ^ byte) & 0xff];
  return crc;
}
