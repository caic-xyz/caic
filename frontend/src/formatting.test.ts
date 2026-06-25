// Tests for shared formatting utilities.
import { describe, expect, it } from "vitest";
import { stateColor, toolCallDetail } from "./formatting";

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
