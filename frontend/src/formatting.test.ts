// Tests for shared formatting utilities.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import {
  formatBytes,
  formatCost,
  formatElapsed,
  formatTokens,
  staleStateColor,
  stateColor,
  toolCallDetail,
} from "./formatting";

describe("formatTokens", () => {
  it("scales to the largest unit that keeps a token count readable", () => {
    expect(formatTokens(1_000_000_000)).toBe("1Gt");
    expect(formatTokens(1_500_000_000)).toBe("1.5Gt");
    expect(formatTokens(999_000_000)).toBe("999Mt");
    // Three significant digits escalate across a unit boundary instead of
    // printing 1000Mt.
    expect(formatTokens(999_900_000)).toBe("1Gt");
    expect(formatTokens(15_000_000)).toBe("15Mt");
    expect(formatTokens(500_000_000)).toBe("500Mt");
    expect(formatTokens(1_500)).toBe("1.5kt");
    expect(formatTokens(15_000)).toBe("15kt");
    expect(formatTokens(999)).toBe("999t");
  });
});

describe("formatCost", () => {
  it("separates an exact zero from a cost below one cent", () => {
    expect(formatCost(0)).toBe("$0");
    expect(formatCost(0.004)).toBe("<$0.01");
    expect(formatCost(1.234)).toBe("$1.23");
  });

  it("drops whole dollars' cents", () => {
    expect(formatCost(200)).toBe("$200");
    expect(formatCost(30.3)).toBe("$30.30");
    expect(formatCost(30.34)).toBe("$30.34");
  });
});

describe("formatBytes", () => {
  it("formats resident memory using binary units", () => {
    expect(formatBytes(2_097_152)).toBe("2.0 MiB");
  });

  it("keeps a fractional byte visible until an amount reaches ten", () => {
    expect(formatBytes(0.5)).toBe("0.5 B");
    expect(formatBytes(750.5)).toBe("751 B");
    expect(formatBytes(1_537)).toBe("1.5 KiB");
  });

  it("reports an absent amount without a decimal", () => {
    expect(formatBytes(0)).toBe("0 B");
  });
});

describe("formatElapsed", () => {
  it("formats process age in whole seconds", () => {
    expect(formatElapsed(90_000)).toBe("1m 30s");
  });
});

describe("stateColor", () => {
  it("uses green for busy task states", () => {
    const busyStates = ["pending", "branching", "provisioning", "starting", "running", "pulling", "pushing"] as const;

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
    expect(
      toolCallDetail("edit", {
        path: "gomode/docs/SERVER_LIBRARY.md",
        edits: [{ oldText: "a", newText: "b" }],
      }),
    ).toBe("SERVER_LIBRARY.md");
  });
});
