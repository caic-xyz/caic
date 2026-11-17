// Tests optional-code failure tagging while preserving loaded values and original rejection details.

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import { CodeLoadError, loadCode } from "./codeLoad";

describe("loadCode", () => {
  it("returns the loaded module without changing its identity", async () => {
    const module = { default: () => "Loaded" };
    expect(await loadCode(Promise.resolve(module))).toBe(module);
  });

  for (const cause of [new TypeError("Failed to fetch module"), "module evaluation failed", undefined]) {
    it(`tags an import rejection and preserves its ${typeof cause} cause`, async () => {
      await assert.rejects(loadCode(Promise.reject(cause)), (error: unknown) => {
        assert(error instanceof CodeLoadError);
        expect(error.cause).toBe(cause);
        return true;
      });
    });
  }
});

describe("CodeLoadError recovery", () => {
  for (const message of [
    "Failed to fetch dynamically imported module",
    "Failed to fetch dynamically imported module: https://caic.example/assets/view.js",
    "error loading dynamically imported module: https://caic.example/assets/view.js",
    "Importing a module script failed",
    "Importing a module script failed.",
  ]) {
    it(`permits an in-place retry for native fetch TypeError: ${message}`, () => {
      expect(new CodeLoadError(new TypeError(message)).canRetry).toBe(true);
    });
  }

  for (const [name, cause] of [
    ["evaluation TypeError", new TypeError("Module evaluation failed")],
    ["stylesheet preload", new Error("Unable to preload CSS for https://caic.example/assets/view.css")],
    ["non-TypeError with fetch-like message", new Error("Failed to fetch dynamically imported module")],
    ["unrecognized fetch-like TypeError", new TypeError("Failed to fetch module")],
    ["primitive fetch-like rejection", "Failed to fetch dynamically imported module"],
    ["undefined rejection", undefined],
  ] as const) {
    it(`requires reload for ${name}`, () => {
      expect(new CodeLoadError(cause).canRetry).toBe(false);
    });
  }
});
