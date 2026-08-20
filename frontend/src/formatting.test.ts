// Tests for shared formatting utilities.

import { describe, expect, it } from "vitest";

import { formatBytes, formatElapsed, staleStateColor, stateColor, toolCallDetail } from "./formatting";

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
      expect(stateColor(state)).toBe("var(--color-state-active-bg)");
    }
  });

  it("uses the teardown tint for stopping and purging", () => {
    expect(stateColor("stopping")).toBe("var(--color-state-stopping-bg)");
    expect(stateColor("purging")).toBe("var(--color-state-stopping-bg)");
  });

  it("keeps distinct failure and attention states", () => {
    expect(stateColor("crashed")).toBe("var(--color-state-crashed-bg)");
    expect(stateColor("failed")).toBe("var(--color-state-failed-bg)");
    expect(stateColor("waiting")).toBe("var(--color-state-waiting-bg)");
  });

  it("mixes stale states toward danger", () => {
    expect(staleStateColor("running")).toBe(
      "color-mix(in srgb, var(--color-state-active-bg) 75%, var(--color-danger))",
    );
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
