// Tests paged STEP byte ownership and UTF-8 record decoding across page boundaries.
import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import { StepBytes } from "./stepBytes";

describe("StepBytes", () => {
  it("decodes UTF-8 split across pages and returns exact contiguous bytes only when requested", () => {
    const text = "x".repeat(256 * 1024 - 1) + "éΩ" + "z".repeat(256 * 1024);
    const source = new TextEncoder().encode(text);
    const bytes = new StepBytes();
    for (let offset = 0; offset < source.length; offset += 1237) bytes.append(source.subarray(offset, offset + 1237));
    expect(bytes.length).toBe(source.length);
    expect(bytes.text(256 * 1024 - 2, 256 * 1024 + 4)).toBe("xéΩz");
    expect(bytes.text(0, source.length)).toBe(text);
    expect(bytes.takeContiguous()).toEqual(source);
  });
});
