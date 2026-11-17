// Tests frontend diagnostic reports, original rejection details, and cyclic causes.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import { formatErrorReport } from "./errorReport";
import { CodeLoadError } from "./codeLoad";

describe("formatErrorReport", () => {
  it("includes the stack and browser state needed to diagnose a resumed client", () => {
    const error = new RangeError("Maximum call stack size exceeded");
    error.stack = "RangeError: Maximum call stack size exceeded\n    at renderTask";

    expect(
      formatErrorReport(error, {
        occurredAt: new Date("2026-05-06T12:34:56.000Z"),
        url: "https://quick.caic.xyz/task/@task-123",
        online: false,
        visibilityState: "visible",
        serviceWorkerURL: "https://quick.caic.xyz/sw.js",
        userAgent: "test-browser",
      }),
    ).toBe(
      [
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
      ].join("\n"),
    );
  });
});

it("keeps the original module stack and primitive causes in diagnostic details", () => {
  const original = new Error("Module evaluation failed", { cause: "invalid deployed asset" });
  original.stack = "Error: Module evaluation failed\n    at broken-module.js:12";
  const report = formatErrorReport(new CodeLoadError(original), {
    occurredAt: new Date("2026-11-17T12:00:00Z"),
    url: "https://caic.example/settings",
    online: true,
    visibilityState: "visible",
    serviceWorkerURL: "none",
    userAgent: "test-browser",
  });
  expect(report).toContain("CodeLoadError: Page content could not be loaded.");
  expect(report).toContain("Caused by:\nError: Module evaluation failed\n    at broken-module.js:12");
  expect(report).toContain("Caused by:\ninvalid deployed asset");
});

it("stops diagnostic formatting at circular error causes", () => {
  const error = new Error("Circular failure");
  error.cause = error;
  const report = formatErrorReport(error, {
    occurredAt: new Date("2026-11-17T12:00:00Z"),
    url: "https://caic.example/",
    online: true,
    visibilityState: "visible",
    serviceWorkerURL: "none",
    userAgent: "test-browser",
  });
  expect(report).toContain("[Circular error cause]");
});

it("includes an explicitly undefined rejection cause", () => {
  const report = formatErrorReport(new CodeLoadError(undefined), {
    occurredAt: new Date("2026-11-17T12:00:00Z"),
    url: "https://caic.example/settings",
    online: true,
    visibilityState: "visible",
    serviceWorkerURL: "none",
    userAgent: "test-browser",
  });
  expect(report).toContain("Caused by:\nundefined");
});
