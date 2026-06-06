// Tests for Go Mode host-mode detection.
import { describe, expect, it } from "vitest";
import { hasHostModeQuery } from "./hostMode";

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
});
