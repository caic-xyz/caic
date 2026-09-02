// Tests duration parsing and formatting for user-configurable settings.

import { describe, expect, it } from "vitest";

import { formatDuration, parseDuration } from "./duration";

describe("duration settings", () => {
  it("parses compound and fractional durations as nanoseconds", () => {
    expect(parseDuration("1m31s")).toBe(91_000_000_000);
    expect(parseDuration("1.5s")).toBe(1_500_000_000);
    expect(parseDuration("2h3m4s")).toBe(7_384_000_000_000);
  });

  it("rejects malformed or non-positive durations", () => {
    expect(parseDuration("")).toBeNull();
    expect(parseDuration("91")).toBeNull();
    expect(parseDuration("1m 31s")).toBeNull();
    expect(parseDuration("0s")).toBeNull();
  });

  it("formats nanoseconds as compact duration syntax", () => {
    expect(formatDuration(91_000_000_000)).toBe("1m31s");
    expect(formatDuration(5_000_000_000)).toBe("5s");
    expect(formatDuration(1_500_000_000)).toBe("1.5s");
  });
});
