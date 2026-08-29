// Tests for Go Mode host-mode detection.

import { describe, expect, it } from "vitest";

import { hasHostModeQuery, nativeVoiceConnected } from "./HostMode";

describe("hasHostModeQuery", () => {
  it("is false in the normal browser path", () => {
    expect(hasHostModeQuery({})).toBe(false);
  });

  it("detects the WebView query marker", () => {
    expect(hasHostModeQuery({ goModeHost: "1" })).toBe(true);
  });

  it("ignores unrelated query values", () => {
    expect(hasHostModeQuery({ goModeHost: "0" })).toBe(false);
  });

  it("reads voice state only from the native host bridge", () => {
    expect(nativeVoiceConnected(undefined)).toBe(false);
    expect(nativeVoiceConnected({})).toBe(false);
    expect(nativeVoiceConnected({ isVoiceConnected: () => false })).toBe(false);
    expect(nativeVoiceConnected({ isVoiceConnected: () => true })).toBe(true);
  });
});
