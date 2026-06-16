// Tests for subagent input parsing and tool-call detail formatting.
import { describe, expect, it } from "vitest";
import { parseSubagentInput, subagentDetail, toolCallDetail } from "./formatting";

describe("parseSubagentInput", () => {
  it("parses a single spawn", () => {
    const info = parseSubagentInput({ agent: "reviewer", task: "Review", cwd: "/w" });
    expect(info.kind).toBe("single");
    expect(info.spawns).toEqual([{ agent: "reviewer", task: "Review", label: undefined, phase: undefined }]);
  });

  it("parses a parallel batch", () => {
    const info = parseSubagentInput({ tasks: [{ agent: "reviewer", task: "a" }, { agent: "reviewer", task: "b" }] });
    expect(info.kind).toBe("parallel");
    expect(info.spawns).toHaveLength(2);
  });

  it("parses a chain preserving order", () => {
    const info = parseSubagentInput({
      chain: [
        { parallel: [{ agent: "reviewer", task: "p1", phase: "Plan" }, { agent: "reviewer", task: "p2" }] },
        { agent: "worker", task: "do it" },
      ],
    });
    expect(info.kind).toBe("chain");
    expect(info.spawns.map((s) => s.agent)).toEqual(["reviewer", "reviewer", "worker"]);
    expect(info.spawns[0].phase).toBe("Plan");
  });

  it("treats introspection actions as spawning nothing", () => {
    const info = parseSubagentInput({ action: "list" });
    expect(info.kind).toBe("action");
    expect(info.action).toBe("list");
    expect(info.spawns).toHaveLength(0);
  });
});

describe("subagentDetail", () => {
  it("summarises a single spawn by label", () => {
    expect(subagentDetail({ agent: "reviewer", label: "security", task: "x" })).toBe("reviewer — security");
  });

  it("falls back to the task first line", () => {
    expect(subagentDetail({ agent: "planner", task: "\n  Plan it  \nmore" })).toBe("planner — Plan it");
  });

  it("counts agents for an orchestration", () => {
    expect(subagentDetail({ chain: [{ agent: "reviewer", task: "a" }, { agent: "reviewer", task: "b" }, { agent: "worker", task: "c" }] }))
      .toBe("chain · reviewer ×2, worker");
  });
});

describe("toolCallDetail", () => {
  it("routes agent/subagent tools through the subagent summary", () => {
    const input = { agent: "reviewer", task: "Review the diff" };
    expect(toolCallDetail("Agent", input)).toBe("reviewer — Review the diff");
    expect(toolCallDetail("subagent", input)).toBe("reviewer — Review the diff");
  });
});
