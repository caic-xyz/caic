// Tests for shared formatting utilities.

import { describe, expect, it } from "vitest";

import { formatBytes, formatElapsed, stateColor, toolCallDetail } from "./formatting";

describe("formatBytes", () => {
  it("formats resident memory using binary units", () => {
    expect(formatBytes(2_097_152)).toBe("2.0 MiB");
  });
});

describe("formatElapsed", () => {
  it("formats process age in whole seconds", () => {
    expect(formatElapsed(90_000)).toBe("1m 30s");
  });
});

describe("stateColor", () => {
  it("uses green for busy task states", () => {
    const busyStates = [
      "pending",
      "branching",
      "provisioning",
      "starting",
      "running",
      "pulling",
      "pushing",
    ] as const;

    for (const state of busyStates) {
      expect(stateColor(state)).toBe("#d4edda");
    }
  });

  it("keeps stopping and purging orange", () => {
    expect(stateColor("stopping")).toBe("#fde2c8");
    expect(stateColor("purging")).toBe("#fde2c8");
  });
});

describe("toolCallDetail", () => {
  it("uses backend-generic agent descriptions", () => {
    const input = { description: "Review the diff" };
    expect(toolCallDetail("Agent", input)).toBe("Review the diff");
    expect(toolCallDetail("subagent", input)).toBe("Review the diff");
  });

  it("summarises Pi path-based file tools", () => {
    expect(toolCallDetail("read", { path: "gomode/docs/SERVER_LIBRARY.md" })).toBe("SERVER_LIBRARY.md");
    expect(toolCallDetail("edit", { path: "gomode/docs/SERVER_LIBRARY.md", edits: [{ oldText: "a", newText: "b" }] })).toBe("SERVER_LIBRARY.md");
  });
});
