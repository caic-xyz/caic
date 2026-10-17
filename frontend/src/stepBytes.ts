// Retains decoded STEP in bounded pages for random entity lookup without whole-file copies.

const pageSize = 256 * 1024;
const maxBytes = 512 * 1024 * 1024;

export class StepBytes {
  private pages: Uint8Array<ArrayBuffer>[] = [];
  private size = 0;
  private readonly decoder = new TextDecoder();

  get length(): number {
    return this.size;
  }

  append(chunk: Uint8Array) {
    if (this.size + chunk.length > maxBytes) throw new Error("Decompressed STEP file exceeds 512 MiB");
    let offset = 0;
    while (offset < chunk.length) {
      const position = this.size % pageSize;
      if (!position) this.pages.push(new Uint8Array(pageSize));
      const count = Math.min(pageSize - position, chunk.length - offset);
      this.pages[this.pages.length - 1].set(chunk.subarray(offset, offset + count), position);
      offset += count;
      this.size += count;
    }
  }

  byte(offset: number): number {
    return this.pages[Math.floor(offset / pageSize)][offset % pageSize];
  }

  text(start: number, end: number): string {
    if (end <= start) return "";
    const first = Math.floor(start / pageSize);
    if (first === Math.floor((end - 1) / pageSize))
      return this.decoder.decode(this.pages[first].subarray(start % pageSize, ((end - 1) % pageSize) + 1));
    const parts: string[] = [];
    for (let offset = start; offset < end;) {
      const count = Math.min(pageSize - (offset % pageSize), end - offset);
      parts.push(
        this.decoder.decode(
          this.pages[Math.floor(offset / pageSize)].subarray(offset % pageSize, (offset % pageSize) + count),
          { stream: true },
        ),
      );
      offset += count;
    }
    parts.push(this.decoder.decode());
    return parts.join("");
  }

  release() {
    this.pages = [];
  }

  // Only OCCT requires contiguous input. Relinquish pages before its WASM heap
  // and numeric geometry are allocated; direct imports never make this copy.
  takeContiguous(): Uint8Array<ArrayBuffer> {
    const result = new Uint8Array(this.size);
    for (let index = 0; index < this.pages.length; index++)
      result.set(this.pages[index].subarray(0, Math.min(pageSize, this.size - index * pageSize)), index * pageSize);
    this.release();
    return result;
  }
}
