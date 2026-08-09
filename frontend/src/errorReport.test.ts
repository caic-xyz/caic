// Tests for frontend error diagnostic report formatting.

import { describe, expect, it } from "vitest";

import { formatErrorReport } from "./errorReport";

describe("formatErrorReport", () => {
  it("includes the stack and browser state needed to diagnose a resumed client", () => {
    const error = new RangeError("Maximum call stack size exceeded");
    error.stack = "RangeError: Maximum call stack size exceeded\n    at renderTask";

    expect(formatErrorReport(error, {
      occurredAt: new Date("2026-05-06T12:34:56.000Z"),
      url: "https://quick.caic.xyz/task/@task-123",
      online: false,
      visibilityState: "visible",
      serviceWorkerURL: "https://quick.caic.xyz/sw.js",
      userAgent: "test-browser",
    })).toBe([
      "caic frontend error report",
      "Occurred at: 2026-05-06T12:34:56.000Z",
      "URL: https://quick.caic.xyz/task/@task-123",
      "Online: no",
      "Visibility: visible",
      "Service worker: https://quick.caic.xyz/sw.js",
      "User agent: test-browser",
      "",
      "RangeError: Maximum call stack size exceeded",
      "    at renderTask",
    ].join("\n"));
  });
});
