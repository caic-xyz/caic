// Tests for tool-call detail formatting.
import { describe, expect, it } from "vitest";
import { toolCallDetail } from "./formatting";

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
